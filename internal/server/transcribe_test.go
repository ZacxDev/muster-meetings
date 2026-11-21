package server_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The phase-3a tests: track Egress to the ingest, the engines, the panels,
// the archive and its exports. The fake LiveKit acts as Egress: it accepts a
// StartTrackEgress, dials the websocket URL the plugin gave it with no other
// credential, and streams Egress's PCM (1920-byte messages, S16LE stereo
// 48 kHz, identical channels).

// speech is the fixture every fake Egress streams by default: two
// utterances (1 s of speech, 1 s of silence, twice).
var speech = egresstest.Script(1000, 1000, 1000)

// segEvent is a "segment" event.
type segEvent struct {
	ID      int64  `json:"id"`
	Speaker string `json:"speaker"`
	Kind    string `json:"kind"`
	StartMS int64  `json:"start_ms"`
	EndMS   *int64 `json:"end_ms"`
	Text    string `json:"text"`
}

// started opens the owner's call (their token marks the meeting started at
// the env's clock: meeting time 0).
func (e *env) started(c call) {
	e.t.Helper()
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 200 {
		e.t.Fatalf("owner token = %d %s", resp.StatusCode, raw)
	}
}

// admitGuest knocks as name, is admitted, and answers the ticket and the
// LiveKit identity.
func (e *env) admitGuest(c call, name string) (ticket, identity string) {
	e.t.Helper()
	ticket = e.knock(c.Code, name)
	e.admit(c, name)
	tok := admittedToken(e.t, e.wait(c.Code, ticket))
	return ticket, tok["identity"]
}

// publish publishes identity's microphone; the webhook must be taken.
func (e *env) publish(c call, identity, source string) string {
	e.t.Helper()
	sid, code, err := e.lk.Publish(c.Room, identity, source)
	if err != nil || code != 200 {
		e.t.Fatalf("track_published for %s = %d, %v", identity, code, err)
	}
	return sid
}

// egressOf waits for the n-th (1-based) egress of track and answers it.
func (e *env) egressOf(track string, n int) livekittest.Egress {
	e.t.Helper()
	var out livekittest.Egress
	eventually(e.t, 5*time.Second, "egress "+track, func() bool {
		k := 0
		for _, g := range e.lk.Egresses() {
			if g.Track == track {
				if k++; k == n {
					out = g
					return true
				}
			}
		}
		return false
	})
	return out
}

// segmentsOf reads s until want text segments (kind "text") arrived.
func segmentsOf(t *testing.T, s *stream, want int) []segEvent {
	t.Helper()
	var out []segEvent
	for len(out) < want {
		ev := s.want("segment")
		var g segEvent
		if err := json.Unmarshal([]byte(ev.Data), &g); err != nil {
			t.Fatal(err)
		}
		if g.Kind == "text" {
			out = append(out, g)
		}
	}
	return out
}

func lines(gs []segEvent) []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Speaker+": "+g.Text)
	}
	return out
}

// ---- the fake engine end to end, through both panels ----

// The owner and an admitted guest each publish a microphone; the plugin
// starts a track egress for each; the (fake) Egress streams PCM to the
// ingest with only the URL's token; the fake engine's lines arrive, under
// the right speaker, on BOTH event streams (the owner's panel and the
// guest's), identical, and are stored for the archive.
func TestTheFakeEngineEndToEndThroughTheOwnersAndTheGuestsPanels(t *testing.T) {
	e := newEnv(t)
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	ticket, gid := e.admitGuest(c, grace)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	guest := e.guestEvents(c.Code, ticket)

	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	ownerLines := segmentsOf(t, owner, 2)
	e.publish(c, gid, "MICROPHONE")
	ownerLines = append(ownerLines, segmentsOf(t, owner, 2)...)
	guestLines := segmentsOf(t, guest, 4)

	want := []string{"Ada L.: " + fakeScript[0], "Ada L.: " + fakeScript[1], grace + " (guest): " + fakeScript[0], grace + " (guest): " + fakeScript[1]}
	if !slices.Equal(lines(ownerLines), want) {
		t.Fatalf("the owner's panel: %q, want %q", lines(ownerLines), want)
	}
	if !slices.Equal(lines(guestLines), want) {
		t.Fatalf("the guest's panel: %q, want %q", lines(guestLines), want)
	}
	// Each egress was given a URL on the ingest listener, and the Egress
	// connected to it with nothing but that URL.
	for _, g := range e.lk.Egresses() {
		if !strings.HasPrefix(g.URL, "ws"+strings.TrimPrefix(e.ingest.URL, "http")+"/stt/ingest/"+g.Track+"/") || g.DialErr != "" {
			t.Errorf("egress %s: URL %q, dial error %q", g.ID, g.URL, g.DialErr)
		}
	}
	// The archive holds the same lines.
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript", c.Sess, nil)
	var tr struct {
		Segments []segEvent `json:"segments"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &tr) != nil {
		t.Fatalf("transcript = %d %s", resp.StatusCode, raw)
	}
	var stored []string
	for _, l := range lines(tr.Segments) {
		stored = append(stored, l)
	}
	slices.Sort(stored)
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	if !slices.Equal(stored, sorted) {
		t.Fatalf("the archive: %q, want %q", stored, sorted)
	}
	// And no line, and no guest name, ever reached the log.
	for _, s := range append(slices.Clone(fakeScript), grace) {
		if strings.Contains(e.log.String(), s) {
			t.Fatalf("the log carries %q", s)
		}
	}
}

// 🔴 Only the microphone of someone who may be in the room is transcribed:
// a waiting guest's, a stranger's, and the owner's camera start no egress.
func TestOnlyAnAdmittedIdentitysMicrophoneIsTranscribed(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	// Linus is waiting: his identity is not yet one that may be in the room.
	e.knock(c.Code, linus)
	var waiting string
	if err := e.dbQuery(`SELECT identity FROM participants WHERE display_name = ?`, linus).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	e.publish(c, waiting, "MICROPHONE")
	e.publish(c, "guest-Stranger00000000000", "MICROPHONE")
	e.publish(c, store.OwnerIdentity, "CAMERA")
	time.Sleep(200 * time.Millisecond)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 0 {
		t.Fatalf("%d egress starts for a stranger's microphone and a camera, want none", n)
	}
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	eventually(t, 5*time.Second, "the owner's microphone gets an egress (the positive control)", func() bool {
		return e.lk.CountCalls("StartTrackEgress") == 1
	})
}

// ---- 🔴 the ingest's token ----

// dialStatus dials url as Egress does and answers 101 on success, or the
// status the upgrade was refused with.
func dialStatus(t *testing.T, url string) (int, *egresstest.Client) {
	t.Helper()
	cl, err := egresstest.Dial(url)
	var ref *egresstest.ErrRefused
	if errors.As(err, &ref) {
		return ref.Status, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Drop() })
	return 101, cl
}

// "Ingest token" (design): an upgrade to /stt/ingest/<track>/<token> with a
// wrong, tampered, or other track's token, or an older egress's, is 401;
// with the right one it is accepted, and no header is ever needed (Egress
// sends none). The access log names the track and never the token. Neither
// public nor internal listener routes it.
func TestTheIngestTokenGatesEveryStream(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	_, gid := e.admitGuest(c, grace)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	b := e.publish(c, gid, "MICROPHONE")
	ea, eb := e.egressOf(a, 1), e.egressOf(b, 1)
	tokA := ea.URL[strings.LastIndex(ea.URL, "/")+1:]
	tokB := eb.URL[strings.LastIndex(eb.URL, "/")+1:]
	base := "ws" + strings.TrimPrefix(e.ingest.URL, "http") + "/stt/ingest/"

	flip := []byte(tokA)
	flip[len(flip)-2] ^= 1
	for name, u := range map[string]string{
		"a tampered token":      base + a + "/" + string(flip),
		"another track's token": base + a + "/" + tokB,
		"no token":              base + a + "/",
		"garbage":               base + a + "/x.y.z",
		"an unknown track":      base + "TR_nobody0001/" + tokA,
	} {
		if code, _ := dialStatus(t, u); code != http.StatusUnauthorized && !(name == "no token" && code == http.StatusNotFound) {
			t.Errorf("%s: the upgrade = %d, want 401", name, code)
		}
	}
	if code, _ := dialStatus(t, ea.URL); code != 101 {
		t.Fatalf("the right token: the upgrade = %d, want 101", code)
	}
	// A newer egress of the track (the old one died) makes the old token
	// dead: Egress logs the full URL it was given.
	e.lk.KillEgress(ea.ID)
	ea2 := e.egressOf(a, 2)
	if code, _ := dialStatus(t, ea.URL); code != http.StatusUnauthorized {
		t.Errorf("an older egress's token: the upgrade = %d, want 401", code)
	}
	if code, _ := dialStatus(t, ea2.URL); code != 101 {
		t.Errorf("the newer egress's token: the upgrade = %d, want 101", code)
	}
	// The ingest is on its own listener only.
	for name, srv := range map[string]string{"public": e.srv.URL, "internal": e.internal.URL} {
		if code, _ := dialStatus(t, "ws"+strings.TrimPrefix(srv, "http")+"/stt/ingest/"+a+"/"+tokA); code != http.StatusNotFound {
			t.Errorf("the %s listener answers the ingest route %d, want 404", name, code)
		}
	}
	// The log names the track, never a token.
	log := e.log.String()
	if !strings.Contains(log, "/stt/ingest/"+a) {
		t.Errorf("the log never names /stt/ingest/%s:\n%s", a, log)
	}
	for _, tok := range []string{tokA, tokB, string(flip), ea2.URL[strings.LastIndex(ea2.URL, "/")+1:]} {
		nonce, _, _ := strings.Cut(tok, ".")
		if strings.Contains(log, nonce) {
			t.Fatalf("the log carries a token:\n%s", log)
		}
	}
}

// 🔴 Audio is refused for a meeting that has ended (410, with a token that
// was good), and once it ends the open streams are closed and the egresses
// stopped.
func TestAnEndedMeetingTakesNoAudio(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	ea := e.egressOf(a, 1)
	code, cl := dialStatus(t, ea.URL)
	if code != 101 {
		t.Fatalf("before the end: %d, want 101", code)
	}
	cl.Watch()
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 5*time.Second, "the open stream is closed by the plugin (a close frame)", func() bool {
		got := cl.Received()
		return len(got) >= 2 && got[0] == 0x88
	})
	if e.lk.CountCalls("StopEgress") == 0 {
		t.Error("End meeting did not stop the egress")
	}
	if code, _ := dialStatus(t, ea.URL); code != http.StatusGone {
		t.Fatalf("after the end, the egress's own token: %d, want 410", code)
	}
}

// With no engine, transcription is off: no egress starts and the ingest
// refuses every stream.
func TestWithTranscriptionOffNoEgressStartsAndTheIngestRefuses(t *testing.T) {
	e := newEnvOpts(t, nil, func(c *server.Config) { c.Engine = nil })
	c := e.meeting(ada)
	e.started(c)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	time.Sleep(200 * time.Millisecond)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 0 {
		t.Fatalf("%d egress starts with transcription off, want none", n)
	}
	if code, _ := dialStatus(t, "ws"+strings.TrimPrefix(e.ingest.URL, "http")+"/stt/ingest/"+a+"/"+strings.Repeat("A", 22)+".9999999999."+strings.Repeat("A", 43)); code != http.StatusNotFound {
		t.Fatalf("the ingest with transcription off: %d, want 404", code)
	}
}

// ---- the time base ----

// "Timestamps by sample count" (design): the same fixture delivered evenly,
// and delivered with a 7 s stall and then a burst, yields the same segment
// times, and they are the fixture's own (speech at 0 and 2000 ms).
func TestSegmentTimesComeFromTheSampleCountNotTheArrival(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	_, gid := e.admitGuest(c, grace)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	b := e.publish(c, gid, "MICROPHONE")
	_, even := dialStatus(t, e.egressOf(a, 1).URL)
	_, stall := dialStatus(t, e.egressOf(b, 1).URL)

	if err := even.Stream(speech, 0); err != nil {
		t.Fatal(err)
	}
	evenSegs := segmentsOf(t, owner, 2)
	// The stalled stream: its first utterance, then 7 s with nothing (the
	// clock moves on), then the rest in one burst.
	half := len(speech) / 2
	if err := stall.Stream(speech[:half], 0); err != nil {
		t.Fatal(err)
	}
	first := segmentsOf(t, owner, 1)
	e.clock.Add(7 * time.Second)
	if err := stall.Stream(speech[half:], 0); err != nil {
		t.Fatal(err)
	}
	stallSegs := append(first, segmentsOf(t, owner, 1)...)

	span := func(gs []segEvent) [][2]int64 {
		var out [][2]int64
		for _, g := range gs {
			out = append(out, [2]int64{g.StartMS, *g.EndMS})
		}
		return out
	}
	if !slices.Equal(span(evenSegs), span(stallSegs)) {
		t.Fatalf("segment times: even %v, after a 7 s stall %v; want them equal", span(evenSegs), span(stallSegs))
	}
	// The fixture's own times: speech 0–1000 ms and 2000–3000 ms (with the
	// endpointer's 200 ms pre-roll and 100 ms tail).
	s := span(stallSegs)
	if s[0][0] != 0 || s[0][1] < 1000 || s[0][1] > 1200 || s[1][0] < 1700 || s[1][0] > 2000 || s[1][1] < 3000 || s[1][1] > 3200 {
		t.Fatalf("segment times %v, want about [0 1100] [1800 3100]", s)
	}
}

// ---- egress restart and start retry ----

// "Egress restart and start retry" (design): a refused start is retried
// with backoff until it is accepted, with a gap marker (and "Transcript
// paused") until audio flows; an egress that dies (egress_ended, failed)
// while its track is published gets a new egress, a gap, and the gap closes
// when the new stream's audio arrives.
func TestARefusedStartIsRetriedAndADeadEgressIsReplacedWithAGapUntilAudioFlows(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.lk.RefuseEgress(2)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	ea := e.egressOf(a, 1)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 3 {
		t.Fatalf("%d starts, want 3 (two refused, then accepted)", n)
	}
	status := func() string {
		for {
			ev := owner.want("transcript")
			var v struct{ Status string }
			json.Unmarshal([]byte(ev.Data), &v)
			return v.Status
		}
	}
	// The gap is open, and says so, until audio flows.
	gap := owner.want("segment")
	var g segEvent
	json.Unmarshal([]byte(gap.Data), &g)
	if g.Kind != "gap" || g.EndMS != nil || g.Speaker != "Ada L." {
		t.Fatalf("the first segment = %s, want an open gap for Ada L.", gap.Data)
	}
	for st := status(); st != server.TranscriptPaused; st = status() {
	}
	e.clock.Add(3 * time.Second)
	_, cl := dialStatus(t, ea.URL)
	cl.Stream(egresstest.Silence(100), 0)
	closed := owner.want("segment")
	json.Unmarshal([]byte(closed.Data), &g)
	if g.Kind != "gap" || g.EndMS == nil || *g.EndMS != 3000 {
		t.Fatalf("after audio flows the gap = %s, want it closed at 3000 ms", closed.Data)
	}
	for st := status(); st != ""; st = status() {
	}

	// The egress dies: a new one is started, and a new gap opens.
	e.clock.Add(2 * time.Second)
	e.lk.KillEgress(ea.ID)
	ea2 := e.egressOf(a, 2)
	reopened := owner.want("segment")
	json.Unmarshal([]byte(reopened.Data), &g)
	if g.Kind != "gap" || g.EndMS != nil || g.StartMS != 5000 {
		t.Fatalf("after the egress died: %s, want a gap opened at 5000 ms", reopened.Data)
	}
	e.clock.Add(time.Second)
	_, cl2 := dialStatus(t, ea2.URL)
	cl2.Stream(speech, 0)
	closed = owner.want("segment")
	json.Unmarshal([]byte(closed.Data), &g)
	if g.Kind != "gap" || g.EndMS == nil || *g.EndMS != 6000 {
		t.Fatalf("after the new egress's audio: %s, want the gap closed at 6000 ms", closed.Data)
	}
	// The lines go on, placed after the gap.
	ls := segmentsOf(t, owner, 1)
	if ls[0].StartMS < 6000 {
		t.Fatalf("the first line after the restart starts at %d ms, want after 6000", ls[0].StartMS)
	}
}

// A track that is unpublished is not restarted when its egress ends.
func TestAnUnpublishedTracksEgressIsNotRestarted(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	e.egressOf(a, 1)
	if code, err := e.lk.Unpublish(c.Room, store.OwnerIdentity, a); err != nil || code != 200 {
		t.Fatalf("unpublish = %d %v", code, err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := e.lk.CountCalls("StartTrackEgress"); n != 1 {
		t.Fatalf("%d starts after the track was unpublished and its egress ended, want 1", n)
	}
}

// ---- the archive and its exports ----

// The transcript is its owner's alone: another person, a guest's ticket
// and no session get nothing.
func TestTheTranscriptIsTheOwnersAlone(t *testing.T) {
	e := newEnv(t)
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	ticket, _ := e.admitGuest(c, grace)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	segmentsOf(t, owner, 2)
	bea := e.enter(bea)
	for name, bearer := range map[string]string{"another person": bea, "a guest's ticket": ticket, "no session": ""} {
		for _, f := range []string{"json", "txt", "vtt"} {
			resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript?format="+f, bearer, nil)
			if resp.StatusCode != 404 && resp.StatusCode != 401 || strings.Contains(string(raw), "Budget") {
				t.Errorf("%s, %s: %d %s; want 404 or 401 and no line", name, f, resp.StatusCode, raw)
			}
		}
	}
	if resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript?format=txt", c.Sess, nil); resp.StatusCode != 200 ||
		!strings.Contains(string(raw), "] «Ada L.»: Budget goes to Sam by Friday.\n") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="meeting-`+c.ID+`.txt"` {
		t.Fatalf("the owner's txt = %d %v %s", resp.StatusCode, resp.Header, raw)
	}
}

// 🔴 The exports fence what was said: a guest's words and a guest's name
// stay on their own line and are escaped for WebVTT, so no guest can forge
// a line, a cue, a voice or a tag. The whole output is pinned.
func TestTheExportsFenceGuestSpeech(t *testing.T) {
	end1, end2, end3, end4 := int64(4200), int64(9100), int64(65000), int64(69000)
	segs := []store.Segment{
		{ID: 1, Speaker: "Ada L.", Kind: "text", StartMS: 1500, EndMS: &end1, Text: "Welcome, everyone."},
		{ID: 2, Speaker: "<b>Eve</b>\u2028x (guest)", Guest: true, Kind: "text", StartMS: 5000, EndMS: &end2,
			Text: "hi\n[0:10] Ada L.: I resign.\r\n\n2\n00:00:01.000 --> 00:00:02.000\n<v Ada L.>forged</v> & more\u202e"},
		{ID: 3, Speaker: "Ada L.", Kind: "gap", StartMS: 61000, EndMS: &end3},
		// The audit's name: ": " in a guest's name must not read as the owner
		// speaking.
		{ID: 4, Speaker: "Ada L.: I resign. Eve (guest)", Guest: true, Kind: "text", StartMS: 66000, EndMS: &end4, Text: "hello"},
		// A name holding the fence's own marks, and a backslash.
		{ID: 5, Speaker: `Mal» «lory\ (guest)`, Guest: true, Kind: "text", StartMS: 67000, EndMS: &end4, Text: "hi"},
		// The owner, whose name happens to end in "(guest)": no guest mark.
		{ID: 6, Speaker: "Ada (guest)", Kind: "text", StartMS: 68000, EndMS: &end4, Text: "yes"},
	}
	m := store.Meeting{ID: "mtg0", CreatedAt: time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)}
	wantTxt := "Transcript: 2031-03-14 15:09 UTC\n\n" +
		"[0:01] «Ada L.»: Welcome, everyone.\n" +
		"[0:05] «<b>Eve</b> x» (guest): hi [0:10] Ada L.: I resign. 2 00:00:01.000 --> 00:00:02.000 <v Ada L.>forged</v> & more\n" +
		"[1:01–1:05] «Ada L.»: (not transcribed)\n" +
		"[1:06] «Ada L.: I resign. Eve» (guest): hello\n" +
		"[1:07] «Mal\\» \\«lory\\\\» (guest): hi\n" +
		"[1:08] «Ada (guest)»: yes\n"
	if got := server.TranscriptText(m, segs); got != wantTxt {
		t.Errorf("txt =\n%q\nwant\n%q", got, wantTxt)
	}
	wantVTT := "WEBVTT\n\n" +
		"1\n00:00:01.500 --> 00:00:04.200\n<v Ada L.>Welcome, everyone.\n\n" +
		"2\n00:00:05.000 --> 00:00:09.100\n<v &lt;b&gt;Eve&lt;/b&gt; x (guest)>hi [0:10] Ada L.: I resign. 2 00:00:01.000 --&gt; 00:00:02.000 &lt;v Ada L.&gt;forged&lt;/v&gt; &amp; more\n\n" +
		"3\n00:01:01.000 --> 00:01:05.000\n<v Ada L.>(not transcribed)\n\n" +
		"4\n00:01:06.000 --> 00:01:09.000\n<v Ada L.: I resign. Eve (guest)>hello\n\n" +
		"5\n00:01:07.000 --> 00:01:09.000\n<v Mal» «lory\\ (guest)>hi\n\n" +
		"6\n00:01:08.000 --> 00:01:09.000\n<v Ada (guest)>yes\n\n"
	if got := server.TranscriptVTT(segs); got != wantVTT {
		t.Errorf("vtt =\n%q\nwant\n%q", got, wantVTT)
	}
}

// dbQuery runs one read on the env's database file.
func (e *env) dbQuery(q string, args ...any) *sql.Row {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?mode=ro")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return db.QueryRow(q, args...)
}
