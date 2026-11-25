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
// participants, segments, action items, recordings and the
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

	// 5: contacts, phase 1b (muster's docs/design/contacts.md: "How meetings
	// delivers it", "Failure behaviour"). The guest card: one per admitted
	// guest, its id stored only as its SHA-256, live 30 minutes, spent once
	// (sent or skipped). contacts_outbox: a card submission waiting to reach
	// the contacts plugin, written in the transaction that spends the card.
	// 🔴 body holds a guest's name and email: never logged, deleted on
	// delivery, on a 4xx that will never succeed, after 72 h, and on every
	// boot with no contacts URL configured. Every column added is NULL on
	// every existing row, and nothing an older build reads changes.
	// 🔴 One-way, like every migration here: an older build refuses a
	// database at schema 5 ("newer than this build"). Roll back by unsetting
	// the two contacts variables (README: "Contacts (optional)"), never by
	// running an older image on this database.
	`ALTER TABLE participants ADD COLUMN card_sha256 TEXT;
	ALTER TABLE participants ADD COLUMN card_expires_at INTEGER;
	ALTER TABLE participants ADD COLUMN card_state TEXT CHECK (card_state IN ('offered', 'sent', 'skipped'));
	CREATE UNIQUE INDEX participants_card ON participants (card_sha256) WHERE card_sha256 IS NOT NULL;
	CREATE TABLE contacts_outbox (
		id         INTEGER PRIMARY KEY,
		key        TEXT    NOT NULL UNIQUE, -- the Idempotency-Key, e.g. card:<participant id>
		kind       TEXT    NOT NULL CHECK (kind IN ('submission', 'attendance')),
		path       TEXT    NOT NULL,        -- the contacts route it is POSTed to
		body       TEXT    NOT NULL,        -- the JSON body: a guest's name and email; never logged
		created_at INTEGER NOT NULL,
		next_at    INTEGER NOT NULL,
		attempts   INTEGER NOT NULL DEFAULT 0
	) STRICT;
	CREATE INDEX contacts_outbox_due ON contacts_outbox (next_at, id);`,

	// 6: phase 5, the recording (design: "Media: LiveKit, TURN, Egress",
	// "Retention"; recordings.go). The owner's recording retention (30, 90 or
	// 365 days, or 0: forever, the default) and each meeting's purge time,
	// stamped when it ends with a finite retention (NULL: kept forever), or set
	// to the moment of Delete now. recordings is one row per room composite egress: a PART of the
	// meeting's recording, its object, and its file's start in MEETING TIME
	// as the egress reported it (start_ms; NULL until the egress says when
	// its file began). recording_gaps are the stretches of a meeting that
	// should have been recorded and were not (the egress starting, a failed
	// egress until its successor began), as the transcript's gap markers
	// are. recording_shares are the recording's capability links, stored
	// only as SHA-256. screen_shares are when a screen was shared, in
	// meeting time (the post-call analysis samples frames inside them).
	// Every column added to an existing table is NULL or its default on every
	// existing row, and nothing an older build reads changes. 🔴 One-way,
	// like every migration here: an older build refuses a database at
	// schema 6 ("newer than this build"); roll back by unsetting the S3
	// variables (README: "Recording (optional)"), never by running an older
	// image on this database.
	`ALTER TABLE owners ADD COLUMN retention_days_recording INTEGER NOT NULL DEFAULT 0 CHECK (retention_days_recording IN (0, 30, 90, 365)); -- 0: forever
	ALTER TABLE meetings ADD COLUMN purge_recording_at INTEGER; -- NULL: kept forever (or not ended)
	CREATE INDEX meetings_purge_recording ON meetings (purge_recording_at) WHERE purge_recording_at IS NOT NULL;
	CREATE TABLE recordings (
		id           INTEGER PRIMARY KEY,
		meeting_id   TEXT    NOT NULL REFERENCES meetings (id),
		part         INTEGER NOT NULL CHECK (part >= 1),
		egress_id    TEXT    NOT NULL DEFAULT '',
		object_key   TEXT    NOT NULL,
		state        TEXT    NOT NULL CHECK (state IN ('starting', 'active', 'complete', 'failed')),
		requested_at INTEGER NOT NULL,  -- Unix ms: when the plugin asked for the egress
		start_ms     INTEGER,           -- meeting time of the file's first frame, from the egress's own report
		duration_ms  INTEGER,
		bytes        INTEGER,
		ended_at     INTEGER,           -- Unix ms
		error        TEXT    NOT NULL DEFAULT '', -- a fixed phrase or an egress status, never Egress's message
		egress_ended INTEGER NOT NULL DEFAULT 0 CHECK (egress_ended IN (0, 1)), -- its egress's end was observed (or none exists): its object is final
		unseen_since INTEGER,           -- Unix ms: the first reconcile pass LiveKit did not list its egress; NULL once listed again
		UNIQUE (meeting_id, part)
	) STRICT;
	CREATE INDEX recordings_egress ON recordings (egress_id) WHERE egress_id != '';
	CREATE TABLE recording_gaps (
		id         INTEGER PRIMARY KEY,
		meeting_id TEXT    NOT NULL REFERENCES meetings (id),
		from_ms    INTEGER NOT NULL,
		to_ms      INTEGER,             -- NULL while it is open
		why        TEXT    NOT NULL CHECK (why IN ('starting', 'failed'))
	) STRICT;
	CREATE UNIQUE INDEX recording_gaps_open ON recording_gaps (meeting_id) WHERE to_ms IS NULL;
	CREATE TABLE recording_shares (
		token_sha256 TEXT    PRIMARY KEY, -- the link's token is never stored
		meeting_id   TEXT    NOT NULL REFERENCES meetings (id),
		created_at   INTEGER NOT NULL,
		revoked_at   INTEGER
	) STRICT;
	CREATE INDEX recording_shares_meeting ON recording_shares (meeting_id);
	CREATE TABLE screen_shares (
		track      TEXT    PRIMARY KEY,
		meeting_id TEXT    NOT NULL REFERENCES meetings (id),
		identity   TEXT    NOT NULL,
		speaker    TEXT    NOT NULL,   -- a name: render as text only, never logged
		start_ms   INTEGER NOT NULL,
		end_ms     INTEGER             -- NULL while it is shared
	) STRICT;
	CREATE INDEX screen_shares_meeting ON screen_shares (meeting_id, start_ms);`,

	// 7: the post-call pass (design: "Action items"; decisions
	// 1–4 and 7). ai_runs is one row per analysis of a meeting: the live
	// worker's (kind live, one per meeting) and every post-call pass (kind
	// post: at End, and each reprocess), with the models and provider
	// preferences that made it; exactly one run per meeting is CURRENT (the
	// newest finished one, or the one the owner reverted to). ai_items,
	// ai_items and ai_notes are what a run made, each citing transcript
	// segment ids (and frame times). mail_drafts is one email per recipient
	// per run; 🔴 a recipient has at most ONE row queued, sending, sent or
	// of unknown outcome per meeting (mail_drafts_one_send), so nothing a
	// reprocess does can send a second email or change a sent one.
	// participants.card_email is the address a guest typed on the card,
	// kept for the drafts: cleared when their notes are sent, and for every
	// meeting MailKeepDays after it ended. owners gain the owner's address,
	// confirmed by a mailed link (its token stored only as SHA-256).
	// recording_shares gain a kind: the notes' video links (mail) are not
	// turned off by the owner sharing again. agent_tokens are owner-minted,
	// read-only, one-meeting tokens for a muster agent (SHA-256 only).
	// 🔴 One-way, like every migration here: an older build refuses a
	// database at schema 7; roll back by unsetting OPENROUTER_API_KEY and
	// MEETINGS_SMTP_ADDR (README: "Post-call notes"), never by running an
	// older image on this database. Every column added is NULL or its
	// default on every existing row.
	`ALTER TABLE owners ADD COLUMN email TEXT;
	ALTER TABLE owners ADD COLUMN email_pending TEXT;
	ALTER TABLE owners ADD COLUMN email_token_sha256 TEXT;
	ALTER TABLE owners ADD COLUMN email_token_expires_at INTEGER;
	CREATE UNIQUE INDEX owners_email_token ON owners (email_token_sha256) WHERE email_token_sha256 IS NOT NULL;
	ALTER TABLE participants ADD COLUMN card_email TEXT; -- a guest's address from the card: never logged
	ALTER TABLE meetings ADD COLUMN ai_frames INTEGER NOT NULL DEFAULT 0 CHECK (ai_frames IN (0, 1)); -- its notice named the frames of shared screens
	ALTER TABLE recording_shares ADD COLUMN kind TEXT NOT NULL DEFAULT 'owner' CHECK (kind IN ('owner', 'mail'));
	CREATE TABLE ai_runs (
		id           INTEGER PRIMARY KEY,
		meeting_id   TEXT    NOT NULL REFERENCES meetings (id),
		kind         TEXT    NOT NULL CHECK (kind IN ('live', 'post')),
		items_model  TEXT    NOT NULL,
		items_prefs  TEXT    NOT NULL,           -- the provider object as sent (JSON)
		vision_model TEXT    NOT NULL DEFAULT '', -- '' for no frame analysis
		vision_prefs TEXT    NOT NULL DEFAULT '',
		state        TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),
		current      INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0, 1)),
		attempts     INTEGER NOT NULL DEFAULT 0,
		next_at      INTEGER NOT NULL,
		anchor       INTEGER NOT NULL DEFAULT 0, -- live: the last segment a successful call covered
		failures     INTEGER NOT NULL DEFAULT 0, -- live: failed calls in a row
		partial      INTEGER NOT NULL DEFAULT 0 CHECK (partial IN (0, 1)),
		error        TEXT    NOT NULL DEFAULT '', -- a fixed phrase, never a model's words
		requested_by TEXT    NOT NULL DEFAULT '' CHECK (requested_by IN ('', 'end', 'owner', 'admin')),
		created_at   INTEGER NOT NULL,
		finished_at  INTEGER
	) STRICT;
	CREATE INDEX ai_runs_meeting ON ai_runs (meeting_id, id);
	CREATE INDEX ai_runs_due ON ai_runs (state, next_at) WHERE state IN ('queued', 'running');
	CREATE UNIQUE INDEX ai_runs_live ON ai_runs (meeting_id) WHERE kind = 'live';
	CREATE UNIQUE INDEX ai_runs_current ON ai_runs (meeting_id) WHERE current = 1;
	CREATE TABLE ai_items (
		id         INTEGER PRIMARY KEY,
		run_id     INTEGER NOT NULL REFERENCES ai_runs (id),
		source     TEXT    NOT NULL CHECK (source IN ('live', 'transcript', 'frames')),
		text       TEXT    NOT NULL, -- a model's words over what was said: untrusted, never logged
		assignee   TEXT    NOT NULL DEFAULT '',
		due        TEXT    NOT NULL DEFAULT '',
		segments   TEXT    NOT NULL, -- JSON segment ids
		frames     TEXT    NOT NULL DEFAULT '[]', -- JSON meeting times
		created_at INTEGER NOT NULL
	) STRICT;
	CREATE INDEX ai_items_run ON ai_items (run_id, id);
	CREATE TABLE ai_notes (
		id       INTEGER PRIMARY KEY,
		run_id   INTEGER NOT NULL REFERENCES ai_runs (id),
		frame_ms INTEGER NOT NULL,
		text     TEXT    NOT NULL,
		segments TEXT    NOT NULL
	) STRICT;
	CREATE INDEX ai_notes_run ON ai_notes (run_id, frame_ms);
	CREATE TABLE mail_drafts (
		id          TEXT    PRIMARY KEY,           -- random; the Message-ID's local part
		meeting_id  TEXT    NOT NULL REFERENCES meetings (id),
		run_id      INTEGER NOT NULL REFERENCES ai_runs (id),
		recipient   TEXT    NOT NULL,              -- 'owner', or the guest's participant id
		name        TEXT    NOT NULL,              -- the name they spoke under
		email       TEXT,                          -- a guest's address; NULL for the owner's (read at send), once sent, or purged
		items       TEXT    NOT NULL,              -- JSON [{text, assignee, due}], as the owner left them
		state       TEXT    NOT NULL CHECK (state IN ('draft', 'removed', 'superseded', 'queued', 'sending', 'sent', 'failed', 'unknown')),
		attempts    INTEGER NOT NULL DEFAULT 0,
		next_at     INTEGER,
		approved_at INTEGER,
		sent_at     INTEGER,
		error       TEXT    NOT NULL DEFAULT '',   -- a fixed phrase and a reply code
		changed     INTEGER NOT NULL DEFAULT 0 CHECK (changed IN (0, 1)), -- a reprocess changed the items the owner had edited
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	) STRICT;
	CREATE INDEX mail_drafts_meeting ON mail_drafts (meeting_id, run_id);
	CREATE INDEX mail_drafts_due ON mail_drafts (state, next_at) WHERE state = 'queued';
	CREATE UNIQUE INDEX mail_drafts_one_send ON mail_drafts (meeting_id, recipient) WHERE state IN ('queued', 'sending', 'sent', 'unknown');
	CREATE TABLE agent_tokens (
		token_sha256 TEXT    PRIMARY KEY,
		meeting_id   TEXT    NOT NULL REFERENCES meetings (id),
		subject      TEXT    NOT NULL REFERENCES owners (subject),
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	) STRICT;`,

	// 8: the owner's MCP tokens (README: "Using Meetings from Claude Code
	// (MCP)"). One row per token an owner minted in Meetings settings for
	// their own Claude Code session: stored ONLY as its SHA-256, shown once,
	// no expiry, revoked by the owner (revoked_at) or by muster ending the
	// app for them (EndSubjects, EndAll). id is the token's public handle
	// in the settings list, not a credential. 🔴 One-way like every
	// migration here: an older build refuses a database at schema 8; to
	// turn the MCP door off, revoke every token (nothing else depends on
	// the table).
	`CREATE TABLE mcp_tokens (
		id           TEXT    PRIMARY KEY,
		token_sha256 TEXT    NOT NULL UNIQUE,
		subject      TEXT    NOT NULL REFERENCES owners (subject),
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER,
		revoked_at   INTEGER
	) STRICT;
	CREATE INDEX mcp_tokens_subject ON mcp_tokens (subject, created_at);`,
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
	// 🔴 Every MCP token dies with the app: a reinstall does not bring one
	// back.
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = ? WHERE revoked_at IS NULL`, ms(now)); err != nil {
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
	// 🔴 Their MCP tokens too (made at or before asOf, like the sessions):
	// a person muster no longer lets the app act for is not served by
	// a token they minted earlier.
	if _, err := tx.ExecContext(ctx, `UPDATE mcp_tokens SET revoked_at = ? WHERE revoked_at IS NULL AND subject IN (SELECT value FROM json_each(?)) AND created_at <= ?`,
		ms(now), list, ms(asOf)); err != nil {
		return out, err
	}
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
// unexpired owner session, an open meeting, or an unrevoked MCP token. They
// are the subjects the owners/check poll asks muster about (so a person
// muster revokes loses their MCP tokens even with no session open).
func (s *Store) HeldSubjects(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT subject FROM owner_sessions WHERE expires_at > ?
		UNION SELECT subject FROM meetings WHERE state = 'open'
		UNION SELECT subject FROM mcp_tokens WHERE revoked_at IS NULL ORDER BY 1`, ms(now))
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
