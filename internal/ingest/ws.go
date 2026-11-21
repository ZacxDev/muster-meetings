// Package ingest is the plugin's side of LiveKit track Egress (design:
// "Media: LiveKit, TURN, Egress", the S2 rules): a websocket server that
// Egress streams one audio track's raw PCM to, the parser for that PCM, and
// the URL token that is the stream's only credential.
//
// The websocket server here is a minimal RFC 6455 server written for this
// one client, rather than a library, because of what S2 measured about it:
//
//   - 🔴 IT NEVER PINGS. Egress never answers a websocket ping and never
//     reconnects: with a 20 s ping interval and a 20 s pong timeout every
//     stream died at exactly 40 s. This server sends nothing to the client
//     but a close frame and pongs to the client's own pings, and sets no
//     read deadline: a stream lives until Egress closes it, the TCP
//     connection dies (the kernel's keepalive, which Egress's kernel answers),
//     or the plugin closes it (the meeting ended).
//   - Egress sends binary frames only; a text frame is not audio and is
//     dropped. A frame larger than MaxMessage is a protocol error.
package ingest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PingEvery is how often the server pings a client: NEVER (zero). Egress
// does not answer pings (S2), so a server that pings and then times the
// pong out kills every stream. It is a constant so no configuration can
// turn it on.
const PingEvery time.Duration = 0

// MaxMessage is the largest message (after reassembly) the server reads.
// Egress sends 1920 bytes; anything near this is not Egress.
const MaxMessage = 64 << 10

// The websocket close codes the server sends.
const (
	CloseNormal      = 1000
	CloseGoingAway   = 1001
	CloseProtocol    = 1002
	CloseUnsupported = 1003
	CloseTooBig      = 1009
)

// ErrClosed is a read on a connection the server closed.
var ErrClosed = errors.New("ingest: the connection is closed")

// ErrHandshake is a request that is not a websocket upgrade.
var ErrHandshake = errors.New("ingest: not a websocket upgrade")

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// CheckUpgrade says r is a websocket upgrade this server can accept: GET,
// Upgrade: websocket, Connection: upgrade, version 13, and a 16-byte key.
func CheckUpgrade(r *http.Request) error {
	if r.Method != http.MethodGet ||
		!headerHas(r.Header, "Upgrade", "websocket") ||
		!headerHas(r.Header, "Connection", "upgrade") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" {
		return ErrHandshake
	}
	k, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key"))
	if err != nil || len(k) != 16 {
		return ErrHandshake
	}
	return nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// AcceptKey is the Sec-WebSocket-Accept value for a client's key.
func AcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Conn is an accepted websocket connection, read by one goroutine.
type Conn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
	// closed is set once a close frame was sent (by Close, or in answer to
	// the client's).
	closed bool
	once   sync.Once
}

// Upgrade answers r's handshake (CheckUpgrade must have passed) and takes
// the connection over. Nothing is ever written to it after the 101 but a
// close frame and pongs.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if err := CheckUpgrade(r); err != nil {
		return nil, err
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("ingest: hijack: %w", err)
	}
	// No deadline of any kind: see the package comment.
	_ = c.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		AcceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		c.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		c.Close()
		return nil, err
	}
	conn := &Conn{c: c, br: brw.Reader}
	if PingEvery > 0 {
		go conn.pinger(PingEvery)
	}
	return conn, nil
}

// pinger is what a library's default keepalive does: ping every d, and
// close when no pong comes within d. It is never started (PingEvery is
// zero); it exists so the sweep's "ingest pings" mutant is one constant.
func (c *Conn) pinger(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for range t.C {
		if c.write(0x9, nil) != nil {
			return
		}
		_ = c.c.SetReadDeadline(time.Now().Add(d))
	}
}

// Message is one data message: binary (Egress's audio) or text.
type Message struct {
	Binary bool
	Data   []byte
}

// ProtocolError is a client that broke the protocol; Code is the close code
// the server sent.
type ProtocolError struct {
	Code int
	Why  string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("ingest: protocol error %d: %s", e.Code, e.Why)
}

// ReadMessage reads the next data message, answering pings and handling
// close. It returns io.EOF when the client closed cleanly, a
// *ProtocolError (after sending the close) when the client broke the
// protocol, or the network error.
func (c *Conn) ReadMessage() (Message, error) {
	var msg []byte
	var opcode byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return Message{}, err
		}
		switch op {
		case 0x8: // close
			code := CloseNormal
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
			}
			c.closeWith(code, "")
			return Message{}, io.EOF
		case 0x9: // ping: answered, never sent
			if err := c.write(0xA, payload); err != nil {
				return Message{}, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x0: // continuation
			if !started {
				return Message{}, c.fail(CloseProtocol, "a continuation with nothing to continue")
			}
		case 0x1, 0x2:
			if started {
				return Message{}, c.fail(CloseProtocol, "a new message inside a fragmented one")
			}
			started, opcode = true, op
		default:
			return Message{}, c.fail(CloseProtocol, fmt.Sprintf("opcode %#x", op))
		}
		if len(msg)+len(payload) > MaxMessage {
			return Message{}, c.fail(CloseTooBig, "a message over MaxMessage")
		}
		msg = append(msg, payload...)
		if fin {
			return Message{Binary: opcode == 0x2, Data: msg}, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		return false, 0, nil, c.fail(CloseProtocol, "reserved bits set")
	}
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= 0x8 && (n > 125 || !fin) {
		return false, 0, nil, c.fail(CloseProtocol, "a bad control frame")
	}
	// A client's frames are masked (RFC 6455 5.1).
	if !masked {
		return false, 0, nil, c.fail(CloseProtocol, "an unmasked client frame")
	}
	if n > MaxMessage {
		return false, 0, nil, c.fail(CloseTooBig, "a frame over MaxMessage")
	}
	var key [4]byte
	if _, err = io.ReadFull(c.br, key[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	return fin, op, payload, nil
}

func (c *Conn) fail(code int, why string) error {
	c.closeWith(code, why)
	return &ProtocolError{Code: code, Why: why}
}

// write sends one unmasked frame (server frames are never masked).
func (c *Conn) write(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed && op != 0x8 {
		return ErrClosed
	}
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n <= 125:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	if _, err := c.c.Write(append(hdr, payload...)); err != nil {
		return err
	}
	if op == 0x8 {
		c.closed = true
	}
	return nil
}

// closeWith sends a close frame with code (once) and closes the socket.
func (c *Conn) closeWith(code int, reason string) {
	c.once.Do(func() {
		p := binary.BigEndian.AppendUint16(nil, uint16(code))
		if len(reason) > 120 {
			reason = reason[:120]
		}
		p = append(p, reason...)
		_ = c.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = c.write(0x8, p)
		c.c.Close()
	})
}

// Close closes the connection with code (CloseNormal: the meeting ended or
// the stream is no longer wanted).
func (c *Conn) Close(code int, reason string) { c.closeWith(code, reason) }
