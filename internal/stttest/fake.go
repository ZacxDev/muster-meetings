// Package stttest is a fake Nemotron streaming server for tests: it speaks
// the protocol of muster's images/stt-nemotron (its README is the contract)
// exactly, with scripted text in place of a model.
//
//   - GET /v1/stream, a websocket upgrade with `Authorization: Bearer
//     <token>`. Every refusal is an HTTP status BEFORE the upgrade: 401 for a
//     missing or wrong bearer (checked first, so an unauthenticated client
//     never learns the capacity), 503 + Retry-After at capacity (MaxStreams
//     open), or the status Refuse names.
//   - Client → server: binary frames of exactly 2560 bytes (1280 samples of
//     S16LE mono 16 kHz); `{"type":"flush"}`; `{"type":"end","samples":N}`
//     with total-1280 < N <= total (or 0 and 0).
//   - Server → client: `partial` (the segment's text so far, growing a word
//     per PartialEvery frames, only when it changed and is not empty),
//     `final` (exactly one per flush and per end, even when empty), times in
//     ms of SAMPLES; after the end's final, a close with 1000.
//   - Errors: an `error` frame, then a close: bad_frame / bad_message /
//     bad_end 1003, idle 1001 (no frame for IdleTimeout).
//   - A segment whose samples are all one value (padding excluded) yields
//     empty text and no partials, as the server's constant-signal rule.
//
// It records every stream: the frames' sizes, the sample count at each
// flush, the end's samples, and the bearer it was opened with; and it can
// drop every live stream (DropAll: the TCP connection cut, no close frame)
// or close them with an error code (CloseAll).
package stttest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FrameBytes is the only binary frame size the server takes.
const FrameBytes = 2560

// FrameSamples is one frame's samples.
const FrameSamples = FrameBytes / 2

// Record is what the server saw on one stream.
type Record struct {
	ID     int
	Auth   string  // the Authorization header it was opened with
	Frames []int   // each binary frame's size, in order
	Last   []int16 // the last frame's samples (to see its padding)
	// Flushes is the sample count received at each flush; End the end's
	// samples (-1 for none); Total the samples received (padding
	// included).
	Flushes []int64
	End     int64
	Total   int64
	// Sent is every message the server sent, in order (JSON lines); Close
	// the close code it sent (0 for none: dropped, or still open).
	Sent  []string
	Close int
}

// Server is the fake.
type Server struct {
	*httptest.Server
	Token string

	mu sync.Mutex
	// MaxStreams is the capacity (default 8); RetryAfter its Retry-After in
	// seconds (default 30). Refuse, when non-zero, answers every
	// authenticated upgrade with that status (with Retry-After for 503).
	MaxStreams int
	RetryAfter int
	Refuse     int
	// Lines is the script: the n-th non-empty segment of a stream says
	// Lines[n % len]. PartialEvery is how many frames each partial's word
	// takes (default 7: 560 ms, the model's chunk).
	Lines        []string
	PartialEvery int
	// IdleTimeout closes a stream that sends nothing for it (default 30 s).
	IdleTimeout time.Duration
	// FinalDelay holds each final back (a slow model).
	FinalDelay time.Duration
	// Hangup cuts every upgrade's connection with no answer at all: a
	// failed connect, not a refusal.
	Hangup bool

	hangups int

	active   int
	upgrades int
	refusals []int
	records  []*Record
	conns    map[*fconn]bool
}

// DefaultLines is the script when none is set.
var DefaultLines = []string{"so the next thing is the budget", "we agreed to ship on friday", "thanks everyone"}

// New starts the fake with token as its bearer.
func New(token string) *Server {
	s := &Server{Token: token, MaxStreams: 8, RetryAfter: 30, PartialEvery: 7, IdleTimeout: 30 * time.Second,
		Lines: DefaultLines, conns: map[*fconn]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL is the stream endpoint, ws://…/v1/stream.
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.Server.URL, "http") + "/v1/stream" }

// Set changes the configuration under the lock.
func (s *Server) Set(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Active is the open streams; Upgrades the accepted upgrades in all;
// Refusals the statuses refused upgrades were answered with, in order.
func (s *Server) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *Server) Upgrades() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upgrades
}

// Hangups is how many upgrades Hangup cut.
func (s *Server) Hangups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hangups
}

func (s *Server) Refusals() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.refusals...)
}

// Records is a copy of every stream's record, in the order they opened.
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	for i, r := range s.records {
		out[i] = *r
		out[i].Frames = append([]int(nil), r.Frames...)
		out[i].Flushes = append([]int64(nil), r.Flushes...)
		out[i].Sent = append([]string(nil), r.Sent...)
		out[i].Last = append([]int16(nil), r.Last...)
	}
	return out
}

// DropAll cuts every live stream's TCP connection, with no close frame: a
// mid-stream drop.
func (s *Server) DropAll() {
	s.mu.Lock()
	cs := make([]*fconn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.c.Close()
	}
}

// CloseAll sends every live stream an error frame with code errCode and a
// close with closeCode (e.g. "overloaded", 1013).
func (s *Server) CloseAll(errCode string, closeCode int) {
	s.mu.Lock()
	cs := make([]*fconn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.fail(errCode, closeCode)
	}
}

func refuse(w http.ResponseWriter, status int, retry int, why string) {
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": why})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/stream" {
		http.NotFound(w, r)
		return
	}
	auth := r.Header.Get("Authorization")
	scheme, cred, _ := strings.Cut(auth, " ")
	s.mu.Lock()
	if s.Hangup {
		s.hangups++
		s.mu.Unlock()
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			c.Close()
		}
		return
	}
	if !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(cred) != s.Token || s.Token == "" {
		s.refusals = append(s.refusals, 401)
		s.mu.Unlock()
		refuse(w, 401, 0, "invalid or missing bearer token")
		return
	}
	if s.Refuse != 0 {
		st := s.Refuse
		ra := 0
		if st == 503 {
			ra = s.RetryAfter
		}
		s.refusals = append(s.refusals, st)
		s.mu.Unlock()
		refuse(w, st, ra, "refused")
		return
	}
	if s.active >= s.MaxStreams {
		ra := s.RetryAfter
		s.refusals = append(s.refusals, 503)
		s.mu.Unlock()
		refuse(w, 503, ra, "at capacity")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		s.mu.Unlock()
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return
	}
	s.active++
	s.upgrades++
	rec := &Record{ID: s.upgrades, Auth: auth, End: -1}
	s.records = append(s.records, rec)
	idle, every, lines, delay := s.IdleTimeout, s.PartialEvery, append([]string(nil), s.Lines...), s.FinalDelay
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	// The stream is live (DropAll and CloseAll reach it) BEFORE the 101 is
	// written: a client that has its 101 can be dropped at once.
	fc := &fconn{s: s, c: c, br: brw.Reader, rec: rec}
	s.mu.Lock()
	s.conns[fc] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, fc)
		s.mu.Unlock()
		c.Close()
	}()
	h := sha1.New()
	io.WriteString(h, r.Header.Get("Sec-WebSocket-Key")+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	fc.wmu.Lock()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(h.Sum(nil)) + "\r\n\r\n")
	err = brw.Flush()
	fc.wmu.Unlock()
	if err != nil {
		return
	}
	fc.serve(idle, every, lines, delay)
}

// fconn is one accepted stream.
type fconn struct {
	s   *Server
	c   net.Conn
	br  *bufio.Reader
	rec *Record
	wmu sync.Mutex
	// closed: a close frame was sent.
	closed bool
}

// segment is the decode state of the current segment.
type segment struct {
	start    int64 // sample the segment began at
	frames   int   // frames in it
	lo, hi   int16 // the samples' range, padding excluded
	any      bool
	pending  []int16 // the newest frame, held back until the next (it may be the padded last)
	line     string
	partial  string
	nonempty int // non-empty segments so far in the stream
}

func (sg *segment) fold(pcm []int16) {
	for _, v := range pcm {
		if !sg.any {
			sg.lo, sg.hi, sg.any = v, v, true
			continue
		}
		sg.lo, sg.hi = min(sg.lo, v), max(sg.hi, v)
	}
}

// constant is the server's constant-signal rule over the segment so far
// (the held-back frame included in full).
func (sg *segment) constant() bool {
	lo, hi, any := sg.lo, sg.hi, sg.any
	for _, v := range sg.pending {
		if !any {
			lo, hi, any = v, v, true
			continue
		}
		lo, hi = min(lo, v), max(hi, v)
	}
	return !any || lo == hi
}

func (f *fconn) serve(idle time.Duration, every int, lines []string, delay time.Duration) {
	var total int64
	sg := &segment{}
	wordsFor := func() string {
		if len(lines) == 0 {
			return ""
		}
		return lines[sg.nonempty%len(lines)]
	}
	final := func(end int64) {
		text := ""
		if !sg.constant() && sg.frames > 0 {
			text = wordsFor()
			sg.nonempty++
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		f.send(map[string]any{"type": "final", "text": text, "start_ms": sg.start * 1000 / 16000, "end_ms": end * 1000 / 16000})
		*sg = segment{start: end, nonempty: sg.nonempty}
	}
	for {
		_ = f.c.SetReadDeadline(time.Now().Add(idle))
		op, data, err := f.read()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				f.fail("idle", 1001)
			}
			return
		}
		f.s.mu.Lock()
		if op == 0x2 {
			f.rec.Frames = append(f.rec.Frames, len(data))
		}
		f.s.mu.Unlock()
		switch op {
		case 0x2:
			if len(data) != FrameBytes {
				f.fail("bad_frame", 1003)
				return
			}
			pcm := make([]int16, FrameSamples)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
			}
			total += FrameSamples
			f.s.mu.Lock()
			f.rec.Total, f.rec.Last = total, pcm
			f.s.mu.Unlock()
			if sg.pending != nil {
				sg.fold(sg.pending)
			}
			sg.pending = pcm
			sg.frames++
			if every > 0 && sg.frames%every == 0 && !sg.constant() {
				words := strings.Fields(wordsFor())
				k := min(sg.frames/every, len(words))
				if p := strings.Join(words[:k], " "); p != "" && p != sg.partial {
					sg.partial = p
					f.send(map[string]any{"type": "partial", "text": p, "start_ms": sg.start * 1000 / 16000, "end_ms": (sg.start + int64(sg.frames/every*every*FrameSamples)) * 1000 / 16000})
				}
			}
		case 0x1:
			var m struct {
				Type    string `json:"type"`
				Samples *json.RawMessage
			}
			if json.Unmarshal(data, &m) != nil {
				f.fail("bad_message", 1003)
				return
			}
			switch m.Type {
			case "flush":
				f.s.mu.Lock()
				f.rec.Flushes = append(f.rec.Flushes, total)
				f.s.mu.Unlock()
				if sg.pending != nil {
					sg.fold(sg.pending)
					sg.pending = nil
				}
				final(total)
			case "end":
				var n int64
				ok := m.Samples != nil && json.Unmarshal(*m.Samples, &n) == nil && !strings.ContainsAny(string(*m.Samples), ".eE\"")
				if !ok || !((total == 0 && n == 0) || (total-FrameSamples < n && n <= total)) {
					f.fail("bad_end", 1003)
					return
				}
				f.s.mu.Lock()
				f.rec.End = n
				f.s.mu.Unlock()
				if sg.pending != nil {
					pad := total - n
					sg.fold(sg.pending[:int64(len(sg.pending))-pad])
					sg.pending = nil
				}
				final(n)
				f.closeWith(1000)
				return
			default:
				f.fail("bad_message", 1003)
				return
			}
		}
	}
}

// read reads one data message (masked client frames, no fragmentation from
// the plugin; pings answered).
func (f *fconn) read() (byte, []byte, error) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(f.br, h[:]); err != nil {
			return 0, nil, err
		}
		op := h[0] & 0x0F
		if h[1]&0x80 == 0 {
			return 0, nil, errors.New("an unmasked client frame")
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(f.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(f.br, b[:]); err != nil {
				return 0, nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 64<<10 {
			return 0, nil, errors.New("too big")
		}
		var key [4]byte
		if _, err := io.ReadFull(f.br, key[:]); err != nil {
			return 0, nil, err
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(f.br, p); err != nil {
			return 0, nil, err
		}
		for i := range p {
			p[i] ^= key[i%4]
		}
		switch op {
		case 0x8:
			return 0, nil, io.EOF
		case 0x9:
			f.write(0xA, p)
			continue
		case 0xA:
			continue
		}
		return op, p, nil
	}
}

func (f *fconn) write(op byte, p []byte) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	hdr := []byte{0x80 | op}
	switch n := len(p); {
	case n <= 125:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_ = f.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := f.c.Write(append(hdr, p...))
	if op == 0x8 {
		f.closed = true
	}
	return err
}

func (f *fconn) send(m map[string]any) {
	b, _ := json.Marshal(m)
	f.s.mu.Lock()
	f.rec.Sent = append(f.rec.Sent, string(b))
	f.s.mu.Unlock()
	f.write(0x1, b)
}

func (f *fconn) closeWith(code int) {
	f.s.mu.Lock()
	if f.rec.Close == 0 {
		f.rec.Close = code
	}
	f.s.mu.Unlock()
	f.write(0x8, binary.BigEndian.AppendUint16(nil, uint16(code)))
	f.c.Close()
}

func (f *fconn) fail(code string, close int) {
	f.send(map[string]any{"type": "error", "code": code, "message": code})
	f.closeWith(close)
}
