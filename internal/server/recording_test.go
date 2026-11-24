package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/s3"
	"github.com/ZacxDev/muster-meetings/internal/s3test"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// fixtureMP4 is 20 s of 64x64 video (internal/frames/testdata): what the fake
// Egress "uploads" for every part.
var fixtureMP4 = func() []byte {
	b, err := os.ReadFile("../frames/testdata/recording-h264.mp4")
	if err != nil {
		panic(err)
	}
	return b
}()

// recEnv is an env whose plugin records, to the fake store.
func recEnv(t *testing.T, opt ...func(*server.Config)) (*env, *s3test.Fake) {
	t.Helper()
	f := s3test.New()
	t.Cleanup(f.Close)
	cl, err := s3.New(f.Config())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvOpts(t, nil, func(c *server.Config) {
		c.S3 = cl
		c.Frames = frames.Find(os.Getenv("MEETINGS_FFMPEG"))
		c.RetentionEvery = time.Hour
		for _, o := range opt {
			o(c)
		}
	})
	e.lk.Upload = func(key string) int64 { f.Put(key, fixtureMP4); return int64(len(fixtureMP4)) }
	return e, f
}

// composite waits for the n-th (1-based) room composite and answers it.
func (e *env) composite(n int) livekittest.Egress {
	e.t.Helper()
	var out livekittest.Egress
	eventually(e.t, 5*time.Second, "composite egress "+string(rune('0'+n)), func() bool {
		cs := e.lk.Composites()
		if len(cs) >= n {
			out = cs[n-1]
			return true
		}
		return false
	})
	return out
}

func (e *env) recording(c call) (parts []map[string]any, gaps []map[string]any) {
	e.t.Helper()
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/recording", c.Sess, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("recording = %d %s", resp.StatusCode, raw)
	}
	var v struct {
		Parts []map[string]any `json:"parts"`
		Gaps  []map[string]any `json:"gaps"`
	}
	json.Unmarshal(raw, &v)
	return v.Parts, v.Gaps
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

// wantMeetingEvent reads s until a "meeting" event whose data holds want.
func wantMeetingEvent(t *testing.T, s *stream, want string) string {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		ev, ok := s.next(time.Until(end))
		if !ok {
			break
		}
		if ev.Name == "meeting" && strings.Contains(ev.Data, want) {
			return ev.Data
		}
	}
	t.Fatalf("no meeting event holding %q (the stream so far: %q)", want, s.text())
	return ""
}

// 🔴 The part's start in meeting time is the EGRESS'S OWN report of when its
// file began, never when the plugin heard of it. The owner joined at t0
// (meeting time 0); the guest was admitted 5 s in; the egress reports its
// file began at 6.2 s, and that report reaches the plugin at 9 s.
func TestRecordingStartsAtTheFirstAdmitAndItsStartIsTheEgressReport(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.clock.Add(5 * time.Second)
	if n := len(e.lk.Composites()); n != 0 {
		t.Fatalf("%d composites before any guest was admitted", n)
	}
	e.admitGuest(c, grace)
	g := e.composite(1)
	if g.Room != c.Room || g.Filepath != "meetings/"+c.Subject+"/"+c.ID+"/recording-1.mp4" {
		t.Fatalf("composite = %+v", g)
	}
	e.clock.Add(4 * time.Second)
	if code, err := e.lk.BeginComposite(g.ID, t0.Add(6200*time.Millisecond)); err != nil || code != 200 {
		t.Fatalf("egress_updated = %d %v", code, err)
	}
	parts, gaps := e.recording(c)
	if len(parts) != 1 || num(parts[0]["start_ms"]) != 6_200 || parts[0]["state"] != "active" {
		t.Fatalf("part 1 = %v: want its start at meeting time 6200 (the egress's report), not 9000 (its arrival)", parts)
	}
	if len(gaps) != 1 || num(gaps[0]["from_ms"]) != 5_000 || num(gaps[0]["to_ms"]) != 6_200 || gaps[0]["why"] != "starting" {
		t.Fatalf("gaps = %v: want the egress's start-up, 5000 to 6200", gaps)
	}
	// A second admit starts no second recording.
	e.admitGuest(c, linus)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.lk.Composites()); n != 1 {
		t.Fatalf("%d composites after a second admit", n)
	}
}

// An egress that dies is restarted as a new part, the stretch between is a
// gap, everyone's badge says "Recording failed" until the new part begins,
// and each transcript line maps to its part's offset. Every value literal:
// part 1's file began at 6 200 and the egress died at 60 000 (a 53 800 ms
// file); part 2's file began at 63 500 and ended with the meeting at 120 000.
func TestAFailedEgressRestartsAsANewPartWithItsGapAndTheBadgeSaysSo(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.clock.Add(5 * time.Second)
	ticket, _ := e.admitGuest(c, grace)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	guest := e.guestEvents(c.Code, ticket)
	g1 := e.composite(1)
	e.lk.BeginComposite(g1.ID, t0.Add(6200*time.Millisecond))
	wantMeetingEvent(t, owner, `"badge":"Recording · Transcribing · AI"`)
	wantMeetingEvent(t, guest, `"badge":"Recording · Transcribing · AI"`)

	e.clock.Add(55 * time.Second) // t0 + 60 s
	e.lk.KillEgress(g1.ID)
	g2 := e.composite(2)
	if g2.Filepath != "meetings/"+c.Subject+"/"+c.ID+"/recording-2.mp4" {
		t.Fatalf("part 2's key = %q", g2.Filepath)
	}
	wantMeetingEvent(t, owner, `"badge":"Recording failed · Transcribing · AI"`)
	wantMeetingEvent(t, guest, `"badge":"Recording failed · Transcribing · AI"`)
	e.clock.Add(3500 * time.Millisecond)
	e.lk.BeginComposite(g2.ID, e.clock.Now())
	wantMeetingEvent(t, owner, `"badge":"Recording · Transcribing · AI"`)
	wantMeetingEvent(t, guest, `"badge":"Recording · Transcribing · AI"`)

	// Three lines: in part 1, in the gap, in part 2.
	ctx := context.Background()
	if _, err := e.store.UpsertTrack(ctx, c.Room, "TR_offsets1", store.OwnerIdentity, "Ada L.", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{30_000, 61_000, 90_000} {
		if _, err := e.store.AddSegment(ctx, "TR_offsets1", store.SegText, at, at+1500, "line", "fake"); err != nil {
			t.Fatal(err)
		}
	}
	e.clock.Add(56500 * time.Millisecond) // t0 + 120 s
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 5*time.Second, "part 2 complete", func() bool {
		p, _ := e.recording(c)
		return len(p) == 2 && p[1]["state"] == "complete"
	})
	parts, gaps := e.recording(c)
	if num(parts[0]["start_ms"]) != 6_200 || num(parts[0]["duration_ms"]) != 53_800 || parts[0]["state"] != "failed" ||
		num(parts[1]["start_ms"]) != 63_500 || num(parts[1]["duration_ms"]) != 56_500 {
		t.Fatalf("parts = %v", parts)
	}
	if len(gaps) != 2 || num(gaps[1]["from_ms"]) != 60_000 || num(gaps[1]["to_ms"]) != 63_500 || gaps[1]["why"] != "failed" {
		t.Fatalf("gaps = %v: want the failure's gap, 60000 to 63500", gaps)
	}
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/timeline", c.Sess, nil)
	var b struct {
		Segments []struct {
			StartMS   int64 `json:"start_ms"`
			Recording *struct {
				Part     int   `json:"part"`
				OffsetMS int64 `json:"offset_ms"`
			} `json:"recording"`
		} `json:"segments"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &b) != nil || len(b.Segments) != 3 {
		t.Fatalf("timeline = %d %s", resp.StatusCode, raw)
	}
	if r := b.Segments[0].Recording; r == nil || r.Part != 1 || r.OffsetMS != 23_800 {
		t.Errorf("the line at 30000: %+v, want part 1 offset 23800", r)
	}
	if r := b.Segments[1].Recording; r != nil {
		t.Errorf("the line at 61000 (in the gap): %+v, want none", r)
	}
	if r := b.Segments[2].Recording; r == nil || r.Part != 2 || r.OffsetMS != 26_500 {
		t.Errorf("the line at 90000: %+v, want part 2 offset 26500", r)
	}
}

func TestARefusedStartRetriesAndTheBadgeSaysFailedMeanwhile(t *testing.T) {
	e, _ := recEnv(t, func(c *server.Config) { c.EgressBackoff = []time.Duration{300 * time.Millisecond} })
	c := e.meeting(ada)
	e.started(c)
	e.lk.RefuseComposite(1)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.admitGuest(c, grace)
	wantMeetingEvent(t, owner, `"badge":"Recording failed · Transcribing · AI"`)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	wantMeetingEvent(t, owner, `"badge":"Recording · Transcribing · AI"`)
	parts, _ := e.recording(c)
	if len(parts) != 2 || parts[0]["state"] != "failed" || parts[0]["error"] == "" || parts[1]["state"] != "active" {
		t.Fatalf("parts = %v: want the refused part 1 and the running part 2", parts)
	}
}

func TestRecordOffStopsTheEgressAndNothingRestartsIt(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("record off = %d %s", resp.StatusCode, raw)
	}
	for _, x := range e.lk.Composites() {
		if x.ID == g.ID && x.Active() {
			t.Fatal("Record off: the egress is still active")
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(e.lk.Composites()); n != 1 {
		t.Fatalf("Record off: %d composites (a restart)", n)
	}
	parts, _ := e.recording(c)
	if len(parts) != 1 || parts[0]["state"] != "complete" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestEndMeetingStopsTheEgressItself(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	calls := strings.Join(e.lk.Calls(), "\n")
	stop, del := strings.Index(calls, "StopEgress  "+g.ID), strings.Index(calls, "DeleteRoom "+c.Room)
	if stop < 0 || del < 0 || stop > del {
		t.Fatalf("End: StopEgress of the composite at %d, DeleteRoom at %d:\n%s", stop, del, calls)
	}
}

// finishedRecording is a meeting of ada's that ended with one playable part.
func finishedRecording(t *testing.T, e *env) call {
	t.Helper()
	c := e.meeting(ada)
	e.started(c)
	n := len(e.lk.Composites()) + 1
	e.admitGuest(c, grace)
	g := e.composite(n)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	e.clock.Add(20 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	eventually(t, 5*time.Second, "the part complete", func() bool {
		p, _ := e.recording(c)
		return len(p) == 1 && p[0]["state"] == "complete"
	})
	return c
}

func (e *env) share(c call) string {
	e.t.Helper()
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/share", c.Sess, nil)
	var v struct {
		URL string `json:"url"`
	}
	if resp.StatusCode != 201 || json.Unmarshal(raw, &v) != nil || !strings.HasPrefix(v.URL, publicURL+"/r/") {
		e.t.Fatalf("share = %d %s", resp.StatusCode, raw)
	}
	return strings.TrimPrefix(v.URL, publicURL)
}

// 🔴 The share link's door: an unknown, malformed, revoked or expired token,
// and a part that is not there, all answer the SAME 404, byte for byte and
// header for header, for the page and its media alike.
func TestTheShareDoorAnswersOneNotFoundForEveryClosedLink(t *testing.T) {
	e, _ := recEnv(t)
	c := finishedRecording(t, e)
	path := e.share(c)
	resp, page := e.do("GET", path, "", nil)
	if resp.StatusCode != 200 || !bytes.Contains(page, []byte(`<video class="share-video" controls preload="metadata" playsinline src="`+path+`/1">`)) {
		t.Fatalf("the live link = %d %s", resp.StatusCode, page)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self'") || strings.Contains(csp, "script-src") {
		t.Errorf("the share page's CSP = %q", csp)
	}
	ref := func(p string) answer {
		resp, raw := e.do("GET", p, "", nil)
		return answerOf(resp, raw)
	}
	unknown := "/r/" + strings.Repeat("A", 43)
	want := ref(unknown)
	if want.Status != 404 {
		t.Fatalf("an unknown token = %d", want.Status)
	}
	check := func(why, p string) {
		t.Helper()
		if got := ref(p); got.Status != want.Status || got.Body != want.Body || !sameHeaders(got.Headers, want.Headers) {
			t.Errorf("%s (%s): %+v\nwant the unknown token's %+v", why, p, got, want)
		}
	}
	check("an unknown token's media", unknown+"/1")
	check("a malformed token", "/r/short")
	check("a part that is not there", path+"/2")
	// Revoked.
	if resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/share", c.Sess, nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"revoked":true`) {
		t.Fatalf("revoke = %d %s", resp.StatusCode, raw)
	}
	check("a revoked token", path)
	check("a revoked token's media", path+"/1")
	// Expired: a new link, then the clock past the 30-day purge time.
	path = e.share(c)
	if resp, _ := e.do("GET", path+"/1", "", nil); resp.StatusCode != 200 {
		t.Fatalf("the new link's media = %d", resp.StatusCode)
	}
	e.clock.Add(30 * 24 * time.Hour)
	check("an expired token", path)
	check("an expired token's media", path+"/1")
}

func sameHeaders(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestTheShareMediaAnswersRangeRequests(t *testing.T) {
	e, _ := recEnv(t)
	c := finishedRecording(t, e)
	path := e.share(c) + "/1"
	resp, raw := e.do("GET", path, "", nil, "Range", "bytes=0-99")
	if resp.StatusCode != 206 || len(raw) != 100 || !bytes.Equal(raw, fixtureMP4[:100]) ||
		resp.Header.Get("Content-Range") != "bytes 0-99/"+itoa(len(fixtureMP4)) || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("bytes=0-99: %d %d bytes, Content-Range %q", resp.StatusCode, len(raw), resp.Header.Get("Content-Range"))
	}
	resp, raw = e.do("GET", path, "", nil, "Range", "bytes=-16")
	if resp.StatusCode != 206 || !bytes.Equal(raw, fixtureMP4[len(fixtureMP4)-16:]) {
		t.Fatalf("the tail (where Egress's MP4 keeps its index): %d %d bytes", resp.StatusCode, len(raw))
	}
	resp, raw = e.do("GET", path, "", nil)
	if resp.StatusCode != 200 || !bytes.Equal(raw, fixtureMP4) || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("whole: %d %d bytes", resp.StatusCode, len(raw))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// 🔴 The owner's signed media URL: valid, it streams with Range; expired,
// tampered, or another meeting's path under this signature, it is 403.
func TestSignedMediaURLs(t *testing.T) {
	e, _ := recEnv(t)
	c := finishedRecording(t, e)
	other := finishedRecording(t, e)
	url := func(c call) string {
		resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/recording-url", c.Sess, nil)
		var v struct {
			Parts []struct {
				URL     string `json:"url"`
				StartMS int64  `json:"start_ms"`
			} `json:"parts"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(raw, &v) != nil || len(v.Parts) != 1 {
			t.Fatalf("recording-url = %d %s", resp.StatusCode, raw)
		}
		return v.Parts[0].URL
	}
	u := url(c)
	if resp, raw := e.do("GET", u, "", nil, "Range", "bytes=0-99"); resp.StatusCode != 206 || len(raw) != 100 {
		t.Fatalf("a valid URL = %d (%d bytes)", resp.StatusCode, len(raw))
	}
	q := u[strings.Index(u, "?"):]
	if resp, _ := e.do("GET", "/media/"+other.ID+"/1"+q, "", nil); resp.StatusCode != 403 {
		t.Errorf("another meeting's path under this signature = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do("GET", strings.Replace(u, "exp=", "exp=9", 1), "", nil); resp.StatusCode != 403 {
		t.Errorf("a tampered expiry = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do("GET", u[:len(u)-2]+"AA", "", nil); resp.StatusCode != 403 {
		t.Errorf("a tampered signature = %d, want 403", resp.StatusCode)
	}
	e.clock.Add(server.MediaURLLifetime)
	if resp, _ := e.do("GET", u, "", nil); resp.StatusCode != 403 {
		t.Errorf("an expired URL = %d, want 403", resp.StatusCode)
	}
	// Bea cannot mint one for ada's meeting.
	bsess := e.enter(bea)
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/recording-url", bsess, nil); resp.StatusCode != 404 {
		t.Errorf("another owner's recording-url = %d, want 404", resp.StatusCode)
	}
	for _, p := range []string{"/recording", "/timeline", "/frame?at=1"} {
		if resp, _ := e.do("GET", "/api/meetings/"+c.ID+p, bsess, nil); resp.StatusCode != 404 {
			t.Errorf("another owner's %s = %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/share", bsess, nil); resp.StatusCode != 404 {
		t.Errorf("another owner's share = %d, want 404", resp.StatusCode)
	}
}

// 🔴 The sweeper deletes each part's OBJECT, then its row; with the store
// down it deletes neither, and the next pass (the store back) does both.
func TestTheSweeperDeletesTheObjectThenTheRowAndRetriesWhenTheStoreIsDown(t *testing.T) {
	e, f := recEnv(t)
	c := finishedRecording(t, e)
	key := "meetings/" + c.Subject + "/" + c.ID + "/recording-1.mp4"
	if !f.Has(key) {
		t.Fatalf("no object %s: %v", key, f.Keys())
	}
	path := e.share(c)
	e.clock.Add(30*24*time.Hour - time.Second)
	if d, l := e.server.SweepRecordings(context.Background()); d != 0 || l != 0 || !f.Has(key) {
		t.Fatalf("before the purge time: deleted %d, left %d, object kept %v", d, l, f.Has(key))
	}
	e.clock.Add(time.Second)
	f.SetDown(true)
	if d, l := e.server.SweepRecordings(context.Background()); d != 0 || l != 1 {
		t.Fatalf("the store down: deleted %d, left %d", d, l)
	}
	if parts, _, _ := e.store.ForSubject(c.Subject).Recordings(context.Background(), c.ID); len(parts) != 1 {
		t.Fatalf("the store down: the row was deleted while the object stays (%v)", parts)
	}
	f.SetDown(false)
	if d, l := e.server.SweepRecordings(context.Background()); d != 1 || l != 0 {
		t.Fatalf("the store back: deleted %d, left %d", d, l)
	}
	if f.Has(key) {
		t.Fatal("the store back: the object still stored")
	}
	if parts, _, _ := e.store.ForSubject(c.Subject).Recordings(context.Background(), c.ID); len(parts) != 0 {
		t.Fatalf("the row is kept after its object went: %v", parts)
	}
	// Repeating is harmless, and the link is gone with it.
	if d, l := e.server.SweepRecordings(context.Background()); d != 0 || l != 0 {
		t.Fatalf("a repeat: deleted %d, left %d", d, l)
	}
	if resp, _ := e.do("GET", path, "", nil); resp.StatusCode != 404 {
		t.Fatalf("the link after the purge = %d", resp.StatusCode)
	}
	if strings.Contains(e.log.String(), key) || strings.Contains(e.log.String(), "recording-1.mp4") {
		t.Errorf("the sweeper logged an object key:\n%s", e.log.String())
	}
}

func TestRetentionSettingsAndThePurgeTimeTheEndGives(t *testing.T) {
	e, _ := recEnv(t)
	sess := e.enter(ada)
	resp, raw := e.do("GET", "/api/settings", sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"recording_retention_days":30`) {
		t.Fatalf("settings = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.do("PUT", "/api/settings", sess, map[string]int{"recording_retention_days": 90}); resp.StatusCode != 400 {
		t.Fatalf("90 days = %d, want 400", resp.StatusCode)
	}
	if resp, raw := e.do("PUT", "/api/settings", sess, map[string]int{"recording_retention_days": 365}); resp.StatusCode != 200 {
		t.Fatalf("365 days = %d %s", resp.StatusCode, raw)
	}
	c := finishedRecording(t, e)
	resp, raw = e.do("GET", "/api/meetings/"+c.ID+"/recording", c.Sess, nil)
	var v struct {
		PurgeAt time.Time `json:"purge_at"`
	}
	json.Unmarshal(raw, &v)
	if want := e.clock.Now().Add(365 * 24 * time.Hour); !v.PurgeAt.Equal(want) {
		t.Fatalf("purge_at = %v, want %v", v.PurgeAt, want)
	}
}

func TestTheShareTokenNeverReachesTheLog(t *testing.T) {
	e, _ := recEnv(t)
	c := finishedRecording(t, e)
	path := e.share(c)
	e.do("GET", path, "", nil)
	e.do("GET", path+"/1", "", nil, "Range", "bytes=0-9")
	tok := strings.TrimPrefix(path, "/r/")
	if strings.Contains(e.log.String(), tok) {
		t.Fatalf("the log holds the share token:\n%s", e.log.String())
	}
	if !strings.Contains(e.log.String(), "/r/{token}/1") {
		t.Fatalf("no redacted share line in the log:\n%s", e.log.String())
	}
}

func TestRedactPathHidesAShareToken(t *testing.T) {
	tok := strings.Repeat("Q", 43)
	for in, want := range map[string]string{
		"/r/" + tok:          "/r/{token}",
		"/r/" + tok + "/1":   "/r/{token}/1",
		"//r/" + tok:         "/r/{token}",
		"/x/../r/" + tok:     "/r/{token}",
		"/static/r/" + tok:   "/{redacted}",
		"/api/meetings/abcd": "/api/meetings/abcd",
	} {
		if got := server.RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bundle's screen shares and frame refs: a line spoken while a screen
// was shared and recorded gets one frame ref, at the middle of that overlap;
// with an ffmpeg, the frame route answers the JPEG and its place.
func TestTheTimelineBundleNamesFramesWhileAScreenWasShared(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, t0.Add(1000*time.Millisecond)) // file began at meeting time 1000
	e.clock.Add(4 * time.Second)
	scr := e.publish(c, store.OwnerIdentity, "SCREEN_SHARE") // shared from 4000
	ctx := context.Background()
	e.store.UpsertTrack(ctx, c.Room, "TR_bundle1", store.OwnerIdentity, "Ada L.", e.clock.Now())
	e.store.AddSegment(ctx, "TR_bundle1", store.SegText, 2_000, 3_000, "before the share", "fake")
	e.store.AddSegment(ctx, "TR_bundle1", store.SegText, 5_000, 7_000, "on the slide", "fake")
	e.clock.Add(6 * time.Second)
	if code, err := e.lk.Unpublish(c.Room, store.OwnerIdentity, scr); err != nil || code != 200 {
		t.Fatalf("unpublish = %d %v", code, err)
	} // shared until 10000
	e.clock.Add(5 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil) // ends at 15000; the file is 14 s
	eventually(t, 5*time.Second, "the part complete", func() bool {
		p, _ := e.recording(c)
		return len(p) == 1 && p[0]["state"] == "complete"
	})
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/timeline", c.Sess, nil)
	var b struct {
		ScreenShares []struct {
			Speaker        string `json:"speaker"`
			StartMS, EndMS int64
		} `json:"screen_shares"`
		Segments []struct {
			Text        string `json:"text"`
			ScreenShare bool   `json:"screen_share"`
		} `json:"segments"`
		Frames []struct {
			MeetingMS int64  `json:"meeting_ms"`
			Part      int    `json:"part"`
			OffsetMS  int64  `json:"offset_ms"`
			Href      string `json:"href"`
		} `json:"frames"`
		FramesAvailable bool `json:"frames_available"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &b) != nil {
		t.Fatalf("timeline = %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"screen_shares":[{"speaker":"Ada L.","start_ms":4000,"end_ms":10000}]`) {
		t.Errorf("screen shares: %s", raw)
	}
	if len(b.Segments) != 2 || b.Segments[0].ScreenShare || !b.Segments[1].ScreenShare {
		t.Errorf("segments: %+v", b.Segments)
	}
	// The line 5000–7000 overlaps the share 4000–10000 at 5000–7000: one
	// frame at 6000, part 1, 5000 ms into its file.
	if len(b.Frames) != 1 || b.Frames[0].MeetingMS != 6_000 || b.Frames[0].Part != 1 || b.Frames[0].OffsetMS != 5_000 ||
		b.Frames[0].Href != "/api/meetings/"+c.ID+"/frame?at=6000" {
		t.Fatalf("frames: %+v", b.Frames)
	}
	if !b.FramesAvailable {
		if os.Getenv("MEETINGS_REQUIRE_FFMPEG") == "1" {
			t.Fatal("no ffmpeg (MEETINGS_REQUIRE_FFMPEG=1)")
		}
		t.Skip("no ffmpeg: the frame route is not exercised")
	}
	resp, img := e.do("GET", b.Frames[0].Href, c.Sess, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || len(img) < 4 || img[0] != 0xFF || img[1] != 0xD8 ||
		resp.Header.Get("X-Recording-Offset-Ms") != "5000" || resp.Header.Get("X-Meeting-Ms") != "6000" {
		t.Fatalf("frame = %d %q %d bytes, offset %q", resp.StatusCode, resp.Header.Get("Content-Type"), len(img), resp.Header.Get("X-Recording-Offset-Ms"))
	}
	if resp, _ := e.do("GET", "/api/meetings/"+c.ID+"/frame?at=500", c.Sess, nil); resp.StatusCode != 404 {
		t.Errorf("a frame before the recording began = %d, want 404", resp.StatusCode)
	}
}

// A lost egress_ended webhook: the reconcile asks LiveKit, and the part ends
// as the webhook would have ended it.
func TestTheReconcileEndsAPartWhoseWebhookWasLost(t *testing.T) {
	e, _ := recEnv(t)
	c := e.meeting(ada)
	e.started(c)
	e.admitGuest(c, grace)
	g := e.composite(1)
	e.lk.BeginComposite(g.ID, e.clock.Now())
	e.lk.WebhookURL = "" // webhooks lost from here on
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	if parts, _ := e.recording(c); parts[0]["state"] != "active" {
		t.Fatalf("without the webhook the part is %v", parts[0]["state"])
	}
	e.server.Reconcile(context.Background())
	if parts, _ := e.recording(c); parts[0]["state"] != "complete" {
		t.Fatalf("after the reconcile the part is %v", parts[0]["state"])
	}
}
