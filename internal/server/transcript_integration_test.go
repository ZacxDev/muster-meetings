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
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
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
	e := newEnvWith(t, &liveKitAt{API: api, WS: ws, Keys: keys, RealClock: true})

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
	// With no Egress attached LiveKit refuses the start (measured: it waits
	// 7 s for an Egress, then answers 408 "canceled"): the plugin says so,
	// retries, and the transcript shows a gap.
	eventually(t, 25*time.Second, "a refused StartTrackEgress is retried", func() bool {
		return strings.Count(e.log.String(), "transcript: starting a track egress failed") >= 2
	})
	gap := owner.want("segment")
	var g segEvent
	json.Unmarshal([]byte(gap.Data), &g)
	if g.Kind != "gap" || g.EndMS != nil || g.Speaker != "Ada L." {
		t.Fatalf("the first segment = %s, want an open gap for Ada L.", gap.Data)
	}

	// The test is Egress now: the token of the plugin's CURRENT attempt.
	var meetingExp int64
	e.dbQuery(`SELECT nonce FROM tracks WHERE track = ?`, track).Scan(&nonce)
	e.dbQuery(`SELECT created_at FROM meetings WHERE id = ?`, c.ID).Scan(&meetingExp)
	tok := ingest.Token(ingestKey, c.ID, track, nonce, time.UnixMilli(meetingExp).Add(store.LinkLifetime))
	code, cl := dialStatus(t, "ws"+strings.TrimPrefix(e.ingest.URL, "http")+ingest.Path(track, tok))
	if code != 101 {
		// A retry may have drawn a new nonce in between: once more.
		e.dbQuery(`SELECT nonce FROM tracks WHERE track = ?`, track).Scan(&nonce)
		tok = ingest.Token(ingestKey, c.ID, track, nonce, time.UnixMilli(meetingExp).Add(store.LinkLifetime))
		if code, cl = dialStatus(t, "ws"+strings.TrimPrefix(e.ingest.URL, "http")+ingest.Path(track, tok)); code != 101 {
			t.Fatalf("the ingest refused the current attempt's token: %d", code)
		}
	}
	if err := cl.Stream(egresstest.Script(1000, 1000), 0); err != nil {
		t.Fatal(err)
	}
	ls := segmentsOf(t, owner, 1)
	if ls[0].Speaker != "Ada L." || ls[0].Text != fakeScript[0] {
		t.Fatalf("the line = %+v", ls[0])
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
