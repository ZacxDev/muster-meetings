package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The transcript against a REAL livekit-server (design: "Plugin integration
// tests"), with LiveKit's CLI publishing a real Opus microphone track
// (testdata/tone.ogg: 40 s of a 300 Hz tone, 6 kb/s Opus; the CLI plays it
// once and then unpublishes it). What is real: the room, the track_published and
// track_unpublished webhooks as LiveKit sends them (their JSON is what the
// plugin must read: the track's id and source), ListParticipants' tracks,
// and LiveKit's answer to StartTrackEgress.
//
// 🔴 What is NOT real: Egress. LiveKit's Egress is not in nixpkgs, and it is
// a container (GStreamer and Chrome) this repository does not run in CI. So
// this server runs with no Egress attached, LiveKit refuses every
// StartTrackEgress, and the test checks the plugin retries with a gap
// marker; then the TEST plays Egress, dialling the ingest with the token
// the plugin's current egress attempt would have been given, and streaming
// Egress's PCM format (S2's measured one). The fake Egress elsewhere
// (internal/livekittest) does the same for the unit and e2e tests.
func TestTheTranscriptAgainstARealLiveKitServer(t *testing.T) {
	serverBin, cliBin := liveKitBinaries(t)
	keys := livekit.Keys{Key: "APItranscript", Secret: "a-transcript-integration-secret-0123456789"}
	port := freePort(t, "tcp")
	api, ws := "http://127.0.0.1:"+strconv.Itoa(port), "ws://127.0.0.1:"+strconv.Itoa(port)
	// 🔴 The loop's SECOND retry wait (after attempt 2's refusal) is held
	// until the test, playing Egress, has connected; every other wait is the
	// real one. See "The test is Egress now" below for why.
	held, release := gate(t)
	inWait := newSignal()
	var waits atomic.Int32
	e := newEnvOpts(t, &liveKitAt{API: api, WS: ws, Keys: keys, RealClock: true}, func(c *server.Config) {
		c.After = func(d time.Duration) <-chan time.Time {
			if waits.Add(1) != 2 {
				return time.After(d)
			}
			inWait.fire()
			ch := make(chan time.Time, 1)
			go func() { <-held; ch <- time.Now() }()
			return ch
		}
	})

	cfg := fmt.Sprintf(`port: %d
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: %d
  udp_port: %d
  use_external_ip: false
  node_ip: 127.0.0.1
keys:
  %s: %s
webhook:
  api_key: %s
  urls: ["%s/livekit/webhook"]
logging:
  level: info
`, port, freePort(t, "tcp"), freePort(t, "udp"), keys.Key, keys.Secret, keys.Key, e.internal.URL)
	cfgPath := filepath.Join(t.TempDir(), "livekit.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	lkLog := &syncBuffer{}
	srv := exec.Command(serverBin, "--config", cfgPath)
	srv.Stdout, srv.Stderr = lkLog, lkLog
	started(t, srv)
	eventually(t, 20*time.Second, "livekit-server answers", func() bool {
		resp, err := http.Get(api)
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("livekit-server's log:\n%s\nthe plugin's log:\n%s", lkLog.String(), e.log.String())
		}
	})

	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)

	// The owner's microphone, as a real client publishes it.
	tone, _ := filepath.Abs("testdata/tone.ogg")
	out := &syncBuffer{}
	pub := exec.Command(cliBin, "room", "join", "--identity", store.OwnerIdentity, "--publish", tone, c.Room)
	pub.Env = append(os.Environ(), "LIVEKIT_URL="+ws, "LIVEKIT_API_KEY="+keys.Key, "LIVEKIT_API_SECRET="+keys.Secret, "LK_NO_SKILLS_PROMPT=1")
	pub.Stdout, pub.Stderr = out, out
	started(t, pub)

	// The real track_published reached the plugin, which read it as the
	// owner's MICROPHONE and recorded the track under the owner's name.
	var track, speaker, state, nonce string
	eventually(t, 20*time.Second, "the real track_published is recorded as a microphone track", func() bool {
		return e.dbQuery(`SELECT track, speaker, state, nonce FROM tracks WHERE meeting_id = ?`, c.ID).Scan(&track, &speaker, &state, &nonce) == nil && nonce != ""
	})
	if !ingest.ValidTrack(track) || speaker != "Ada L." || state != store.TrackActive {
		t.Fatalf("the track row: %q %q %q, want a LiveKit track id, Ada L., active", track, speaker, state)
	}
	// With no Egress attached the start never completes: the plugin's own
	// HTTP client gives up after 10 s (internal/livekit/api.go), and
	// livekit-server then logs a 408 "request canceled" because the plugin
	// hung up, not because it refused. The plugin says so, retries, and the
	// transcript shows a gap.
	eventually(t, 25*time.Second, "a refused StartTrackEgress is retried", func() bool {
		return strings.Count(e.log.String(), "transcript: starting a track egress failed") >= 2
	})
	gap := owner.want("segment")
	var g segEvent
	json.Unmarshal([]byte(gap.Data), &g)
	if g.Kind != "gap" || g.EndMS != nil || g.Speaker != "Ada L." {
		t.Fatalf("the first segment = %s, want an open gap for Ada L.", gap.Data)
	}

	// The test is Egress now, for attempt 2, whose refusal the loop is
	// waiting out (held: it draws no new nonce meanwhile).
	//
	// 🔴 Why held. Unheld, the next attempt starts 40 ms after attempt 2's
	// refusal, and a stream dialled in that moment can pass the token check
	// on attempt 2's nonce, then register only after attempt 3 has checked
	// for a stream and drawn a new nonce: a superseded attempt's stream, which
	// the plugin cuts off by design (TestAStaleStreamDoesNotStopTheRetries;
	// in production attempt 3's own Egress takes over). Here nothing plays
	// attempt 3's Egress, so that cut was this test's "connection reset by
	// peer" (1 run in 20 on main, and once in CI).
	inWait.wait(t, "the loop waits out attempt 2's refusal")
	nonce = e.nonceOf(track)
	code, cl := dialStatus(t, e.ingestURL(c, track, nonce))
	if code != 101 {
		t.Fatalf("the ingest refused the current attempt's token: %d", code)
	}
	eventually(t, 5*time.Second, "the stream registers as the current attempt's", func() bool {
		return e.server.ConnNonceForTest(track) == nonce
	})
	fails := strings.Count(e.log.String(), "transcript: starting a track egress failed")
	release()
	// Its wait over, the loop sees a current stream and ends: no attempt
	// draws a new nonce (one would within milliseconds; 300 ms is ample).
	time.Sleep(300 * time.Millisecond)
	if n := e.nonceOf(track); n != nonce || e.server.ConnNonceForTest(track) != nonce {
		t.Fatalf("an attempt after the stream connected superseded it: nonce %q, stream %q, want both %q", n, e.server.ConnNonceForTest(track), nonce)
	}
	if err := cl.Stream(egresstest.Script(1000, 1000), 0); err != nil {
		t.Fatal(err)
	}
	ls := segmentsOf(t, owner, 1)
	if ls[0].Speaker != "Ada L." || ls[0].Text != fakeScript[0] {
		t.Fatalf("the line = %+v", ls[0])
	}
	// Invariant guard, not proof the loop ended: an attempt started at the
	// release would log its failure only ~10 s later, after this runs. The
	// proof is the nonce and registration check above.
	if now := strings.Count(e.log.String(), "transcript: starting a track egress failed"); now != fails {
		t.Fatalf("%d failed attempts logged, want %d", now, fails)
	}

	// The participant leaves: the real track_unpublished ends the track, and
	// no egress is started for it again.
	pub.Process.Kill()
	eventually(t, 30*time.Second, "the real track_unpublished (or the participant's departure) ends the track", func() bool {
		return e.dbQuery(`SELECT state FROM tracks WHERE track = ?`, track).Scan(&state) == nil && state == store.TrackEnded
	})
	if strings.Contains(e.log.String(), "webhook: refused") {
		t.Errorf("the plugin refused a webhook from the real server")
	}
}
