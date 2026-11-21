package ingest_test

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
)

// ---- the PCM: Egress's own frames (spike S2) ----

// egressFrames is a fixture as Egress sends it: messages of exactly 1920
// bytes (10 ms of S16LE stereo at 48 kHz), the two channels as given.
func egressFrames(left, right []int16) [][]byte {
	b := egresstest.StereoLR(left, right)
	var out [][]byte
	for i := 0; i < len(b); i += 1920 {
		out = append(out, b[i:i+1920])
	}
	return out
}

// sine48 is n samples of a sine at f Hz and amplitude a, at 48 kHz.
func sine48(n int, f, a float64) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(a * math.Sin(2*math.Pi*f*float64(i)/48000))
	}
	return s
}

// "Real bytes" (design: plugin unit tests): one second of Egress's frames
// (100 messages of 1920 bytes, identical channels) parses to 16 kHz mono:
// twelve chunks of EXACTLY 1280 samples (80 ms) and 640 left for the end,
// 16,000 samples in all, and the samples are the tone at 16 kHz (checked
// against the sine itself, not against the decoder's own arithmetic).
func TestEgressFramesParseTo16kHzMonoIn80msChunks(t *testing.T) {
	tone := sine48(48000, 300, 9000)
	frames := egressFrames(tone, tone)
	if len(frames) != 100 || len(frames[0]) != 1920 {
		t.Fatalf("fixture: %d messages of %d bytes, want 100 of 1920", len(frames), len(frames[0]))
	}
	d := ingest.NewDecoder()
	var got []int16
	for i, f := range frames {
		chunks, err := d.Push(f)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		for _, c := range chunks {
			if len(c) != 1280 {
				t.Fatalf("a chunk of %d samples, want exactly 1280 (80 ms at 16 kHz)", len(c))
			}
			got = append(got, c...)
		}
	}
	if len(got) != 12*1280 {
		t.Fatalf("%d samples in whole chunks, want %d", len(got), 12*1280)
	}
	tail := d.Flush()
	if len(tail) != 640 {
		t.Fatalf("the tail is %d samples, want 640", len(tail))
	}
	got = append(got, tail...)
	if d.Samples() != 16000 {
		t.Fatalf("Samples() = %d, want 16000 (one second at 16 kHz)", d.Samples())
	}
	// The 300 Hz tone at 16 kHz: y[n] = 9000 sin(2π·300·(3n − delay)/48000),
	// the filter's delay being (63−1)/2 input samples.
	worst := 0.0
	for n := 100; n < len(got); n++ {
		want := 9000 * math.Sin(2*math.Pi*300*(float64(3*n)-31)/48000)
		worst = max(worst, math.Abs(float64(got[n])-want))
	}
	if worst > 200 {
		t.Fatalf("the 16 kHz samples are off the tone by up to %.0f (of 9000), want under 200", worst)
	}
}

// A fixture whose channels differ is read from the FIRST channel only: the
// output is the left tone, and nothing of the right one.
func TestOnlyTheFirstChannelIsRead(t *testing.T) {
	left, right := sine48(48000, 300, 9000), sine48(48000, 1100, 9000)
	d := ingest.NewDecoder()
	var got []int16
	for _, f := range egressFrames(left, right) {
		cs, err := d.Push(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			got = append(got, c...)
		}
	}
	worst := 0.0
	for n := 100; n < len(got); n++ {
		want := 9000 * math.Sin(2*math.Pi*300*(float64(3*n)-31)/48000)
		worst = max(worst, math.Abs(float64(got[n])-want))
	}
	if worst > 200 {
		t.Fatalf("with different channels the output is off the left channel by up to %.0f: not the first channel alone", worst)
	}
}

// 🔴 Exact frame sizes: a message that is not a whole number of stereo
// samples (4 bytes) is refused and changes nothing, so every later sample
// is still read from the right byte.
func TestAMisalignedMessageIsRefusedAndChangesNothing(t *testing.T) {
	tone := sine48(480, 300, 9000)
	good := egresstest.StereoLR(tone, tone)
	for _, n := range []int{1921, 1919, 1918, 3, 2} {
		d := ingest.NewDecoder()
		bad := append(append([]byte(nil), good...), make([]byte, 4)...)[:n]
		if _, err := d.Push(bad); !errors.Is(err, ingest.ErrMisaligned) {
			t.Errorf("a %d-byte message: Push = %v, want ErrMisaligned", n, err)
		}
		if d.Samples() != 0 {
			t.Errorf("a refused %d-byte message produced %d samples", n, d.Samples())
		}
	}
	d := ingest.NewDecoder()
	for _, n := range []int{1920, 4, 0} {
		if _, err := d.Push(good[:n]); err != nil {
			t.Errorf("a %d-byte message: %v, want it read", n, err)
		}
	}
	if d.Samples() != 161 { // 480 input samples → 160, then 1 → 1 (a sample at phase 0)
		t.Errorf("Samples() = %d after 481 stereo samples, want 161", d.Samples())
	}
}

// ---- the token ----

var key = []byte("ingest fixture key 7c6b5a4938271605")

func TestTheTokenOpensOneEgressOfOneTrackOfOneMeetingUntilItsExpiry(t *testing.T) {
	now := time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
	exp := now.Add(time.Hour)
	nonce := ingest.NewNonce()
	tok := ingest.Token(key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, exp)
	p, err := ingest.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, now); err != nil {
		t.Fatalf("the right token: %v", err)
	}
	other := ingest.NewNonce()
	for name, c := range map[string]struct {
		key            []byte
		meeting, track string
		nonce          string
		now            time.Time
	}{
		"another track":          {key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_bea0002", nonce, now},
		"another meeting":        {key, "mtgBBBBBBBBBBBBBBBBBBB", "TR_ada0001", nonce, now},
		"a newer egress's nonce": {key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", other, now},
		"another key":            {[]byte("another key of thirty-two bytes!!"), "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, now},
		"at its expiry":          {key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, exp},
		"after its expiry":       {key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, exp.Add(time.Second)},
	} {
		if err := p.Check(c.key, c.meeting, c.track, c.nonce, c.now); !errors.Is(err, ingest.ErrToken) {
			t.Errorf("%s: Check = %v, want ErrToken", name, err)
		}
	}
	// A token whose expiry was moved, or whose MAC was changed, is refused.
	parts := strings.Split(tok, ".")
	later := strings.Join([]string{parts[0], "9999999999", parts[2]}, ".")
	if p2, err := ingest.Parse(later); err == nil {
		if err := p2.Check(key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, now); err == nil {
			t.Error("a token with its expiry moved later opened the track")
		}
	}
	mac, _ := base64.RawURLEncoding.DecodeString(parts[2])
	mac[0] ^= 1
	if p3, err := ingest.Parse(parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(mac)); err == nil {
		if err := p3.Check(key, "mtgAAAAAAAAAAAAAAAAAAA", "TR_ada0001", nonce, now); err == nil {
			t.Error("a token with one MAC bit flipped opened the track")
		}
	}
	for _, bad := range []string{"", "x", "a.b.c", parts[0] + "." + parts[1], tok + ".x", "short.1.AAAA"} {
		if _, err := ingest.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted it", bad)
		}
	}
}

func TestLogPathNamesTheTrackNeverTheToken(t *testing.T) {
	for in, want := range map[string]string{
		"/stt/ingest/TR_ada0001/nonce.123.mac": "/stt/ingest/TR_ada0001",
		"/stt/ingest/TR_ada0001":               "/stt/ingest/TR_ada0001",
		"/stt/ingest/not-a-track/secret":       "/stt/ingest/{track}",
		"/other/secret":                        "/{redacted}",
	} {
		if got := ingest.LogPath(in); got != want {
			t.Errorf("LogPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSecret(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("thirty-two bytes of ingest secret"))
	if k, err := ingest.ParseSecret(" " + good + "\n"); err != nil || len(k) != 33 {
		t.Fatalf("ParseSecret(good) = %d bytes, %v", len(k), err)
	}
	for _, bad := range []string{"", "s3cretValue", base64.StdEncoding.EncodeToString([]byte("31 bytes of ingest secret 12345"))} {
		if _, err := ingest.ParseSecret(bad); err == nil {
			t.Errorf("ParseSecret(%q) accepted it", bad)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
}

// ---- the websocket server ----

// The RFC 6455 handshake example: key → accept.
func TestAcceptKeyIsRFC6455s(t *testing.T) {
	if got := ingest.AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q", got)
	}
}

// server is a websocket server that hands every message it reads to got.
func server(t *testing.T) (string, chan ingest.Message, chan error) {
	t.Helper()
	got, ended := make(chan ingest.Message, 1024), make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ingest.Upgrade(w, r)
		if err != nil {
			http.Error(w, "no", 400)
			return
		}
		for {
			m, err := c.ReadMessage()
			if err != nil {
				ended <- err
				return
			}
			got <- m
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/x", got, ended
}

func TestTheServerReadsEgressesBinaryFramesAndAClose(t *testing.T) {
	url, got, ended := server(t)
	cl, err := egresstest.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	frames := egressFrames(sine48(4800, 300, 9000), sine48(4800, 300, 9000))
	for _, f := range frames {
		if err := cl.Send(f); err != nil {
			t.Fatal(err)
		}
	}
	for i := range frames {
		select {
		case m := <-got:
			if !m.Binary || string(m.Data) != string(frames[i]) {
				t.Fatalf("message %d: binary %v, %d bytes; want the frame as sent", i, m.Binary, len(m.Data))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
	cl.Close()
	select {
	case err := <-ended:
		if err != io.EOF {
			t.Fatalf("a clean close read as %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close was not read")
	}
}

// rawConn dials the server and completes the handshake by hand, for frames
// the fake Egress would never send.
func rawConn(t *testing.T, url string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", egresstest.HostOf(url))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	io.WriteString(c, "GET /x HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	return c, br
}

func frame(op byte, fin, masked bool, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b := []byte{b0}
	m := byte(0)
	if masked {
		m = 0x80
	}
	switch n := len(payload); {
	case n <= 125:
		b = append(b, m|byte(n))
	default:
		b = append(b, m|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	}
	if masked {
		k := []byte{1, 2, 3, 4}
		b = append(b, k...)
		for i, v := range payload {
			b = append(b, v^k[i%4])
		}
		return b
	}
	return append(b, payload...)
}

func TestTheServerAnswersAPingReassemblesFragmentsAndRefusesUnmaskedFrames(t *testing.T) {
	url, got, ended := server(t)
	c, br := rawConn(t, url)
	// A ping is answered with a pong carrying the same payload.
	c.Write(frame(0x9, true, true, []byte("hi")))
	h := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, h); err != nil || h[0] != 0x8A || h[1] != 2 || string(h[2:]) != "hi" {
		t.Fatalf("the answer to a ping = %x, %v; want a pong with its payload", h, err)
	}
	// Two fragments make one binary message.
	c.Write(frame(0x2, false, true, []byte("abcd")))
	c.Write(frame(0x0, true, true, []byte("efgh")))
	select {
	case m := <-got:
		if !m.Binary || string(m.Data) != "abcdefgh" {
			t.Fatalf("fragments read as %q (binary %v)", m.Data, m.Binary)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fragmented message never arrived")
	}
	// An unmasked client frame breaks RFC 6455: a 1002 close, and the end.
	c.Write(frame(0x2, true, false, []byte("abcd")))
	select {
	case err := <-ended:
		var pe *ingest.ProtocolError
		if !errors.As(err, &pe) || pe.Code != 1002 {
			t.Fatalf("an unmasked frame ended the read with %v, want a 1002 protocol error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unmasked frame was accepted")
	}
}

func TestAnUpgradeThatIsNotOneIsRefused(t *testing.T) {
	for name, mut := range map[string]func(r *http.Request){
		"no Upgrade":    func(r *http.Request) { r.Header.Del("Upgrade") },
		"version 8":     func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") },
		"a short key":   func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "c2hvcnQ=") },
		"a POST":        func(r *http.Request) { r.Method = http.MethodPost },
		"no Connection": func(r *http.Request) { r.Header.Del("Connection") },
	} {
		r := httptest.NewRequest("GET", "/stt/ingest/TR_a/x", nil)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "keep-alive, Upgrade")
		r.Header.Set("Sec-WebSocket-Version", "13")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		if err := ingest.CheckUpgrade(r); err != nil {
			t.Fatalf("the well-formed upgrade: %v", err)
		}
		mut(r)
		if err := ingest.CheckUpgrade(r); err == nil {
			t.Errorf("%s: CheckUpgrade accepted it", name)
		}
	}
}
