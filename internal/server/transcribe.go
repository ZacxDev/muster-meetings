package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
)

// The transcript (design: "Transcription", "Media: LiveKit, TURN, Egress",
// "Segments", "Failure modes"):
//
//   - On every track_published webhook for a MICROPHONE track of an identity
//     that may be in the room (store.MayBeInRoom), the plugin starts a LiveKit
//     track egress that streams that track's raw PCM to its ingest listener,
//     ws://<ingest>/stt/ingest/<track>/<token>. The token is the only
//     credential (Egress sends none; internal/ingest).
//   - A refused start is retried with backoff (1, 2, 4 … 30 s) while a gap
//     marker says the track is not transcribed; so is an egress that ends
//     while its track is still published (a dropped stream ends an egress for
//     good: spike S2). The gap closes when audio flows again.
//   - The ingest takes one channel of Egress' S16LE stereo 48 kHz, resamples
//     it to 16 kHz, and writes it to the configured engine's stream. Segment
//     times are the stream's SAMPLE COUNT, placed in meeting time at the
//     stream's first sample: never by arrival.
//   - Finals and gap markers are stored; partials are only sent. Both event
//     streams, the owner's and an admitted guest's, carry them.
//
// With no engine (MEETINGS_STT_ENGINE unset or "off") transcription is off:
// no egress is started and the ingest refuses every stream.

// The transcript panel's status lines (design: "Segments", "Failure
// modes"): "Transcript paused" when no engine can take a track's audio (a
// gap), which wins over "Captions delayed" while a track is on the
// nemotron engine's fallback.
const (
	TranscriptPaused = "Transcript paused"
	CaptionsDelayed  = "Captions delayed"
)

// DefaultEgressBackoff is the wait before each retry of a refused track
// egress start (design: "retries a refused track start with backoff (1, 2,
// 4 … 30 s)"); the last is repeated.
var DefaultEgressBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

// transcriber is the in-memory side: the start loops, the open ingest
// streams, the partials and the engines' paused tracks.
type transcriber struct {
	mu       sync.Mutex
	runs     map[string]*run               // track → its egress start loop
	conns    map[string]*liveConn          // track → its ingest stream
	partials map[string]map[string]partial // meeting → track → the partial
	paused   map[string]bool               // track → the engine dropped audio, until its next final
	// delayed is track → the ingest stream whose engine stream is on the
	// nemotron engine's fallback (stt.EngineSwitch). 🔴 Keyed by STREAM: an
	// old stream draining after its replacement can neither set nor clear
	// its successor's state, and counts only while it is the track's
	// current stream (transcriptStatus).
	delayed  map[string]*liveConn
	meetings map[string]string // track → meeting, for paused
	// restarts counts a track's egress restarts since its last healthy
	// stretch; audioAt is when its current stream's audio began.
	restarts map[string]int
	audioAt  map[string]time.Time
	textOnce atomic.Bool
	// flags counts the silent-start check's flags (stt.Flag) since the
	// plugin started: /healthz says so.
	flags atomic.Int64
	// gapMu makes "is a current stream connected? if not, open a gap"
	// (egressLoop) and "a current stream's first audio closes the gap"
	// (serveIngest) one step each, so neither can fall between the other's
	// check and write and leave a gap open while audio flows.
	gapMu sync.Mutex
	// amends is meeting → the lines amended in place (a late closing mark,
	// stt.Segment.Lead), in order of amendSeq, the last MaxAmendLog kept:
	// what each event stream re-sends as an `amend` event for a line it has
	// already sent. amendSeq numbers every amend of every meeting.
	amends   map[string][]amendRec
	amendSeq int64
}

// amendRec is one amend: its number and the line's segment id.
type amendRec struct {
	seq int64
	id  int64
}

// MaxAmendLog is how many amends of one meeting are kept for the event
// streams. Every stream catches up on each change of its meeting, so it is
// never more than a few behind; a stream further behind than this misses
// the oldest (the archive and the export still read the amended line).
const MaxAmendLog = 1024

// noteAmend records that segment id of meetingID was amended, AFTER the
// store has it: a stream that reads the record then reads the new text.
func (s *Server) noteAmend(meetingID string, id int64) {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	s.tr.amendSeq++
	l := append(s.tr.amends[meetingID], amendRec{seq: s.tr.amendSeq, id: id})
	if len(l) > MaxAmendLog {
		l = append([]amendRec(nil), l[len(l)-MaxAmendLog:]...)
	}
	s.tr.amends[meetingID] = l
}

// amendSeqNow is the number of the latest amend of any meeting.
func (s *Server) amendSeqNow() int64 {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	return s.tr.amendSeq
}

// amendsSince is the ids of meetingID's lines amended after seq (each once,
// oldest first), and the latest amend's number.
func (s *Server) amendsSince(meetingID string, seq int64) ([]int64, int64) {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	l := s.tr.amends[meetingID]
	i := sort.Search(len(l), func(i int) bool { return l[i].seq > seq })
	var ids []int64
	seen := map[int64]bool{}
	for _, r := range l[i:] {
		if !seen[r.id] {
			seen[r.id] = true
			ids = append(ids, r.id)
		}
		seq = r.seq
	}
	return ids, seq
}

// liveConn is a registered ingest stream and the egress nonce its token
// carried.
type liveConn struct {
	conn  *ingest.Conn
	nonce string
}

// Test hooks (nil outside tests): testHookBeforeRegister runs in
// handleIngest between the token check and the stream's registration;
// testHookGapCheck inside egressLoop's gap step, after its check.
var (
	testHookBeforeRegister func(track string)
	testHookGapCheck       func(track string)
)

// HealthyStream is how long a track's stream must have carried audio for
// its egress-restart backoff to start again from the first step.
const HealthyStream = 60 * time.Second

type run struct{ cancel context.CancelFunc }

type partial struct {
	Speaker string `json:"speaker"`
	StartMS int64  `json:"start_ms"`
	Text    string `json:"text"` // untrusted: render as text only
}

func newTranscriber() *transcriber {
	return &transcriber{runs: map[string]*run{}, conns: map[string]*liveConn{},
		partials: map[string]map[string]partial{}, paused: map[string]bool{}, delayed: map[string]*liveConn{}, meetings: map[string]string{},
		restarts: map[string]int{}, audioAt: map[string]time.Time{}, amends: map[string][]amendRec{}}
}

// Transcribing says the plugin has an engine.
func (s *Server) Transcribing() bool { return s.cfg.Engine != nil }

// noticeFor is the consent notice for a meeting's switches on this plugin:
// the "transcribed" clause and the Transcribing badge only while it has an
// engine (lobby.NoticeWith). Every page and stream takes its notice here.
func (s *Server) noticeFor(record, ai bool) lobby.Notice {
	return lobby.NoticeWith(record, ai, s.Transcribing())
}

// sinceStart is now in meeting time (ms from start), never negative.
func (s *Server) sinceStart(start time.Time) int64 {
	return max(s.cfg.Now().Sub(start).Milliseconds(), 0)
}

// lkKind names a LiveKit error by its status and code only: LiveKit's
// message could echo what it was sent, and a start's websocket URL carries
// the ingest token.
func lkKind(err error) string {
	var e *livekit.APIError
	if errors.As(err, &e) {
		return e.Code + " " + http.StatusText(e.Status)
	}
	return "unreachable"
}

// ---- the webhook's part ----

// onTrackPublished starts transcribing a microphone track of someone who may
// be in the room.
func (s *Server) onTrackPublished(ctx context.Context, room, identity string, tr livekit.TrackInfo) {
	if !s.Transcribing() || !tr.Microphone() || !ingest.ValidTrack(tr.SID) {
		return
	}
	// 🔴 Only an identity that may be in the room is transcribed: the
	// participant_joined check removes anyone else, and until it has, their
	// audio is not taken.
	ok, err := s.store.MayBeInRoom(ctx, room, identity, s.cfg.Now())
	if err != nil || !ok {
		return
	}
	name, guest, err := s.store.NameInRoom(ctx, room, identity)
	if err != nil {
		return
	}
	speaker := name
	if guest {
		speaker = guestLabel(name)
	} else if speaker == "" {
		speaker = "Host"
	}
	t, err := s.store.UpsertTrack(ctx, room, tr.SID, identity, speaker, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("transcript: a published track was not recorded: %v", err)
		return
	}
	s.startEgress(t.Track)
}

// onTrackUnpublished stops transcribing a track: nothing restarts its egress,
// and its token opens nothing.
func (s *Server) onTrackUnpublished(ctx context.Context, tr livekit.TrackInfo) {
	if !ingest.ValidTrack(tr.SID) {
		return
	}
	t, err := s.store.TrackByID(ctx, tr.SID, s.cfg.Now())
	if err != nil {
		return
	}
	s.cancelRun(t.Track)
	if err := s.store.EndTrack(ctx, t.Track); err != nil {
		s.cfg.Log.Printf("transcript: ending a track: %v", err)
	}
	s.forgetTrack(t.MeetingID, t.Track)
	if closed, _ := s.store.CloseGap(ctx, t.Track, s.sinceStart(t.MeetingStart)); closed {
		s.hub.Changed(t.MeetingID)
	}
}

// onEgressEnded restarts the egress of a track that is still being
// transcribed: Egress never reconnects a dropped stream (S2), so an ended
// egress whose track is still published and whose meeting is live is a gap
// until a new one streams. An egress that is not a track's CURRENT one (an
// older one, or not the plugin's) changes nothing.
//
// 🔴 A restart waits on the same backoff as a refused start (1, 2, 4 … 30 s),
// counted per track, so an egress that keeps dying is not restarted in a
// tight loop. The count starts again once a stream has carried audio for
// HealthyStream.
func (s *Server) onEgressEnded(ctx context.Context, info livekit.EgressInfo) {
	if !s.Transcribing() {
		return
	}
	t, err := s.store.TrackByEgress(ctx, info.EgressID, s.cfg.Now())
	if err != nil || t.State != store.TrackActive || !t.MeetingLive {
		return
	}
	// Its stream is dead with it, even if the ingest has not yet seen the
	// socket close (the webhook can come first): it is closed here, so it
	// neither stops the new start (egressLoop) nor counts as a second death.
	s.closeConn(t.Track, ingest.CloseGoingAway, "its egress ended")
	wait := s.restartWait(t.Track)
	s.cfg.Log.Printf("transcript: a track's egress ended (%s); starting a new one in %s", info.Status, wait)
	if err := s.store.OpenGap(ctx, t.Track, s.sinceStart(t.MeetingStart), s.cfg.Engine.Name(), s.cfg.Now()); err == nil {
		s.hub.Changed(t.MeetingID)
	}
	s.startEgressAfter(t.Track, wait)
}

// restartWait is how long track's next egress restart waits: the backoff's
// step for the restarts since its last healthy stretch (a stream that
// carried audio for HealthyStream resets the count).
//
// The count is written first and the track read after: a track that ended
// meanwhile (its end writes the database, then forgetTrack) leaves no count
// behind, whichever of the two came first.
func (s *Server) restartWait(track string) time.Duration {
	s.tr.mu.Lock()
	if at, ok := s.tr.audioAt[track]; ok && s.cfg.Now().Sub(at) >= HealthyStream {
		s.tr.restarts[track] = 0
	}
	delete(s.tr.audioAt, track)
	n := s.tr.restarts[track]
	s.tr.restarts[track] = n + 1
	s.tr.mu.Unlock()
	if t, err := s.store.TrackByID(context.Background(), track, s.cfg.Now()); err != nil || t.State != store.TrackActive || !t.MeetingLive {
		s.tr.mu.Lock()
		delete(s.tr.restarts, track)
		s.tr.mu.Unlock()
	}
	b := s.cfg.EgressBackoff
	return b[min(n, len(b)-1)]
}

// forgetTrack drops the in-memory state of a track that is no longer
// transcribed, and the meeting's partials map once it is empty.
func (s *Server) forgetTrack(meetingID, track string) {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	delete(s.tr.paused, track)
	delete(s.tr.delayed, track)
	delete(s.tr.meetings, track)
	delete(s.tr.restarts, track)
	delete(s.tr.audioAt, track)
	if m := s.tr.partials[meetingID]; m != nil {
		delete(m, track)
		if len(m) == 0 {
			delete(s.tr.partials, meetingID)
		}
	}
}

// after is the clock's timer (Config.After, else time.After).
func (s *Server) after(d time.Duration) <-chan time.Time {
	if s.cfg.After != nil {
		return s.cfg.After(d)
	}
	return time.After(d)
}

// ---- starting a track egress ----

func (s *Server) cancelRun(track string) {
	s.tr.mu.Lock()
	if r := s.tr.runs[track]; r != nil {
		r.cancel()
		delete(s.tr.runs, track)
	}
	s.tr.mu.Unlock()
}

// startEgress runs track's start loop, replacing one already running.
func (s *Server) startEgress(track string) { s.startEgressAfter(track, 0) }

// startEgressAfter is startEgress after a wait (cancelled with the loop).
func (s *Server) startEgressAfter(track string, wait time.Duration) {
	ctx, cancel := context.WithCancel(s.bg)
	me := &run{cancel: cancel}
	s.tr.mu.Lock()
	if r := s.tr.runs[track]; r != nil {
		r.cancel()
	}
	s.tr.runs[track] = me
	s.tr.mu.Unlock()
	go func() {
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-s.after(wait):
			}
		}
		if ctx.Err() == nil {
			s.egressLoop(ctx, track)
		}
		cancel()
		s.tr.mu.Lock()
		// Only this loop's own entry is removed: a newer loop may have
		// replaced it.
		if s.tr.runs[track] == me {
			delete(s.tr.runs, track)
		}
		s.tr.mu.Unlock()
	}()
}

// egressLoop starts a track egress for track, retrying a refusal with
// backoff until it is accepted, the track is unpublished or ended, its
// meeting is no longer live, or an egress is already streaming the track.
// Each attempt draws a new nonce, so the token of any earlier attempt opens
// nothing.
//
// 🔴 A connected stream ends the retries. A start can fail at LiveKit (it
// waits for an Egress to answer, then times out) while the Egress it asked
// does start and connect; a further attempt would draw a new nonce and cut
// that live stream off. So once the track has a stream, no attempt is made,
// and a failed attempt does not mark a gap or wait to retry.
func (s *Server) egressLoop(ctx context.Context, track string) {
	backoff := s.cfg.EgressBackoff
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		t, err := s.store.TrackByID(ctx, track, s.cfg.Now())
		if err != nil || t.State != store.TrackActive || !t.MeetingLive {
			return
		}
		if s.streamingCurrent(ctx, track) {
			return
		}
		room := store.RoomName(t.MeetingID)
		ps, err := s.cfg.LiveKit.ListParticipants(ctx, room)
		if err == nil && !livekit.Published(ps, t.Identity, track) {
			// Gone from the room: nothing to transcribe.
			s.store.EndTrack(ctx, track)
			if closed, _ := s.store.CloseGap(ctx, track, s.sinceStart(t.MeetingStart)); closed {
				s.hub.Changed(t.MeetingID)
			}
			return
		}
		if err == nil {
			nonce := ingest.NewNonce()
			if err = s.store.SetNonce(ctx, track, nonce); err == nil {
				url := s.cfg.IngestURL + ingest.Path(track, ingest.Token(s.cfg.IngestKey, t.MeetingID, track, nonce, t.MeetingExp))
				var info livekit.EgressInfo
				if info, err = s.cfg.LiveKit.StartTrackEgress(ctx, room, track, url); err == nil {
					s.store.SetEgress(ctx, track, nonce, info.EgressID)
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait := backoff[min(attempt, len(backoff)-1)]
		// The track is not transcribed until a start is accepted and audio
		// flows: a gap marker says so; unless a current stream connected
		// while this attempt was in flight (LiveKit timed the start out, the
		// Egress ran), when the loop is done. Check and gap are one step
		// against the first audio (gapMu).
		s.tr.gapMu.Lock()
		if s.streamingCurrent(ctx, track) {
			s.tr.gapMu.Unlock()
			return
		}
		if testHookGapCheck != nil {
			testHookGapCheck(track)
		}
		s.cfg.Log.Printf("transcript: starting a track egress failed (attempt %d: %s); retrying in %s", attempt+1, lkKind(err), wait)
		opened := s.store.OpenGap(ctx, track, s.sinceStart(t.MeetingStart), s.cfg.Engine.Name(), s.cfg.Now()) == nil
		s.tr.gapMu.Unlock()
		if opened {
			s.hub.Changed(t.MeetingID)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.after(wait):
		}
	}
}

// resumeTranscription is the start of a restarted plugin: every active track
// of a live meeting lost its egress when the plugin went away (Egress never
// reconnects), so each gets a gap and a new egress (design: "re-opens STT
// streams; speech in the gap is not transcribed (gap marker)").
func (s *Server) resumeTranscription(ctx context.Context) {
	if !s.Transcribing() {
		return
	}
	ts, err := s.store.LiveTracks(ctx, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("transcript: resuming: %v", err)
		return
	}
	for _, t := range ts {
		if err := s.store.OpenGap(ctx, t.Track, s.sinceStart(t.MeetingStart), s.cfg.Engine.Name(), s.cfg.Now()); err == nil {
			s.hub.Changed(t.MeetingID)
		}
		s.startEgress(t.Track)
	}
}

// stopTranscription is a meeting's end: its start loops stop, its ingest
// streams close, and its current egresses are stopped (DeleteRoom ends them
// too; this is not left to it).
func (s *Server) stopTranscription(ctx context.Context, meetingID string) {
	ts, err := s.store.LiveTracksOf(ctx, meetingID)
	if err != nil {
		return
	}
	// The meeting's event streams close at its end, and its amend log goes
	// with them (an amend by a last final after this still reaches the
	// store, which the archive and the export read).
	s.tr.mu.Lock()
	delete(s.tr.amends, meetingID)
	s.tr.mu.Unlock()
	for _, t := range ts {
		s.cancelRun(t.Track)
		s.closeConn(t.Track, ingest.CloseNormal, "the meeting ended")
		s.forgetTrack(meetingID, t.Track)
		if t.EgressID != "" {
			if err := s.cfg.LiveKit.StopEgress(ctx, t.EgressID); err != nil {
				s.cfg.Log.Printf("transcript: stopping an egress: %s", lkKind(err))
			}
		}
	}
}

// streamingCurrent says track has an ingest stream connected with its
// CURRENT egress's nonce. 🔴 A stream of an older attempt (its Egress came
// late, after a newer attempt drew a new nonce) is not current: it is being
// closed (the watcher), and must not stop the retries.
func (s *Server) streamingCurrent(ctx context.Context, track string) bool {
	s.tr.mu.Lock()
	lc := s.tr.conns[track]
	s.tr.mu.Unlock()
	if lc == nil {
		return false
	}
	t, err := s.store.TrackByID(ctx, track, s.cfg.Now())
	return err == nil && t.Nonce == lc.nonce
}

func (s *Server) closeConn(track string, code int, why string) {
	s.tr.mu.Lock()
	c := s.tr.conns[track]
	delete(s.tr.conns, track)
	s.tr.mu.Unlock()
	if c != nil {
		c.conn.Close(code, why)
	}
}

// streamEnded is the end of lc's read loop, BEFORE the engine is drained (up
// to 2 × stt.CallTimeout): lc is deregistered, and when it was the track's
// stream, the track is still transcribed, no start loop is running and no
// egress id is known (its start timed out at LiveKit yet the Egress ran, so
// no egress_ended will name it), its end is the egress's death: a gap from
// now, and a restart on the backoff.
func (s *Server) streamEnded(t store.Track, lc *liveConn) {
	s.tr.mu.Lock()
	current := s.tr.conns[t.Track] == lc
	if current {
		delete(s.tr.conns, t.Track)
	}
	running := s.tr.runs[t.Track] != nil
	s.tr.mu.Unlock()
	if !current || running {
		return
	}
	cur, err := s.store.TrackByID(context.Background(), t.Track, s.cfg.Now())
	if err != nil || cur.State != store.TrackActive || !cur.MeetingLive || cur.EgressID != "" {
		return
	}
	if err := s.store.OpenGap(context.Background(), t.Track, s.sinceStart(t.MeetingStart), s.cfg.Engine.Name(), s.cfg.Now()); err == nil {
		s.hub.Changed(t.MeetingID)
	}
	wait := s.restartWait(t.Track)
	s.cfg.Log.Printf("transcript: a stream of an unknown egress ended; starting a new egress in %s", wait)
	s.startEgressAfter(t.Track, wait)
}

// ---- the ingest listener ----

// Ingest is the ingest listener's handler: WS /stt/ingest/<track>/<token>,
// and nothing else. It is never exposed by an ingress: Egress reaches it in
// the cluster, and a NetworkPolicy admits only Egress to its port. Its access
// log records the track, never the token.
func (s *Server) Ingest() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stt/ingest/{track}/{token}", s.handleIngest)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.cfg.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(sw, r)
		if sw.hijacked {
			return // handleIngest logs the stream itself
		}
		s.cfg.Log.Printf("ingest %s %s %d %s", r.Method, ingest.LogPath(r.URL.Path), sw.status, s.cfg.Now().Sub(start).Round(time.Millisecond))
	})
}

// handleIngest is one track egress's stream. 🔴 Before the upgrade: the
// token must open THIS track's current egress (ingest.Parsed.Check: the MAC
// over meeting, track, nonce and expiry, in constant time; the nonce is the
// track's current one; unexpired), the track must still be transcribed, and
// its meeting live (410 once it has ended). Anything else is refused and
// never upgraded.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	track := r.PathValue("track")
	refuse := func(code int, why string) {
		s.cfg.Log.Printf("ingest: refused %d for %s: %s", code, ingest.LogPath(r.URL.Path), why)
		http.Error(w, http.StatusText(code), code)
	}
	if !s.Transcribing() {
		refuse(http.StatusNotFound, "transcription is off")
		return
	}
	p, err := ingest.Parse(r.PathValue("token"))
	if err != nil || !ingest.ValidTrack(track) {
		refuse(http.StatusUnauthorized, "not a token")
		return
	}
	now := s.cfg.Now()
	t, err := s.store.TrackByID(r.Context(), track, now)
	if err != nil {
		refuse(http.StatusUnauthorized, "no such track")
		return
	}
	if err := p.Check(s.cfg.IngestKey, t.MeetingID, track, t.Nonce, now); err != nil {
		refuse(http.StatusUnauthorized, "the token does not open this track")
		return
	}
	if t.State != store.TrackActive {
		refuse(http.StatusUnauthorized, "the track is no longer transcribed")
		return
	}
	if !t.MeetingLive {
		refuse(http.StatusGone, "the meeting has ended")
		return
	}
	if err := ingest.CheckUpgrade(r); err != nil {
		refuse(http.StatusBadRequest, "not a websocket upgrade")
		return
	}
	if sw, ok := w.(*statusWriter); ok {
		sw.hijacked = true
	}
	conn, err := ingest.Upgrade(w, r)
	if err != nil {
		s.cfg.Log.Printf("ingest: upgrade for %s failed: %v", ingest.LogPath(r.URL.Path), err)
		return
	}
	s.cfg.Log.Printf("ingest GET %s 101: streaming", ingest.LogPath(r.URL.Path))
	if testHookBeforeRegister != nil {
		testHookBeforeRegister(track)
	}
	s.serveIngest(conn, t)
}

// ingestAllowed says the stream for t (opened with nonce) may go on.
func (s *Server) ingestAllowed(ctx context.Context, track, nonce string) bool {
	t, err := s.store.TrackByID(ctx, track, s.cfg.Now())
	return err == nil && s.Transcribing() && t.State == store.TrackActive && t.MeetingLive && t.Nonce == nonce
}

// serveIngest reads one egress's stream into the engine until it ends, the
// meeting ends, the track is no longer transcribed, or a newer egress of the
// track replaces it.
func (s *Server) serveIngest(conn *ingest.Conn, t store.Track) {
	lc := &liveConn{conn: conn, nonce: t.Nonce}
	s.tr.mu.Lock()
	if old := s.tr.conns[t.Track]; old != nil {
		old.conn.Close(ingest.CloseNormal, "replaced by a newer stream")
	}
	s.tr.conns[t.Track] = lc
	s.tr.meetings[t.Track] = t.MeetingID
	s.tr.mu.Unlock()
	ctx, cancel := context.WithCancel(s.bg)
	defer func() {
		cancel()
		s.tr.mu.Lock()
		if s.tr.conns[t.Track] == lc {
			delete(s.tr.conns, t.Track)
		}
		if s.tr.delayed[t.Track] == lc {
			delete(s.tr.delayed, t.Track)
		}
		if m := s.tr.partials[t.MeetingID]; m != nil {
			delete(m, t.Track)
			if len(m) == 0 {
				delete(s.tr.partials, t.MeetingID)
			}
		}
		s.tr.mu.Unlock()
		// The track or its meeting is over (not merely this egress
		// replaced): nothing of it is kept in memory.
		cur, err := s.store.TrackByID(context.Background(), t.Track, s.cfg.Now())
		if err != nil || cur.State != store.TrackActive || !cur.MeetingLive {
			s.forgetTrack(t.MeetingID, t.Track)
		}
		s.hub.Changed(t.MeetingID)
		conn.Close(ingest.CloseNormal, "")
	}()
	stream, err := s.cfg.Engine.Open(ctx, stt.TrackRef{MeetingID: t.MeetingID, Track: t.Track, Identity: t.Identity})
	if err != nil {
		s.cfg.Log.Printf("transcript: the engine did not open a stream: %v", err)
		return
	}

	// base is the meeting time of the stream's first sample: fixed once,
	// when it arrives. Every segment is placed at base + its sample time.
	var base atomic.Int64
	base.Store(-1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for seg := range stream.Segments() {
			s.storeSegment(ctx, t, lc, base.Load(), seg)
		}
	}()
	// The watcher closes the stream when it may no longer go on: the
	// meeting ended (any way: End, the 24 h cap, an uninstall, LiveKit's
	// room_finished), the track ended, or a newer egress took its place.
	go func() {
		wake, stop := s.hub.Watch(t.MeetingID, false)
		defer stop()
		tick := time.NewTicker(s.cfg.StreamRecheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-tick.C:
			}
			if !s.ingestAllowed(ctx, t.Track, t.Nonce) {
				conn.Close(ingest.CloseNormal, "the stream is no longer wanted")
				return
			}
		}
	}()

	dec := ingest.NewDecoder()
read:
	for {
		msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if !msg.Binary {
			if s.tr.textOnce.CompareAndSwap(false, true) {
				s.cfg.Log.Printf("ingest: a text frame on an audio stream was dropped (Egress sends none)")
			}
			continue
		}
		if base.Load() < 0 {
			b := s.sinceStart(t.MeetingStart)
			base.Store(b)
			// Audio flows again: the gap closes, the panel's "Transcript
			// paused" clears, and the stream's healthy stretch starts. Only
			// for the CURRENT egress's stream, and as one step against
			// egressLoop's gap (gapMu).
			s.tr.gapMu.Lock()
			if s.streamingCurrent(ctx, t.Track) {
				s.tr.mu.Lock()
				delete(s.tr.paused, t.Track)
				s.tr.audioAt[t.Track] = s.cfg.Now()
				s.tr.mu.Unlock()
				s.store.CloseGap(ctx, t.Track, b)
			}
			s.tr.gapMu.Unlock()
			s.hub.Changed(t.MeetingID)
		}
		chunks, err := dec.Push(msg.Data)
		if err != nil {
			s.cfg.Log.Printf("ingest: %v; closing the stream", err)
			conn.Close(ingest.CloseUnsupported, "misaligned PCM")
			break
		}
		for _, c := range chunks {
			if err := stream.Write(c); err != nil {
				// The engine's stream is over: stop reading this one.
				conn.Close(ingest.CloseGoingAway, "the transcription stream ended")
				break read
			}
		}
	}
	s.streamEnded(t, lc)
	if tail := dec.Flush(); tail != nil {
		_ = stream.Write(tail)
	}
	_ = stream.Close()
	select {
	case <-drained:
	case <-time.After(2 * stt.CallTimeout):
	}
}

// storeSegment stores or forwards one segment of t's stream, placed in
// meeting time at base (the stream's first sample) plus its own sample time.
// 🔴 Never its arrival time: a burst after a stall carries the times its
// samples say.
func (s *Server) storeSegment(ctx context.Context, t store.Track, lc *liveConn, base int64, seg stt.Segment) {
	if base < 0 {
		base = 0
	}
	start, end := base+seg.StartMS, base+seg.EndMS
	engine := seg.Engine
	if engine == "" {
		engine = s.cfg.Engine.Name()
	}
	switch seg.Kind {
	case stt.Partial:
		// 🔴 A partial is a caption: sent, NEVER stored. An empty one
		// clears the track's caption (its connection dropped, or the
		// utterance had no words).
		s.tr.mu.Lock()
		m := s.tr.partials[t.MeetingID]
		if m == nil {
			m = map[string]partial{}
			s.tr.partials[t.MeetingID] = m
		}
		if seg.Text == "" {
			delete(m, t.Track)
		} else {
			m[t.Track] = partial{Speaker: t.Speaker, StartMS: start, Text: seg.Text}
		}
		s.tr.mu.Unlock()
	case stt.EngineSwitch:
		// "Captions delayed" exactly while the track is not on the
		// configured engine (the nemotron engine's fallback).
		// Only the track's current stream sets it, and a stream clears only
		// its own.
		s.tr.mu.Lock()
		if seg.Engine != s.cfg.Engine.Name() {
			if s.tr.conns[t.Track] == lc {
				s.tr.delayed[t.Track] = lc
				s.tr.meetings[t.Track] = t.MeetingID
			}
		} else if s.tr.delayed[t.Track] == lc {
			delete(s.tr.delayed, t.Track)
		}
		s.tr.mu.Unlock()
	case stt.Flag:
		// The silent-start check: telemetry only (the track, never its
		// speaker or any text).
		s.tr.flags.Add(1)
		s.cfg.Log.Printf("transcript: track %s flagged %s (speech energy or a constant level and no words for %s)", t.Track, seg.Text, stt.SilentStart)
		return
	case stt.Final:
		// The previous line's late closing marks FIRST: once this final's
		// own line is stored, it is the participant's latest. The amend
		// goes to the same participant's previous line only (store:
		// AmendLastLine), and every open panel updates that line in place.
		if seg.Lead != "" {
			if id, changed, err := s.store.AmendLastLine(ctx, t.MeetingID, t.Identity, seg.Lead, stt.Marks); err != nil {
				s.cfg.Log.Printf("transcript: a line's closing mark was not stored: %v", err)
			} else if changed {
				s.noteAmend(t.MeetingID, id)
			}
		}
		// A final with only a lead stores no line (any other final is
		// stored exactly as before).
		if seg.Text != "" || seg.Lead == "" {
			if _, err := s.store.AddSegment(ctx, t.Track, store.SegText, start, end, seg.Text, engine); err != nil {
				s.cfg.Log.Printf("transcript: a line was not stored: %v", err)
			}
		}
		s.tr.mu.Lock()
		if m := s.tr.partials[t.MeetingID]; m != nil {
			delete(m, t.Track)
		}
		delete(s.tr.paused, t.Track)
		s.tr.mu.Unlock()
	case stt.Gap:
		if _, err := s.store.AddSegment(ctx, t.Track, store.SegGap, start, end, "", engine); err != nil {
			s.cfg.Log.Printf("transcript: a gap marker was not stored: %v", err)
		}
		s.tr.mu.Lock()
		s.tr.paused[t.Track] = true
		s.tr.meetings[t.Track] = t.MeetingID
		s.tr.mu.Unlock()
	}
	s.hub.Changed(t.MeetingID)
}

// partialsOf is the meeting's partials, oldest first.
func (s *Server) partialsOf(meetingID string) []partial {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	out := []partial{}
	for _, p := range s.tr.partials[meetingID] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartMS != out[j].StartMS {
			return out[i].StartMS < out[j].StartMS
		}
		return out[i].Speaker < out[j].Speaker
	})
	return out
}

// transcriptStatus is the panel's status line: "Transcript paused" while a
// track of the meeting has an open gap (open: no egress streaming) or its
// engine dropped audio since its last line; else "Captions delayed" while a
// track of the meeting is on the nemotron engine's fallback; "" otherwise.
func (s *Server) transcriptStatus(meetingID string, open []int64) string {
	if len(open) > 0 {
		return TranscriptPaused
	}
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	for track, p := range s.tr.paused {
		if p && s.tr.meetings[track] == meetingID {
			return TranscriptPaused
		}
	}
	for track, d := range s.tr.delayed {
		if d != nil && d == s.tr.conns[track] && s.tr.meetings[track] == meetingID {
			return CaptionsDelayed
		}
	}
	return ""
}

// SilentStartFlags is how many silent-start flags the engines raised since
// the plugin started.
func (s *Server) SilentStartFlags() int64 { return s.tr.flags.Load() }
