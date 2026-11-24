package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The analyses of a meeting (migration 7): the live worker's run, and each
// post-call pass. 🔴 Every text column here is a model's words over what was
// said: untrusted, rendered as text, never logged.

// Run kinds and states.
const (
	RunLive = "live"
	RunPost = "post"

	RunQueued  = "queued"
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
)

// Item sources.
const (
	SrcLive       = "live"
	SrcTranscript = "transcript"
	SrcFrames     = "frames"
)

// Run is one analysis of a meeting.
type Run struct {
	ID          int64      `json:"id"`
	MeetingID   string     `json:"-"`
	Kind        string     `json:"kind"`
	ItemsModel  string     `json:"items_model"`
	ItemsPrefs  string     `json:"items_prefs"`
	VisionModel string     `json:"vision_model"`
	VisionPrefs string     `json:"vision_prefs"`
	State       string     `json:"state"`
	Current     bool       `json:"current"`
	Attempts    int        `json:"attempts"`
	Anchor      int64      `json:"-"`
	Failures    int        `json:"-"`
	Partial     bool       `json:"partial"`
	Error       string     `json:"error"`
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	// Subject is the meeting's owner (for the worker).
	Subject string `json:"-"`
}

const runCols = `r.id, r.meeting_id, r.kind, r.items_model, r.items_prefs, r.vision_model, r.vision_prefs, r.state, r.current, r.attempts,
	r.anchor, r.failures, r.partial, r.error, r.requested_by, r.created_at, r.finished_at, m.subject`

func scanRun(sc interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var created int64
	var fin sql.NullInt64
	err := sc.Scan(&r.ID, &r.MeetingID, &r.Kind, &r.ItemsModel, &r.ItemsPrefs, &r.VisionModel, &r.VisionPrefs, &r.State, &r.Current, &r.Attempts,
		&r.Anchor, &r.Failures, &r.Partial, &r.Error, &r.RequestedBy, &created, &fin, &r.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	r.CreatedAt = time.UnixMilli(created).UTC()
	r.FinishedAt = fromMS(fin)
	return r, err
}

// AIItem is one stored action item.
type AIItem struct {
	ID       int64   `json:"id"`
	RunID    int64   `json:"run_id"`
	Source   string  `json:"source"`
	Text     string  `json:"text"`
	Assignee string  `json:"assignee"`
	Due      string  `json:"due"`
	Segments []int64 `json:"segments"`
	Frames   []int64 `json:"frames"`
}

// NewItem is an item to store.
type NewItem struct {
	Source               string
	Text, Assignee, Due  string
	Segments, FrameTimes []int64
}

// Note is one stored frame note.
type Note struct {
	FrameMS  int64   `json:"frame_ms"`
	Text     string  `json:"text"`
	Segments []int64 `json:"segments"`
}

// Point is a summary point.
type Point struct {
	Text     string  `json:"text"`
	Segments []int64 `json:"segments"`
}

// Summary is a run's summary.
type Summary struct {
	Points    []Point `json:"points"`
	Decisions []Point `json:"decisions"`
}

func ints(v []int64) string {
	if v == nil {
		v = []int64{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseInts(s string) []int64 {
	out := []int64{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// ErrNoRun is a meeting with no such run.
var ErrNoRun = errors.New("no such run")

// EnsureLiveRun is the meeting's live run, made on first use with these
// models. It is current when the meeting has no current run yet.
func (s *Store) EnsureLiveRun(ctx context.Context, meetingID, model, prefs string, now time.Time) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND r.kind = 'live'`, meetingID))
	if err == nil || !errors.Is(err, ErrNotFound) {
		return r, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ai_runs (meeting_id, kind, items_model, items_prefs, state, current, next_at, created_at)
		VALUES (?, 'live', ?, ?, 'running', NOT EXISTS (SELECT 1 FROM ai_runs WHERE meeting_id = ? AND current = 1), ?, ?)`,
		meetingID, model, prefs, meetingID, ms(now), ms(now))
	if err != nil {
		return Run{}, err
	}
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND r.kind = 'live'`, meetingID))
}

// SaveLive stores one live window's outcome: the items it kept, the
// worker's anchor and its run of failures.
func (s *Store) SaveLive(ctx context.Context, runID int64, kept []NewItem, anchor int64, failures int, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertItems(ctx, tx, runID, kept, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_runs SET anchor = ?, failures = ? WHERE id = ? AND kind = 'live'`, anchor, failures, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// StopLive marks a live run that stopped on a fatal answer (or whose
// meeting ended): it is done.
func (s *Store) StopLive(ctx context.Context, runID int64, phrase string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_runs SET state = CASE WHEN ? = '' THEN 'done' ELSE 'failed' END, error = ?, finished_at = coalesce(finished_at, ?)
		WHERE id = ? AND kind = 'live' AND state = 'running'`, phrase, phrase, ms(now), runID)
	return err
}

func insertItems(ctx context.Context, x execer, runID int64, items []NewItem, now time.Time) error {
	for _, it := range items {
		if _, err := x.ExecContext(ctx, `INSERT INTO ai_items (run_id, source, text, assignee, due, segments, frames, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, it.Source, it.Text, it.Assignee, it.Due, ints(it.Segments), ints(it.FrameTimes), ms(now)); err != nil {
			return err
		}
	}
	return nil
}

// RunItems is a run's items, in order.
func (s *Store) RunItems(ctx context.Context, runID int64) ([]AIItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, source, text, assignee, due, segments, frames FROM ai_items WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AIItem{}
	for rows.Next() {
		var it AIItem
		var segs, frames string
		if err := rows.Scan(&it.ID, &it.RunID, &it.Source, &it.Text, &it.Assignee, &it.Due, &segs, &frames); err != nil {
			return nil, err
		}
		it.Segments, it.Frames = parseInts(segs), parseInts(frames)
		out = append(out, it)
	}
	return out, rows.Err()
}

// LiveState is what the live panel shows: the live run's items, and whether
// action items are paused (failures in a row) or stopped.
type LiveState struct {
	Items    []AIItem
	Failures int
	Stopped  bool
	Started  bool
}

// Live is a meeting's live state (Started false: no live run yet).
func (s *Store) Live(ctx context.Context, meetingID string) (LiveState, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND r.kind = 'live'`, meetingID))
	if errors.Is(err, ErrNotFound) {
		return LiveState{Items: []AIItem{}}, nil
	}
	if err != nil {
		return LiveState{}, err
	}
	items, err := s.RunItems(ctx, r.ID)
	if err != nil {
		return LiveState{}, err
	}
	return LiveState{Items: items, Failures: r.Failures, Stopped: r.State == RunFailed, Started: true}, nil
}

// QueuePost queues a post-call pass of an ended meeting with these models;
// it answers the run's id. 🔴 Only a meeting whose AI action items were on:
// with them off its guests were told nothing goes to an AI service, and
// ErrNotFound is the answer.
func (s *Store) QueuePost(ctx context.Context, meetingID, itemsModel, itemsPrefs, visionModel, visionPrefs, by string, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO ai_runs (meeting_id, kind, items_model, items_prefs, vision_model, vision_prefs, state, next_at, requested_by, created_at)
		SELECT id, 'post', ?, ?, ?, ?, 'queued', ?, ?, ? FROM meetings WHERE id = ? AND state = 'ended' AND ai = 1`,
		itemsModel, itemsPrefs, visionModel, visionPrefs, ms(now), by, ms(now), meetingID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return res.LastInsertId()
}

// QueueEnded queues the post-call pass of every meeting that ended after
// since with its AI switch on and has none yet: what End (and the sweep, a
// revocation, a room LiveKit finished) leaves to do. It answers how many.
func (s *Store) QueueEnded(ctx context.Context, itemsModel, itemsPrefs, visionModel, visionPrefs string, since, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO ai_runs (meeting_id, kind, items_model, items_prefs, vision_model, vision_prefs, state, next_at, requested_by, created_at)
		SELECT m.id, 'post', ?, ?, ?, ?, 'queued', ?, 'end', ? FROM meetings m
		WHERE m.state = 'ended' AND m.ai = 1 AND m.ended_at > ?
		AND EXISTS (SELECT 1 FROM segments g WHERE g.meeting_id = m.id AND g.kind = 'text')
		AND NOT EXISTS (SELECT 1 FROM ai_runs r WHERE r.meeting_id = m.id AND r.kind = 'post')`,
		itemsModel, itemsPrefs, visionModel, visionPrefs, ms(now), ms(now), ms(since))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// EndedBetween is the ended meetings with their AI action items on that
// ended in [from, to) (the reprocess command's range).
func (s *Store) EndedBetween(ctx context.Context, from, to time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM meetings WHERE state = 'ended' AND ai = 1 AND ended_at >= ? AND ended_at < ? ORDER BY ended_at`, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResetRuns puts every post run a stopped process was running back in the
// queue (at boot).
func (s *Store) ResetRuns(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_runs SET state = 'queued' WHERE kind = 'post' AND state = 'running'`)
	return err
}

// ClaimRun takes the oldest due post run: it is running from now, its
// attempts counted.
func (s *Store) ClaimRun(ctx context.Context, now time.Time) (Run, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM ai_runs WHERE kind = 'post' AND state = 'queued' AND next_at <= ? ORDER BY next_at, id LIMIT 1`, ms(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_runs SET state = 'running', attempts = attempts + 1 WHERE id = ?`, id); err != nil {
		return Run{}, false, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id WHERE r.id = ?`, id))
	if err != nil {
		return Run{}, false, err
	}
	return r, true, tx.Commit()
}

// NextRun is when the next queued post run is due.
func (s *Store) NextRun(ctx context.Context) (time.Time, bool, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT min(next_at) FROM ai_runs WHERE kind = 'post' AND state = 'queued'`).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false, err
	}
	return time.UnixMilli(v.Int64), true, nil
}

// RetryRun puts a running run back in the queue for next.
func (s *Store) RetryRun(ctx context.Context, id int64, next time.Time, phrase string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_runs SET state = 'queued', next_at = ?, error = ? WHERE id = ? AND state = 'running'`, ms(next), phrase, id)
	return err
}

// FailRun ends a run that will not finish.
func (s *Store) FailRun(ctx context.Context, id int64, phrase string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_runs SET state = 'failed', error = ?, finished_at = ? WHERE id = ? AND state = 'running'`, phrase, ms(now), id)
	return err
}

// Outcome is what a finished post run made.
type Outcome struct {
	Items   []NewItem
	Notes   []Note
	Summary *Summary
	Partial bool
	Phrase  string
}

// FinishRun stores a post run's outcome and makes it the meeting's current
// run; with mail on it drafts the notes anew for every recipient not yet
// sent to (RedraftResult).
func (s *Store) FinishRun(ctx context.Context, run Run, out Outcome, mail bool, now time.Time) (RedraftResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RedraftResult{}, err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM ai_runs WHERE id = ?`, run.ID).Scan(&state); err != nil {
		return RedraftResult{}, err
	}
	if state != RunRunning {
		return RedraftResult{}, ErrNoRun
	}
	if err := insertItems(ctx, tx, run.ID, out.Items, now); err != nil {
		return RedraftResult{}, err
	}
	for _, n := range out.Notes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_notes (run_id, frame_ms, text, segments) VALUES (?, ?, ?, ?)`, run.ID, n.FrameMS, n.Text, ints(n.Segments)); err != nil {
			return RedraftResult{}, err
		}
	}
	if out.Summary != nil {
		p, _ := json.Marshal(nonNilPoints(out.Summary.Points))
		d, _ := json.Marshal(nonNilPoints(out.Summary.Decisions))
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_summaries (run_id, points, decisions) VALUES (?, ?, ?)`, run.ID, string(p), string(d)); err != nil {
			return RedraftResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_runs SET state = 'done', partial = ?, error = ?, finished_at = ? WHERE id = ?`, out.Partial, out.Phrase, ms(now), run.ID); err != nil {
		return RedraftResult{}, err
	}
	res, err := makeCurrent(ctx, tx, run.MeetingID, run.ID, mail, now)
	if err != nil {
		return RedraftResult{}, err
	}
	if err := audit(ctx, tx, now, run.Subject, run.MeetingID, "analysis.done", runDetail(run.ID, len(out.Items), len(out.Notes))); err != nil {
		return RedraftResult{}, err
	}
	return res, tx.Commit()
}

func runDetail(id int64, items, notes int) string {
	b, _ := json.Marshal(map[string]int64{"run": id, "items": int64(items), "notes": int64(notes)})
	return string(b)
}

func nonNilPoints(p []Point) []Point {
	if p == nil {
		return []Point{}
	}
	return p
}

// makeCurrent makes runID the meeting's current run and, with mail on,
// redrafts.
func makeCurrent(ctx context.Context, tx *sql.Tx, meetingID string, runID int64, mail bool, now time.Time) (RedraftResult, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE ai_runs SET current = 0 WHERE meeting_id = ? AND current = 1`, meetingID); err != nil {
		return RedraftResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_runs SET current = 1 WHERE id = ? AND meeting_id = ?`, runID, meetingID); err != nil {
		return RedraftResult{}, err
	}
	if !mail {
		return RedraftResult{}, nil
	}
	return redraft(ctx, tx, meetingID, runID, now)
}

// ---- the owner's reads ----

// Runs is one of the owner's meetings' runs, newest first.
func (r *OwnerRepo) Runs(ctx context.Context, meetingID string) ([]Run, error) {
	if _, err := r.Meeting(ctx, meetingID); err != nil {
		return nil, err
	}
	rows, err := r.s.db.QueryContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND m.subject = ? ORDER BY r.id DESC`, meetingID, r.subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Run is one of the owner's meetings' runs.
func (r *OwnerRepo) Run(ctx context.Context, meetingID string, runID int64) (Run, error) {
	run, err := scanRun(r.s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.id = ? AND r.meeting_id = ? AND m.subject = ?`, runID, meetingID, r.subject))
	if errors.Is(err, ErrNotFound) {
		return Run{}, ErrNoRun
	}
	return run, err
}

// RunOutput is everything a run made.
type RunOutput struct {
	Items   []AIItem `json:"items"`
	Notes   []Note   `json:"notes"`
	Summary *Summary `json:"summary"`
}

// Output is a run's output (the run already checked as the owner's).
func (s *Store) Output(ctx context.Context, runID int64) (RunOutput, error) {
	items, err := s.RunItems(ctx, runID)
	if err != nil {
		return RunOutput{}, err
	}
	out := RunOutput{Items: items, Notes: []Note{}}
	rows, err := s.db.QueryContext(ctx, `SELECT frame_ms, text, segments FROM ai_notes WHERE run_id = ? ORDER BY frame_ms, id`, runID)
	if err != nil {
		return RunOutput{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Note
		var segs string
		if err := rows.Scan(&n.FrameMS, &n.Text, &segs); err != nil {
			return RunOutput{}, err
		}
		n.Segments = parseInts(segs)
		out.Notes = append(out.Notes, n)
	}
	if err := rows.Err(); err != nil {
		return RunOutput{}, err
	}
	var p, d string
	err = s.db.QueryRowContext(ctx, `SELECT points, decisions FROM ai_summaries WHERE run_id = ?`, runID).Scan(&p, &d)
	if err == nil {
		sum := &Summary{}
		_ = json.Unmarshal([]byte(p), &sum.Points)
		_ = json.Unmarshal([]byte(d), &sum.Decisions)
		out.Summary = sum
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RunOutput{}, err
	}
	return out, nil
}

// ErrRunNotDone is a revert to a run that has not finished.
var ErrRunNotDone = errors.New("that run has not finished")

// SetCurrent makes one of the owner's finished runs current again (revert),
// and with mail on redrafts every recipient not yet sent to from it.
func (r *OwnerRepo) SetCurrent(ctx context.Context, meetingID string, runID int64, mail bool, now time.Time) (RedraftResult, error) {
	run, err := r.Run(ctx, meetingID, runID)
	if err != nil {
		return RedraftResult{}, err
	}
	if run.State != RunDone {
		return RedraftResult{}, ErrRunNotDone
	}
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return RedraftResult{}, err
	}
	defer tx.Rollback()
	res, err := makeCurrent(ctx, tx, meetingID, runID, mail, now)
	if err != nil {
		return RedraftResult{}, err
	}
	if err := audit(ctx, tx, now, r.subject, meetingID, "analysis.current", runDetail(runID, 0, 0)); err != nil {
		return RedraftResult{}, err
	}
	return res, tx.Commit()
}

// CurrentRun is the meeting's current run (ErrNoRun for none).
func (s *Store) CurrentRun(ctx context.Context, meetingID string) (Run, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM ai_runs r JOIN meetings m ON m.id = r.meeting_id
		WHERE r.meeting_id = ? AND r.current = 1`, meetingID))
	if errors.Is(err, ErrNotFound) {
		return Run{}, ErrNoRun
	}
	return run, err
}

// AIMeetings is every open meeting with its AI switch on (the live
// worker's), with when its time starts.
func (s *Store) AIMeetings(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM meetings WHERE state = 'open' AND ai = 1 AND created_at > ? ORDER BY created_at`, ms(now.Add(-LinkLifetime)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FinalSegments is a meeting's transcript lines (no gap markers), in id
// order: what a model may read and cite.
func (s *Store) FinalSegments(ctx context.Context, meetingID string) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+segCols+` FROM segments s WHERE s.meeting_id = ? AND s.kind = 'text' ORDER BY s.id`, meetingID)
	if err != nil {
		return nil, err
	}
	return scanSegments(rows)
}

// SetAIFrames records that a meeting's notice names the frames of shared
// screens (the plugin had a vision model when it was made): only then may
// its post runs send frames.
func (s *Store) SetAIFrames(ctx context.Context, meetingID string, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE meetings SET ai_frames = ? WHERE id = ?`, on, meetingID)
	return err
}

// AIFrames says a meeting's notice named the frames (SetAIFrames).
func (s *Store) AIFrames(ctx context.Context, meetingID string) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT ai_frames = 1 FROM meetings WHERE id = ?`, meetingID).Scan(&on)
	return on, err
}

// MeetingAI is whether a meeting's AI switch is on and it is open.
func (s *Store) MeetingAI(ctx context.Context, meetingID string) (ai, open bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT ai = 1, state = 'open' FROM meetings WHERE id = ?`, meetingID).Scan(&ai, &open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, ErrNotFound
	}
	return ai, open, err
}

// Speakers is every name a meeting's lines were said under, and every
// guest's knock name, without " (guest)".
func (s *Store) Speakers(ctx context.Context, meetingID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT speaker FROM segments WHERE meeting_id = ?
		UNION SELECT display_name FROM participants WHERE meeting_id = ?`, meetingID, meetingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSuffix(n, " (guest)"))
	}
	return out, rows.Err()
}
