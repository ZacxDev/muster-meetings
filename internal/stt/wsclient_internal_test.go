package stt

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// Retry-After is read as the server may send it: seconds or an HTTP date
// (one in the future is the wait from now; one in the past, no wait), both
// capped at 24 h; a negative number or garbage is no wait. Fixture values
// are distinct from the 30 s floor and from each other.
func TestRetryAfterReadsSecondsAndDatesCappedAt24h(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).Format(http.TimeFormat) }
	for _, c := range []struct {
		name, v string
		want    time.Duration
	}{
		{"seconds", "47", 47 * time.Second},
		{"zero", "0", 0},
		{"none", "", 0},
		{"negative", "-12", 0},
		{"garbage", "soon", 0},
		{"seconds over the cap", "90061", 24 * time.Hour},
		{"a future date", date(93 * time.Second), 93 * time.Second},
		{"a past date", date(-93 * time.Second), 0},
		{"a date over the cap", date(49 * time.Hour), 24 * time.Hour},
	} {
		if got := retryAfter(c.v, now); got != c.want {
			t.Errorf("%s: retryAfter(%q) = %s, want %s", c.name, c.v, got, c.want)
		}
	}
}

// serverSends has a fake server write raw onto a fresh client connection,
// then answers the client's readMessage error and every byte the client
// wrote back (its close frame).
func serverSends(t *testing.T, raw []byte) (error, []byte) {
	t.Helper()
	cl, sv := net.Pipe()
	w := &wsConn{c: cl, br: bufio.NewReader(cl), writeTimeout: 2 * time.Second}
	back := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(sv)
		back <- b
	}()
	go func() {
		sv.Write(raw)
		// A client that took the frame waits for the next: the server then
		// closes normally (1000), so the test ends either way and the
		// client's answer (an echoed 1000, not a 1002) shows it.
		time.Sleep(300 * time.Millisecond)
		sv.Write([]byte{0x88, 2, 0x03, 0xE8})
	}()
	_, _, err := w.readMessage()
	w.close(1001) // a no-op once the client already closed
	select {
	case b := <-back:
		return err, b
	case <-time.After(5 * time.Second):
		t.Fatal("the client never closed")
		return nil, nil
	}
}

// closeSent finds the client's close frame among what it wrote (a pong may
// come first) and answers its unmasked payload.
func closeSent(t *testing.T, b []byte) []byte {
	t.Helper()
	for len(b) >= 6 {
		if b[1]&0x80 == 0 {
			t.Fatalf("the client sent an unmasked frame: %x", b)
		}
		n, h := int(b[1]&0x7F), 2
		if n == 126 {
			n, h = int(binary.BigEndian.Uint16(b[2:4])), 4
		}
		key, p := b[h:h+4], append([]byte(nil), b[h+4:h+4+n]...)
		for i := range p {
			p[i] ^= key[i%4]
		}
		if b[0] == 0x88 {
			return p
		}
		b = b[h+4+n:]
	}
	t.Fatalf("the client sent no close frame")
	return nil
}

// The client holds the server to RFC 6455: reserved bits, a control frame
// over 125 bytes and a fragmented control frame are protocol errors (the
// client closes with 1002); a close with no code is answered with an empty
// close, never a 1005 on the wire; a close with a code is echoed.
func TestTheClientHoldsTheServerToTheWebsocketRules(t *testing.T) {
	ping126 := append([]byte{0x89, 126, 0, 126}, make([]byte, 126)...)
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"a reserved bit", []byte{0x81 | 0x40, 2, '{', '}'}},
		{"a 126-byte ping", ping126},
		{"a fragmented ping", []byte{0x09, 1, 'x'}},
		{"a masked frame", []byte{0x81, 0x82, 1, 2, 3, 4, 'a', 'b'}},
		{"a fragmented close frame", []byte{0x08, 2, 0x03, 0xE8}},
	} {
		name := c.name
		err, back := serverSends(t, c.raw)
		if err == nil {
			t.Errorf("%s: readMessage accepted it", name)
			continue
		}
		if p := closeSent(t, back); len(p) != 2 || binary.BigEndian.Uint16(p) != 1002 {
			t.Errorf("%s: the client closed with %x, want 1002 (a protocol error)", name, p)
		}
	}
	err, back := serverSends(t, []byte{0x88, 0})
	if ce, ok := err.(*CloseError); !ok || ce.Code != 1005 {
		t.Fatalf("an empty close: %v, want a CloseError 1005", err)
	}
	if p := closeSent(t, back); len(p) != 0 {
		t.Fatalf("an empty close was answered with %x, want an empty close", p)
	}
	err, back = serverSends(t, []byte{0x88, 2, 0x03, 0xF5})
	if ce, ok := err.(*CloseError); !ok || ce.Code != 1013 {
		t.Fatalf("a 1013 close: %v", err)
	}
	if p := closeSent(t, back); len(p) != 2 || binary.BigEndian.Uint16(p) != 1013 {
		t.Fatalf("a 1013 close was answered with %x, want 1013 echoed", p)
	}
}
