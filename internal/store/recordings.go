package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/timeline"
)

// The recording (design: "Media: LiveKit, TURN, Egress", "Retention",
// "Failure modes"; migration 6). A meeting's recording is one or more PARTS,
// one per room composite egress; each part's start is in meeting time as the
// egress itself reported it (timeline). A stretch that should have been
// recorded and was not is a recording gap, opened and closed as the
// transcript's gap markers are.

// The recording states.
const (
	RecStarting = "starting" // asked for; the egress has not said its file began
	RecActive   = "active"   // the egress reported its file's start
	RecComplete = "complete" // ended, its file uploaded
	RecFailed   = "failed"   // refused, or died (a partial file may still exist)
)

// The recording gap reasons.
const (
	GapStarting = "starting" // the first egress of the meeting has not begun
	GapFailed   = "failed"   // an egress died, and its successor has not begun
)

// RetentionChoices are the recording retention settings (days); the first is
// the default (decision 9; the operator's decision 5: 30 days).
var RetentionChoices = []int{30, 365}

// DefaultRetentionDays is a new owner's recording retention.
const DefaultRetentionDays = 30

// ErrRetention is a retention setting that is not one of RetentionChoices.
var ErrRetention = errors.New("the recording retention is 30 or 365 days")

// Recording is one part's row.
type Recording struct {
	ID          int64  `json:"-"`
	MeetingID   string `json:"-"`
	Part        int    `json:"part"`
	EgressID    string `json:"-"`
	ObjectKey   string `json:"-"`
	State       string `json:"state"`
	RequestedAt int64  `json:"-"`
	// StartMS is the meeting time of the file's first frame (nil until the
	// egress reports it); DurationMS and Bytes are known once it has ended.
	StartMS    *int64 `json:"start_ms"`
	DurationMS *int64 `json:"duration_ms"`
	Bytes      *int64 `json:"bytes"`
	Error      string `json:"error,omitempty"`
}

// Playable says the part has a file to play: its start and duration are
// known and it has bytes.
func (r Recording) Playable() bool {
	return r.StartMS != nil && r.DurationMS != nil && *r.DurationMS > 0 && r.Bytes != nil && *r.Bytes > 0 && r.ObjectKey != ""
}

// Part is the part's place in meeting time (DurationMS -1 while unknown).
func (r Recording) TimelinePart() timeline.Part {
	p := timeline.Part{N: r.Part, DurationMS: -1}
	if r.StartMS != nil {
		p.StartMS = *r.StartMS
	}
	if r.Playable() {
		p.DurationMS = *r.DurationMS
	}
	return p
}

// Parts is the playable parts' places in meeting time, in order.
func Parts(rs []Recording) []timeline.Part {
	var out []timeline.Part
	for _, r := range rs {
		if r.Playable() {
			out = append(out, r.TimelinePart())
		}
	}
	return out
}

const recCols = `r.id, r.meeting_id, r.part, r.egress_id, r.object_key, r.state, r.requested_at, r.start_ms, r.duration_ms, r.bytes, r.error`

func scanRecording(sc interface{ Scan(...any) error }) (Recording, error) {
	var r Recording
	var start, dur, size sql.NullInt64
	err := sc.Scan(&r.ID, &r.MeetingID, &r.Part, &r.EgressID, &r.ObjectKey, &r.State, &r.RequestedAt, &start, &dur, &size, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrNotFound
	}
	if start.Valid {
		v := start.Int64
		r.StartMS = &v
	}
	if dur.Valid {
		v := dur.Int64
		r.DurationMS = &v
	}
	if size.Valid {
		v := size.Int64
		r.Bytes = &v
	}
	return r, err
}

func scanRecordings(rows *sql.Rows) ([]Recording, error) {
	defer rows.Close()
	out := []Recording{}
	for rows.Next() {
		r, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObjectKey is a part's key in the bucket (design: "meetings/<owner>/<meeting
// id>/recording.mp4", one per part).
func ObjectKey(subject, meetingID string, part int) string {
	return "meetings/" + subject + "/" + meetingID + "/recording-" + strconv.Itoa(part) + ".mp4"
}

// ErrRecordingRunning is BeginRecording on a meeting whose current part is
// still starting or active.
var ErrRecordingRunning = errors.New("the meeting's recording is already running")

// ErrNotRecorded is BeginRecording on a meeting that is not live, or whose
// Record switch is off.
var ErrNotRecorded = errors.New("the meeting is not live with Record on")

// BeginRecording makes the meeting's next part, starting, and opens a
// recording gap at atMS (meeting time) unless one is open. 🔴 In one
// transaction it checks the meeting is live with Record on and has no part
// starting or active, so two starts racing (an admit and a retry) make one
// part, never two egresses.
func (s *Store) BeginRecording(ctx context.Context, meetingID string, atMS int64, now time.Time) (Recording, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Recording{}, err
	}
	defer tx.Rollback()
	var subject string
	var rec int
	var isLive bool
	err = tx.QueryRowContext(ctx, `SELECT m.subject, m.record, `+live+` FROM meetings m WHERE m.id = ?`, cutoff(now), meetingID).Scan(&subject, &rec, &isLive)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrNotFound
	}
	if err != nil {
		return Recording{}, err
	}
	if !isLive || rec != 1 {
		return Recording{}, ErrNotRecorded
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM recordings WHERE meeting_id = ? AND state IN ('starting', 'active')`, meetingID).Scan(&running); err != nil {
		return Recording{}, err
	}
	if running > 0 {
		return Recording{}, ErrRecordingRunning
	}
	var part int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(part), 0) + 1 FROM recordings WHERE meeting_id = ?`, meetingID).Scan(&part); err != nil {
		return Recording{}, err
	}
	why := GapStarting
	if part > 1 {
		why = GapFailed
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO recording_gaps (meeting_id, from_ms, why) SELECT ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM recording_gaps WHERE meeting_id = ? AND to_ms IS NULL)`, meetingID, max(atMS, 0), why, meetingID); err != nil {
		return Recording{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO recordings (meeting_id, part, object_key, state, requested_at) VALUES (?, ?, ?, 'starting', ?)`,
		meetingID, part, ObjectKey(subject, meetingID, part), ms(now))
	if err != nil {
		return Recording{}, err
	}
	id, _ := res.LastInsertId()
	if err := audit(ctx, tx, now, subject, meetingID, "recording.requested", fmt.Sprintf("part %d", part)); err != nil {
		return Recording{}, err
	}
	r, err := scanRecording(tx.QueryRowContext(ctx, `SELECT `+recCols+` FROM recordings r WHERE r.id = ?`, id))
	if err != nil {
		return Recording{}, err
	}
	return r, tx.Commit()
}

// SetRecordingEgress records the egress LiveKit started for a part.
func (s *Store) SetRecordingEgress(ctx context.Context, id int64, egressID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET egress_id = ? WHERE id = ? AND egress_id = ''`, egressID, id)
	return err
}

// RecordingStartRefused marks a part whose egress LiveKit refused (or never
// answered for): failed, with kind (a fixed phrase). Its gap stays open; the
// caller retries with a new part.
func (s *Store) RecordingStartRefused(ctx context.Context, id int64, kind string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET state = 'failed', error = ?, ended_at = ? WHERE id = ? AND state = 'starting'`, kind, ms(now), id)
	return err
}

// RecordingRef is a part found by its egress, with what its meeting is now.
type RecordingRef struct {
	Recording
	Subject      string
	MeetingLive  bool
	Record       bool
	MeetingStart time.Time
}

// RecordingByEgress is the part egressID records; ErrNotFound for an egress
// that is no part's (a track egress, or not the plugin's).
func (s *Store) RecordingByEgress(ctx context.Context, egressID string, now time.Time) (RecordingRef, error) {
	if egressID == "" {
		return RecordingRef{}, ErrNotFound
	}
	var ref RecordingRef
	var start sql.NullInt64
	var dur, size sql.NullInt64
	var startedAt int64
	var rec int
	err := s.db.QueryRowContext(ctx, `SELECT `+recCols+`, m.subject, `+live+`, m.record, coalesce(m.started_at, m.created_at)
		FROM recordings r JOIN meetings m ON m.id = r.meeting_id WHERE r.egress_id = ?`, cutoff(now), egressID).
		Scan(&ref.ID, &ref.MeetingID, &ref.Part, &ref.EgressID, &ref.ObjectKey, &ref.State, &ref.RequestedAt, &start, &dur, &size, &ref.Error,
			&ref.Subject, &ref.MeetingLive, &rec, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RecordingRef{}, ErrNotFound
	}
	if err != nil {
		return RecordingRef{}, err
	}
	if start.Valid {
		v := start.Int64
		ref.StartMS = &v
	}
	if dur.Valid {
		v := dur.Int64
		ref.DurationMS = &v
	}
	if size.Valid {
		v := size.Int64
		ref.Bytes = &v
	}
	ref.Record = rec == 1
	ref.MeetingStart = time.UnixMilli(startedAt).UTC()
	return ref, nil
}

// RecordingBegan records a part's file start (startMS, meeting time, from the
// egress's report): the part is active, and the meeting's open recording gap
// closes there. A start already recorded is kept (the first report wins; the
// end's report is the same value). It answers whether a gap closed.
func (s *Store) RecordingBegan(ctx context.Context, id int64, startMS int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var meetingID string
	err = tx.QueryRowContext(ctx, `UPDATE recordings SET state = CASE WHEN state = 'starting' THEN 'active' ELSE state END,
		start_ms = coalesce(start_ms, ?) WHERE id = ? RETURNING meeting_id`, startMS, id).Scan(&meetingID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE recording_gaps SET to_ms = max(from_ms, ?) WHERE meeting_id = ? AND to_ms IS NULL`, startMS, meetingID)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, tx.Commit()
}

// Ended is what an egress's end reported for its file.
type RecordingEnd struct {
	State      string // RecComplete or RecFailed
	StartMS    *int64 // the file's start, if reported
	DurationMS *int64
	Bytes      *int64
	Error      string // a fixed phrase or the egress's status
	// GapAtMS, when set, opens a recording gap there (meeting time): the
	// part ended while the meeting still wanted recording.
	GapAtMS *int64
}

// EndRecording records a part's end. A part already ended keeps its first
// end (a repeated webhook, or the reconcile and the webhook both).
func (s *Store) EndRecording(ctx context.Context, id int64, e RecordingEnd, now time.Time) error {
	if e.State != RecComplete && e.State != RecFailed {
		return errors.New("store: a part ends complete or failed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var meetingID string
	err = tx.QueryRowContext(ctx, `UPDATE recordings SET state = ?, start_ms = coalesce(start_ms, ?), duration_ms = ?, bytes = ?, error = ?, ended_at = ?
		WHERE id = ? AND state IN ('starting', 'active') RETURNING meeting_id`,
		e.State, nullable(e.StartMS), nullable(e.DurationMS), nullable(e.Bytes), e.Error, ms(now), id).Scan(&meetingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.GapAtMS != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recording_gaps (meeting_id, from_ms, why) SELECT ?, ?, 'failed'
			WHERE NOT EXISTS (SELECT 1 FROM recording_gaps WHERE meeting_id = ? AND to_ms IS NULL)`, meetingID, max(*e.GapAtMS, 0), meetingID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullable(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// CloseRecordingGap closes the meeting's open recording gap at atMS (Record
// was turned off: from then on nothing is meant to be recorded).
func (s *Store) CloseRecordingGap(ctx context.Context, meetingID string, atMS int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recording_gaps SET to_ms = max(from_ms, ?) WHERE meeting_id = ? AND to_ms IS NULL`, atMS, meetingID)
	return err
}

// CurrentRecording is the meeting's part that is starting or active;
// ErrNotFound when none is.
func (s *Store) CurrentRecording(ctx context.Context, meetingID string) (Recording, error) {
	return scanRecording(s.db.QueryRowContext(ctx, `SELECT `+recCols+` FROM recordings r WHERE r.meeting_id = ? AND r.state IN ('starting', 'active')
		ORDER BY r.part DESC LIMIT 1`, meetingID))
}

// RunningRecordings is every part still starting or active, with an egress:
// what the reconcile asks LiveKit about (a lost egress_ended webhook).
func (s *Store) RunningRecordings(ctx context.Context) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recCols+` FROM recordings r WHERE r.state IN ('starting', 'active') AND r.egress_id != '' ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	return scanRecordings(rows)
}

// RecordingStatus is what the badge says about the recording of a meeting
// with Record on: "" (recording, or about to), or RecStatusFailed while a
// recording gap opened by a failure is open (design: "Recording failed"
// badge for everyone).
const RecStatusFailed = "failed"

// RecordingStatus is the meeting's recording status for the badge.
func (s *Store) RecordingStatus(ctx context.Context, meetingID string) (string, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM recording_gaps WHERE meeting_id = ? AND to_ms IS NULL AND why = 'failed'`, meetingID).Scan(&n)
	if n > 0 {
		return RecStatusFailed, err
	}
	return "", err
}

// Gap is a stretch of meeting time that should have been recorded and was
// not. ToMS is nil while it is open.
type Gap struct {
	FromMS int64  `json:"from_ms"`
	ToMS   *int64 `json:"to_ms"`
	Why    string `json:"why"`
}

// ---- the owner's view ----

// Recordings is one of the owner's meetings' parts, in order, and its
// recording gaps. Another person's meeting is ErrNotFound.
func (r *OwnerRepo) Recordings(ctx context.Context, meetingID string) ([]Recording, []Gap, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return nil, nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT `+recCols+` FROM recordings r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND m.subject = ? ORDER BY r.part`, meetingID, r.subject)
	if err != nil {
		return nil, nil, err
	}
	recs, err := scanRecordings(rows)
	if err != nil {
		return nil, nil, err
	}
	gaps, err := r.s.recordingGaps(ctx, meetingID)
	return recs, gaps, err
}

func (s *Store) recordingGaps(ctx context.Context, meetingID string) ([]Gap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT from_ms, to_ms, why FROM recording_gaps WHERE meeting_id = ? ORDER BY from_ms, id`, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Gap{}
	for rows.Next() {
		var g Gap
		var to sql.NullInt64
		if err := rows.Scan(&g.FromMS, &to, &g.Why); err != nil {
			return nil, err
		}
		if to.Valid {
			v := to.Int64
			g.ToMS = &v
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Purge is when a meeting's recording is deleted: nil while it runs.
func (r *OwnerRepo) PurgeRecordingAt(ctx context.Context, meetingID string) (*time.Time, error) {
	var v sql.NullInt64
	err := r.s.db.QueryRowContext(ctx, `SELECT purge_recording_at FROM meetings WHERE id = ? AND subject = ?`, meetingID, r.subject).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return fromMS(v), err
}

// Settings are the owner's settings.
type Settings struct {
	// RecordingRetentionDays is how long a recording is kept after its
	// meeting ends: one of RetentionChoices.
	RecordingRetentionDays int `json:"recording_retention_days"`
}

// Settings reads the owner's settings.
func (r *OwnerRepo) Settings(ctx context.Context) (Settings, error) {
	var out Settings
	err := r.s.db.QueryRowContext(ctx, `SELECT retention_days_recording FROM owners WHERE subject = ?`, r.subject).Scan(&out.RecordingRetentionDays)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{RecordingRetentionDays: DefaultRetentionDays}, nil
	}
	return out, err
}

// SetSettings writes the owner's settings. It applies from the next meeting
// to end: a meeting already ended keeps the purge time it was given (design:
// "changing the setting later does not shorten meetings already kept").
func (r *OwnerRepo) SetSettings(ctx context.Context, v Settings, now time.Time) error {
	ok := false
	for _, c := range RetentionChoices {
		ok = ok || v.RecordingRetentionDays == c
	}
	if !ok {
		return ErrRetention
	}
	if r.session == "" {
		return ErrNoSession
	}
	res, err := r.s.db.ExecContext(ctx, `UPDATE owners SET retention_days_recording = ?, updated_at = ? WHERE subject = ?`,
		v.RecordingRetentionDays, ms(now), r.subject)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNoSession
	}
	return audit(ctx, r.s.db, now, r.subject, "", "settings.changed", fmt.Sprintf("recording retention %d days", v.RecordingRetentionDays))
}

// closeEnded is the rest of every meeting's end, in the transaction that
// ended it (endParticipants calls it): each meeting that has just ended is
// given its recording purge time (its owner's retention from its end), and
// its open recording gap and open screen shares close where it ended.
func closeEnded(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE meetings SET purge_recording_at = ended_at + 86400000 * coalesce(
			(SELECT o.retention_days_recording FROM owners o WHERE o.subject = meetings.subject), `+strconv.Itoa(DefaultRetentionDays)+`)
		WHERE state = 'ended' AND ended_at IS NOT NULL AND purge_recording_at IS NULL`); err != nil {
		return err
	}
	const endMS = `(SELECT m.ended_at - coalesce(m.started_at, m.created_at) FROM meetings m WHERE m.id = %s.meeting_id)`
	if _, err := tx.ExecContext(ctx, `UPDATE recording_gaps SET to_ms = max(from_ms, `+fmt.Sprintf(endMS, "recording_gaps")+`)
		WHERE to_ms IS NULL AND meeting_id IN (SELECT id FROM meetings WHERE state = 'ended')`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE screen_shares SET end_ms = max(start_ms, `+fmt.Sprintf(endMS, "screen_shares")+`)
		WHERE end_ms IS NULL AND meeting_id IN (SELECT id FROM meetings WHERE state = 'ended')`)
	return err
}

// ---- the share link: a capability for one meeting's recording ----

// ShareTokenBytes is a share link's token: 256 bits, 43 base64url characters.
const ShareTokenBytes = 32

var shareTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ValidShareToken checks a share token's shape before it is hashed.
func ValidShareToken(s string) bool { return shareTokenRE.MatchString(s) }

// ErrNotShareable is a share asked for a meeting that has not ended, or has
// no playable part.
var ErrNotShareable = errors.New("the meeting has no finished recording to share")

// Share mints the share link of one of the owner's meetings: a new random
// token, stored only as its SHA-256, valid until the meeting's recording is
// purged. 🔴 Any earlier link of the meeting is revoked in the same
// transaction: one live link per meeting, and the token is shown once.
func (r *OwnerRepo) Share(ctx context.Context, meetingID string, now time.Time) (string, time.Time, error) {
	if r.session == "" {
		return "", time.Time{}, ErrNoSession
	}
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return "", time.Time{}, err
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback()
	var purge sql.NullInt64
	var playable int
	err = tx.QueryRowContext(ctx, `SELECT m.purge_recording_at, (SELECT count(*) FROM recordings p WHERE p.meeting_id = m.id AND p.start_ms IS NOT NULL
			AND p.duration_ms > 0 AND p.bytes > 0 AND p.object_key != '')
		FROM meetings m WHERE m.id = ? AND m.subject = ? AND m.state = 'ended'`, meetingID, r.subject).Scan(&purge, &playable)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!purge.Valid || playable == 0 || purge.Int64 <= ms(now))) {
		return "", time.Time{}, ErrNotShareable
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE recording_shares SET revoked_at = ? WHERE meeting_id = ? AND revoked_at IS NULL`, ms(now), meetingID); err != nil {
		return "", time.Time{}, err
	}
	tok := random(ShareTokenBytes)
	if _, err := tx.ExecContext(ctx, `INSERT INTO recording_shares (token_sha256, meeting_id, created_at) VALUES (?, ?, ?)`, Hash(tok), meetingID, ms(now)); err != nil {
		return "", time.Time{}, err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "recording.shared", ""); err != nil {
		return "", time.Time{}, err
	}
	return tok, time.UnixMilli(purge.Int64).UTC(), tx.Commit()
}

// RevokeShare revokes the meeting's live share link. It answers whether there
// was one.
func (r *OwnerRepo) RevokeShare(ctx context.Context, meetingID string, now time.Time) (bool, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return false, err
	}
	res, err := r.s.db.ExecContext(ctx, `UPDATE recording_shares SET revoked_at = ? WHERE meeting_id = ? AND revoked_at IS NULL
		AND meeting_id IN (SELECT id FROM meetings WHERE subject = ?)`, ms(now), meetingID, r.subject)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if err := audit(ctx, r.s.db, now, r.subject, meetingID, "recording.unshared", ""); err != nil {
			return true, err
		}
	}
	return n > 0, nil
}

// ShareState is whether the meeting has a live share link, and until when.
type ShareState struct {
	Live      bool       `json:"live"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// ShareOf is the meeting's share link state.
func (r *OwnerRepo) ShareOf(ctx context.Context, meetingID string, now time.Time) (ShareState, error) {
	var n int
	var purge sql.NullInt64
	err := r.s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM recording_shares s WHERE s.meeting_id = m.id AND s.revoked_at IS NULL), m.purge_recording_at
		FROM meetings m WHERE m.id = ? AND m.subject = ?`, meetingID, r.subject).Scan(&n, &purge)
	if errors.Is(err, sql.ErrNoRows) {
		return ShareState{}, ErrNotFound
	}
	if err != nil {
		return ShareState{}, err
	}
	if n == 0 || !purge.Valid || purge.Int64 <= ms(now) {
		return ShareState{}, nil
	}
	return ShareState{Live: true, ExpiresAt: fromMS(purge)}, nil
}

// Shared is what a share link opens: one meeting's playable parts.
type Shared struct {
	MeetingID string
	Title     string
	CreatedAt time.Time
	ExpiresAt time.Time
	Parts     []Recording
	Gaps      []Gap
}

// ShareByToken is what a share link's token opens at now. 🔴 An unknown,
// malformed, revoked or expired token (past the recording's purge time), or
// one whose meeting has no playable part, is ErrNotFound alike: the caller
// answers every one of them the same 404. The stored hash is compared in
// constant time after the indexed lookup.
func (s *Store) ShareByToken(ctx context.Context, token string, now time.Time) (Shared, error) {
	if !ValidShareToken(token) {
		return Shared{}, ErrNotFound
	}
	h := Hash(token)
	var stored string
	var revoked, purge sql.NullInt64
	var out Shared
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT s.token_sha256, s.revoked_at, m.id, m.title, m.created_at, m.purge_recording_at
		FROM recording_shares s JOIN meetings m ON m.id = s.meeting_id WHERE s.token_sha256 = ?`, h).
		Scan(&stored, &revoked, &out.MeetingID, &out.Title, &created, &purge)
	if errors.Is(err, sql.ErrNoRows) {
		return Shared{}, ErrNotFound
	}
	if err != nil {
		return Shared{}, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 || revoked.Valid || !purge.Valid || purge.Int64 <= ms(now) {
		return Shared{}, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+recCols+` FROM recordings r WHERE r.meeting_id = ? ORDER BY r.part`, out.MeetingID)
	if err != nil {
		return Shared{}, err
	}
	all, err := scanRecordings(rows)
	if err != nil {
		return Shared{}, err
	}
	for _, p := range all {
		if p.Playable() {
			out.Parts = append(out.Parts, p)
		}
	}
	if len(out.Parts) == 0 {
		return Shared{}, ErrNotFound
	}
	if out.Gaps, err = s.recordingGaps(ctx, out.MeetingID); err != nil {
		return Shared{}, err
	}
	out.CreatedAt = time.UnixMilli(created).UTC()
	out.ExpiresAt = time.UnixMilli(purge.Int64).UTC()
	return out, nil
}

// ---- retention: the sweeper's side ----

// Due is a part whose meeting's purge time has passed.
type Due struct {
	ID        int64
	MeetingID string
	ObjectKey string
}

// DueRecordings is up to limit parts past their meeting's purge time, oldest
// first.
func (s *Store) DueRecordings(ctx context.Context, now time.Time, limit int) ([]Due, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.meeting_id, r.object_key FROM recordings r JOIN meetings m ON m.id = r.meeting_id
		WHERE m.purge_recording_at IS NOT NULL AND m.purge_recording_at <= ? ORDER BY m.purge_recording_at, r.id LIMIT ?`, ms(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Due
	for rows.Next() {
		var d Due
		if err := rows.Scan(&d.ID, &d.MeetingID, &d.ObjectKey); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteRecordingRow deletes one part's row: only after its object is gone.
func (s *Store) DeleteRecordingRow(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id)
	return err
}

// PurgeRecordingMeta deletes the share links and recording gaps of every
// meeting past its recording purge time that has no part left. It answers
// the share links deleted.
func (s *Store) PurgeRecordingMeta(ctx context.Context, now time.Time) (int64, error) {
	const past = `SELECT m.id FROM meetings m WHERE m.purge_recording_at IS NOT NULL AND m.purge_recording_at <= ?
		AND NOT EXISTS (SELECT 1 FROM recordings r WHERE r.meeting_id = m.id)`
	r, err := s.db.ExecContext(ctx, `DELETE FROM recording_shares WHERE meeting_id IN (`+past+`)`, ms(now))
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	_, err = s.db.ExecContext(ctx, `DELETE FROM recording_gaps WHERE meeting_id IN (`+past+`)`, ms(now))
	return n, err
}

// ---- screen shares ----

// StartScreenShare records that identity began sharing a screen (track) in
// room's live meeting at atMS (meeting time). A track already recorded is
// left as it is.
func (s *Store) StartScreenShare(ctx context.Context, room, track, identity, speaker string, atMS int64, now time.Time) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `INSERT INTO screen_shares (track, meeting_id, identity, speaker, start_ms)
		SELECT ?, m.id, ?, ?, ? FROM meetings m WHERE m.room = ? AND `+live+`
		ON CONFLICT (track) DO NOTHING RETURNING meeting_id`, track, identity, speaker, max(atMS, 0), room, cutoff(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// EndScreenShare records that a screen share's track ended at atMS. It
// answers the meeting, "" when the track was not a screen share's.
func (s *Store) EndScreenShare(ctx context.Context, track string, atMS func(meetingStart time.Time) int64) (string, error) {
	var id string
	var start int64
	err := s.db.QueryRowContext(ctx, `SELECT s.meeting_id, coalesce(m.started_at, m.created_at) FROM screen_shares s JOIN meetings m ON m.id = s.meeting_id
		WHERE s.track = ? AND s.end_ms IS NULL`, track).Scan(&id, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE screen_shares SET end_ms = max(start_ms, ?) WHERE track = ? AND end_ms IS NULL`, atMS(time.UnixMilli(start).UTC()), track)
	return id, err
}

// ScreenShare is one shared screen's stretch of meeting time; EndMS nil while
// it is shared.
type ScreenShare struct {
	Speaker string `json:"speaker"` // a name: render as text only
	StartMS int64  `json:"start_ms"`
	EndMS   *int64 `json:"end_ms"`
}

// ScreenShares is one of the owner's meetings' screen shares, in order.
func (r *OwnerRepo) ScreenShares(ctx context.Context, meetingID string) ([]ScreenShare, error) {
	rows, err := r.s.db.QueryContext(ctx, `SELECT s.speaker, s.start_ms, s.end_ms FROM screen_shares s JOIN meetings m ON m.id = s.meeting_id
		WHERE s.meeting_id = ? AND m.subject = ? ORDER BY s.start_ms, s.track`, meetingID, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScreenShare{}
	for rows.Next() {
		var v ScreenShare
		var end sql.NullInt64
		if err := rows.Scan(&v.Speaker, &v.StartMS, &end); err != nil {
			return nil, err
		}
		if end.Valid {
			e := end.Int64
			v.EndMS = &e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- signed media URLs (design: "/media/… HMAC over the path and expiry") ----

// MediaKey is the HMAC key of the owner's signed media URLs, derived from the
// link key under its own label (so it is never the join codes' key).
func (s *Store) MediaKey() []byte {
	m := hmac.New(sha256.New, s.linkKey)
	m.Write([]byte("muster-meetings media url v1"))
	return m.Sum(nil)
}

// ByIDForMedia is a meeting's part by number, for a signed media URL (the
// signature has already bound the meeting): ErrNotFound unless playable.
func (s *Store) PlayablePart(ctx context.Context, meetingID string, part int) (Recording, error) {
	r, err := scanRecording(s.db.QueryRowContext(ctx, `SELECT `+recCols+` FROM recordings r WHERE r.meeting_id = ? AND r.part = ?`, meetingID, part))
	if err != nil {
		return Recording{}, err
	}
	if !r.Playable() {
		return Recording{}, ErrNotFound
	}
	return r, nil
}
