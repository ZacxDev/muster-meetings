package stt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The `nemotron` engine (design: "Serving Nemotron", decision 19): one
// websocket per track to the Nemotron server (images/stt-nemotron in
// muster; its README is the protocol), partials shown as live captions,
// finals stored. The plugin's own endpointer (vad.go, shared with the
// utterance engine) decides where an utterance ends and sends `flush`.
//
// The wire: binary frames of exactly FrameSamples (1280 samples, 80 ms,
// 2560 bytes of S16LE mono 16 kHz); the last frame zero-padded, and
// `{"type":"end","samples":N}` with the TRUE count N. The server's times are
// its own sample count, so every segment's time is the connection's offset
// (where in this track's audio the connection's first sample was) plus the
// server's time. Never arrival.
//
// 🔴 Every frame is sent, silence included: the server's clock is the
// samples it received, so a frame left out would move every later time. The
// server closes a stream that has sent nothing for 30 s (`idle`); Egress
// streams a frame every 10 ms while a track is published, so a live stream
// never idles. When the ingest itself delivers nothing for IdleEnd (the
// publisher went away, a long stall), the stream is ENDED cleanly (padded
// last frame and `end`, its final stored) rather than kept open with
// invented silence, and the next audio opens a new connection at its own
// offset. Inserting silence would put a sample in the server's clock that
// the track never had.
//
// The fallback, in the design's order of precedence:
//
//  1. At stream open, a refusal (401, 503, 503 + Retry-After) or a failed
//     connect: this participant goes to the utterance engine at once, the
//     panel says "Captions delayed", and Nemotron is tried again after
//     max(MinRetry, Retry-After); the participant moves back when a stream
//     opens.
//  2. A mid-stream drop (the socket closes or errors after the upgrade, an
//     error frame such as bad_frame included): up to len(Reconnects)
//     reconnects, at 1, 2 and 4 s, with a gap marker for the audio not
//     transcribed meanwhile; if all fail, rule 1. A reconnect REFUSED with a
//     status is the server saying it is full or will not serve: rule 1 at
//     once, without the remaining reconnects.
//  3. On the utterance engine, its own cap (4 in flight per plugin, 30 s
//     queued per participant, the oldest dropped with a gap marker).
//
// The silent-start check (design: "Silent start and empty-output check"):
// a track whose audio had speech energy (the endpointer's) for SilentStart
// with no word coming back from any engine, or whose raw signal was one
// constant NON-ZERO value for SilentStart (a stuck capture: after the DC
// filter it has no energy, so the first rule alone would never see it),
// raises a Flag segment, once per stretch. Digital silence (all zeros: a
// muted microphone) is not flagged.

// StreamPath is the Nemotron server's one endpoint.
const StreamPath = "/v1/stream"

// CheckStreamURL validates MEETINGS_STREAM_STT_URL: wss://host[:port], or
// ws:// (the server inside the cluster, as MEETINGS_STT_URL is plain http
// there), with the path /v1/stream or none (it is added); no user, query or
// fragment. It answers the URL to dial.
func CheckStreamURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/" && u.Path != StreamPath) {
		return "", errors.New("MEETINGS_STREAM_STT_URL must be wss://host[:port]/v1/stream (ws:// inside the cluster), with no user, query or fragment")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + StreamPath, nil
}

// CheckToken validates a bearer variable: set, and visible ASCII only (it
// goes into an HTTP header, so a space, a control character or a line
// break is refused: it could not be a token, and a CR/LF would forge a
// header). The error never shows the value.
func CheckToken(name, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("%s is not set (the server's bearer; its value is not shown)", name)
	}
	for _, r := range v {
		if r <= ' ' || r > '~' {
			return "", fmt.Errorf("%s holds a space, a control character or a non-ASCII character (its value is not shown)", name)
		}
	}
	return v, nil
}

// FrameSamples is one wire frame: 80 ms at 16 kHz.
const FrameSamples = Rate * 80 / 1000

// The nemotron engine's defaults (design: "Serving Nemotron").
const (
	// MinRetry is the least wait before Nemotron is tried again after a
	// fallback: the wait is max(MinRetry, Retry-After).
	MinRetry = 30 * time.Second
	// IdleEnd is how long the ingest may deliver nothing before the stream is
	// ended cleanly: under the server's 30 s idle close.
	IdleEnd = 20 * time.Second
	// SilentStart is the silent-start check's span.
	SilentStart = 20 * time.Second
	// DialTimeout bounds one connect and upgrade; EndTimeout one stream's
	// last final after `end`.
	DialTimeout = 5 * time.Second
	EndTimeout  = 10 * time.Second
)

// DefaultReconnects is the wait before each reconnect after a mid-stream
// drop: 1, 2 and 4 s.
var DefaultReconnects = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// The two flags a Flag segment's Text names.
const (
	FlagSilentStart = "silent_start" // speech energy and no words
	FlagDeadInput   = "dead_input"   // one constant non-zero value
)

// NemotronEngine is the `nemotron` engine.
type NemotronEngine struct {
	// URL is the server's websocket endpoint (ws:// or wss://…/v1/stream);
	// Token its bearer. 🔴 The token is never logged.
	URL, Token string
	// Fallback is the utterance engine a participant goes to.
	Fallback Engine
	// Log receives the engine's own lines: no token, no text, no name.
	Log *log.Logger
	// Now is the clock the retry waits are measured on (default
	// time.Now); Reconnects the waits before each reconnect (default
	// DefaultReconnects); IdleEnd and SilentStart default to the constants.
	Now         func() time.Time
	Reconnects  []time.Duration
	IdleEnd     time.Duration
	SilentStart time.Duration
	// Processed, when set (tests), is called after each chunk is handled,
	// with the track's samples consumed so far.
	Processed func(pos int64)
}

// NewNemotron is the nemotron engine at url with token, falling back to
// fallback.
func NewNemotron(url, token string, fallback Engine, logger *log.Logger) *NemotronEngine {
	return &NemotronEngine{URL: url, Token: token, Fallback: fallback, Log: logger}
}

// Name is "nemotron".
func (*NemotronEngine) Name() string { return "nemotron" }

func (e *NemotronEngine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *NemotronEngine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

// Open starts a stream; its first connection is made by the stream's own
// goroutine (Open never waits on the server).
func (e *NemotronEngine) Open(ctx context.Context, track TrackRef) (Stream, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &nemStream{e: e, track: track, ctx: ctx, cancel: cancel, ep: NewEndpointer(),
		segs: make(chan Segment, 256), wake: make(chan struct{}, 1), drops: make(chan *nemConn, 4), runDone: make(chan struct{})}
	s.idleEnd, s.silent = e.IdleEnd, e.SilentStart
	if s.idleEnd <= 0 {
		s.idleEnd = IdleEnd
	}
	if s.silent <= 0 {
		s.silent = SilentStart
	}
	s.reconnects = e.Reconnects
	if s.reconnects == nil {
		s.reconnects = DefaultReconnects
	}
	s.senders.Add(1)
	go s.run()
	return s, nil
}

// The stream's modes.
type nemMode int

const (
	modeIdle      nemMode = iota // no connection: the next audio opens one (rule 1 on failure)
	modeLive                     // a connection takes the audio
	modeReconnect                // dropped: audio is lost (a gap) until a reconnect or rule 1
	modeFallback                 // on the utterance engine until retryAt
)

type nemStream struct {
	e      *NemotronEngine
	track  TrackRef
	ctx    context.Context
	cancel context.CancelFunc
	segs   chan Segment
	// senders counts every goroutine that may still send on segs (run, each
	// connection's reader, each fallback forwarder): segs closes after all.
	senders sync.WaitGroup

	// The queue Write fills and run drains.
	mu     sync.Mutex
	queue  [][]int16
	closed bool
	wake   chan struct{}
	drops  chan *nemConn
	// runDone closes when run returns: a reader's drop report is then
	// nobody's.
	runDone chan struct{}

	// Everything below is run's alone.
	ep          *Endpointer
	pos         int64 // samples of this track consumed
	mode        nemMode
	conn        *nemConn
	attempts    int       // reconnects tried since the drop
	nextAt      time.Time // the next reconnect (modeReconnect) or retry (modeFallback)
	gapFrom     int64     // sample where the lost audio began (modeReconnect)
	fb          Stream
	idleEnd     time.Duration
	silent      time.Duration
	reconnects  []time.Duration
	words       atomic.Bool // a word came back since run last looked
	speechBase  int64       // the endpointer's speech count at the last word
	constRun    int64       // samples of one constant non-zero value, running
	constVal    int16
	flaggedSS   bool
	flaggedDead bool
}

// nemConn is one websocket to the server, read by its own goroutine.
type nemConn struct {
	ws     *wsConn
	offset int64   // the track's sample at this connection's first sample
	frame  []int16 // samples not yet a whole frame
	sent   int64   // real samples sent (padding excluded)
	ending atomic.Bool
	done   chan struct{} // the reader is finished
	// confirmed is the track sample up to which a final has come back:
	// where a drop's gap begins.
	confirmed atomic.Int64
	reason    atomic.Value // the server's error frame code, if any
}

func (s *nemStream) Segments() <-chan Segment { return s.segs }

// Write queues pcm for the stream's goroutine: it never waits on the server.
func (s *nemStream) Write(pcm []int16) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.queue = append(s.queue, append([]int16(nil), pcm...))
	s.mu.Unlock()
	s.signal()
	return nil
}

// Close ends the stream: the queued audio is sent, the connection is ended
// with `end` (its last final stored) or the fallback drained, then Segments
// closes.
func (s *nemStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *nemStream) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// emit sends seg unless the stream's context ended.
func (s *nemStream) emit(seg Segment) {
	select {
	case s.segs <- seg:
	case <-s.ctx.Done():
	}
}

// take is the queued audio, and whether Close was called.
func (s *nemStream) take() ([][]int16, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queue
	s.queue = nil
	return q, s.closed
}

func (s *nemStream) run() {
	defer func() {
		close(s.runDone)
		if s.ctx.Err() != nil && s.conn != nil {
			s.conn.ws.close(1001)
		}
		s.senders.Done()
		// Segments closes once every sender is done: the last final after
		// `end`, and the fallback's drain.
		go func() { s.senders.Wait(); s.cancel(); close(s.segs) }()
	}()
	s.attempt()
	idle := time.NewTimer(s.idleEnd)
	defer idle.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case c := <-s.drops:
			s.dropped(c)
			continue
		case <-idle.C:
			if s.mode == modeLive {
				s.e.logf("transcript: a track's audio stopped for %s; ending its Nemotron stream (the next audio opens a new one)", s.idleEnd)
				s.endConn()
				s.mode = modeIdle
			}
			idle.Reset(s.idleEnd)
			continue
		case <-s.wake:
		}
		q, closed := s.take()
		if len(q) > 0 {
			idle.Reset(s.idleEnd)
		}
		for _, c := range q {
			s.chunk(c)
			if s.e.Processed != nil {
				s.e.Processed(s.pos)
			}
		}
		if closed {
			s.finish()
			return
		}
	}
}

// attempt is rule 1's attempt (a stream's start, the first audio after an
// idle end, a retry from the fallback): a connection at the current
// position, or the fallback until max(MinRetry, Retry-After).
func (s *nemStream) attempt() {
	c, err := s.dial()
	if err == nil {
		s.live(c)
		return
	}
	s.toFallback(err)
}

// dial opens a connection whose first sample is the current position.
func (s *nemStream) dial() (*nemConn, error) {
	ws, err := wsDial(s.ctx, s.e.URL, s.e.Token, DialTimeout)
	if err != nil {
		return nil, err
	}
	c := &nemConn{ws: ws, offset: s.pos, done: make(chan struct{})}
	c.confirmed.Store(s.pos)
	s.senders.Add(1)
	go s.read(c)
	return c, nil
}

// live makes c the stream's connection. Coming from the fallback, the
// fallback stream is closed (what it holds is still transcribed) and the
// panel's "Captions delayed" clears.
func (s *nemStream) live(c *nemConn) {
	back := s.mode == modeFallback
	s.conn, s.mode, s.attempts = c, modeLive, 0
	if back {
		s.closeFallback()
		s.e.logf("transcript: a track is back on Nemotron")
		s.emit(Segment{Kind: EngineSwitch, Engine: s.e.Name()})
	}
}

// toFallback is rule 1: the utterance engine at once, Nemotron again after
// max(MinRetry, Retry-After).
func (s *nemStream) toFallback(err error) {
	wait := MinRetry
	var ref *RefusedError
	if errors.As(err, &ref) && ref.RetryAfter > wait {
		wait = ref.RetryAfter
	}
	s.nextAt = s.e.now().Add(wait)
	s.conn = nil
	if s.mode == modeFallback {
		s.e.logf("transcript: Nemotron still unavailable (%v); retrying in %s", err, wait)
		return
	}
	s.mode = modeFallback
	s.e.logf("transcript: a track goes to the utterance engine (%v); retrying Nemotron in %s", err, wait)
	if s.e.Fallback != nil {
		fb, ferr := s.e.Fallback.Open(s.ctx, s.track)
		if ferr == nil {
			s.fb = fb
			s.forward(fb, s.pos)
		}
	}
	s.emit(Segment{Kind: EngineSwitch, Engine: s.fallbackName()})
}

func (s *nemStream) fallbackName() string {
	if s.e.Fallback != nil {
		return s.e.Fallback.Name()
	}
	return ""
}

// forward re-sends fb's segments at offset (fb's times start at its own
// first sample) and notes their words for the silent-start check.
func (s *nemStream) forward(fb Stream, offset int64) {
	s.senders.Add(1)
	go func() {
		defer s.senders.Done()
		for seg := range fb.Segments() {
			seg.StartMS += ms(offset)
			seg.EndMS += ms(offset)
			if seg.Engine == "" {
				seg.Engine = s.fallbackName()
			}
			if seg.Text != "" {
				s.words.Store(true)
			}
			s.emit(seg)
		}
	}()
}

func (s *nemStream) closeFallback() {
	if s.fb != nil {
		s.fb.Close()
		s.fb = nil
	}
}

// chunk is one piece of the track's audio, in order.
func (s *nemStream) chunk(pcm []int16) {
	evs := s.ep.Push(pcm)
	s.check(pcm)
	// A change of engine happens at this chunk's first sample (s.pos): a
	// new connection's offset, the fallback's, a gap's end.
	switch s.mode {
	case modeIdle:
		// The first audio after an idle end.
		s.attempt()
	case modeReconnect:
		if !s.e.now().Before(s.nextAt) {
			s.reconnect()
		}
	case modeFallback:
		if !s.e.now().Before(s.nextAt) {
			s.attempt()
		}
	}
	at := s.pos
	s.pos += int64(len(pcm))
	switch s.mode {
	case modeLive:
		if err := s.send(pcm); err != nil {
			s.drop(s.conn, at)
			return
		}
		for _, ev := range evs {
			if ev.Utterance != nil {
				// The endpointer found the end of an utterance (a pause of
				// SilenceEnd, or MaxUtterance): the server finalises it.
				if err := s.conn.ws.writeText(`{"type":"flush"}`); err != nil {
					s.drop(s.conn, s.pos)
					return
				}
			}
		}
	case modeFallback:
		if s.fb != nil {
			_ = s.fb.Write(pcm)
		}
	}
}

// send frames pcm onto the connection: whole frames only; the rest waits
// for the next chunk (or the padded last frame at `end`).
func (s *nemStream) send(pcm []int16) error {
	c := s.conn
	c.frame = append(c.frame, pcm...)
	for len(c.frame) >= FrameSamples {
		if err := c.ws.writeBinary(frameBytes(c.frame[:FrameSamples])); err != nil {
			return err
		}
		c.frame = c.frame[FrameSamples:]
		c.sent += FrameSamples
	}
	return nil
}

// frameBytes is samples as one wire frame: S16LE, zero-padded to
// FrameSamples.
func frameBytes(pcm []int16) []byte {
	b := make([]byte, FrameSamples*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

// endConn ends the live connection cleanly: the last frame zero-padded,
// `end` with the TRUE sample count, and the reader left to store the final.
func (s *nemStream) endConn() {
	c := s.conn
	s.conn = nil
	if c == nil {
		return
	}
	c.ending.Store(true)
	n := c.sent + int64(len(c.frame))
	if len(c.frame) > 0 {
		if err := c.ws.writeBinary(frameBytes(c.frame)); err != nil {
			c.ws.close(1001)
			return
		}
		c.frame = nil
	}
	if err := c.ws.writeText(`{"type":"end","samples":` + itoa(n) + `}`); err != nil {
		c.ws.close(1001)
		return
	}
	// The reader stores the last final and returns at the server's 1000
	// close; a server that never answers is cut off after EndTimeout.
	go func() {
		select {
		case <-c.done:
		case <-time.After(EndTimeout):
			c.ws.close(1001)
		case <-s.ctx.Done():
			c.ws.close(1001)
		}
	}()
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// drop is rule 2's start: c failed at track sample at. The audio from its
// last final to the reconnect is a gap.
func (s *nemStream) drop(c *nemConn, at int64) {
	if c == nil || s.conn != c {
		return
	}
	c.ws.close(1001)
	s.conn = nil
	why := "the socket failed"
	if r, ok := c.reason.Load().(string); ok && r != "" {
		why = "the server sent " + r
	}
	s.e.logf("transcript: a track's Nemotron stream dropped (%s); reconnecting", why)
	s.gapFrom = min(c.confirmed.Load(), at)
	// The caption in progress is gone with the connection.
	s.emit(Segment{Kind: Partial, StartMS: ms(s.gapFrom), EndMS: ms(s.gapFrom)})
	s.mode, s.attempts = modeReconnect, 0
	s.nextAt = s.e.now().Add(s.reconnects[0])
}

// dropped is a reader's report that its connection ended unasked.
func (s *nemStream) dropped(c *nemConn) {
	if s.conn == c && s.mode == modeLive {
		s.drop(c, s.pos)
	}
}

// reconnect is one of rule 2's attempts, at the current position.
func (s *nemStream) reconnect() {
	c, err := s.dial()
	if err == nil {
		s.closeGap()
		s.live(c)
		return
	}
	s.attempts++
	var ref *RefusedError
	if errors.As(err, &ref) || s.attempts >= len(s.reconnects) {
		// Refused (the server is full, or will not serve): rule 1 at once.
		// Or every reconnect failed: rule 1.
		s.closeGap()
		s.toFallback(err)
		return
	}
	s.nextAt = s.e.now().Add(s.reconnects[s.attempts])
}

// closeGap stores the audio lost since the drop as a gap marker.
func (s *nemStream) closeGap() {
	if s.pos > s.gapFrom {
		s.emit(Segment{Kind: Gap, StartMS: ms(s.gapFrom), EndMS: ms(s.pos), Engine: s.e.Name()})
	}
}

// finish is Close: the connection ended with `end`, a drop's gap stored, the
// fallback drained.
func (s *nemStream) finish() {
	switch s.mode {
	case modeLive:
		s.endConn()
	case modeReconnect:
		s.closeGap()
	case modeFallback:
		s.closeFallback()
	}
}

// serverMsg is one of the server's JSON lines.
type serverMsg struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Code    string `json:"code"`
}

// read is c's reader: partials and finals at c's offset; its end, unless
// the stream ended it, is a drop.
func (s *nemStream) read(c *nemConn) {
	defer s.senders.Done()
	defer close(c.done)
	off := ms(c.offset)
	for {
		data, text, err := c.ws.readMessage()
		if err != nil {
			var ce *CloseError
			if errors.As(err, &ce) && ce.Code == 1000 && c.ending.Load() {
				return // the end we asked for
			}
			c.ws.close(1001)
			if s.ctx.Err() == nil && !c.ending.Load() {
				select {
				case s.drops <- c:
				case <-s.runDone:
				case <-s.ctx.Done():
				}
			}
			return
		}
		if !text {
			continue
		}
		var m serverMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "partial":
			if m.Text = strings.TrimSpace(m.Text); m.Text == "" {
				continue
			}
			s.words.Store(true)
			s.emit(Segment{Kind: Partial, StartMS: off + m.StartMS, EndMS: off + m.EndMS, Text: m.Text, Engine: s.e.Name()})
		case "final":
			c.confirmed.Store(c.offset + m.EndMS*Rate/1000)
			if m.Text = strings.TrimSpace(m.Text); m.Text == "" {
				// Nothing was said (or a constant signal): no line, and
				// the caption in progress, if any, clears.
				s.emit(Segment{Kind: Partial, StartMS: off + m.StartMS, EndMS: off + m.EndMS})
				continue
			}
			s.words.Store(true)
			s.emit(Segment{Kind: Final, StartMS: off + m.StartMS, EndMS: off + m.EndMS, Text: m.Text, Engine: s.e.Name()})
		case "error":
			// The close follows; the code says why (bad_frame is a bug in
			// the plugin, never expected).
			c.reason.Store(m.Code)
			s.e.logf("transcript: the Nemotron server reported %q on a track's stream", m.Code)
		}
	}
}

// check is the silent-start check over one chunk (see the package comment
// above): speech energy with no word for the span, or one constant non-zero
// value for the span. Each flag is raised once per stretch: a word clears
// the first, a varying signal the second.
func (s *nemStream) check(pcm []int16) {
	speech := s.ep.SpeechSamples()
	if s.words.Swap(false) {
		s.speechBase, s.flaggedSS = speech, false
	}
	span := int64(s.silent/time.Millisecond) * Rate / 1000
	if !s.flaggedSS && speech-s.speechBase >= span {
		s.flaggedSS = true
		s.e.logf("transcript: a track had %s of speech energy and no words (silent start)", s.silent)
		s.emit(Segment{Kind: Flag, Text: FlagSilentStart, Engine: s.e.Name()})
	}
	for _, v := range pcm {
		if v != 0 && v == s.constVal {
			s.constRun++
			continue
		}
		// The value changed (or is zero: digital silence is not a dead
		// capture): a new stretch.
		s.constVal, s.constRun, s.flaggedDead = v, 0, false
		if v != 0 {
			s.constRun = 1
		}
	}
	if !s.flaggedDead && s.constRun >= span {
		s.flaggedDead = true
		s.e.logf("transcript: a track's audio was one constant level for %s (a dead capture)", s.silent)
		s.emit(Segment{Kind: Flag, Text: FlagDeadInput, Engine: s.e.Name()})
	}
}
