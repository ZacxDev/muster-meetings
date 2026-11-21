// Package store is the plugin's SQLite database (design: "Data model (the
// plugin's SQLite)"): owners, owner sessions, meetings, and an audit trail
// that never holds transcript text, guest names or codes.
//
// An owner is a person muster's server-app redeem named, known only by the
// pairwise subject it gave and a display name. 🔴 Nothing here holds a muster
// user id or an email (design: fork 2); the plugin never receives either.
//
// 🔴 Every owner-facing read and write goes through OwnerRepo, which is built
// only by ForSession or ForSubject and carries the subject in every
// statement. No handler writes its own `WHERE subject`; a handler that holds
// an OwnerRepo can only ever see that person's rows.
//
// Times are stored as Unix milliseconds (UTC). Tokens are stored only as
// their SHA-256 (Hash). A meeting's join code is not stored at all: it is
// derived from MEETINGS_LINK_KEY and the meeting id (JoinCode), and only its
// SHA-256 is kept, for the guest link's lookup.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, no cgo
)

// SessionLifetime is how long an owner session lives (decision 12).
const SessionLifetime = 8 * time.Hour

// LinkLifetime is the longest a meeting stays open, and so its link: 24 h
// after it was created.
const LinkLifetime = 24 * time.Hour

// ErrNotFound is a row that does not exist or is not this owner's.
var ErrNotFound = errors.New("not found")

// migrations are applied in order; PRAGMA user_version counts those done.
// Never edit one that has shipped: add the next. Later phases add
// participants, segments, action items, recordings, summaries and the
// owner's settings as their own migrations.
var migrations = []string{
	// 1: the 2a core.
	`CREATE TABLE owners (
		owner_id    INTEGER PRIMARY KEY,
		owner_login TEXT    NOT NULL DEFAULT '',
		token_ref   INTEGER NOT NULL, -- the plugin id whose token serves this owner; never the token
		updated_at  INTEGER NOT NULL
	) STRICT;
	CREATE TABLE owner_sessions (
		token_sha256 TEXT    PRIMARY KEY,
		owner_id     INTEGER NOT NULL,
		plugin_id    INTEGER NOT NULL,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	) STRICT;
	CREATE INDEX owner_sessions_plugin ON owner_sessions (plugin_id);
	CREATE TABLE meetings (
		id               TEXT    PRIMARY KEY,
		owner_id         INTEGER NOT NULL,
		title            TEXT    NOT NULL DEFAULT '',
		room             TEXT    NOT NULL UNIQUE,
		join_code_sha256 TEXT    NOT NULL UNIQUE, -- the guest link's lookup; the code itself is never stored
		state            TEXT    NOT NULL CHECK (state IN ('open', 'ended')),
		record           INTEGER NOT NULL CHECK (record IN (0, 1)),
		ai               INTEGER NOT NULL CHECK (ai IN (0, 1)),
		created_at       INTEGER NOT NULL,
		started_at       INTEGER,
		ended_at         INTEGER
	) STRICT;
	CREATE INDEX meetings_owner_newest ON meetings (owner_id, created_at DESC, id DESC);
	CREATE INDEX meetings_open ON meetings (state, created_at) WHERE state = 'open';
	CREATE TABLE audit (
		at         INTEGER NOT NULL,
		owner_id   INTEGER,
		meeting_id TEXT,
		event      TEXT    NOT NULL,
		detail     TEXT    NOT NULL DEFAULT ''  -- never transcript text, item text, guest names or codes
	) STRICT;`,

	// 2: core PR 1h, the server-installed app (design: "What changes in the
	// plugin's phase 2a"). One server token serves every person, who is known
	// only by the pairwise subject the redeem gives: owners, sessions,
	// meetings and audit are keyed on it, and nothing holds a muster user id
	// or an email. 2a's tables are dropped and made again with nothing
	// carried over: migrate refuses this step on a database that holds 2a
	// owners or meetings (ErrNeedsSubjectMap), since those are keyed on muster
	// user ids and need a one-time map to subjects first.
	`DROP TABLE audit;
	DROP TABLE meetings;
	DROP TABLE owner_sessions;
	DROP TABLE owners;
	CREATE TABLE owners (
		subject      TEXT    PRIMARY KEY CHECK (length(subject) = 22), -- the redeem's pairwise subject; never a muster user id
		display_name TEXT    NOT NULL DEFAULT '',                       -- from the redeem; never an email
		updated_at   INTEGER NOT NULL
	) STRICT;
	CREATE TABLE owner_sessions (
		token_sha256 TEXT    PRIMARY KEY,
		subject      TEXT    NOT NULL REFERENCES owners (subject),
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	) STRICT;
	CREATE INDEX owner_sessions_subject ON owner_sessions (subject);
	CREATE TABLE meetings (
		id               TEXT    PRIMARY KEY,
		subject          TEXT    NOT NULL REFERENCES owners (subject),
		title            TEXT    NOT NULL DEFAULT '',
		room             TEXT    NOT NULL UNIQUE,
		join_code_sha256 TEXT    NOT NULL UNIQUE, -- the guest link's lookup; the code itself is never stored
		state            TEXT    NOT NULL CHECK (state IN ('open', 'ended')),
		record           INTEGER NOT NULL CHECK (record IN (0, 1)),
		ai               INTEGER NOT NULL CHECK (ai IN (0, 1)),
		created_at       INTEGER NOT NULL,
		started_at       INTEGER,
		ended_at         INTEGER
	) STRICT;
	CREATE INDEX meetings_subject_newest ON meetings (subject, created_at DESC, id DESC);
	CREATE INDEX meetings_open ON meetings (state, created_at) WHERE state = 'open';
	CREATE TABLE audit (
		at         INTEGER NOT NULL,
		subject    TEXT,
		meeting_id TEXT,
		event      TEXT    NOT NULL,
		detail     TEXT    NOT NULL DEFAULT ''  -- never transcript text, item text, guest names, display names or codes
	) STRICT;`,

	// 3: phase 2b, the lobby (design: "Guests: links, the lobby, admission").
	// One row per knock. The ticket and the LiveKit identity are random; the
	// ticket is stored only as its SHA-256. display_name is what the guest
	// typed (already checked: lobby.CleanName): shown to the owner as text,
	// and never logged or audited.
	`CREATE TABLE participants (
		id            TEXT    PRIMARY KEY,
		meeting_id    TEXT    NOT NULL REFERENCES meetings (id),
		kind          TEXT    NOT NULL CHECK (kind IN ('owner', 'guest')),
		display_name  TEXT    NOT NULL,
		identity      TEXT    NOT NULL UNIQUE,
		ticket_sha256 TEXT    NOT NULL UNIQUE,
		state         TEXT    NOT NULL CHECK (state IN ('waiting', 'admitted', 'denied', 'removed', 'left', 'ended')),
		knocked_at    INTEGER NOT NULL,
		admitted_at   INTEGER,
		left_at       INTEGER
	) STRICT;
	CREATE INDEX participants_meeting ON participants (meeting_id, knocked_at, id);`,

	// 4: phase 3a, the transcript (design: "Segments", "Media: LiveKit,
	// TURN, Egress"). tracks is one row per microphone track the plugin
	// transcribes: the track egress feeding it (egress_id) and that egress's
	// nonce, which the ingest URL's token must carry (internal/ingest), and
	// the open gap marker while no audio flows. segments is the stored
	// transcript: finals and gap markers (partials are never stored), in
	// meeting time (ms from the meeting's start). speaker is the name shown
	// for the line when it was said (the owner's display name, or what the
	// guest typed and "(guest)"): text a stranger typed, rendered as text.
	// text is what was SAID: untrusted, never logged, never audited.
	`CREATE TABLE tracks (
		track        TEXT    PRIMARY KEY,
		meeting_id   TEXT    NOT NULL REFERENCES meetings (id),
		identity     TEXT    NOT NULL,
		speaker      TEXT    NOT NULL,
		state        TEXT    NOT NULL CHECK (state IN ('active', 'ended')),
		nonce        TEXT    NOT NULL DEFAULT '',
		egress_id    TEXT    NOT NULL DEFAULT '',
		gap_segment  INTEGER,
		published_at INTEGER NOT NULL
	) STRICT;
	CREATE INDEX tracks_meeting ON tracks (meeting_id, state);
	CREATE INDEX tracks_egress ON tracks (egress_id) WHERE egress_id != '';
	CREATE TABLE segments (
		id         INTEGER PRIMARY KEY,
		meeting_id TEXT    NOT NULL REFERENCES meetings (id),
		identity   TEXT    NOT NULL,
		speaker    TEXT    NOT NULL,
		kind       TEXT    NOT NULL CHECK (kind IN ('text', 'gap')),
		start_ms   INTEGER NOT NULL,
		end_ms     INTEGER,             -- NULL only for a gap still open
		text       TEXT    NOT NULL DEFAULT '',
		engine     TEXT    NOT NULL,
		pass       TEXT    NOT NULL DEFAULT 'live' CHECK (pass IN ('live', 'final'))
	) STRICT;
	CREATE INDEX segments_meeting ON segments (meeting_id, id);`,
}

// ErrNeedsSubjectMap is a phase-2a database that holds owners or meetings:
// they are keyed on muster user ids, and this schema keys on subjects.
var ErrNeedsSubjectMap = errors.New("this database holds phase 2a owners or meetings, keyed on muster user ids; " +
	"the server-app schema keys on the subjects muster's redeem gives, so they need a one-time map from owner_id to subject " +
	"before this build can open it (design: \"What changes in the plugin's phase 2a\")")

// needsSubjectMap says a database at schema 1 holds rows migration 2 would
// drop.
func needsSubjectMap(tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT (SELECT count(*) FROM owners) + (SELECT count(*) FROM meetings)`).Scan(&n)
	return n > 0, err
}

// Store is the database.
type Store struct {
	db      *sql.DB
	linkKey []byte
}

// Open opens (or creates) the database at path and brings its schema up to
// date. The directory is created 0700 and the file is 0600. linkKey is
// MEETINGS_LINK_KEY (ParseLinkKey): 32 bytes, or Open refuses.
func Open(path string, linkKey []byte) (*Store, error) {
	if len(linkKey) != LinkKeyBytes {
		return nil, ErrLinkKey
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, err
	} else {
		f.Close()
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One writer (design: one replica, one SQLite file). A single connection
	// serialises every statement, so no request ever sees SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, linkKey: append([]byte(nil), linkKey...)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("the database is at schema %d, newer than this build's %d: run a newer muster-meetings", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if i == 1 {
			if held, err := needsSubjectMap(tx); err != nil || held {
				tx.Rollback()
				if err == nil {
					err = ErrNeedsSubjectMap
				}
				return err
			}
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func audit(ctx context.Context, x execer, now time.Time, subject, meetingID, event, detail string) error {
	var sub, mid any
	if subject != "" {
		sub = subject
	}
	if meetingID != "" {
		mid = meetingID
	}
	_, err := x.ExecContext(ctx, `INSERT INTO audit (at, subject, meeting_id, event, detail) VALUES (?, ?, ?, ?, ?)`,
		ms(now), sub, mid, event, detail)
	return err
}

// Session is a live owner session.
type Session struct {
	// TokenSHA256 is the stored form of the bearer that opened it.
	TokenSHA256 string
	// Subject is the person it is for: the pairwise subject muster's redeem
	// gave.
	Subject   string
	ExpiresAt time.Time
}

// ErrBadSubject is a subject that is not 22 base64url characters.
var ErrBadSubject = errors.New("not a subject")

// CreateSession records the person a redeem named (subject and display
// name) and mints their owner session, valid SessionLifetime from now. It
// returns the token, which is stored only as its hash.
func (s *Store) CreateSession(ctx context.Context, subject, displayName string, now time.Time) (string, time.Time, error) {
	if !ValidSubject(subject) {
		return "", time.Time{}, ErrBadSubject
	}
	tok := NewSessionToken()
	exp := now.Add(SessionLifetime)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owners (subject, display_name, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (subject) DO UPDATE SET display_name = excluded.display_name, updated_at = excluded.updated_at`,
		subject, displayName, ms(now)); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_sessions (token_sha256, subject, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		Hash(tok), subject, ms(now), ms(exp)); err != nil {
		return "", time.Time{}, err
	}
	if err := audit(ctx, tx, now, subject, "", "session.created", ""); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, tx.Commit()
}

// LookupSession returns the live session token opens at now. A malformed,
// unknown or expired token is ErrNotFound.
func (s *Store) LookupSession(ctx context.Context, token string, now time.Time) (Session, error) {
	if !ValidSessionToken(token) {
		return Session{}, ErrNotFound
	}
	var out Session
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT subject, expires_at FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,
		Hash(token), ms(now)).Scan(&out.Subject, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	out.ExpiresAt = time.UnixMilli(exp).UTC()
	out.TokenSHA256 = Hash(token)
	return out, nil
}

// Ended is what EndAll or EndSubjects did.
type Ended struct {
	Sessions int64 // owner sessions deleted
	Meetings int64 // open meetings ended
}

// EndAll acts on muster's word that the server token is dead (a 401: the
// app was uninstalled, or its token rotated away): every owner session is
// deleted and every open meeting ended, in one transaction.
func (s *Store) EndAll(ctx context.Context, why string, now time.Time) (Ended, error) {
	var out Ended
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE 1`)
	if err != nil {
		return out, err
	}
	out.Sessions, _ = r.RowsAffected()
	r, err = tx.ExecContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE state = 'open'`, ms(now))
	if err != nil {
		return out, err
	}
	out.Meetings, _ = r.RowsAffected()
	if err := endParticipants(ctx, tx, now); err != nil {
		return out, err
	}
	if err := audit(ctx, tx, now, "", "", "app.revoked",
		fmt.Sprintf("%s: %d sessions deleted, %d meetings ended", why, out.Sessions, out.Meetings)); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// EndSubjects acts on muster's word that the app may no longer act for these
// people (owners/check answered them revoked): every session of theirs, and
// every open meeting of theirs, created at or before asOf is ended, in one
// transaction. asOf is when the check was sent: a session or meeting the
// person made after it (they reopened the tab while the answer was on its
// way) is newer than what muster answered about, and is kept. No one
// else's row is touched.
func (s *Store) EndSubjects(ctx context.Context, subjects []string, asOf, now time.Time) (Ended, error) {
	var out Ended
	if len(subjects) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	list := jsonStrings(subjects)
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE subject IN (SELECT value FROM json_each(?)) AND created_at <= ?`, list, ms(asOf))
	if err != nil {
		return out, err
	}
	out.Sessions, _ = r.RowsAffected()
	rows, err := tx.QueryContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE state = 'open' AND subject IN (SELECT value FROM json_each(?)) AND created_at <= ? RETURNING id, subject`,
		ms(now), list, ms(asOf))
	if err != nil {
		return out, err
	}
	type ended struct{ id, subject string }
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.id, &e.subject); err != nil {
			rows.Close()
			return out, err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Meetings = int64(len(all))
	for _, e := range all {
		if err := audit(ctx, tx, now, e.subject, e.id, "meeting.ended", "muster no longer lets the app act for its owner"); err != nil {
			return out, err
		}
	}
	if err := endParticipants(ctx, tx, now); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// EndSessions acts on muster's word that these people's grants expired:
// every owner session of theirs created at or before asOf (when the check
// was sent; see EndSubjects) is deleted, and their open meetings are left
// running for the guests. It returns the sessions deleted.
func (s *Store) EndSessions(ctx context.Context, subjects []string, asOf, now time.Time) (int64, error) {
	if len(subjects) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE subject IN (SELECT j.value FROM json_each(?) AS j) AND created_at <= ?`, jsonStrings(subjects), ms(asOf))
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if err := audit(ctx, tx, now, "", "", "grants.expired", fmt.Sprintf("%d people, %d sessions deleted", len(subjects), n)); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// HeldSubjects is every person the plugin holds something live for: an
// unexpired owner session or an open meeting. They are the subjects the
// owners/check poll asks muster about.
func (s *Store) HeldSubjects(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject FROM owner_sessions WHERE expires_at > ?
		UNION SELECT subject FROM meetings WHERE state = 'open' ORDER BY 1`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func jsonStrings(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Sweep ends every meeting open LinkLifetime past its creation, and deletes
// expired sessions. It returns the number of meetings it ended.
func (s *Store) Sweep(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE expires_at <= ?`, ms(now)); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE state = 'open' AND created_at <= ? RETURNING id, subject`,
		ms(now), ms(now.Add(-LinkLifetime)))
	if err != nil {
		return 0, err
	}
	type ended struct{ id, subject string }
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.id, &e.subject); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range all {
		if err := audit(ctx, tx, now, e.subject, e.id, "meeting.ended", "the 24 h cap"); err != nil {
			return 0, err
		}
	}
	if err := endParticipants(ctx, tx, now); err != nil {
		return 0, err
	}
	return int64(len(all)), tx.Commit()
}

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// CountSessions is the number of owner sessions stored, live or not yet
// swept.
func (s *Store) CountSessions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM owner_sessions`).Scan(&n)
	return n, err
}
