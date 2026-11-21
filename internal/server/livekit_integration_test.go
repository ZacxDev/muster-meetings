package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
)

// The integration test runs the plugin against a REAL livekit-server (design:
// "Plugin integration tests"), with LiveKit's own CLI (`lk room join`) as the
// participant: no browser, no media, a real signalling connection to the
// real SFU. It finds the binaries in MEETINGS_LIVEKIT_SERVER and
// MEETINGS_LK_CLI, or on PATH (`nix-shell -p livekit livekit-cli` provides
// both). Without them it skips, saying so, unless MEETINGS_REQUIRE_LIVEKIT=1
// (CI sets it), when it fails: CI must never pass it by skipping.
func liveKitBinaries(t *testing.T) (server, cli string) {
	t.Helper()
	find := func(env, name string) string {
		if p := os.Getenv(env); p != "" {
			return p
		}
		p, _ := exec.LookPath(name)
		return p
	}
	server, cli = find("MEETINGS_LIVEKIT_SERVER", "livekit-server"), find("MEETINGS_LK_CLI", "lk")
	if server == "" || cli == "" {
		msg := fmt.Sprintf("livekit-server (%q) or lk (%q) not found: set MEETINGS_LIVEKIT_SERVER and MEETINGS_LK_CLI, or run under `nix-shell -p livekit livekit-cli`", server, cli)
		if os.Getenv("MEETINGS_REQUIRE_LIVEKIT") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	return server, cli
}

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// syncBuffer collects a process's output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// started kills a process this test started, by its own handle, when the
// test ends.
func started(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
}

func eventually(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAgainstARealLiveKitServer(t *testing.T) {
	serverBin, cliBin := liveKitBinaries(t)
	keys := livekit.Keys{Key: "APIintegration", Secret: "an-integration-secret-of-32-chars-plus"}
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
			t.Logf("livekit-server's log:\n%s", lkLog.String())
		}
	})
	lk, err := livekit.NewClient(api, keys)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inRoom := func(room string) []string {
		ps, err := lk.ListParticipants(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range ps {
			ids = append(ids, p.Identity)
		}
		return ids
	}
	// join connects identity to room with LiveKit's CLI: a real participant.
	// The CLI mints its own token with the API secret, so this is a client
	// holding a valid credential, whatever the plugin's state says.
	join := func(room, identity string) *syncBuffer {
		out := &syncBuffer{}
		cmd := exec.Command(cliBin, "room", "join", "--identity", identity, room)
		cmd.Env = append(os.Environ(), "LIVEKIT_URL="+ws, "LIVEKIT_API_KEY="+keys.Key, "LIVEKIT_API_SECRET="+keys.Secret, "LK_NO_SKILLS_PROMPT=1")
		cmd.Stdout, cmd.Stderr = out, out
		started(t, cmd)
		go cmd.Wait()
		return out
	}

	// New meeting makes the room in the real server, with the design's settings.
	c := e.meeting(ada)
	rooms, err := lk.ListRooms(ctx)
	if err != nil || !slices.ContainsFunc(rooms, func(r livekit.Room) bool { return r.Name == c.Room }) {
		t.Fatalf("the real server's rooms = %+v, %v; want %s", rooms, err, c.Room)
	}

	// The tokens the plugin mints are ones the real server accepts, and one
	// signed with another secret is refused (the control).
	_, otRaw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil)
	var ot map[string]string
	json.Unmarshal(otRaw, &ot)
	gt := e.knock(c.Code, grace)
	e.admit(c, grace)
	gtok := admittedToken(t, e.wait(c.Code, gt))
	bad, _ := livekit.Keys{Key: keys.Key, Secret: "another-secret-entirely-0000000000"}.Mint(livekit.Participant{Identity: "guest-x", Name: "x"}, livekit.GuestGrant(c.Room), time.Now())
	for name, tc := range map[string]struct {
		tok  string
		want int
	}{"the owner's": {ot["token"], 200}, "a guest's": {gtok["token"], 200}, "another secret's": {bad, 401}} {
		resp, err := http.Get(api + "/rtc/validate?access_token=" + tc.tok)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("the real server's /rtc/validate of %s token = %d %s, want %d", name, resp.StatusCode, body, tc.want)
		}
	}

	// Two admitted guests join: the real server's participant_joined webhooks
	// arrive, verify, and remove neither (the positive control).
	lt := e.knock(c.Code, linus)
	e.admit(c, linus)
	ltok := admittedToken(t, e.wait(c.Code, lt))
	gid, lid := gtok["identity"], ltok["identity"]
	join(c.Room, gid)
	linusOut := join(c.Room, lid)
	eventually(t, 10*time.Second, "both admitted guests are in the room", func() bool {
		in := inRoom(c.Room)
		return slices.Contains(in, gid) && slices.Contains(in, lid)
	})
	time.Sleep(time.Second)
	if in := inRoom(c.Room); !slices.Contains(in, gid) || !slices.Contains(in, lid) {
		t.Fatalf("an admitted guest was removed: the room holds %v", in)
	}

	// The owner removes Grace: she is dropped from the real room.
	resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/participants/"+e.pidAny(c, grace), c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"disconnected":true`) {
		t.Fatalf("remove = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 5*time.Second, "the removed guest leaves the real room", func() bool { return !slices.Contains(inRoom(c.Room), gid) })

	// She comes back with a valid credential: the real server lets her in,
	// fires participant_joined, and the plugin throws her out at once.
	rejoin := join(c.Room, gid)
	eventually(t, 10*time.Second, "the rejoining removed guest is disconnected by the plugin (PARTICIPANT_REMOVED)", func() bool {
		return strings.Contains(rejoin.String(), "PARTICIPANT_REMOVED")
	})
	// And an identity the plugin never admitted, the same.
	stranger := join(c.Room, "guest-Stranger0000000000000")
	eventually(t, 10*time.Second, "an identity never admitted is disconnected by the plugin", func() bool {
		return strings.Contains(stranger.String(), "PARTICIPANT_REMOVED")
	})
	if in := inRoom(c.Room); !slices.Contains(in, lid) || slices.Contains(in, gid) {
		t.Fatalf("after the rejoin the room holds %v: want Linus and not Grace", in)
	}
	if strings.Contains(linusOut.String(), "PARTICIPANT_REMOVED") {
		t.Fatal("Linus, admitted, was removed")
	}

	// End meeting deletes the real room, which disconnects Linus.
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	eventually(t, 10*time.Second, "End meeting deletes the real room", func() bool {
		rooms, _ := lk.ListRooms(ctx)
		return !slices.ContainsFunc(rooms, func(r livekit.Room) bool { return r.Name == c.Room })
	})
	eventually(t, 10*time.Second, "Linus is disconnected when the room is deleted", func() bool {
		return strings.Contains(linusOut.String(), "disconnected from room")
	})

	// LiveKit closes a room on its own (here: deleted outside the plugin, as
	// its empty timeout would): the real room_finished webhook ends the
	// meeting and kills the link.
	c2 := e.meeting(bea)
	if err := lk.DeleteRoom(ctx, c2.Room); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the real room_finished webhook ends the meeting", func() bool {
		return e.meetingState(c2.Subject, c2.ID) == "ended"
	})
	if resp, _ := e.do("GET", "/j/"+c2.Code, "", nil); resp.StatusCode != 410 {
		t.Errorf("the link after room_finished = %d, want 410", resp.StatusCode)
	}
	if strings.Contains(e.log.String(), "webhook: refused") {
		t.Errorf("the plugin refused a webhook from the real server:\n%s", e.log.String())
	}
}
