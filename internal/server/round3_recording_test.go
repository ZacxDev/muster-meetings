package server_test

// Round 3 of the recording's audit (PR #14, the last: not re-audited).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// waitRecorder is a Config.After that records each wait and never fires.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *waitRecorder) after(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	return make(chan time.Time)
}

func (w *waitRecorder) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

// unownedPart is a live, recorded meeting with a part starting and no egress
// id, and (when key is true) a composite LiveKit runs under its key.
func unownedPart(t *testing.T, e *env, withEgress bool) (call, store.Recording, string) {
	t.Helper()
	c := e.meeting(ada)
	e.started(c)
	ctx := context.Background()
	p, err := e.store.BeginRecording(ctx, c.ID, 0, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	if withEgress {
		lkc, _ := livekit.NewClient(e.lk.URL, livekittest.Keys)
		lkc.Now = e.clock.Now
		g, err := lkc.StartRoomCompositeEgress(ctx, c.Room, p.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		id = g.EgressID
	}
	return c, p, id
}

// 🟡1: an egress whose part the database cannot read is left alone: it is
// not stopped as unowned. Once the database answers, it is adopted.
func TestAnEgressIsNotStoppedWhenItsPartCannotBeRead(t *testing.T) {
	e, _ := recEnv(t)
	_, p, g := unownedPart(t, e, true)
	server.SetRecordingLookupFaultForTest(t, errors.New("database is locked"))
	e.server.Reconcile(context.Background())
	if !activeComposite(e, g) {
		t.Fatal("a database error stopped the meeting's own recording as unowned")
	}
	server.SetRecordingLookupFaultForTest(t, nil)
	e.server.Reconcile(context.Background())
	if ref, err := e.store.RecordingByKey(context.Background(), p.ObjectKey, e.clock.Now()); err != nil || ref.EgressID != g || !activeComposite(e, g) {
		t.Fatalf("after the database answers: egress %q (%v), active %v", ref.EgressID, err, activeComposite(e, g))
	}
}

// 🟡2: "lost" counts from the first pass that could not list the egress,
// and is cleared when a pass lists it again: one empty answer an hour into
// a part does not end it; two minutes of empty answers do.
func TestAnEgressIsLostTwoMinutesAfterItWasFirstUnseen(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	state := func() any { p, _ := e.recording(c); return p[0]["state"] }
	e.clock.Add(time.Hour)
	ctx := context.Background()
	e.lk.HideList(1)
	e.server.Reconcile(ctx)
	if s := state(); s != "active" {
		t.Fatalf("one empty answer an hour in: the part is %v", s)
	}
	e.clock.Add(3 * time.Minute)
	e.server.Reconcile(ctx) // listed again: the window is cleared
	e.lk.HideList(1)
	e.server.Reconcile(ctx) // unseen again, from now
	if s := state(); s != "active" {
		t.Fatalf("unseen again after being listed: the part is %v (the old window was kept)", s)
	}
	e.clock.Add(server.LostEgress)
	e.lk.HideList(1)
	e.server.Reconcile(ctx)
	if s := state(); s != "failed" {
		t.Fatalf("unseen for %s: the part is %v, want failed (lost)", server.LostEgress, s)
	}
}

// 🟢1: a part resolved as gone restarts the meeting's recording on the
// restart backoff, not at once.
func TestAGonePartResumesOnTheBackoff(t *testing.T) {
	w := &waitRecorder{}
	e, _ := recEnv(t, func(c *server.Config) {
		c.After = w.after
		c.EgressBackoff = []time.Duration{7 * time.Second, 9 * time.Second}
	})
	unownedPart(t, e, false)
	ctx := context.Background()
	e.server.Reconcile(ctx)
	e.clock.Add(server.LostEgress)
	e.server.Reconcile(ctx)
	eventually(t, 5*time.Second, "the resume's wait", func() bool { return len(w.got()) > 0 })
	if got := w.got(); len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("waits = %v, want one of 7s (the backoff's first step)", got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.lk.Composites()); n != 0 {
		t.Fatalf("%d composites started at once, not after the backoff", n)
	}
}

// 🟢1: a part resolved as gone while the meeting's start loop is waiting to
// retry leaves that loop alone (no second loop, no reset wait).
func TestAGonePartKeepsTheRunningLoop(t *testing.T) {
	w := &waitRecorder{}
	e, _ := recEnv(t, func(c *server.Config) {
		c.After = w.after
		c.EgressBackoff = []time.Duration{7 * time.Second}
	})
	e.lk.RefuseComposite(100)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace) // part 1 refused: the loop waits 7 s
	eventually(t, 5*time.Second, "the loop's wait", func() bool { return len(w.got()) == 1 })
	ctx := context.Background()
	e.server.Reconcile(ctx)
	e.clock.Add(server.LostEgress)
	e.server.Reconcile(ctx) // part 1 (no egress, unseen) is gone
	time.Sleep(100 * time.Millisecond)
	if got := w.got(); len(got) != 1 {
		t.Fatalf("waits = %v: the resolution replaced the running loop", got)
	}
	if p, _ := e.recording(c); len(p) != 1 {
		t.Fatalf("parts = %v", p)
	}
}

// 🟢2: a composite LiveKit lists in the room with no file named yet (still
// STARTING) keeps a part with no egress id from being resolved as gone.
func TestAPartIsNotGoneWhileTheRoomListsAnUnnamedComposite(t *testing.T) {
	e, _ := recEnv(t)
	e.lk.FilelessUntilBegin = true
	c, _, _ := unownedPart(t, e, true)
	ctx := context.Background()
	for range 3 {
		e.server.Reconcile(ctx)
		e.clock.Add(server.LostEgress)
	}
	e.server.Reconcile(ctx)
	if p, _ := e.recording(c); len(p) != 1 || p[0]["state"] != "starting" {
		t.Fatalf("a part resolved as gone while its egress may still be starting: %v", p)
	}
}

// 🟢3: one part LiveKit will not list for does not stop the pass: the next
// part's lost webhook is still made up for.
func TestOneFailedListDoesNotEndThePass(t *testing.T) {
	e, _ := recEnv(t)
	var gs []livekittest.Egress
	var cs []call
	for i, who := range []string{grace, linus} {
		c := e.meeting(ada)
		e.started(c)
		e.admitGuest(c, who)
		g := e.composite(i + 1)
		e.lk.BeginComposite(g.ID, e.clock.Now())
		gs, cs = append(gs, g), append(cs, c)
	}
	e.lk.WebhookURL = "" // the ends' webhooks are lost
	for _, c := range cs {
		e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	}
	e.lk.FailList(gs[0].ID)
	e.server.Reconcile(context.Background())
	if p, _ := e.recording(cs[1]); p[0]["state"] != "complete" {
		t.Fatalf("the second meeting's part is %v: one failed list ended the pass", p[0]["state"])
	}
}
