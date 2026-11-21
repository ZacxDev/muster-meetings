package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
	"github.com/ZacxDev/muster-meetings/internal/stttest"
)

// The phase-3c tests through the server: the nemotron engine against the
// fake Nemotron server (internal/stttest), its fallback the utterance
// engine against a fake STT service, the fake LiveKit as Egress.

const streamToken = "env-stream-tok-5e1"

// fixedSTT is a fallback STT service that answers one line.
type fixedSTT string

func (f fixedSTT) Transcribe(ctx context.Context, pcm []int16) (string, error) { return string(f), nil }

// newNemEnv is an env whose engine is nemotron against a fresh fake server.
func newNemEnv(t *testing.T) (*env, *stttest.Server) {
	t.Helper()
	srv := stttest.New(streamToken)
	t.Cleanup(srv.Close)
	var e *env
	e = newEnvOpts(t, nil, func(c *server.Config) {
		eng := stt.NewNemotron(srv.URL(), streamToken, stt.NewUtterance(fixedSTT("The fallback heard this.")), c.Log)
		eng.Now = c.Now
		c.Engine = eng
	})
	return e, srv
}

type statusEvent struct {
	Status string `json:"status"`
}

type partialsEvent struct {
	Partials []struct {
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	} `json:"partials"`
}

// 🔴 Partials are live captions: they reach the panels (a "partials"
// event, the speaker's line in progress) and are NEVER stored; finals are
// stored, under the nemotron engine's name. And with Nemotron taking every
// participant, the status never says "Captions delayed".
func TestNemotronPartialsAreCaptionsNeverStoredAndFinalsAre(t *testing.T) {
	e, srv := newNemEnv(t)
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")

	var captions []string
	var statuses []string
	var stored []segEvent
	for len(stored) < 2 {
		ev, ok := owner.next(10 * time.Second)
		if !ok {
			t.Fatalf("the panel stopped short: captions %q, lines %+v (stream: %q)", captions, stored, owner.text())
		}
		switch ev.Name {
		case "partials":
			var p partialsEvent
			json.Unmarshal([]byte(ev.Data), &p)
			for _, x := range p.Partials {
				if x.Speaker != "Ada L." {
					t.Fatalf("a caption under %q", x.Speaker)
				}
				captions = append(captions, x.Text)
			}
		case "transcript":
			var s statusEvent
			json.Unmarshal([]byte(ev.Data), &s)
			statuses = append(statuses, s.Status)
		case "segment":
			var g segEvent
			json.Unmarshal([]byte(ev.Data), &g)
			if g.Kind == "text" {
				stored = append(stored, g)
			}
		}
	}
	if len(captions) == 0 || !strings.HasPrefix(stttest.DefaultLines[0], captions[0]) {
		t.Fatalf("captions %q, want the first line as it grows", captions)
	}
	if got := lines(stored); !slices.Equal(got, []string{"Ada L.: " + stttest.DefaultLines[0], "Ada L.: " + stttest.DefaultLines[1]}) {
		t.Fatalf("the panel's lines: %q", got)
	}
	for _, s := range statuses {
		if s != "" {
			t.Fatalf("the status said %q while Nemotron took the audio", s)
		}
	}
	// The archive: the finals and nothing else; each stored as nemotron's.
	resp, raw := e.do("GET", "/api/meetings/"+c.ID+"/transcript", c.Sess, nil)
	var tr struct {
		Segments []segEvent `json:"segments"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &tr) != nil {
		t.Fatalf("transcript = %d %s", resp.StatusCode, raw)
	}
	if got := lines(tr.Segments); !slices.Equal(got, []string{"Ada L.: " + stttest.DefaultLines[0], "Ada L.: " + stttest.DefaultLines[1]}) {
		t.Fatalf("the archive holds %q, want the two finals only (a partial is never stored)", got)
	}
	var n int
	e.dbQuery(`SELECT COUNT(*) FROM segments WHERE engine = 'nemotron'`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d segments stored as nemotron's, want 2", n)
	}
	if srv.Upgrades() != 1 || srv.Records()[0].Auth != "Bearer "+streamToken {
		t.Fatalf("upgrades %d", srv.Upgrades())
	}
	if strings.Contains(e.log.String(), streamToken) {
		t.Fatal("the log carries the stream token")
	}
}

// 🔴 "Captions delayed" shows EXACTLY while a participant is on the
// fallback: the server full (503) sends the owner's track to the utterance
// engine, whose lines are stored as utterance's, and the panel says
// "Captions delayed"; when Nemotron takes the track again (after 30 s),
// it clears, and the next lines are Nemotron's.
func TestCaptionsDelayedShowsExactlyWhileOnTheFallback(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Refuse, s.RetryAfter = 503, 1 })
	// 30 utterances (1 s of speech, 1 s of silence), at 2 ms per 10 ms.
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

	wantStatus := func(want string) {
		t.Helper()
		for {
			ev := owner.want("transcript")
			var s statusEvent
			json.Unmarshal([]byte(ev.Data), &s)
			if s.Status == want {
				return
			}
		}
	}
	wantStatus(server.CaptionsDelayed)
	g := segmentsOf(t, owner, 1)[0]
	if g.Text != "The fallback heard this." {
		t.Fatalf("the fallback's line: %+v", g)
	}
	var eng string
	e.dbQuery(`SELECT engine FROM segments WHERE id = ?`, g.ID).Scan(&eng)
	if eng != "utterance" {
		t.Fatalf("the fallback's line is stored as %q's, want utterance's", eng)
	}
	// The server has room again; Nemotron is retried 30 s on (Retry-After
	// 1 s: the floor is 30 s), with audio still flowing.
	srv.Set(func(s *stttest.Server) { s.Refuse = 0 })
	e.clock.Add(30 * time.Second)
	wantStatus("")
	for {
		g := segmentsOf(t, owner, 1)[0]
		if g.Text != "The fallback heard this." {
			if !slices.Contains(stttest.DefaultLines, g.Text) {
				t.Fatalf("after the return: %+v", g)
			}
			break
		}
	}
	if srv.Upgrades() != 1 {
		t.Fatalf("upgrades %d, want 1 (the return)", srv.Upgrades())
	}
}

// The silent-start flag is telemetry: a log line naming the track (never a
// speaker or text) and /healthz's warning; nothing is stored or shown.
func TestASilentStartFlagIsLoggedAndCountedNotStored(t *testing.T) {
	e, srv := newNemEnv(t)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{""} })
	e.lk.EgressAudio = func(room, identity, track string) []byte { return egresstest.Speech(21000) }
	e.lk.EgressPace = time.Millisecond / 4
	c := e.meeting(ada)
	e.started(c)
	tr := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	eventually(t, 15*time.Second, "the silent-start flag", func() bool { return e.server.SilentStartFlags() == 1 })
	if !strings.Contains(e.log.String(), "track "+tr+" flagged silent_start") {
		t.Fatalf("the log: %s", e.log.String())
	}
	resp, err := http.Get(e.internal.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "1 silent-start flag") {
		t.Fatalf("/healthz = %d %s", resp.StatusCode, raw)
	}
	var n int
	e.dbQuery(`SELECT COUNT(*) FROM segments`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d segments stored, want none", n)
	}
}
