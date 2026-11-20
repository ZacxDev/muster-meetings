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
}

// ForOwner is ownerID's repository. The server builds it from an owner
// session, never from anything a request names.
func (s *Store) ForOwner(ownerID int64) *OwnerRepo { return &OwnerRepo{s: s, ownerID: ownerID} }

// OwnerID is the owner this repository serves.
func (r *OwnerRepo) OwnerID() int64 { return r.ownerID }

// Settings are an owner's defaults (design: "Retention").
type Settings struct {
	RecordDefault bool `json:"record_default"`
	AIDefault     bool `json:"ai_default"`
	// nil is "forever".
	RetentionDaysRecording *int `json:"retention_days_recording"`
	RetentionDaysText      *int `json:"retention_days_text"`
}

// DefaultSettings are an owner's settings before they change any: record and
// AI on, recordings kept 30 days, text 365 (decision 9).
func DefaultSettings() Settings {
	rec, text := 30, 365
	return Settings{RecordDefault: true, AIDefault: true, RetentionDaysRecording: &rec, RetentionDaysText: &text}
}

// Settings returns the owner's settings, the defaults when they have no row.
func (r *OwnerRepo) Settings(ctx context.Context) (Settings, error) {
	var rec, ai int
	var dr, dt sql.NullInt64
	err := r.s.db.QueryRowContext(ctx, `SELECT record_default, ai_default, retention_days_recording, retention_days_text FROM owners WHERE owner_id = ?`,
		r.ownerID).Scan(&rec, &ai, &dr, &dt)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	out := Settings{RecordDefault: rec == 1, AIDefault: ai == 1}
	if dr.Valid {
		v := int(dr.Int64)
		out.RetentionDaysRecording = &v
	}
	if dt.Valid {
		v := int(dt.Int64)
		out.RetentionDaysText = &v
	}
	return out, nil
}

// Meeting is one meeting as its owner sees it. It carries no code: the join
// code is shown once, when the meeting is made, and stored only hashed.
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

// Participant is one person in (or knocking at) a meeting. The identity and
// the ticket's hash never leave the store.
type Participant struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"` // owner, guest
	DisplayName string     `json:"display_name"`
	State       string     `json:"state"`
	KnockedAt   *time.Time `json:"knocked_at"`
	AdmittedAt  *time.Time `json:"admitted_at"`
	LeftAt      *time.Time `json:"left_at"`
}

// NewMeeting is what the owner may choose when making a meeting. Record nil
// takes the owner's default; AI always takes it (the switch is in the call).
type NewMeeting struct {
	Title  string
	Record *bool
}

// Created is a meeting just made, with its join code: the only time the code
// exists outside a guest's address bar.
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

// CreateMeeting makes an open meeting with a fresh link.
func (r *OwnerRepo) CreateMeeting(ctx context.Context, in NewMeeting, now time.Time) (Created, error) {
	set, err := r.Settings(ctx)
	if err != nil {
		return Created{}, err
	}
	record := set.RecordDefault
	if in.Record != nil {
		record = *in.Record
	}
	id, code := NewMeetingID(), NewJoinCode()
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO meetings (id, owner_id, title, room, join_code_sha256, join_code_sealed, state, record, ai, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		id, r.ownerID, in.Title, "meeting-"+id, Hash(code), r.s.seal.seal(id, code), boolInt(record), boolInt(set.AIDefault), ms(now)); err != nil {
		return Created{}, err
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

// Meeting returns one of the owner's meetings and its participants. Another
// owner's meeting is ErrNotFound, exactly as a meeting that does not exist.
func (r *OwnerRepo) Meeting(ctx context.Context, id string) (Meeting, []Participant, error) {
	if !ValidMeetingID(id) {
		return Meeting{}, nil, ErrNotFound
	}
	m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND owner_id = ?`, id, r.ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Meeting{}, nil, ErrNotFound
	}
	if err != nil {
		return Meeting{}, nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT p.id, p.kind, p.display_name, p.state, p.knocked_at, p.admitted_at, p.left_at
		FROM participants p JOIN meetings m ON m.id = p.meeting_id
		WHERE p.meeting_id = ? AND m.owner_id = ? ORDER BY p.knocked_at, p.id`, id, r.ownerID)
	if err != nil {
		return Meeting{}, nil, err
	}
	defer rows.Close()
	ps := []Participant{}
	for rows.Next() {
		var p Participant
		var k, a, l sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Kind, &p.DisplayName, &p.State, &k, &a, &l); err != nil {
			return Meeting{}, nil, err
		}
		p.KnockedAt, p.AdmittedAt, p.LeftAt = fromMS(k), fromMS(a), fromMS(l)
		ps = append(ps, p)
	}
	return m, ps, rows.Err()
}

// ErrNoLink is a meeting of the owner's whose link is gone: it has ended, or
// its LinkLifetime has passed.
var ErrNoLink = errors.New("the meeting's link is gone")

// Link returns the join code of one of the owner's open meetings, so the
// owner can copy the link again. Another owner's meeting, or one that does
// not exist, is ErrNotFound. A meeting that has ended or is past its link
// lifetime is ErrNoLink, and its sealed code is deleted here and now, not
// only refused. A code that does not open (another key, a value moved from
// another row) is ErrSealed and is left as it is.
func (r *OwnerRepo) Link(ctx context.Context, id string, now time.Time) (string, error) {
	if !ValidMeetingID(id) {
		return "", ErrNotFound
	}
	var state string
	var created int64
	var sealed []byte
	err := r.s.db.QueryRowContext(ctx, `SELECT state, created_at, join_code_sealed FROM meetings WHERE id = ? AND owner_id = ?`,
		id, r.ownerID).Scan(&state, &created, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if state != "open" || !now.Before(time.UnixMilli(created).Add(LinkLifetime)) {
		if sealed != nil {
			if _, err := r.s.db.ExecContext(ctx, `UPDATE meetings SET join_code_sealed = NULL WHERE id = ? AND owner_id = ?`, id, r.ownerID); err != nil {
				return "", err
			}
		}
		return "", ErrNoLink
	}
	if sealed == nil {
		return "", ErrNoLink
	}
	return r.s.seal.open(id, sealed)
}
