package server_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
)

// The round-2 audit's interleavings, each ordered by hooks: the fake
// LiveKit's StartHook (a start held, then refused), the ingest's hook
// between the token check and the stream's registration, and egressLoop's
// hook inside its gap step.

// ingestURL is the URL an Egress of the attempt with nonce would be given.
func (e *env) ingestURL(c call, track, nonce string) string {
	e.t.Helper()
	var created int64
	if err := e.dbQuery(`SELECT created_at FROM meetings WHERE id = ?`, c.ID).Scan(&created); err != nil {
		e.t.Fatal(err)
	}
	tok := ingest.Token(ingestKey, c.ID, track, nonce, time.UnixMilli(created).Add(store.LinkLifetime))
	return "ws" + strings.TrimPrefix(e.ingest.URL, "http") + ingest.Path(track, tok)
}

func (e *env) nonceOf(track string) string {
	e.t.Helper()
	var n string
	e.dbQuery(`SELECT nonce FROM tracks WHERE track = ?`, track).Scan(&n)
	return n
}

// openGaps counts the meeting's gap markers still open.
func (e *env) openGaps(c call) int {
	e.t.Helper()
	ids, err := e.store.OpenGaps(context.Background(), c.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(ids)
}

// signal is a one-shot "it happened".
type signal struct {
	once sync.Once
	ch   chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }
func (s *signal) fire()  { s.once.Do(func() { close(s.ch) }) }

// gate is a channel a hook blocks on until the test opens it; it is opened
// when the test ends too, so a failed test never leaves a hook (and the
// server it holds) blocked.
func gate(t *testing.T) (ch chan struct{}, open func()) {
	ch = make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return ch, open
}
func (s *signal) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("not within 5s: %s", what)
	}
}

// 🔴 (audit 1) A STALE stream does not stop the retries. Attempt 1's Egress
// comes late: its token passes the check on attempt 1's nonce, attempt 2
// draws a new nonce, and only then does the stream register. Attempt 2 is
// refused: the loop must go on (the stale stream is not the current
// egress's), and attempt 3 is accepted.
func TestAStaleStreamDoesNotStopTheRetries(t *testing.T) {
	afterGate, waiting := make(chan time.Time), newSignal()
	first := true
	var mu sync.Mutex
	e := newEnvOpts(t, nil, func(c *server.Config) {
		// No periodic recheck: the watcher would close the stale stream
		// (not the current egress's) before the loop looks at it.
		c.StreamRecheck = time.Hour
		c.After = func(d time.Duration) <-chan time.Time {
			mu.Lock()
			defer mu.Unlock()
			if first { // attempt 1's wait is held
				first = false
				waiting.fire()
				return afterGate
			}
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		}
	})
	var afterOnce sync.Once
	t.Cleanup(func() { afterOnce.Do(func() { close(afterGate) }) })
	c := e.meeting(ada)
	e.started(c)
	start2, openStart2 := gate(t)
	held2 := newSignal()
	e.lk.StartHook = func(n int) bool {
		switch n {
		case 1:
			return true
		case 2:
			held2.fire()
			<-start2
			return true
		}
		return false
	}
	regGate, openReg := gate(t)
	atRegister := newSignal()
	var track string
	server.SetIngestHooksForTest(t, func(tr string) {
		if tr == track {
			atRegister.fire()
			<-regGate
		}
	}, nil)
	track = e.publish(c, store.OwnerIdentity, "MICROPHONE")
	waiting.wait(t, "attempt 1 refused and waiting")
	n1 := e.nonceOf(track)
	// Attempt 1's late Egress. Its client is kept (an unreferenced
	// connection's finalizer would close it) and dropped when the test ends.
	late := make(chan *egresstest.Client, 1)
	go func() {
		cl, err := egresstest.Dial(e.ingestURL(c, track, n1))
		if err == nil {
			late <- cl
		}
	}()
	t.Cleanup(func() {
		select {
		case cl := <-late:
			cl.Drop()
		default:
		}
	})
	atRegister.wait(t, "the late Egress passed the token check")
	afterOnce.Do(func() { close(afterGate) }) // attempt 2: a new nonce, its start held
	held2.wait(t, "attempt 2 in flight")
	if n2 := e.nonceOf(track); n2 == n1 {
		t.Fatal("attempt 2 drew no new nonce")
	}
	openReg() // the stale stream registers now
	eventually(t, 5*time.Second, "the stale stream registers", func() bool { return e.server.ConnNonceForTest(track) == n1 })
	openStart2() // attempt 2 refused
	e.egressOf(track, 1)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 3 {
		t.Fatalf("%d starts, want 3: a stale stream stopped the retries", n)
	}
}

// startHeld connects a CURRENT stream while attempt 1's start is in flight,
// and sends its first audio before that start is refused.
func startHeld(t *testing.T, e *env, c call) (track string, cl *egresstest.Client, release func()) {
	t.Helper()
	start1, open1 := gate(t)
	held := newSignal()
	e.lk.StartHook = func(n int) bool {
		if n == 1 {
			held.fire()
			<-start1
		}
		return true // LiveKit refuses every start: no Egress attached
	}
	track = e.publish(c, store.OwnerIdentity, "MICROPHONE")
	held.wait(t, "attempt 1 in flight")
	n1 := e.nonceOf(track)
	code, cl := dialStatus(t, e.ingestURL(c, track, n1))
	if code != 101 {
		t.Fatalf("the current attempt's Egress: %d, want 101", code)
	}
	eventually(t, 5*time.Second, "the stream registers", func() bool { return e.server.ConnNonceForTest(track) == n1 })
	if err := cl.Stream(egresstest.Silence(100), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // its first audio is in
	return track, cl, open1
}

// 🔴 (audit 4) A current stream connects while a start is in flight that
// then fails: no gap is left open, no further start is made, and the stream
// lives (phase 1, the check after a failed attempt). A track_published
// delivered again then starts nothing either (phase 2, the check before an
// attempt).
func TestAStreamThatConnectsDuringAFailingStartEndsTheLoop(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	track, cl, release := startHeld(t, e, c)
	release()
	time.Sleep(300 * time.Millisecond)
	if n, g := e.lk.CountCalls("StartTrackEgress"), e.openGaps(c); n != 1 || g != 0 {
		t.Fatalf("phase 1: %d starts and %d open gaps after the stream connected during a failing start, want 1 and 0", n, g)
	}
	if err := cl.Stream(egresstest.Silence(100), 0); err != nil {
		t.Fatalf("phase 1: the stream was cut off: %v", err)
	}
	// The same track_published again (LiveKit retries a webhook).
	if code, err := e.lk.PostEvent(e.lk.WebhookURL, map[string]any{"event": "track_published", "room": map[string]any{"name": c.Room},
		"participant": map[string]any{"identity": store.OwnerIdentity}, "track": map[string]any{"sid": track, "source": "MICROPHONE"}}); err != nil || code != 200 {
		t.Fatalf("track_published again = %d, %v", code, err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 1 {
		t.Fatalf("phase 2: %d starts after track_published again with a current stream, want 1", n)
	}
	if err := cl.Stream(egresstest.Silence(100), 0); err != nil {
		t.Fatalf("phase 2: the stream was cut off: %v", err)
	}
}

// 🔴 (audit 2) The loop's "no current stream, so open a gap" and a current
// stream's first audio are one step each: here the stream connects and
// sends its first audio exactly between the loop's check and its gap, and
// no gap may stay open.
func TestAFirstAudioBetweenTheLoopsCheckAndItsGapLeavesNoGapOpen(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.lk.StartHook = func(int) bool { return true }
	var once sync.Once
	connected := newSignal()
	// The env has this one track: the hook needs no filter (and may run
	// before publish returns its id).
	server.SetIngestHooksForTest(t, nil, func(tr string) {
		once.Do(func() {
			code, cl := dialStatus(t, e.ingestURL(c, tr, e.nonceOf(tr)))
			if code != 101 {
				t.Errorf("the Egress between the check and the gap: %d", code)
				return
			}
			cl.Stream(egresstest.Silence(100), 0)
			time.Sleep(200 * time.Millisecond) // its first audio arrives now
			connected.fire()
		})
	})
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	connected.wait(t, "the stream connected inside the gap step")
	time.Sleep(300 * time.Millisecond)
	if g := e.openGaps(c); g != 0 {
		t.Fatalf("%d gaps stay open while audio flows, want 0", g)
	}
	if n := e.lk.CountCalls("StartTrackEgress"); n != 1 {
		t.Fatalf("%d starts with a current stream, want 1", n)
	}
}

// slowEngine's streams hold their drain until release closes: an engine
// with transcriptions still in flight when a stream ends.
type slowEngine struct{ release chan struct{} }

func (e *slowEngine) Name() string { return "slow" }
func (e *slowEngine) Open(ctx context.Context, tr stt.TrackRef) (stt.Stream, error) {
	return &slowStream{segs: make(chan stt.Segment), release: e.release}, nil
}

type slowStream struct {
	segs    chan stt.Segment
	release chan struct{}
	once    sync.Once
}

func (s *slowStream) Write([]int16) error          { return nil }
func (s *slowStream) Segments() <-chan stt.Segment { return s.segs }
func (s *slowStream) Close() error {
	s.once.Do(func() { go func() { <-s.release; close(s.segs) }() })
	return nil
}

// 🔴 (audit 3) A stream of an unknown egress (its start timed out at
// LiveKit, yet the Egress ran) that ends is the egress's death AT ONCE: the
// gap starts where the stream ended and the restart is kicked, while the
// engine is still draining; never when the drain ends.
func TestAnUnknownEgresssDeathIsAGapFromTheStreamsEndNotTheDrains(t *testing.T) {
	slow := &slowEngine{release: make(chan struct{})}
	e := newEnvOpts(t, nil, func(c *server.Config) { c.Engine = slow })
	defer close(slow.release)
	c := e.meeting(ada)
	e.started(c)
	_, cl, release := startHeld(t, e, c)
	release()
	time.Sleep(200 * time.Millisecond)
	e.clock.Add(5 * time.Second)
	cl.Drop()
	eventually(t, 2*time.Second, "a gap opens while the engine is still draining", func() bool { return e.openGaps(c) == 1 })
	var start int64
	if err := e.dbQuery(`SELECT start_ms FROM segments WHERE kind = 'gap' AND end_ms IS NULL`).Scan(&start); err != nil || start != 5000 {
		t.Fatalf("the gap starts at %d ms (%v), want 5000 (where the stream ended)", start, err)
	}
	eventually(t, 2*time.Second, "the restart is kicked before the drain ends", func() bool { return e.lk.CountCalls("StartTrackEgress") >= 2 })
}

// 🔴 (audit 5) A gap is opened only on an active track of a live meeting
// (in the store, in one transaction), and an ended track keeps no restart
// count.
func TestNoGapOrRestartCountOnAnEndedTrack(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	e.egressOf(a, 1)
	ctx := context.Background()
	if err := e.store.EndTrack(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.store.OpenGap(ctx, a, 1000, "fake", e.clock.Now()); !errors.Is(err, store.ErrGone) || e.openGaps(c) != 0 {
		t.Fatalf("a gap on an ended track: %v, %d open; want ErrGone and none", err, e.openGaps(c))
	}
	e.server.RestartWaitForTest(a)
	if e.server.RestartCountedForTest(a) {
		t.Fatal("an ended track keeps a restart count")
	}
	// The meeting's end, the same.
	c2 := e.meeting(bea)
	e.started(c2)
	b := e.publish(c2, store.OwnerIdentity, "MICROPHONE")
	e.egressOf(b, 1)
	if _, err := e.store.ForSubject(c2.Subject).Meeting(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c2.ID+"/end", c2.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	if err := e.store.OpenGap(ctx, b, 1000, "fake", e.clock.Now()); !errors.Is(err, store.ErrGone) || e.openGaps(c2) != 0 {
		t.Fatalf("a gap in an ended meeting: %v, %d open; want ErrGone and none", err, e.openGaps(c2))
	}
}
