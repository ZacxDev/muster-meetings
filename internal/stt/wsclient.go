package stt

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 websocket CLIENT for the Nemotron server's one
// endpoint, written here (like internal/ingest's server) rather than taken
// from a library: the plugin has no websocket dependency, and the protocol
// needs only what is below.
//
//   - Every refusal is an HTTP status on the upgrade (the server's contract:
//     401, 503, 503 + Retry-After), answered as a *RefusedError, so the
//     engine tells "refused" (go to the fallback at once) from "broken" (a
//     socket that closes or errors after the upgrade: reconnect first).
//   - 🔴 The bearer goes in the Authorization header and NOWHERE else: no
//     error, log line or URL carries it (the error for a refusal names the
//     path and the status, never the request).
//   - It sends no pings (the server requires none) and answers the server's.

// RefusedError is an upgrade the server answered with an HTTP status
// instead of 101.
type RefusedError struct {
	Status int
	// RetryAfter is the server's Retry-After (0 when it sent none).
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	s := fmt.Sprintf("stt: the stream server refused the stream: %d %s", e.Status, http.StatusText(e.Status))
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (Retry-After %s)", e.RetryAfter)
	}
	return s
}

// CloseError is the server closing the socket: its close code, and the code
// of the error frame before it, if any (bad_frame, overloaded, idle …).
type CloseError struct {
	Code   int
	Reason string // the error frame's code, e.g. "bad_frame"; "" for none
}

func (e *CloseError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("stt: the stream server closed the stream: %d (%s)", e.Code, e.Reason)
	}
	return fmt.Sprintf("stt: the stream server closed the stream: %d", e.Code)
}

// wsConn is a client websocket: one writer at a time (wmu), one reader.
type wsConn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
	// WriteTimeout bounds each write: a server that stops reading is a
	// dead stream, never a stuck engine.
	writeTimeout time.Duration
	closeOnce    sync.Once
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxServerMessage is the largest message read from the server: its JSON
// lines are small; anything near this is not the server.
const maxServerMessage = 64 << 10

// wsDial opens a websocket to rawURL (ws:// or wss://, with its path)
// presenting token as a bearer. A status other than 101 is a *RefusedError.
func wsDial(ctx context.Context, rawURL, token string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return nil, errors.New("stt: the stream URL is not ws:// or wss://")
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("stt: connecting to the stream server: %w", err)
	}
	if u.Scheme == "wss" {
		tc := tls.Client(c, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(dctx); err != nil {
			c.Close()
			return nil, fmt.Errorf("stt: TLS to the stream server: %w", err)
		}
		c = tc
	}
	deadline, _ := dctx.Deadline()
	_ = c.SetDeadline(deadline)
	var kb [16]byte
	if _, err := rand.Read(kb[:]); err != nil {
		c.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(kb[:])
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer " + token + "\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, fmt.Errorf("stt: sending the upgrade: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("stt: reading the upgrade's answer: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		c.Close()
		return nil, &RefusedError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h.Sum(nil)) {
		c.Close()
		return nil, errors.New("stt: the stream server's upgrade answer is not a websocket's")
	}
	_ = c.SetDeadline(time.Time{})
	return &wsConn{c: c, br: br, writeTimeout: 5 * time.Second}, nil
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date; 0 for
// none or nonsense.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(min(n, 24*3600)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return min(t.Sub(now), 24*time.Hour)
	}
	return 0
}

// writeFrame sends one masked frame (a client's frames are masked, RFC 6455
// 5.1).
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n <= 125:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	hdr = append(hdr, key[:]...)
	buf := make([]byte, len(hdr)+len(payload))
	copy(buf, hdr)
	for i, b := range payload {
		buf[len(hdr)+i] = b ^ key[i%4]
	}
	_ = w.c.SetWriteDeadline(time.Now().Add(w.writeTimeout))
	_, err := w.c.Write(buf)
	return err
}

func (w *wsConn) writeBinary(b []byte) error { return w.writeFrame(0x2, b) }
func (w *wsConn) writeText(s string) error   { return w.writeFrame(0x1, []byte(s)) }

// readMessage reads the next data message (text: true for a text frame),
// answering pings. A close frame is a *CloseError with the server's code.
func (w *wsConn) readMessage() (data []byte, text bool, err error) {
	var msg []byte
	started := false
	for {
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return nil, false, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0F
		if h[1]&0x80 != 0 {
			return nil, false, errors.New("stt: the stream server sent a masked frame")
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, false, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return nil, false, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > maxServerMessage || uint64(len(msg))+n > maxServerMessage {
			return nil, false, errors.New("stt: a stream server message over 64 KiB")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return nil, false, err
		}
		switch op {
		case 0x8:
			code := 1005 // no status
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
			}
			w.close(code)
			return nil, false, &CloseError{Code: code}
		case 0x9:
			if err := w.writeFrame(0xA, payload); err != nil {
				return nil, false, err
			}
			continue
		case 0xA:
			continue
		case 0x0:
			if !started {
				return nil, false, errors.New("stt: a continuation with nothing to continue")
			}
		case 0x1, 0x2:
			if started {
				return nil, false, errors.New("stt: a new message inside a fragmented one")
			}
			started, text = true, op == 0x1
		default:
			return nil, false, fmt.Errorf("stt: opcode %#x", op)
		}
		msg = append(msg, payload...)
		if fin {
			return msg, text, nil
		}
	}
}

// close sends a close frame with code (once) and closes the socket.
func (w *wsConn) close(code int) {
	w.closeOnce.Do(func() {
		_ = w.writeFrame(0x8, binary.BigEndian.AppendUint16(nil, uint16(code)))
		w.c.Close()
	})
}
