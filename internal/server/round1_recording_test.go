package server_test

// Round 1 of the recording's audit (PR #14): each test names its finding,
// and was watched red on bbb5c7d before its fix.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

func activeComposite(e *env, id string) bool {
	for _, x := range e.lk.Composites() {
		if x.ID == id {
			return x.Active()
		}
	}
	return false
}

func recordOff(t *testing.T, e *env, c call) {
	t.Helper()
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("record off = %d %s", resp.StatusCode, raw)
	}
}

// 🔴1 (consent): Record off whose one StopEgress fails must not leave the
// egress recording while every badge says it is not: the next reconcile
// pass stops it.
func TestRecordOffWithAFailedStopIsStoppedByTheReconcile(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	e.lk.FailStop(1)
	recordOff(t, e, c)
	if !activeComposite(e, g.ID) {
		t.Fatal("the fake's StopEgress did not fail: this test proves nothing")
	}
	e.server.Reconcile(context.Background())
	if activeComposite(e, g.ID) {
		t.Fatal("Record off with one failed stop: the egress is still recording after the reconcile")
	}
}

// 🔴1 (consent): Record off while the egress's start is still in flight
// (LiveKit has accepted it, its answer has not reached the plugin): once the
// answer lands, the egress is stopped.
func TestRecordOffDuringAnInFlightStartStopsTheEgress(t *testing.T) {
	e, _ := recEnv(t)
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	e.lk.CompositeHook = func() { <-hold }
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1) // accepted by LiveKit; its answer held
	recordOff(t, e, c)
	release()
	eventually(t, 5*time.Second, "the in-flight egress stopped after Record off", func() bool { return !activeComposite(e, g.ID) })
}

// 🔴2: Delete now while the last part's egress still runs (ENDING, its file
// uploading): its row stays until the egress ends, and then the uploaded
// object is deleted, and nothing remains.
func TestDeleteNowWhileThePartStillUploadsDeletesItsObjectWhenItEnds(t *testing.T) {
	e, f := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	e.clock.Add(20 * time.Second)
	// The meeting ends without End meeting (LiveKit's room_finished): the
	// room is not deleted, and the egress is still running.
	if _, _, err := e.store.EndRoom(context.Background(), c.Room, "test", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if !activeComposite(e, g.ID) {
		t.Fatal("the egress ended with the room: this test proves nothing")
	}
	resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/recording", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"pending":1`) {
		t.Fatalf("delete with the part still recording = %d %s, want pending 1", resp.StatusCode, raw)
	}
	key := "meetings/" + c.Subject + "/" + c.ID + "/recording-1.mp4"
	e.lk.KillEgress(g.ID) // it ends, and its file lands in the store
	eventually(t, 5*time.Second, "the late upload deleted", func() bool { return !f.Has(key) })
	if parts, _, _ := e.store.ForSubject(c.Subject).Recordings(context.Background(), c.ID); len(parts) != 0 {
		t.Fatalf("rows left after the late upload was deleted: %v", parts)
	}
}

// 🟡1: a guest admitted before the owner first joins fixes the meeting's
// time origin there, for the recording and the transcript alike. Literal:
// created at t0, admitted at t0+60 s (meeting time 0), the file began at
// t0+62 s (part 1 at 2000), the owner joined at t0+70 s (no new origin),
// the meeting ended at t0+120 s (a 58 s file: 2000 to 60000).
func TestAnAdmitBeforeTheOwnerJoinsFixesTheTimeOrigin(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.clock.Add(60 * time.Second)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, t0.Add(62*time.Second))
	e.clock.Add(10 * time.Second)
	e.started(c)
	ctx := context.Background()
	if _, err := e.store.UpsertTrack(ctx, c.Room, "TR_origin1", store.OwnerIdentity, "Ada L.", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{10_000, 30_000} {
		e.store.AddSegment(ctx, "TR_origin1", store.SegText, at, at+1000, "line", "fake")
	}
	e.clock.Add(50 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	eventually(t, 5*time.Second, "the part complete", func() bool {
		p, _ := e.recording(c)
		return len(p) == 1 && p[0]["state"] == "complete"
	})
	_, raw := e.do("GET", "/api/meetings/"+c.ID+"/timeline", c.Sess, nil)
	var b struct {
		Parts []struct {
			StartMS int64 `json:"start_ms"`
		} `json:"parts"`
		Segments []struct {
			Recording *struct {
				Part     int   `json:"part"`
				OffsetMS int64 `json:"offset_ms"`
			} `json:"recording"`
		} `json:"segments"`
	}
	json.Unmarshal(raw, &b)
	if len(b.Parts) != 1 || b.Parts[0].StartMS != 2_000 || len(b.Segments) != 2 {
		t.Fatalf("parts and segments: %s", raw)
	}
	for i, want := range []int64{8_000, 28_000} {
		if r := b.Segments[i].Recording; r == nil || r.Part != 1 || r.OffsetMS != want {
			t.Errorf("segment %d: %+v, want part 1 offset %d (one time origin)", i, r, want)
		}
	}
}

// 🟡2: frame sampling runs ONE ffmpeg at a time, with one thread. A stand-in
// ffmpeg records each run's start and end and its arguments.
func TestFramesRunOneFFmpegAtATimeWithOneThread(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ffmpeg")
	body := "#!/usr/bin/env bash\necho \"$*\" >> " + dir + "/args\necho start >> " + dir + "/events\nsleep 0.3\necho end >> " + dir + "/events\nprintf '\\xff\\xd8\\xff\\xd9'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	e, _ := recEnv(t, func(c *server.Config) { c.Frames = frames.Sampler{FFmpeg: script} })
	c := finishedRecording(t, e)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, _ := e.do("GET", "/api/meetings/"+c.ID+"/frame?at=1000", c.Sess, nil); resp.StatusCode != 200 {
				t.Errorf("frame = %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	ev, _ := os.ReadFile(filepath.Join(dir, "events"))
	if got := strings.Fields(string(ev)); strings.Join(got, " ") != "start end start end" {
		t.Errorf("ffmpeg runs = %v: two ran at once", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, line := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		if !strings.Contains(line, "-threads 1 -filter_threads 1") {
			t.Errorf("an ffmpeg run without one thread: %s", line)
		}
	}
}

// 🟡3: a duplicate egress_ended (the part already ended) restarts nothing.
func TestADuplicateEgressEndedRestartsNothing(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g1 := e.composite(1)
	e.lk.BeginComposite(g1.ID, e.clock.Now())
	e.clock.Add(90 * time.Second)
	e.lk.KillEgress(g1.ID)
	g2 := e.composite(2)
	e.lk.BeginComposite(g2.ID, e.clock.Now())
	if code, err := e.lk.PostEvent(e.lk.WebhookURL, map[string]any{"event": "egress_ended", "room": map[string]any{"sid": "RM_fake", "name": c.Room},
		"egressInfo": map[string]any{"egressId": g1.ID, "roomName": c.Room, "status": "EGRESS_FAILED"}}); err != nil || code != 200 {
		t.Fatalf("duplicate webhook = %d %v", code, err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(e.log.String(), "its egress ended while the meeting is recorded"); n != 1 {
		t.Fatalf("%d restarts for one egress's end (a duplicate webhook restarted it)", n)
	}
	if n := len(e.lk.Composites()); n != 2 {
		t.Fatalf("%d composites", n)
	}
}

var _ = livekittest.Keys
