package server_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stttest"
)

// Late closing marks (the contract between muster's stt-nemotron server and
// this plugin): the model emits a sentence's closing mark (. ? ! ,) only when
// the NEXT speech begins, so the server, which does not hold finals back,
// sends it as the next final's `lead`. The plugin appends it to the same
// participant's previous line: in the store, on every open panel (an `amend`
// event), and so in the archive and the export. The fake server
// (internal/stttest) delivers a script line's trailing marks exactly so.

// amendEvent is an "amend" event.
type amendEvent struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// panel is what one event stream has shown: its lines by id, as amended,
// in the order they came, and the amends it was sent.
type panel struct {
	order  []int64
	text   map[int64]string
	who    map[int64]string
	amends []amendEvent
}

// read reads s into p until done(p) holds, failing after 15 s.
func (p *panel) read(t *testing.T, s *stream, done func(*panel) bool) {
	t.Helper()
	if p.text == nil {
		p.text, p.who = map[int64]string{}, map[int64]string{}
	}
	deadline := time.Now().Add(15 * time.Second)
	for !done(p) {
		ev, ok := s.next(time.Until(deadline))
		if !ok {
			t.Fatalf("the panel stopped short: lines %q, amends %+v (stream: %q)", p.lines(), p.amends, s.text())
		}
		switch ev.Name {
		case "segment":
			var g segEvent
			json.Unmarshal([]byte(ev.Data), &g)
			if g.Kind != "text" {
				continue
			}
			if _, seen := p.text[g.ID]; !seen {
				p.order = append(p.order, g.ID)
			}
			p.text[g.ID], p.who[g.ID] = g.Text, g.Speaker
		case "amend":
			var a amendEvent
			json.Unmarshal([]byte(ev.Data), &a)
			p.amends = append(p.amends, a)
			if _, seen := p.text[a.ID]; !seen {
				t.Fatalf("an amend for line %d, which this panel was never sent", a.ID)
			}
			p.text[a.ID] = a.Text
		}
	}
}

// lines is the panel's lines as "who: text", in order.
func (p *panel) lines() []string {
	var out []string
	for _, id := range p.order {
		out = append(out, p.who[id]+": "+p.text[id])
	}
	return out
}

// archive is the meeting's stored lines as "who: text" (gaps left out).
func (e *env) archive(c call) []string {
	e.t.Helper()
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript", c.Sess, nil)
	var tr struct {
		Segments []segEvent `json:"segments"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &tr) != nil {
		e.t.Fatalf("transcript = %d %s", resp.StatusCode, raw)
	}
	var out []string
	for _, g := range tr.Segments {
		if g.Kind == "text" {
			out = append(out, g.Speaker+": "+g.Text)
		}
	}
	return out
}

// settled waits until the fake server has sent n finals (over all its
// streams) and the archive holds want, then checks it STILL holds want a
// moment later (nothing else was stored or amended after it).
func (e *env) settled(c call, srv *stttest.Server, n int, want []string) {
	e.t.Helper()
	finals := func() int {
		k := 0
		for _, r := range srv.Records() {
			for _, m := range r.Sent {
				if strings.Contains(m, `"type":"final"`) {
					k++
				}
			}
		}
		return k
	}
	for deadline := time.Now().Add(15 * time.Second); !(finals() >= n && slices.Equal(e.archive(c), want)); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			var sent [][]string
			for _, r := range srv.Records() {
				sent = append(sent, r.Sent)
			}
			e.t.Fatalf("the archive holds %q, want %q; the server sent %q", e.archive(c), want, sent)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := e.archive(c); !slices.Equal(got, want) {
		e.t.Fatalf("the archive moved on to %q, want %q", got, want)
	}
}

// 🔴 The measured case: "Can everyone hear me clearly" is finalised at the
// speaker's pause, and its "?" arrives as the next final's lead ({"text":
// "Alice will", "lead": "?"}). Line 1 ends with "?" and line 2 is "Alice
// will": in the store, on the owner's AND an admitted guest's panel (the
// line they were already shown is amended in place, by an `amend` event, not
// sent as a new line), on a panel opened afterwards, in the archive and in
// the text export.
func TestALateClosingMarkAmendsThePreviousLineEverywhere(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"Can everyone hear me clearly?", "Alice will"} })
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	ticket, _ := e.admitGuest(c, grace)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	guest := e.guestEvents(c.Code, ticket)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")

	want := []string{"Ada L.: Can everyone hear me clearly?", "Ada L.: Alice will"}
	for name, s := range map[string]*stream{"the owner's": owner, "the guest's": guest} {
		var p panel
		p.read(t, s, func(p *panel) bool { return slices.Equal(p.lines(), want) })
		// Line 1 was shown WITHOUT its mark (its final came first), then
		// amended in place: one amend, for that line, with the whole text.
		if len(p.amends) != 1 || p.amends[0].ID != p.order[0] || p.amends[0].Text != "Can everyone hear me clearly?" {
			t.Fatalf("%s panel: amends %+v, want one for line %d", name, p.amends, p.order[0])
		}
	}
	e.settled(c, srv, 2, want)
	// A panel opened now is sent line 1 with its mark, and no amend.
	late := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	var p panel
	p.read(t, late, func(p *panel) bool { return len(p.order) == 2 })
	if !slices.Equal(p.lines(), want) || len(p.amends) != 0 {
		t.Fatalf("a panel opened after the amend: %q, amends %+v", p.lines(), p.amends)
	}
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript?format=txt", c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "] «Ada L.»: Can everyone hear me clearly?\n") ||
		!strings.Contains(string(raw), "] «Ada L.»: Alice will\n") {
		t.Fatalf("the txt export = %d %s", resp.StatusCode, raw)
	}
}

// A final that carries only a lead ({"text": "", "lead": "?"}: the speaker
// said nothing more, the mark was decoded at the next flush) amends the
// previous line and stores NO line of its own.
func TestALeadOnlyFinalAmendsAndStoresNoLine(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"Can everyone hear me clearly?", ""} })
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	e.settled(c, srv, 2, []string{"Ada L.: Can everyone hear me clearly?"})
	var n int
	e.dbQuery(`SELECT COUNT(*) FROM segments`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d segments stored, want 1 (a lead-only final stores none)", n)
	}
}

// A participant's first line has no previous line: its leading marks are
// dropped, never stored as the start of the line. (The segment began
// mid-sentence: the fake server sends {"text": "Alice will", "lead": "?"}.)
func TestMarksWithNoPreviousLineAreDropped(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"? Alice will"} })
	e.lk.EgressAudio = func(room, identity, track string) []byte { return egresstest.Script(1000, 1000) }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	e.settled(c, srv, 1, []string{"Ada L.: Alice will"})
}

// 🔴 A lead belongs to ITS participant's previous line. The guest speaks
// first ("Hello from the guest", the meeting's latest line); then the
// owner's first final opens with "?". The owner has no previous line, so the
// mark is dropped: the guest's line is never touched.
func TestALeadNeverTouchesAnotherParticipantsLine(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"Hello from the guest"} })
	e.lk.EgressAudio = func(room, identity, track string) []byte { return egresstest.Script(1000, 1000) }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	_, gid := e.admitGuest(c, grace)
	e.publish(c, gid, "MICROPHONE")
	e.settled(c, srv, 1, []string{grace + " (guest): Hello from the guest"})
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"? Alice will"} })
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	e.settled(c, srv, 2, []string{grace + " (guest): Hello from the guest", "Ada L.: Alice will"})
}

// 🔴 Idempotent: a previous line that already ends with a closing mark gets
// no second one (never "??" or ".?"). The owner's line comes from the
// fallback ("The fallback heard this.", already punctuated); when Nemotron
// takes the track again, its first final opens with "?".
func TestALineThatAlreadyEndsWithAMarkGetsNoSecond(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Refuse, s.RetryAfter, s.Lines = 503, 1, []string{"? Alice will"} })
	e.lk.EgressAudio = func(room, identity, track string) []byte {
		ms := make([]int, 30)
		for i := range ms {
			ms[i] = 1000
		}
		return egresstest.Script(1000, ms...)
	}
	e.lk.EgressPace = 2 * time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	if g := segmentsOf(t, owner, 1)[0]; g.Text != "The fallback heard this." {
		t.Fatalf("the fallback's line: %+v", g)
	}
	srv.Set(func(s *stttest.Server) { s.Refuse = 0 })
	e.clock.Add(30 * time.Second)
	// The first Nemotron line, and everything stored before it.
	var p panel
	p.read(t, owner, func(p *panel) bool {
		return slices.ContainsFunc(p.order, func(id int64) bool { return strings.HasPrefix(p.text[id], "Alice will") })
	})
	got := e.archive(c)
	first := slices.IndexFunc(got, func(l string) bool { return strings.HasPrefix(l, "Ada L.: Alice will") })
	if first < 1 {
		t.Fatalf("the archive: %q", got)
	}
	for _, l := range got[:first] {
		if l != "Ada L.: The fallback heard this." {
			t.Fatalf("a fallback line became %q (the archive: %q)", l, got)
		}
	}
	for _, a := range p.amends {
		if !strings.HasPrefix(a.Text, "Alice will") {
			t.Fatalf("an amend of a line that already ended with a mark: %+v", a)
		}
	}
}

// 🔴 The amend log (what the panels have yet to be sent) is freed however a
// meeting ends, not only by End: by LiveKit's room_finished and by everyone
// leaving (track_unpublished, then room_finished, which closes no stream).
// And a late mark after the end (the end's final, {"text":"","lead":"."})
// still amends the store, which the archive reads, without a new log entry.
func TestEveryEndingFreesTheAmendLog(t *testing.T) {
	server.SetAmendLogGraceForTest(t, 100*time.Millisecond)
	for _, how := range []string{"End", "room_finished", "everyone left, then room_finished"} {
		t.Run(how, func(t *testing.T) {
			e, srv := newNemEnv(t)
			srv.Set(func(s *stttest.Server) { s.Lines = []string{"Can everyone hear me clearly?", "Alice will."} })
			e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
			e.lk.EgressPace = time.Millisecond
			c := e.meeting(ada)
			e.started(c)
			owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
			tr := e.publish(c, store.OwnerIdentity, "MICROPHONE")
			var p panel
			p.read(t, owner, func(p *panel) bool { return len(p.amends) == 1 })
			if n := e.server.AmendLogsForTest(); n != 1 {
				t.Fatalf("%d amend logs while the meeting runs, want 1 (the positive control)", n)
			}
			switch how {
			case "End":
				if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
					t.Fatalf("end = %d %s", resp.StatusCode, raw)
				}
			case "room_finished":
				if st, _ := e.lk.Webhook(e.internal.URL+"/livekit/webhook", "room_finished", c.Room, ""); st != 200 {
					t.Fatal(st)
				}
			default:
				if st, err := e.lk.Unpublish(c.Room, store.OwnerIdentity, tr); err != nil || st != 200 {
					t.Fatalf("track_unpublished = %d, %v", st, err)
				}
				eventually(t, 10*time.Second, "the log freed after the last stream", func() bool { return e.server.AmendLogsForTest() == 0 })
				if st, _ := e.lk.Webhook(e.internal.URL+"/livekit/webhook", "room_finished", c.Room, ""); st != 200 {
					t.Fatal(st)
				}
			}
			// The end's final carries line 2's "." into the store.
			eventually(t, 10*time.Second, "the end's late mark in the archive", func() bool {
				return slices.Equal(e.archive(c), []string{"Ada L.: Can everyone hear me clearly?", "Ada L.: Alice will."})
			})
			eventually(t, 10*time.Second, "no amend log left", func() bool { return e.server.AmendLogsForTest() == 0 })
			time.Sleep(300 * time.Millisecond)
			if n := e.server.AmendLogsForTest(); n != 0 {
				t.Fatalf("%d amend logs after the meeting ended (%s), want 0", n, how)
			}
		})
	}
}

// 🔴 One log line per final that carries a lead, with ids and times only:
// the meeting, the track, the marks, the final's meeting time, and the
// segment it amended (or why it was dropped). It is what shows, on the live
// path, which line a late mark went to. The deployment may hold health
// information, so NO word of the final or of the amended line may reach the
// log: the script's words are distinctive, and the whole log is searched
// for each (the archive holding them is the control).
func TestEachLeadIsLoggedWithIdsAndTimesAndNoText(t *testing.T) {
	words := []string{"Zanzibar", "quokkas", "yodel", "loudly", "Xylophone", "marmalade"}
	leadLine := regexp.MustCompile(`(?m)^transcript: lead (.*)$`)
	logged := regexp.MustCompile(`^"([^"]*)" meeting (\S+) track (\S+) final (\d+)-(\d+) ms: (.*)$`)
	for _, c := range []struct {
		name  string
		lines []string
		want  []string // the archive
		marks string
		what  func(e *env) string
	}{
		{"amended", []string{"Zanzibar quokkas yodel loudly?", "Xylophone marmalade"},
			[]string{"Ada L.: Zanzibar quokkas yodel loudly?", "Ada L.: Xylophone marmalade"}, "?",
			func(e *env) string {
				var id int64
				e.dbQuery(`SELECT MIN(id) FROM segments`).Scan(&id)
				return fmt.Sprintf("amended segment %d", id)
			}},
		{"dropped", []string{"? Xylophone marmalade"}, []string{"Ada L.: Xylophone marmalade"}, "?",
			func(*env) string { return "dropped: " + string(store.AmendNoLine) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, srv := newNemEnv(t)
			srv.Set(func(s *stttest.Server) { s.Lines = c.lines })
			e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
			if c.name == "dropped" {
				e.lk.EgressAudio = func(room, identity, track string) []byte { return egresstest.Script(1000, 1000) }
			}
			e.lk.EgressPace = time.Millisecond
			m := e.meeting(ada)
			e.started(m)
			tr := e.publish(m, store.OwnerIdentity, "MICROPHONE")
			e.settled(m, srv, len(c.lines), c.want)
			// The control: the words are in the system (the archive).
			if a := strings.Join(e.archive(m), " "); !strings.Contains(a, "Xylophone") {
				t.Fatalf("the archive %q does not hold the script's words", a)
			}
			log := e.log.String()
			for _, w := range words {
				if strings.Contains(strings.ToLower(log), strings.ToLower(w)) {
					t.Errorf("the log holds %q, a word of the transcript: %q", w, log)
				}
			}
			got := leadLine.FindAllStringSubmatch(log, -1)
			if len(got) != 1 {
				t.Fatalf("%d lead log lines, want 1 (one final carried a lead): %q", len(got), log)
			}
			f := logged.FindStringSubmatch(got[0][1])
			if f == nil {
				t.Fatalf("the lead line %q is not `\"marks\" meeting <id> track <id> final <start>-<end> ms: <what>`", got[0][0])
			}
			// The final's own times are those of the line it stored (the
			// meeting's latest segment).
			var start, end int64
			e.dbQuery(`SELECT start_ms, end_ms FROM segments ORDER BY id DESC LIMIT 1`).Scan(&start, &end)
			want := []string{c.marks, m.ID, tr, strconv.FormatInt(start, 10), strconv.FormatInt(end, 10), c.what(e)}
			if !slices.Equal(f[1:], want) {
				t.Fatalf("the lead line %q: fields %q, want %q", got[0][0], f[1:], want)
			}
		})
	}
}
