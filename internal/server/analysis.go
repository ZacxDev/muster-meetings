package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/items"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The analysis (design: "Action items"; decisions 4 and the
// reprocess rule): OPTIONAL, only with Config.AI.
//
//   - Live (4b): every MEETINGS_ITEMS_EVERY, each open meeting with its AI
//     switch on gets one window of its final lines through items.Loop (the
//     same rules the evaluation measured); kept items are stored under the
//     meeting's live run and sent on the owner's and the admitted guests'
//     streams ("items"); three failures in a row is "Action items paused".
//   - Post-call: when a meeting with its AI switch on has ended, a post run
//     is queued (in the database: it survives a restart); the worker runs
//     items.PostCall over the whole transcript, then the frames of shared
//     screens with the transcript said around them (items.Vision). The
//     finished run becomes the meeting's current run, and with
//     mail on its drafts are made (notes.go).
//   - Reprocess: the owner (POST …/reprocess) or an admin (the `reprocess`
//     command) queues another post run, under other models if they like;
//     every run is kept, and the owner may make an older one current again.
//
// 🔴 What was said is untrusted: the adapters fence it as data, the guards
// decide what is kept, and no log line carries a word of it, an item, a
// note or a name: ids, counts and error classes only.
//
//	GET  /api/meetings/{id}/analysis[?run=<id>]          the runs, and one run's items and frame notes
//	POST /api/meetings/{id}/reprocess                    {"items_model"?, "items_provider_prefs"?, "vision_model"?, "vision_provider_prefs"?} → 202 {"run"}
//	POST /api/meetings/{id}/analysis/runs/{rid}/current  make a finished run current (revert)
//	POST /api/meetings/{id}/agent-token                  a read-only token for this meeting's timeline, for a muster agent
//	GET  /api/agent/meetings/{id}/timeline               the bundle with the analysis, for Authorization: Bearer mmr_…
//	GET  /api/agent/meetings/{id}/frame?at=<ms>          one frame, for the same token

// AIConfig is the analysis' configuration.
type AIConfig struct {
	// Items is the action items' model (live and post-call).
	Items items.Config
	// Vision is the frame analysis' model; Model "" is none.
	Vision items.Config
	// Every is the live window's period; default items.DefaultEvery.
	Every time.Duration
	// HTTP is the client every adapter uses; nil is each one's own.
	HTTP *http.Client
	// RunRetry is the wait before a failed post run is tried again
	// (default DefaultRunRetry); RecordingWait how long a post run waits
	// for a recording still being written (default DefaultRecordingWait).
	RunRetry      time.Duration
	RecordingWait time.Duration
}

// The post run's waits and limits.
const (
	DefaultRunRetry      = 5 * time.Minute
	DefaultRecordingWait = 20 * time.Minute
	// RunAttempts is how many times a post run that failed outright is
	// tried.
	RunAttempts = 3
)

// The phrases a run's error says (never a model's words).
const (
	PhraseRefused  = "the AI service refused the request (check the model, its provider preferences and the key)"
	PhraseNoAnswer = "the AI service did not answer"
	PhrasePartial  = "part of the meeting could not be read; the items found are kept"
	PhraseNothing  = "nothing was said"
	PhraseTooMany  = "tried too many times; Run again to try once more"
	PhraseNoFrames = "the frames could not be read"
)

type liveRun struct {
	loop  *items.Loop
	runID int64
	busy  bool
	next  time.Time
}

type aiSide struct {
	mu    sync.Mutex
	live  map[string]*liveRun
	kick  chan struct{}
	prefs string // the configured items provider object, as stored
	// vision is the configured vision model and its provider object ("" for
	// none).
	visionModel, visionPrefs string
}

func prefsJSON(p map[string]any) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func newAISide(cfg Config) *aiSide {
	a := &aiSide{live: map[string]*liveRun{}, kick: make(chan struct{}, 1), prefs: prefsJSON(cfg.AI.Items.Prefs)}
	if cfg.AI.Vision.Model != "" && cfg.AI.Vision.Model != items.Off && cfg.S3 != nil {
		a.visionModel, a.visionPrefs = cfg.AI.Vision.Model, prefsJSON(cfg.AI.Vision.Prefs)
	}
	return a
}

// kickPost wakes the post-call worker.
func (s *Server) kickPost() {
	if s.ai == nil {
		return
	}
	select {
	case s.ai.kick <- struct{}{}:
	default:
	}
}

// runAI starts the analysis' workers (Run).
func (s *Server) runAI(ctx context.Context) {
	if s.ai == nil {
		return
	}
	go s.runLive(ctx)
	go s.runPost(ctx)
}

// itemSegments is the meeting's lines as a model reads them: the speaker's
// name without " (guest)", and only lines that have ended.
func itemSegments(segs []store.Segment) []items.Segment {
	out := make([]items.Segment, 0, len(segs))
	for _, g := range segs {
		if g.Kind != store.SegText || g.EndMS == nil {
			continue
		}
		out = append(out, items.Segment{ID: g.ID, Speaker: strings.TrimSuffix(g.Speaker, guestSuffix), Guest: g.Guest, StartMS: g.StartMS, EndMS: *g.EndMS, Text: g.Text})
	}
	return out
}

func fromStored(its []store.AIItem) []items.Item {
	out := make([]items.Item, 0, len(its))
	for _, it := range its {
		out = append(out, items.Item{Text: it.Text, Assignee: it.Assignee, Due: it.Due, Segments: it.Segments, Frames: it.Frames})
	}
	return out
}

func toStored(source string, its []items.Item) []store.NewItem {
	out := make([]store.NewItem, 0, len(its))
	for _, it := range its {
		out = append(out, store.NewItem{Source: source, Text: it.Text, Assignee: it.Assignee, Due: it.Due, Segments: it.Segments, FrameTimes: it.Frames})
	}
	return out
}

// ---- live ----

func (s *Server) runLive(ctx context.Context) {
	every := s.cfg.AI.Every
	if every <= 0 {
		every = items.DefaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.LiveTick(ctx)
		}
	}
}

// LiveTick is one period of the live worker: a window for each open meeting
// with its AI switch on that is not already in a call or backing off. It
// returns once every window it started has finished (a test's tick is a
// whole tick; the worker's goroutine is the only other caller).
func (s *Server) LiveTick(ctx context.Context) {
	now := s.cfg.Now()
	ids, err := s.store.AIMeetings(ctx, now)
	if err != nil {
		s.cfg.Log.Printf("items: listing meetings: %v", err)
		return
	}
	on := map[string]bool{}
	var wg sync.WaitGroup
	s.ai.mu.Lock()
	for _, id := range ids {
		on[id] = true
		lr := s.ai.live[id]
		if lr == nil {
			lr = &liveRun{}
			s.ai.live[id] = lr
		}
		if lr.busy || now.Before(lr.next) {
			continue
		}
		lr.busy = true
		wg.Add(1)
		go func(id string, lr *liveRun) {
			defer wg.Done()
			s.liveWindow(ctx, id, lr)
			s.ai.mu.Lock()
			lr.busy = false
			s.ai.mu.Unlock()
		}(id, lr)
	}
	var gone []*liveRun
	for id, lr := range s.ai.live {
		if !on[id] && !lr.busy {
			delete(s.ai.live, id)
			if lr.runID != 0 {
				gone = append(gone, lr)
			}
		}
	}
	s.ai.mu.Unlock()
	for _, lr := range gone {
		// Its meeting ended (or its switch went off): the live run is done.
		if err := s.store.StopLive(ctx, lr.runID, "", now); err != nil {
			s.cfg.Log.Printf("items: run %d: %v", lr.runID, err)
		}
	}
	wg.Wait()
}

func (s *Server) liveWindow(ctx context.Context, id string, lr *liveRun) {
	now := s.cfg.Now()
	if lr.loop == nil {
		a, err := items.New(s.cfg.AI.Items, s.cfg.AI.HTTP)
		if err != nil {
			s.cfg.Log.Printf("items: %v", err)
			lr.next = now.Add(time.Hour)
			return
		}
		run, err := s.store.EnsureLiveRun(ctx, id, s.cfg.AI.Items.Model, s.ai.prefs, now)
		if err != nil {
			s.cfg.Log.Printf("items: meeting %s: %v", id, err)
			return
		}
		kept, err := s.store.RunItems(ctx, run.ID)
		if err != nil {
			s.cfg.Log.Printf("items: meeting %s: %v", id, err)
			return
		}
		lr.loop, lr.runID = &items.Loop{Adapter: a}, run.ID
		lr.loop.Resume(run.Anchor, fromStored(kept))
		if run.State == store.RunFailed {
			lr.next = now.Add(24 * time.Hour) // stopped on a fatal answer: no more calls
			return
		}
	}
	if lr.loop.Stopped() != nil {
		return
	}
	start, err := s.store.MeetingStart(ctx, id)
	if err != nil {
		return
	}
	segs, err := s.store.FinalSegments(ctx, id)
	if err != nil {
		s.cfg.Log.Printf("items: meeting %s: %v", id, err)
		return
	}
	st, called := lr.loop.Tick(ctx, itemSegments(segs), s.sinceStart(start))
	if !called {
		return
	}
	if err := s.store.SaveLive(ctx, lr.runID, toStored(store.SrcLive, st.Kept), lr.loop.Anchor(), lr.loop.Failures(), s.cfg.Now()); err != nil {
		s.cfg.Log.Printf("items: meeting %s: %v", id, err)
	}
	if st.Err != nil {
		// The error's class and status only: never what was said.
		s.cfg.Log.Printf("items: meeting %s: window of %d lines failed (%s, %d in a row)", id, len(st.Window.Segments), items.ClassOf(st.Err), lr.loop.Failures())
	} else {
		s.cfg.Log.Printf("items: meeting %s: window of %d lines: %d kept, %d dropped", id, len(st.Window.Segments), len(st.Kept), len(st.Dropped))
	}
	if lr.loop.Stopped() != nil {
		if err := s.store.StopLive(ctx, lr.runID, PhraseRefused, s.cfg.Now()); err != nil {
			s.cfg.Log.Printf("items: run %d: %v", lr.runID, err)
		}
	}
	lr.next = s.cfg.Now().Add(lr.loop.Wait())
	s.hub.Changed(id)
}

// liveItem is one item as the live panel shows it.
type liveItem struct {
	ID       int64   `json:"id"`
	Text     string  `json:"text"`
	Assignee string  `json:"assignee"`
	Due      string  `json:"due"`
	Segments []int64 `json:"segments"`
}

// sendItems sends the meeting's live items when they (or the paused state)
// changed since last: nothing at all without the analysis, or with the
// meeting's AI switch off.
func (s *Server) sendItems(ctx context.Context, w interface{ Write([]byte) (int, error) }, f http.Flusher, meetingID string, last *string) error {
	if s.ai == nil {
		return nil
	}
	ai, _, err := s.store.MeetingAI(ctx, meetingID)
	if err != nil || !ai {
		return err
	}
	st, err := s.store.Live(ctx, meetingID)
	if err != nil {
		return err
	}
	out := struct {
		Items  []liveItem `json:"items"`
		Paused bool       `json:"paused"`
	}{Items: []liveItem{}, Paused: st.Stopped || st.Failures >= items.PausedAfter}
	for _, it := range st.Items {
		out.Items = append(out.Items, liveItem{ID: it.ID, Text: it.Text, Assignee: it.Assignee, Due: it.Due, Segments: it.Segments})
	}
	b, _ := json.Marshal(out)
	if string(b) == *last || (!st.Started && *last == "") {
		return nil
	}
	*last = string(b)
	return sse(w, f, "items", out)
}

// ---- post-call ----

func (s *Server) runPost(ctx context.Context) {
	if err := s.store.ResetRuns(ctx); err != nil {
		s.cfg.Log.Printf("postcall: %v", err)
	}
	for {
		s.PostPass(ctx)
		wait := 30 * time.Second
		if next, ok, err := s.store.NextRun(ctx); err == nil && ok {
			if d := next.Sub(s.cfg.Now()); d < wait {
				wait = max(d, time.Second)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.ai.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// PostPass queues the post run of every meeting that ended with its AI
// switch on and has none, then runs every due post run.
func (s *Server) PostPass(ctx context.Context) {
	now := s.cfg.Now()
	if n, err := s.store.QueueEnded(ctx, s.cfg.AI.Items.Model, s.ai.prefs, s.ai.visionModel, s.ai.visionPrefs, now); err != nil {
		s.cfg.Log.Printf("postcall: queueing: %v", err)
	} else if n > 0 {
		s.cfg.Log.Printf("postcall: %d meetings queued", n)
	}
	for ctx.Err() == nil {
		run, ok, err := s.store.ClaimRun(ctx, s.cfg.Now())
		if err != nil {
			s.cfg.Log.Printf("postcall: %v", err)
			return
		}
		if !ok {
			return
		}
		s.postRun(ctx, run)
	}
}

// configOf is the stored model and provider object as a Config: the
// account key and API root are the configured ones.
func (s *Server) configOf(model, prefs string) (items.Config, error) {
	ad := items.AdapterFor(model)
	p, err := items.ParsePrefs(prefs, ad)
	if err != nil {
		return items.Config{}, err
	}
	c := s.cfg.AI.Items
	c.Model, c.Adapter, c.Prefs = model, ad, p
	return c, nil
}

// sleep waits d on the clock, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// recordingPending says a part of the meeting's recording is still being
// written (its egress has not ended): frames would miss its end.
func (s *Server) recordingPending(ctx context.Context, run store.Run) bool {
	parts, _, err := s.store.ForSubject(run.Subject).Recordings(ctx, run.MeetingID)
	if err != nil {
		return false
	}
	for _, p := range parts {
		if p.State == store.RecStarting || p.State == store.RecActive {
			return true
		}
	}
	return false
}

func (s *Server) postRun(ctx context.Context, run store.Run) {
	now := s.cfg.Now()
	retry := s.cfg.AI.RunRetry
	if retry <= 0 {
		retry = DefaultRunRetry
	}
	waitRec := s.cfg.AI.RecordingWait
	if waitRec <= 0 {
		waitRec = DefaultRecordingWait
	}
	// 🔴 A bounded number of tries, whatever made them fail (a model, the
	// store, a restart mid-run): never an endless re-spend on the model.
	if run.Attempts > RunAttempts {
		_ = s.store.FailRun(ctx, run.ID, PhraseTooMany, now)
		s.cfg.Log.Printf("postcall: run %d: failed after %d tries", run.ID, run.Attempts-1)
		return
	}
	if run.VisionModel != "" && s.cfg.S3 != nil && s.recordingPending(ctx, run) && now.Sub(run.CreatedAt) < waitRec {
		_ = s.store.WaitRun(ctx, run.ID, now.Add(30*time.Second))
		return
	}
	raw, err := s.store.FinalSegments(ctx, run.MeetingID)
	if err != nil {
		s.cfg.Log.Printf("postcall: run %d: %v", run.ID, err)
		_ = s.store.RetryRun(ctx, run.ID, now.Add(retry), "")
		return
	}
	segs := itemSegments(raw)
	if len(segs) == 0 {
		s.finish(ctx, run, store.Outcome{Phrase: PhraseNothing})
		return
	}
	ic, err := s.configOf(run.ItemsModel, run.ItemsPrefs)
	if err != nil {
		s.cfg.Log.Printf("postcall: run %d: the stored model's provider preferences: %v", run.ID, err)
		_ = s.store.FailRun(ctx, run.ID, PhraseRefused, now)
		return
	}
	a, err := items.New(items.PostCallConfig(ic), s.cfg.AI.HTTP)
	if err != nil {
		_ = s.store.FailRun(ctx, run.ID, PhraseRefused, now)
		return
	}
	kept, err := items.PostCall(ctx, a, segs, items.PostCallOpts{Sleep: func(d time.Duration) { sleep(ctx, d) }})
	var out store.Outcome
	if err != nil {
		s.cfg.Log.Printf("postcall: run %d: the items pass: %s (%d items kept)", run.ID, items.ClassOf(err), len(kept))
		if len(kept) == 0 {
			phrase := PhraseNoAnswer
			if items.ClassOf(err) == items.Fatal {
				phrase = PhraseRefused
			}
			if run.Attempts < RunAttempts && ctx.Err() == nil {
				_ = s.store.RetryRun(ctx, run.ID, now.Add(retry*time.Duration(run.Attempts)), phrase)
			} else {
				_ = s.store.FailRun(ctx, run.ID, phrase, s.cfg.Now())
			}
			return
		}
		out.Partial, out.Phrase = true, PhrasePartial
	}
	notes, framed, ferr := s.analyseFrames(ctx, run, segs, kept)
	out.Items = toStored(store.SrcTranscript, kept)
	if ferr != nil {
		out.Partial = true
		if out.Phrase == "" {
			out.Phrase = PhraseNoFrames
		}
	}
	out.Notes = notes
	out.Items = append(out.Items, toStored(store.SrcFrames, framed)...)
	s.finish(ctx, run, out)
}

func (s *Server) finish(ctx context.Context, run store.Run, out store.Outcome) {
	res, err := s.store.FinishRun(ctx, run, out, s.notes != nil, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("postcall: run %d: storing: %v", run.ID, err)
		_ = s.store.RetryRun(ctx, run.ID, s.cfg.Now().Add(time.Minute), "")
		return
	}
	s.cfg.Log.Printf("postcall: meeting %s run %d done: %d items, %d frame notes, partial %t; %d drafts made, %d recipients already sent",
		run.MeetingID, run.ID, len(out.Items), len(out.Notes), out.Partial, res.Made, res.AlreadySent)
}

// call runs fn up to PostCallAttempts times: a retryable failure after the
// backoff, a schema failure at once, a fatal one never again.
func call(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= items.PostCallAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		switch items.ClassOf(err) {
		case items.Retry:
			var e *items.Error
			var after time.Duration
			if errors.As(err, &e) {
				after = e.RetryAfter
			}
			sleep(ctx, items.Backoff(attempt, after))
		case items.Schema:
		default:
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

// analyseFrames is the frame analysis of a run: the frames of shared
// screens at the moments a line was said over them (the timeline's
// frames), at most items.MaxFrames spread evenly, in batches of
// items.FramesPerCall with the transcript said around them.
func (s *Server) analyseFrames(ctx context.Context, run store.Run, segs []items.Segment, kept []items.Item) ([]store.Note, []items.Item, error) {
	if run.VisionModel == "" || s.cfg.S3 == nil || !s.cfg.Frames.Available() {
		return nil, nil, nil
	}
	// kept is the caller's: refinements change its items in place.
	// 🔴 Only a meeting whose notice named the frames (SetAIFrames): a
	// meeting from before the frame analysis, reprocessed, sends none.
	if on, err := s.store.AIFrames(ctx, run.MeetingID); err != nil || !on {
		return nil, nil, err
	}
	repo := s.store.ForSubject(run.Subject)
	b, err := s.bundle(ctx, repo, run.MeetingID)
	if err != nil {
		return nil, nil, err
	}
	var times []int64
	for _, f := range b.Frames {
		times = append(times, f.MeetingMS)
	}
	times = items.Spread(times, items.MaxFrames)
	if len(times) == 0 {
		return nil, nil, nil
	}
	vc, err := s.configOf(run.VisionModel, run.VisionPrefs)
	if err != nil {
		return nil, nil, err
	}
	vc.Timeout = items.PostCallTimeout
	v, err := items.NewVision(vc, s.cfg.AI.HTTP)
	if err != nil {
		return nil, nil, err
	}
	keys := map[int]string{}
	for _, p := range b.Parts {
		keys[p.Part] = p.ObjectKey
	}
	limit := items.PostCallCap(segs) - len(kept)
	var notes []store.Note
	var framed []items.Item
	var lastErr error
	for i := 0; i < len(times); i += items.FramesPerCall {
		batch := times[i:min(i+items.FramesPerCall, len(times))]
		s.rec.frames <- struct{}{}
		got, _, err := s.cfg.Frames.Sample(ctx, store.Parts(b.Parts), func(part int) frames.Object {
			return func(ctx context.Context, rng string) (*http.Response, error) {
				return s.cfg.S3.Get(ctx, keys[part], rng)
			}
		}, batch)
		<-s.rec.frames
		if err != nil || len(got) == 0 {
			lastErr = errors.New("no frame")
			continue
		}
		w := items.VisionWindow{Existing: append(append([]items.Item{}, kept...), framed...)}
		var at []int64
		for _, g := range got {
			w.Frames = append(w.Frames, items.FrameIn{MS: g.MeetingMS, JPEG: g.JPEG})
			at = append(at, g.MeetingMS)
		}
		w.Segments = items.Excerpt(segs, at)
		var ns []items.Note
		var its []items.Item
		err = call(ctx, func() error {
			var err error
			ns, its, _, err = v.Describe(ctx, w)
			return err
		})
		if err != nil {
			s.cfg.Log.Printf("postcall: run %d: frames at %v: %s", run.ID, at, items.ClassOf(err))
			lastErr = err
			if items.ClassOf(err) == items.Fatal {
				break
			}
			continue
		}
		kn, ki, refs, dropped := items.GuardVision(w, ns, its, max(limit-len(framed), 0))
		for _, n := range kn {
			notes = append(notes, store.Note{FrameMS: n.FrameMS, Text: n.Text, Segments: n.Segments})
		}
		// A refinement sharpens an item the meeting has (the transcript's,
		// or an earlier batch's), and adds the frames that show it.
		for _, rf := range refs {
			tgt := &framed
			j := rf.Index
			if j < len(kept) {
				tgt = &kept
			} else {
				j -= len(kept)
			}
			(*tgt)[j].Text = rf.Text
			(*tgt)[j].Frames = mergeTimes((*tgt)[j].Frames, rf.Frames)
		}
		framed = append(framed, ki...)
		s.cfg.Log.Printf("postcall: run %d: %d frames: %d notes, %d items kept, %d refined, %d dropped", run.ID, len(at), len(kn), len(ki), len(refs), len(dropped))
	}
	if lastErr != nil && len(notes) == 0 && len(framed) == 0 {
		return nil, nil, lastErr
	}
	return notes, framed, nil
}

func mergeTimes(a, b []int64) []int64 {
	out := append([]int64{}, a...)
	for _, v := range b {
		found := false
		for _, x := range out {
			found = found || x == v
		}
		if !found {
			out = append(out, v)
		}
	}
	return out
}

// ---- the owner's routes ----

func (s *Server) analysisRoutes() {
	s.mux.HandleFunc("GET /api/meetings/{id}/analysis", s.owner(s.handleAnalysis))
	s.mux.HandleFunc("POST /api/meetings/{id}/reprocess", s.owner(s.handleReprocess))
	s.mux.HandleFunc("POST /api/meetings/{id}/analysis/runs/{rid}/current", s.owner(s.handleSetCurrent))
	s.mux.HandleFunc("POST /api/meetings/{id}/agent-token", s.owner(s.handleAgentToken))
	s.mux.HandleFunc("GET /api/agent/meetings/{id}/timeline", s.agent(s.handleTimeline))
	s.mux.HandleFunc("GET /api/agent/meetings/{id}/frame", s.agent(s.handleFrame))
}

// analysisView is GET …/analysis.
type analysisView struct {
	Runs    []store.Run      `json:"runs"`
	Current *int64           `json:"current"`
	Run     *store.Run       `json:"run"`
	Output  *store.RunOutput `json:"output"`
	// Mail says the notes can be emailed (a mail server is configured).
	Mail bool `json:"mail"`
	// Models are the configured ones, for the reprocess form.
	ItemsModel  string `json:"items_model"`
	VisionModel string `json:"vision_model"`
}

func (s *Server) handleAnalysis(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	runs, err := repo.Runs(r.Context(), id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	v := analysisView{Runs: runs, Mail: s.notes != nil, ItemsModel: s.cfg.AI.Items.Model, VisionModel: s.ai.visionModel}
	want := int64(0)
	if q := r.URL.Query().Get("run"); q != "" {
		if want, err = strconv.ParseInt(q, 10, 64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "run is a run id"})
			return
		}
	}
	for i := range runs {
		if runs[i].Current {
			v.Current = &runs[i].ID
			if want == 0 {
				want = runs[i].ID
			}
		}
	}
	for i := range runs {
		if runs[i].ID == want {
			v.Run = &runs[i]
		}
	}
	if want != 0 && v.Run == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such run"})
		return
	}
	if v.Run != nil {
		o, err := s.store.Output(r.Context(), v.Run.ID)
		if err != nil {
			s.meetingErr(w, err)
			return
		}
		v.Output = &o
	}
	writeJSON(w, http.StatusOK, v)
}

// modelRE is an OpenRouter slug: provider/model, with an optional :variant.
var modelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*(:[a-z0-9._-]+)?$`)

// ErrReprocessOpen is a reprocess of a meeting still open; ErrReprocessNoAI
// one whose AI action items were off (its guests were told nothing goes to
// an AI service).
const (
	ErrReprocessOpen = "The meeting is still open: it is processed when it ends."
	ErrReprocessNoAI = "AI action items were off for this meeting, so nothing of it may go to an AI service."
)

func (s *Server) handleReprocess(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	var in struct {
		ItemsModel  string          `json:"items_model"`
		ItemsPrefs  json.RawMessage `json:"items_provider_prefs"`
		VisionModel string          `json:"vision_model"`
		VisionPrefs json.RawMessage `json:"vision_provider_prefs"`
	}
	if !readJSON(w, r, 4096, &in) {
		return
	}
	id := r.PathValue("id")
	m, err := repo.Meeting(r.Context(), id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if m.State != "ended" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": ErrReprocessOpen})
		return
	}
	if !m.AI {
		writeJSON(w, http.StatusConflict, map[string]any{"error": ErrReprocessNoAI})
		return
	}
	im, ip, err := s.pickModel(in.ItemsModel, in.ItemsPrefs, s.cfg.AI.Items.Model, s.ai.prefs, "items")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	vm, vp := s.ai.visionModel, s.ai.visionPrefs
	if in.VisionModel == items.Off {
		vm, vp = "", ""
	} else if in.VisionModel != "" || len(in.VisionPrefs) > 0 {
		if s.cfg.S3 == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "This plugin records nothing, so there are no frames to analyse."})
			return
		}
		if vm, vp, err = s.pickModel(in.VisionModel, in.VisionPrefs, s.ai.visionModel, s.ai.visionPrefs, "vision"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	run, err := s.store.QueuePost(r.Context(), id, im, ip, vm, vp, "owner", s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	s.kickPost()
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run, "items_model": im, "vision_model": vm})
}

// pickModel is a reprocess' model and provider object: the configured ones
// by default; another model needs its own provider object (a pin), which
// is checked as the configuration's is (data_collection deny).
func (s *Server) pickModel(model string, prefs json.RawMessage, defModel, defPrefs, what string) (string, string, error) {
	if model == "" && len(prefs) == 0 {
		if defModel == "" {
			return "", "", errors.New("No " + what + " model is configured.")
		}
		return defModel, defPrefs, nil
	}
	if model == "" {
		model = defModel
	}
	if len(model) > 120 || !modelRE.MatchString(model) {
		return "", "", errors.New("The " + what + " model is an OpenRouter slug, like provider/model-name.")
	}
	raw := defPrefs
	if len(prefs) > 0 {
		raw = string(prefs)
	} else if model != defModel {
		return "", "", errors.New("Another " + what + " model needs its " + what + "_provider_prefs (OpenRouter's provider object, with a provider pin).")
	}
	ad := items.AdapterFor(model)
	if what == "vision" {
		ad = items.AdapterChatJSON
	}
	p, err := items.ParsePrefs(raw, ad)
	if err != nil {
		return "", "", errors.New(strings.Replace(err.Error(), "MEETINGS_ITEMS_PROVIDER_PREFS", what+"_provider_prefs", 1))
	}
	return model, prefsJSON(p), nil
}

func (s *Server) handleSetCurrent(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such run"})
		return
	}
	res, err := repo.SetCurrent(r.Context(), r.PathValue("id"), rid, s.notes != nil, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNoRun):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such run"})
		return
	case errors.Is(err, store.ErrRunNotDone):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "That run has not finished."})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": rid, "drafts": res})
}

// ---- the agent's read path ----

func (s *Server) handleAgentToken(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	tok, exp, err := repo.MintAgentToken(r.Context(), id, s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": exp,
		"timeline_url": s.cfg.PublicURL + "/api/agent/meetings/" + id + "/timeline"})
}

// agent is the agent read token's door: Authorization: Bearer mmr_… for
// exactly the meeting in the path, read through its owner's read-only
// repository. 🔴 A missing, unknown, expired token, or one for another
// meeting, is the same 404.
func (s *Server) agent(next ownerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		subject, meetingID, err := s.store.AgentToken(r.Context(), tok, s.cfg.Now())
		if err != nil || meetingID != r.PathValue("id") || !s.app.Live() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		next(w, r, s.store.ForSubject(subject))
	}
}

// bundleAnalysis is the timeline bundle's analysis: the current run's.
type bundleAnalysis struct {
	Run    store.Run       `json:"run"`
	Output store.RunOutput `json:"output"`
}

// analysisOf is the meeting's current run and its output, nil for none (or
// without the analysis).
func (s *Server) analysisOf(ctx context.Context, meetingID string) (*bundleAnalysis, error) {
	if s.ai == nil {
		return nil, nil
	}
	run, err := s.store.CurrentRun(ctx, meetingID)
	if errors.Is(err, store.ErrNoRun) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := s.store.Output(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	return &bundleAnalysis{Run: run, Output: o}, nil
}
