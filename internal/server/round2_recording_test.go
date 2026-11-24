package server_test

// Round 2 of the recording's audit (PR #14): each test names its finding,
// and was watched red on fe8839d before its fix.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/s3test"
	"github.com/ZacxDev/muster-meetings/internal/server"
)

// ghostMeeting is a recorded meeting whose part 1's start timed out at the
// plugin (its LiveKit client gives up after 300 ms; LiveKit answers after
// 700 ms) while LiveKit ran it anyway: the GHOST egress, writing under part
// 1's key, which the plugin has marked failed with no egress id; and part 2,
// the retry, recording.
func ghostMeeting(t *testing.T) (e *env, f *s3test.Fake, c call, ghost, g2 livekittest.Egress) {
	t.Helper()
	e, f = recEnv(t, func(cfg *server.Config) {
		cfg.LiveKit.HTTP = &http.Client{Timeout: 300 * time.Millisecond}
		cfg.EgressBackoff = []time.Duration{50 * time.Millisecond}
	})
	var n atomic.Int32
	e.lk.CompositeHook = func() {
		if n.Add(1) == 1 {
			time.Sleep(700 * time.Millisecond)
		}
	}
	c = e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	ghost = e.composite(1)
	g2 = e.composite(2)
	if ghost.Filepath != "meetings/"+c.Subject+"/"+c.ID+"/recording-1.mp4" {
		t.Fatalf("the ghost writes %q", ghost.Filepath)
	}
	e.lk.BeginComposite(g2.ID, e.clock.Now())
	eventually(t, 5*time.Second, "part 1 failed, part 2 recording", func() bool {
		p, _ := e.recording(c)
		return len(p) == 2 && p[0]["state"] == "failed" && p[1]["state"] == "active"
	})
	return e, f, c, ghost, g2
}

// 🔴 R2-1: a ghost egress beside the recording is stopped, and its stop is
// retried after one fails; Record off then leaves nothing recording.
func TestAGhostEgressIsStoppedEvenAfterAFailedStop(t *testing.T) {
	e, _, c, ghost, g2 := ghostMeeting(t)
	e.lk.FailStop(1)
	e.lk.BeginComposite(ghost.ID, e.clock.Now()) // the ghost's file begins
	time.Sleep(100 * time.Millisecond)
	if !activeComposite(e, ghost.ID) {
		t.Fatal("the ghost stopped on its first (failing) stop: this test proves nothing")
	}
	e.server.Reconcile(context.Background())
	if activeComposite(e, ghost.ID) {
		t.Fatal("a ghost egress still recording after the reconcile")
	}
	if !activeComposite(e, g2.ID) {
		t.Fatal("the meeting's own recording was stopped")
	}
	recordOff(t, e, c)
	e.server.Reconcile(context.Background())
	for _, x := range e.lk.Composites() {
		if x.Active() {
			t.Fatalf("Record off: %s still recording", x.ID)
		}
	}
}

// 🔴 R2-1: Delete now while a ghost (its stop accepted, still uploading) and
// the last part are still running: neither is purged before its end, and
// each end deletes its object; nothing remains.
func TestDeleteNowWithAGhostStillUploadingDeletesItsObjectWhenItEnds(t *testing.T) {
	e, f, c, ghost, g2 := ghostMeeting(t)
	e.lk.StopEnding = true
	e.lk.BeginComposite(ghost.ID, e.clock.Now()) // adopted by its key, stopped: ENDING
	eventually(t, 5*time.Second, "the ghost ENDING", func() bool {
		for _, x := range e.lk.Composites() {
			if x.ID == ghost.ID {
				return x.Ending
			}
		}
		return false
	})
	if _, _, err := e.store.EndRoom(context.Background(), c.Room, "test", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/recording", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"deleted":0`) || !strings.Contains(string(raw), `"pending":2`) {
		t.Fatalf("Delete now with a ghost uploading = %d %s, want deleted 0, pending 2", resp.StatusCode, raw)
	}
	e.lk.FinishEgress(ghost.ID) // its upload lands
	e.lk.FinishEgress(g2.ID)
	prefix := "meetings/" + c.Subject + "/" + c.ID + "/"
	eventually(t, 5*time.Second, "every object of the deleted recording gone", func() bool {
		for _, k := range f.Keys() {
			if strings.HasPrefix(k, prefix) {
				return false
			}
		}
		return true
	})
	eventually(t, 5*time.Second, "no row left", func() bool {
		p, _, _ := e.store.ForSubject(c.Subject).Recordings(context.Background(), c.ID)
		return len(p) == 0
	})
}

// 🔴 R2-1: a part with no egress id (the plugin stopped mid-start) adopts
// the egress LiveKit runs under its key, found by listing the room; a
// composite in the room that no part owns is stopped.
func TestTheReconcileAdoptsAnEgressItHasNoIDForAndStopsAnUnownedOne(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	ctx := context.Background()
	p, err := e.store.BeginRecording(ctx, c.ID, 0, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	lkc, _ := livekit.NewClient(e.lk.URL, livekittest.Keys)
	lkc.Now = e.clock.Now
	mine, err := lkc.StartRoomCompositeEgress(ctx, c.Room, p.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	stray, err := lkc.StartRoomCompositeEgress(ctx, c.Room, "meetings/"+c.Subject+"/"+c.ID+"/recording-9.mp4")
	if err != nil {
		t.Fatal(err)
	}
	e.server.Reconcile(ctx)
	ref, err := e.store.RecordingByKey(ctx, p.ObjectKey, e.clock.Now())
	if err != nil || ref.EgressID != mine.EgressID {
		t.Fatalf("part 1's egress = %q (%v), want the adopted %s", ref.EgressID, err, mine.EgressID)
	}
	if !activeComposite(e, mine.EgressID) {
		t.Fatal("the adopted egress (the meeting is recorded) was stopped")
	}
	if activeComposite(e, stray.EgressID) {
		t.Fatal("an unowned composite in the room is still recording")
	}
}

// 🟡 R2-2: a part stuck starting with no egress (the plugin stopped between
// making it and starting its egress) is resolved by the reconcile once
// LiveKit knows no egress for it, so Delete now finishes. Green on bbb5c7d,
// red on fe8839d.
func TestAStartingPartWithNoEgressIsResolvedAndBecomesDue(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	ctx := context.Background()
	if _, err := e.store.BeginRecording(ctx, c.ID, 0, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	if resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/recording", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("delete = %d %s", resp.StatusCode, raw)
	}
	e.clock.Add(server.LostEgress)
	e.server.Reconcile(ctx)
	if parts, _, _ := e.store.ForSubject(c.Subject).Recordings(ctx, c.ID); len(parts) != 0 {
		t.Fatalf("a starting part with no egress is never deleted: %v", parts)
	}
	if _, raw := e.do("GET", "/api/meetings/"+c.ID+"/recording", c.Sess, nil); strings.Contains(string(raw), `"deleting":true`) {
		t.Fatalf("the archive says deleting forever: %s", raw)
	}
}

// 🟢 R2-3: an egress whose stop LiveKit accepted (ENDING: it uploads) is not
// stopped again by its webhooks or the reconcile.
func TestAnEgressThatIsEndingIsNotStoppedAgain(t *testing.T) {
	e, _ := recEnv(t)
	e.lk.StopEnding = true
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	recordOff(t, e, c)
	time.Sleep(200 * time.Millisecond) // its egress_updated ENDING arrives
	for range 3 {
		e.server.Reconcile(context.Background())
	}
	if n := e.lk.CountCalls("StopEgress  " + g.ID); n != 1 {
		t.Fatalf("%d StopEgress calls for an egress already ENDING, want 1", n)
	}
}
