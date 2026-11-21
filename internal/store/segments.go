package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ---- tracks: the microphone tracks the plugin transcribes ----

// The track states.
const (
	TrackActive = "active"
	TrackEnded  = "ended"
)

// Track is a transcribed microphone track's row.
type Track struct {
	Track     string
	MeetingID string
	Identity  string
	Speaker   string
	State     string
	Nonce     string
	EgressID  string
	GapOpen   bool
	// MeetingLive is the meeting open and under LinkLifetime; MeetingStart
	// its start (when the owner first joined, else its creation), which
	// meeting time counts from; MeetingExp its LinkLifetime cap.
	MeetingLive  bool
	MeetingStart time.Time
	MeetingExp   time.Time
}

const trackCols = `t.track, t.meeting_id, t.identity, t.speaker, t.state, t.nonce, t.egress_id, t.gap_segment IS NOT NULL,
	m.state = 'open' AND m.created_at > ?, coalesce(m.started_at, m.created_at), m.created_at`

func scanTrack(sc interface{ Scan(...any) error }) (Track, error) {
	var t Track
	var start, created int64
	err := sc.Scan(&t.Track, &t.MeetingID, &t.Identity, &t.Speaker, &t.State, &t.Nonce, &t.EgressID, &t.GapOpen, &t.MeetingLive, &start, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	t.MeetingStart = time.UnixMilli(start).UTC()
	t.MeetingExp = time.UnixMilli(created).UTC().Add(LinkLifetime)
	return t, err
}

// UpsertTrack records that identity published track in room's meeting, to be
// transcribed under the label speaker. A track already known is made active
// again (its nonce and egress are kept until a new egress starts). The room
// must be a live meeting's (else ErrGone, or ErrNotFound for no meeting).
func (s *Store) UpsertTrack(ctx context.Context, room, track, identity, speaker string, now time.Time) (Track, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Track{}, err
	}
	defer tx.Rollback()
	var id string
	var state string
	var created int64
	err = tx.QueryRowContext(ctx, `SELECT id, state, created_at FROM meetings WHERE room = ?`, room).Scan(&id, &state, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	if err != nil {
		return Track{}, err
	}
	if state != "open" || created <= cutoff(now) {
		return Track{}, ErrGone
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tracks (track, meeting_id, identity, speaker, state, published_at) VALUES (?, ?, ?, ?, 'active', ?)
		ON CONFLICT (track) DO UPDATE SET state = 'active' WHERE tracks.meeting_id = excluded.meeting_id AND tracks.identity = excluded.identity`,
		track, id, identity, speaker, ms(now)); err != nil {
		return Track{}, err
	}
	t, err := scanTrack(tx.QueryRowContext(ctx, `SELECT `+trackCols+` FROM tracks t JOIN meetings m ON m.id = t.meeting_id WHERE t.track = ?`, cutoff(now), track))
	if err != nil {
		return Track{}, err
	}
	if t.MeetingID != id || t.Identity != identity {
		// The same track id under another meeting or identity: not ours to
		// take over.
		return Track{}, ErrState
	}
	return t, tx.Commit()
}

// TrackByID is one track's row, with its meeting's liveness at now.
func (s *Store) TrackByID(ctx context.Context, track string, now time.Time) (Track, error) {
	return scanTrack(s.db.QueryRowContext(ctx, `SELECT `+trackCols+` FROM tracks t JOIN meetings m ON m.id = t.meeting_id WHERE t.track = ?`, cutoff(now), track))
}

// TrackByEgress is the track whose CURRENT egress is egressID; ErrNotFound
// for an egress that is not (an older one of a restarted track, or not ours).
func (s *Store) TrackByEgress(ctx context.Context, egressID string, now time.Time) (Track, error) {
	if egressID == "" {
		return Track{}, ErrNotFound
	}
	return scanTrack(s.db.QueryRowContext(ctx, `SELECT `+trackCols+` FROM tracks t JOIN meetings m ON m.id = t.meeting_id WHERE t.egress_id = ?`, cutoff(now), egressID))
}

// SetNonce starts a new egress attempt for track: its nonce becomes nonce
// and its egress id is cleared, so every token of an earlier egress is dead
// from now on.
func (s *Store) SetNonce(ctx context.Context, track, nonce string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET nonce = ?, egress_id = '' WHERE track = ?`, nonce, track)
	return err
}

// SetEgress records the egress LiveKit started for track's attempt nonce.
// It changes nothing when a newer attempt has replaced that one.
func (s *Store) SetEgress(ctx context.Context, track, nonce, egressID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET egress_id = ? WHERE track = ? AND nonce = ?`, egressID, track, nonce)
	return err
}

// EndTrack marks track ended (unpublished): no egress is started for it
// again, and its ingest token opens nothing.
func (s *Store) EndTrack(ctx context.Context, track string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tracks SET state = 'ended', nonce = '' WHERE track = ?`, track)
	return err
}

// LiveTracks is every active track of a live meeting: what a restarted
// plugin resumes.
func (s *Store) LiveTracks(ctx context.Context, now time.Time) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+trackCols+` FROM tracks t JOIN meetings m ON m.id = t.meeting_id
		WHERE t.state = 'active' AND m.state = 'open' AND m.created_at > ? ORDER BY t.published_at, t.track`, cutoff(now), cutoff(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LiveTracksOf is every active track of one meeting, live or not.
func (s *Store) LiveTracksOf(ctx context.Context, meetingID string) ([]Track, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+trackCols+` FROM tracks t JOIN meetings m ON m.id = t.meeting_id
		WHERE t.state = 'active' AND t.meeting_id = ? ORDER BY t.published_at, t.track`, cutoff(time.Now()), meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- gap markers ----

// OpenGap opens a gap marker on track at atMS (meeting time), unless one is
// open already: no audio of it is being transcribed from then on.
func (s *Store) OpenGap(ctx context.Context, track string, atMS int64, engine string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var meetingID, identity, speaker string
	var open bool
	err = tx.QueryRowContext(ctx, `SELECT meeting_id, identity, speaker, gap_segment IS NOT NULL FROM tracks WHERE track = ?`, track).
		Scan(&meetingID, &identity, &speaker, &open)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil || open {
		return err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO segments (meeting_id, identity, speaker, kind, start_ms, engine) VALUES (?, ?, ?, 'gap', ?, ?)`,
		meetingID, identity, speaker, max(atMS, 0), engine)
	if err != nil {
		return err
	}
	id, _ := r.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE tracks SET gap_segment = ? WHERE track = ?`, id, track); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseGap closes track's open gap marker at atMS (audio flows again). It
// answers whether one was open.
func (s *Store) CloseGap(ctx context.Context, track string, atMS int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var seg sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT gap_segment FROM tracks WHERE track = ?`, track).Scan(&seg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if !seg.Valid {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE segments SET end_ms = max(start_ms, ?) WHERE id = ?`, atMS, seg.Int64); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracks SET gap_segment = NULL WHERE track = ?`, track); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ---- segments ----

// The segment kinds as stored.
const (
	SegText = "text"
	SegGap  = "gap"
)

// Segment is one line of a transcript, or a gap marker.
type Segment struct {
	ID      int64  `json:"id"`
	Speaker string `json:"speaker"` // a name: render as text only
	Kind    string `json:"kind"`    // text, gap
	StartMS int64  `json:"start_ms"`
	// EndMS is null for a gap still open.
	EndMS *int64 `json:"end_ms"`
	// Text is what was said: untrusted (a guest's speech). Render it as
	// text, escape it for any format, fence it for any model.
	Text string `json:"text"`
}

// AddSegment stores a final line, or a closed gap marker, for track at
// meeting times startMS..endMS. It answers the segment's id; a track that
// no longer exists is ErrNotFound.
func (s *Store) AddSegment(ctx context.Context, track, kind string, startMS, endMS int64, text, engine string) (int64, error) {
	if kind != SegText && kind != SegGap {
		return 0, errors.New("store: a segment is text or gap")
	}
	r, err := s.db.ExecContext(ctx, `INSERT INTO segments (meeting_id, identity, speaker, kind, start_ms, end_ms, text, engine)
		SELECT meeting_id, identity, speaker, ?, ?, ?, ?, ? FROM tracks WHERE track = ?`,
		kind, max(startMS, 0), max(endMS, startMS, 0), text, engine, track)
	if err != nil {
		return 0, err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return 0, ErrNotFound
	}
	return r.LastInsertId()
}

const segCols = `s.id, s.speaker, s.kind, s.start_ms, s.end_ms, s.text`

func scanSegments(rows *sql.Rows) ([]Segment, error) {
	defer rows.Close()
	out := []Segment{}
	for rows.Next() {
		var g Segment
		var end sql.NullInt64
		if err := rows.Scan(&g.ID, &g.Speaker, &g.Kind, &g.StartMS, &end, &g.Text); err != nil {
			return nil, err
		}
		if end.Valid {
			v := end.Int64
			g.EndMS = &v
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MaxSegmentsPage is the most segments one read answers.
const MaxSegmentsPage = 500

// SegmentsAfter is the meeting's segments with an id above after, oldest
// first, at most limit: what an ADMITTED guest's event stream sends (the
// caller has already bound the ticket to this meeting). A gap is sent when
// it opens and again when it closes (OpenGaps, SegmentsByID).
func (s *Store) SegmentsAfter(ctx context.Context, meetingID string, after int64, limit int) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+segCols+` FROM segments s WHERE s.meeting_id = ? AND s.id > ? ORDER BY s.id LIMIT ?`,
		meetingID, after, min(limit, MaxSegmentsPage))
	if err != nil {
		return nil, err
	}
	return scanSegments(rows)
}

// OpenGaps is the ids of the meeting's gap markers still open. A stream
// keeps the set it last sent; a gap that left it has closed, and is sent
// again with its end.
func (s *Store) OpenGaps(ctx context.Context, meetingID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.gap_segment FROM tracks t WHERE t.meeting_id = ? AND t.gap_segment IS NOT NULL ORDER BY 1`, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SegmentsByID is the meeting's segments with these ids.
func (s *Store) SegmentsByID(ctx context.Context, meetingID string, ids []int64) ([]Segment, error) {
	if len(ids) == 0 {
		return []Segment{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+segCols+` FROM segments s WHERE s.meeting_id = ? AND s.id IN (SELECT value FROM json_each(?)) ORDER BY s.id`,
		meetingID, jsonInts(ids))
	if err != nil {
		return nil, err
	}
	return scanSegments(rows)
}

func jsonInts(v []int64) string {
	b := []byte{'['}
	for i, n := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, n, 10)
	}
	return string(append(b, ']'))
}

// ---- the owner's view: through OwnerRepo, so always the owner's own meeting ----

// SegmentsAfter is SegmentsAfter for one of the owner's meetings: another
// person's meeting is ErrNotFound.
func (r *OwnerRepo) SegmentsAfter(ctx context.Context, meetingID string, after int64, limit int) ([]Segment, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT `+segCols+` FROM segments s JOIN meetings m ON m.id = s.meeting_id
		WHERE s.meeting_id = ? AND m.subject = ? AND s.id > ? ORDER BY s.id LIMIT ?`,
		meetingID, r.subject, after, min(limit, MaxSegmentsPage))
	if err != nil {
		return nil, err
	}
	return scanSegments(rows)
}

// Transcript is one of the owner's meetings and its whole transcript, oldest
// first. A gap still open is shown ending where the meeting ended (or open,
// while it runs).
func (r *OwnerRepo) Transcript(ctx context.Context, meetingID string) (Meeting, []Segment, error) {
	m, err := r.Meeting(ctx, meetingID)
	if err != nil {
		return Meeting{}, nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT `+segCols+` FROM segments s JOIN meetings m ON m.id = s.meeting_id
		WHERE s.meeting_id = ? AND m.subject = ? ORDER BY s.start_ms, s.id`, meetingID, r.subject)
	if err != nil {
		return Meeting{}, nil, err
	}
	segs, err := scanSegments(rows)
	if err != nil {
		return Meeting{}, nil, err
	}
	if m.EndedAt != nil {
		start := m.CreatedAt
		if m.StartedAt != nil {
			start = *m.StartedAt
		}
		end := m.EndedAt.Sub(start).Milliseconds()
		for i := range segs {
			if segs[i].EndMS == nil {
				v := max(end, segs[i].StartMS)
				segs[i].EndMS = &v
			}
		}
	}
	return m, segs, nil
}

// MeetingStart is when a meeting's time starts: when its owner first joined
// the call, else its creation. ErrNotFound for no such meeting.
func (s *Store) MeetingStart(ctx context.Context, meetingID string) (time.Time, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(started_at, created_at) FROM meetings WHERE id = ?`, meetingID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return time.UnixMilli(v).UTC(), err
}
