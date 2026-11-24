package server_test

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// waits records every wait the egress start and restart loops ask the
// clock for, and lets each pass at once.
type waits struct {
	mu sync.Mutex
	ds []time.Duration
}

func (w *waits) after(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	w.ds = append(w.ds, d)
	w.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

func (w *waits) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ds)
}

// 🔴 An egress that keeps dying is restarted on the backoff (here 20, 40,
// 80 ms, the last repeated), not in a tight loop; a stream that carried audio
// for less than HealthyStream does not reset the count; one that carried it
// for HealthyStream (the env's clock) does.
func TestADyingEgressIsRestartedOnTheBackoffAndAHealthyStreamResetsIt(t *testing.T) {
	w := &waits{}
	e := newEnvOpts(t, nil, func(c *server.Config) { c.After = w.after })
	c := e.meeting(ada)
	e.started(c)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	eg := e.egressOf(a, 1)
	kill := func(n int) {
		t.Helper()
		e.lk.KillEgress(eg.ID)
		eg = e.egressOf(a, n)
	}
	// audio streams on the current egress until its gap has closed.
	audio := func() {
		t.Helper()
		_, cl := dialStatus(t, eg.URL)
		if err := cl.Stream(egresstest.Silence(100), 0); err != nil {
			t.Fatal(err)
		}
		eventually(t, 5*time.Second, "the gap closes as audio flows", func() bool {
			var open bool
			return e.dbQuery(`SELECT gap_segment IS NOT NULL FROM tracks WHERE track = ?`, a).Scan(&open) == nil && !open
		})
	}
	for n := 2; n <= 5; n++ { // four deaths in a row, no audio between
		kill(n)
	}
	audio()
	e.clock.Add(30 * time.Second) // healthy, but not for long enough
	kill(6)
	audio()
	e.clock.Add(server.HealthyStream) // healthy for the whole stretch
	kill(7)
	ms := time.Millisecond
	want := []time.Duration{20 * ms, 40 * ms, 80 * ms, 80 * ms, 80 * ms, 20 * ms}
	if got := w.got(); !slices.Equal(got, want) {
		t.Fatalf("the restarts waited %v, want %v", got, want)
	}
	if n := e.lk.CountCalls("StartTrackEgress"); n != 7 {
		t.Fatalf("%d starts, want 7 (one per egress)", n)
	}
}

// 🔴 The notice says "transcribed" (and names the AI service) only while
// the plugin has an engine, and a knock must carry the same answer: another,
// or none, is the 409 with the current notice.
func TestTheNoticeSaysTranscribedOnlyWithAnEngine(t *testing.T) {
	type knockAns struct {
		Notice struct {
			Transcribed bool     `json:"transcribed"`
			Text        []string `json:"text"`
			Badge       string   `json:"badge"`
		} `json:"notice"`
	}
	for _, on := range []bool{true, false} {
		// A plugin that records, so the notice's recording clause is there.
		e, _ := recEnv(t, func(c *server.Config) {
			if !on {
				c.Engine = nil
			}
		})
		c := e.meeting(ada)
		_, page := e.do("GET", "/j/"+c.Code, "", nil)
		saysTranscribed := strings.Contains(string(page), "transcribed.")
		if saysTranscribed != on || strings.Contains(string(page), "OpenRouter") != on ||
			!strings.Contains(string(page), `data-transcribed="`+map[bool]string{true: "true", false: "false"}[on]+`"`) {
			t.Fatalf("engine %v: the guest page says transcribed %v, names the AI service %v:\n%s", on, saysTranscribed, strings.Contains(string(page), "OpenRouter"), page)
		}
		owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
		var snap struct {
			Notice struct {
				Badge string `json:"badge"`
			} `json:"notice"`
		}
		json.Unmarshal([]byte(owner.want("meeting").Data), &snap)
		wantBadge := map[bool]string{true: "Recording · Transcribing · AI", false: "Recording"}[on]
		if snap.Notice.Badge != wantBadge {
			t.Fatalf("engine %v: the owner's badge = %q, want %q", on, snap.Notice.Badge, wantBadge)
		}
		for _, n := range []map[string]any{
			{"record": true, "ai": true, "transcribed": !on},
			{"record": true, "ai": true}, // a page that never said
		} {
			resp, raw := e.do("POST", "/j/"+c.Code+"/knock", "", map[string]any{"name": grace, "notice": n}, "X-Forwarded-For", "198.51.100.7")
			var b knockAns
			json.Unmarshal(raw, &b)
			if resp.StatusCode != 409 || b.Notice.Transcribed != on || b.Notice.Badge != wantBadge {
				t.Fatalf("engine %v: a knock with notice %v = %d %s; want 409 with the current notice", on, n, resp.StatusCode, raw)
			}
		}
		if resp, raw := e.do("POST", "/j/"+c.Code+"/knock", "", map[string]any{"name": grace,
			"notice": map[string]any{"record": true, "ai": true, "transcribed": on}}, "X-Forwarded-For", "198.51.100.7"); resp.StatusCode != 201 {
			t.Fatalf("engine %v: the knock with the current notice = %d %s, want 201", on, resp.StatusCode, raw)
		}
	}
}

// 🔴 A start that LiveKit refuses can still have started an Egress (LiveKit
// times the start out; the Egress connects anyway). Once a stream for the
// track is connected the plugin makes no further attempt: a new attempt's
// nonce would cut the live stream off (CI saw exactly that against a real
// LiveKit). And when that stream, whose egress the plugin never learned,
// ends while the track is published, the plugin starts again.
func TestAConnectedStreamStopsTheStartRetries(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.lk.RefuseEgress(1 << 20) // LiveKit refuses every start, as with no Egress attached
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	eventually(t, 5*time.Second, "starts are refused and retried", func() bool { return e.lk.CountCalls("StartTrackEgress") >= 2 })
	var meetingCreated int64
	e.dbQuery(`SELECT created_at FROM meetings WHERE id = ?`, c.ID).Scan(&meetingCreated)
	// The Egress of some attempt connects with that attempt's token: try the
	// current nonce until the ingest takes it.
	var cl *egresstest.Client
	eventually(t, 5*time.Second, "an Egress connects with a current attempt's token", func() bool {
		var nonce string
		e.dbQuery(`SELECT nonce FROM tracks WHERE track = ?`, a).Scan(&nonce)
		tok := ingest.Token(ingestKey, c.ID, a, nonce, time.UnixMilli(meetingCreated).Add(store.LinkLifetime))
		code, got := dialStatus(t, "ws"+strings.TrimPrefix(e.ingest.URL, "http")+ingest.Path(a, tok))
		cl = got
		return code == 101
	})
	if err := cl.Stream(egresstest.Silence(100), 0); err != nil {
		t.Fatal(err)
	}
	// No more attempts once it is connected (one may have been in flight).
	time.Sleep(100 * time.Millisecond)
	n := e.lk.CountCalls("StartTrackEgress")
	// Well past the backoff (20, 40, 80 ms), audio still flowing.
	for i := 0; i < 10; i++ {
		if err := cl.Stream(egresstest.Silence(50), 0); err != nil {
			t.Fatalf("the connected stream was cut off: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cl.Stream(speech, 0); err != nil {
		t.Fatalf("the connected stream was cut off: %v", err)
	}
	if ls := segmentsOf(t, owner, 1); ls[0].Text != fakeScript[0] {
		t.Fatalf("the line = %+v", ls[0])
	}
	if m := e.lk.CountCalls("StartTrackEgress"); m != n {
		t.Fatalf("%d more starts while a stream was connected, want none", m-n)
	}
	// The stream (of an egress the plugin never learned) ends: a new start.
	cl.Drop()
	eventually(t, 5*time.Second, "a new start once the unknown egress's stream ended", func() bool {
		return e.lk.CountCalls("StartTrackEgress") > n
	})
}
