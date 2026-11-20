// Package store is the plugin's SQLite database (design: "Data model (the
// plugin's SQLite)"): owners, owner sessions, meetings and their
// participants, and an audit trail that never holds transcript text, guest
// names or codes.
//
// 🔴 Every owner-facing read and write goes through OwnerRepo, which is built
// only by ForOwner and carries the owner id in every statement. No handler
// writes its own `WHERE owner_id`; a handler that holds an OwnerRepo can only
// ever see that owner's rows.
//
// Times are stored as Unix milliseconds (UTC). Codes and tokens are stored
// only as their SHA-256 (Hash).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// Never edit one that has shipped: add the next. Later phases add segments,
// action_items, windows, recordings and summaries as their own migrations.
var migrations = []string{
	// 1: the 2a core.
	`CREATE TABLE owners (
		owner_id                 INTEGER PRIMARY KEY,
		owner_login              TEXT    NOT NULL DEFAULT '',
		token_ref                INTEGER NOT NULL,            -- the plugin id whose token serves this owner; never the token
		record_default           INTEGER NOT NULL DEFAULT 1 CHECK (record_default IN (0, 1)),
		ai_default               INTEGER NOT NULL DEFAULT 1 CHECK (ai_default IN (0, 1)),
		retention_days_recording INTEGER          DEFAULT 30,  -- NULL: forever
		retention_days_text      INTEGER          DEFAULT 365, -- NULL: forever
		updated_at               INTEGER NOT NULL
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
		id                 TEXT    PRIMARY KEY,
		owner_id           INTEGER NOT NULL,
		title              TEXT    NOT NULL DEFAULT '',
		room               TEXT    NOT NULL UNIQUE,
		join_code_sha256   TEXT    NOT NULL UNIQUE,
		state              TEXT    NOT NULL CHECK (state IN ('open', 'ended')),
		record             INTEGER NOT NULL CHECK (record IN (0, 1)),
		ai                 INTEGER NOT NULL CHECK (ai IN (0, 1)),
		created_at         INTEGER NOT NULL,
		started_at         INTEGER,
		ended_at           INTEGER,
		purge_recording_at INTEGER,
		purge_text_at      INTEGER
	) STRICT;
	CREATE INDEX meetings_owner_newest ON meetings (owner_id, created_at DESC, id DESC);
	CREATE INDEX meetings_open ON meetings (state, created_at) WHERE state = 'open';
	CREATE TABLE participants (
		id            TEXT    PRIMARY KEY,
		meeting_id    TEXT    NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
		kind          TEXT    NOT NULL CHECK (kind IN ('owner', 'guest')),
		display_name  TEXT    NOT NULL,
		identity      TEXT    NOT NULL UNIQUE,
		ticket_sha256 TEXT             UNIQUE,
		state         TEXT    NOT NULL CHECK (state IN ('waiting', 'admitted', 'denied', 'removed', 'left')),
		knocked_at    INTEGER,
		admitted_at   INTEGER,
		left_at       INTEGER
	) STRICT;
	CREATE INDEX participants_meeting ON participants (meeting_id);
	CREATE TABLE audit (
		at         INTEGER NOT NULL,
		owner_id   INTEGER,
		meeting_id TEXT,
		event      TEXT    NOT NULL,
		detail     TEXT    NOT NULL DEFAULT ''  -- never transcript text, item text, guest names or codes
	) STRICT;`,
}

// Store is the database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and brings its schema up to
// date. The directory is created 0700 and the file is 0600.
func Open(path string) (*Store, error) {
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
	s := &Store{db: db}
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func audit(ctx context.Context, x execer, now time.Time, ownerID int64, meetingID, event, detail string) error {
	var mid any
	if meetingID != "" {
		mid = meetingID
	}
	_, err := x.ExecContext(ctx, `INSERT INTO audit (at, owner_id, meeting_id, event, detail) VALUES (?, ?, ?, ?, ?)`,
		ms(now), ownerID, mid, event, detail)
	return err
}

// UpsertOwner records the owner a token serves, as /self names them. It
// never changes the owner's settings.
func (s *Store) UpsertOwner(ctx context.Context, ownerID int64, login string, pluginID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO owners (owner_id, owner_login, token_ref, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (owner_id) DO UPDATE SET owner_login = excluded.owner_login, token_ref = excluded.token_ref, updated_at = excluded.updated_at`,
		ownerID, login, pluginID, ms(now))
	return err
}

// Session is a live owner session.
type Session struct {
	OwnerID   int64
	PluginID  int64
	ExpiresAt time.Time
}

// CreateSession mints an owner session for ownerID, through the token of
// pluginID, valid SessionLifetime from now. It returns the token, which is
// stored only as its hash.
func (s *Store) CreateSession(ctx context.Context, ownerID, pluginID int64, now time.Time) (string, time.Time, error) {
	tok := NewSessionToken()
	exp := now.Add(SessionLifetime)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_sessions (token_sha256, owner_id, plugin_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		Hash(tok), ownerID, pluginID, ms(now), ms(exp)); err != nil {
		return "", time.Time{}, err
	}
	if err := audit(ctx, tx, now, ownerID, "", "session.created", fmt.Sprintf("plugin %d", pluginID)); err != nil {
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
	err := s.db.QueryRowContext(ctx, `SELECT owner_id, plugin_id, expires_at FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,
		Hash(token), ms(now)).Scan(&out.OwnerID, &out.PluginID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	out.ExpiresAt = time.UnixMilli(exp).UTC()
	return out, nil
}

// Revoked is what RevokePlugin did.
type Revoked struct {
	Sessions int64 // owner sessions deleted
	Meetings int64 // open meetings ended
}

// RevokePlugin acts on muster's word that pluginID's token is dead (a 401 on
// /self): every owner session minted through it is deleted and, when
// endMeetings, every open meeting of ownerID is ended. The caller passes
// endMeetings false only while the owner still has another live token.
func (s *Store) RevokePlugin(ctx context.Context, pluginID, ownerID int64, endMeetings bool, now time.Time) (Revoked, error) {
	var out Revoked
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE plugin_id = ?`, pluginID)
	if err != nil {
		return out, err
	}
	out.Sessions, _ = r.RowsAffected()
	if endMeetings {
		r, err = tx.ExecContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE owner_id = ? AND state = 'open'`, ms(now), ownerID)
		if err != nil {
			return out, err
		}
		out.Meetings, _ = r.RowsAffected()
	}
	if err := audit(ctx, tx, now, ownerID, "", "plugin.revoked",
		fmt.Sprintf("plugin %d: %d sessions deleted, %d meetings ended", pluginID, out.Sessions, out.Meetings)); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// ReconcileLive applies, at boot, what muster said about the plugin's
// tokens: every session minted through a plugin not in livePlugins is
// deleted, and every open meeting of an owner not in liveOwners is ended.
func (s *Store) ReconcileLive(ctx context.Context, livePlugins, liveOwners []int64, now time.Time) (Revoked, error) {
	var out Revoked
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM owner_sessions WHERE plugin_id NOT IN (SELECT value FROM json_each(?))`, jsonInts(livePlugins))
	if err != nil {
		return out, err
	}
	out.Sessions, _ = r.RowsAffected()
	r, err = tx.ExecContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE state = 'open' AND owner_id NOT IN (SELECT value FROM json_each(?))`,
		ms(now), jsonInts(liveOwners))
	if err != nil {
		return out, err
	}
	out.Meetings, _ = r.RowsAffected()
	if out.Sessions > 0 || out.Meetings > 0 {
		if err := audit(ctx, tx, now, 0, "", "boot.reconciled", fmt.Sprintf("%d sessions deleted, %d meetings ended", out.Sessions, out.Meetings)); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

func jsonInts(v []int64) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
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
	rows, err := tx.QueryContext(ctx, `UPDATE meetings SET state = 'ended', ended_at = ? WHERE state = 'open' AND created_at <= ? RETURNING id, owner_id`,
		ms(now), ms(now.Add(-LinkLifetime)))
	if err != nil {
		return 0, err
	}
	type ended struct {
		id    string
		owner int64
	}
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.id, &e.owner); err != nil {
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
		if err := audit(ctx, tx, now, e.owner, e.id, "meeting.ended", "the 24 h cap"); err != nil {
			return 0, err
		}
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
