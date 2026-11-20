package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// OwnerRepo is one owner's view of the database. 🔴 Every statement in it is
// filtered by the owner it was built for; that filter is the ownership gate
// (design: "Every owner-facing query is filtered by the session's owner_id in
// one repository layer").
type OwnerRepo struct {
	s       *Store
	ownerID int64
	// session is the stored hash of the bearer this repository was built
	// from (ForSession), "" for ForOwner. A write needs it: see CreateMeeting.
	session string
}

// ForSession is the repository of a live owner session: what the server
// builds for an owner request, never from anything the request names.
func (s *Store) ForSession(sess Session) *OwnerRepo {
	return &OwnerRepo{s: s, ownerID: sess.OwnerID, session: sess.TokenSHA256}
}

// ForOwner is ownerID's read-only repository: it can read the owner's
// meetings but not make one (it holds no session).
func (s *Store) ForOwner(ownerID int64) *OwnerRepo { return &OwnerRepo{s: s, ownerID: ownerID} }

// OwnerID is the owner this repository serves.
func (r *OwnerRepo) OwnerID() int64 { return r.ownerID }

// Meeting is one meeting as its owner sees it. It carries no code: the owner
// gets the link from Link, and only while the meeting is open.
type Meeting struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	State     string     `json:"state"` // open, ended
	Record    bool       `json:"record"`
	AI        bool       `json:"ai"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	// LinkExpiresAt is when the link dies even if the meeting is never
	// ended: LinkLifetime after creation.
	LinkExpiresAt time.Time `json:"link_expires_at"`
}

// Created is a meeting just made, with its join code.
type Created struct {
	Meeting  Meeting
	JoinCode string
}

const meetingCols = `id, title, state, record, ai, created_at, started_at, ended_at`

func scanMeeting(sc interface{ Scan(...any) error }) (Meeting, error) {
	var m Meeting
	var rec, ai int
	var created int64
	var started, ended sql.NullInt64
	if err := sc.Scan(&m.ID, &m.Title, &m.State, &rec, &ai, &created, &started, &ended); err != nil {
		return Meeting{}, err
	}
	m.Record, m.AI = rec == 1, ai == 1
	m.CreatedAt = time.UnixMilli(created).UTC()
	m.StartedAt, m.EndedAt = fromMS(started), fromMS(ended)
	m.LinkExpiresAt = m.CreatedAt.Add(LinkLifetime)
	return m, nil
}

// ErrNoSession is a write from a repository whose session is gone: deleted
// by a revocation, or expired. The server answers it 401.
var ErrNoSession = errors.New("the owner session is gone")

// CreateMeeting makes an open meeting, record and AI on (decision 9).
//
// 🔴 The insert happens only while the repository's session still exists,
// checked inside the insert itself. A revocation deletes the sessions and
// ends the owner's open meetings in one transaction, and the database runs
// one transaction at a time, so a create that raced a revocation either
// committed first (and the revocation ends its meeting) or finds its session
// gone (ErrNoSession): never an open meeting for a revoked owner.
func (r *OwnerRepo) CreateMeeting(ctx context.Context, now time.Time) (Created, error) {
	if r.session == "" {
		return Created{}, ErrNoSession
	}
	id := NewMeetingID()
	code := r.s.JoinCode(id)
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO meetings (id, owner_id, room, join_code_sha256, state, record, ai, created_at)
		SELECT ?, ?, ?, ?, 'open', 1, 1, ?
		WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND owner_id = ? AND expires_at > ?)`,
		id, r.ownerID, "meeting-"+id, Hash(code), ms(now), r.session, r.ownerID, ms(now))
	if err != nil {
		return Created{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Created{}, ErrNoSession
	}
	if err := audit(ctx, tx, now, r.ownerID, id, "meeting.created", ""); err != nil {
		return Created{}, err
	}
	m, err := scanMeeting(tx.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND owner_id = ?`, id, r.ownerID))
	if err != nil {
		return Created{}, err
	}
	if err := tx.Commit(); err != nil {
		return Created{}, err
	}
	return Created{Meeting: m, JoinCode: code}, nil
}

// Cursor is a page boundary in the newest-first list: the last meeting of
// the previous page.
type Cursor struct {
	CreatedAt int64 // ms
	ID        string
}

// String is the cursor's opaque form, "<ms>.<id>".
func (c Cursor) String() string { return strconv.FormatInt(c.CreatedAt, 10) + "." + c.ID }

// ParseCursor reads String's form.
func ParseCursor(s string) (Cursor, bool) {
	at, id, ok := strings.Cut(s, ".")
	if !ok || !ValidMeetingID(id) {
		return Cursor{}, false
	}
	v, err := strconv.ParseInt(at, 10, 64)
	if err != nil || v < 0 {
		return Cursor{}, false
	}
	return Cursor{CreatedAt: v, ID: id}, true
}

// ListMeetings returns up to limit of the owner's meetings, newest first,
// after the cursor (nil: from the newest), and the cursor of the next page
// (nil when there is none).
func (r *OwnerRepo) ListMeetings(ctx context.Context, after *Cursor, limit int) ([]Meeting, *Cursor, error) {
	q := `SELECT ` + meetingCols + ` FROM meetings WHERE owner_id = ?`
	args := []any{r.ownerID}
	if after != nil {
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := r.s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []Meeting{}
	for rows.Next() {
		m, err := scanMeeting(rows)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) <= limit {
		return out, nil, nil
	}
	out = out[:limit]
	last := out[len(out)-1]
	return out, &Cursor{CreatedAt: ms(last.CreatedAt), ID: last.ID}, nil
}

// Meeting returns one of the owner's meetings. Another owner's meeting is
// ErrNotFound, exactly as a meeting that does not exist.
func (r *OwnerRepo) Meeting(ctx context.Context, id string) (Meeting, error) {
	if !ValidMeetingID(id) {
		return Meeting{}, ErrNotFound
	}
	m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND owner_id = ?`, id, r.ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Meeting{}, ErrNotFound
	}
	return m, err
}

// ErrNoLink is a meeting of the owner's whose link is gone: it has ended, or
// its LinkLifetime has passed.
var ErrNoLink = errors.New("the meeting's link is gone")

// Link returns the join code of one of the owner's meetings, derived again
// (JoinCode), so the owner can copy the link while the meeting is open.
// Another owner's meeting, or one that does not exist, is ErrNotFound. A
// meeting that has ended, or is LinkLifetime old, is ErrNoLink.
func (r *OwnerRepo) Link(ctx context.Context, id string, now time.Time) (string, error) {
	m, err := r.Meeting(ctx, id)
	if err != nil {
		return "", err
	}
	if m.State != "open" || !now.Before(m.LinkExpiresAt) {
		return "", ErrNoLink
	}
	return r.s.JoinCode(m.ID), nil
}
