package stt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/stt"
	"github.com/ZacxDev/muster-meetings/internal/stttest"
)

// The nemotron engine against the fake Nemotron server (internal/stttest),
// which speaks muster's images/stt-nemotron protocol exactly.

const nemToken = "s3cret-stream-tok-77"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type fclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fclock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fclock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

// nem is one engine under test: its fake server, its fallback's fake STT
// service, its log, its clock, and a collector of its stream's segments.
type nem struct {
	t     *testing.T
	srv   *stttest.Server
	eng   *stt.NemotronEngine
	fb    *fakeSTT
	log   *syncBuf
	clock *fclock
	done  chan int64

	s    stt.Stream
	mu   sync.Mutex
	segs []stt.Segment
	end  chan struct{}
}

func newNem(t *testing.T) *nem {
	t.Helper()
	n := &nem{t: t, srv: stttest.New(nemToken), fb: &fakeSTT{answer: func(int) (string, error) { return "fallback line", nil }},
		log: &syncBuf{}, clock: &fclock{t: t0}, done: make(chan int64, 4096)}
	t.Cleanup(n.srv.Close)
	n.eng = stt.NewNemotron(n.srv.URL(), nemToken, stt.NewUtterance(n.fb), log.New(n.log, "", 0))
	n.eng.Now = n.clock.Now
	n.eng.Processed = func(pos int64) { n.done <- pos }
	return n
}

// open starts the stream and collects its segments.
func (n *nem) open() {
	n.t.Helper()
	s, err := n.eng.Open(context.Background(), stt.TrackRef{Track: "TR_nem"})
	if err != nil {
		n.t.Fatal(err)
	}
	n.s, n.end = s, make(chan struct{})
	go func() {
		defer close(n.end)
		for g := range s.Segments() {
			n.mu.Lock()
			n.segs = append(n.segs, g)
			n.mu.Unlock()
		}
	}()
	n.t.Cleanup(func() { n.s.Close() })
}

// feed writes pcm in 80 ms chunks (a shorter last one) and waits until the
// engine has handled each.
func (n *nem) feed(pcm []int16) {
	n.t.Helper()
	for i := 0; i < len(pcm); i += 1280 {
		if err := n.s.Write(pcm[i:min(i+1280, len(pcm))]); err != nil {
			n.t.Fatal(err)
		}
		select {
		case <-n.done:
		case <-time.After(10 * time.Second):
			n.t.Fatal("a chunk was not handled within 10 s")
		}
	}
}

// close ends the stream and waits for its segments to end.
func (n *nem) close() []stt.Segment {
	n.t.Helper()
	n.s.Close()
	select {
	case <-n.end:
	case <-time.After(15 * time.Second):
		n.t.Fatalf("the stream's segments did not end within 15 s: %+v", n.got())
	}
	return n.got()
}

func (n *nem) got() []stt.Segment {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.segs)
}

// waitSeg waits until a segment matching ok has arrived and answers it.
func (n *nem) waitSeg(what string, d time.Duration, ok func(stt.Segment) bool) stt.Segment {
	n.t.Helper()
	deadline := time.Now().Add(d)
	for {
		for _, g := range n.got() {
			if ok(g) {
				return g
			}
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("no %s within %s: %+v", what, d, n.got())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func finals(segs []stt.Segment) []stt.Segment {
	var out []stt.Segment
	for _, g := range segs {
		if g.Kind == stt.Final {
			out = append(out, g)
		}
	}
	return out
}

func kindsOf(segs []stt.Segment, k stt.Kind) []stt.Segment {
	var out []stt.Segment
	for _, g := range segs {
		if g.Kind == k {
			out = append(out, g)
		}
	}
	return out
}

// "The Nemotron protocol" (design: plugin unit tests): every binary frame is
// exactly 2560 bytes; the last is the true tail zero-padded to 1280
// samples; `end` carries the TRUE count; partials and finals come back at
// the server's sample times; the end's final ends at the true count, not
// the padded one; and the stream was opened with the bearer.
func TestTheNemotronProtocolFramesPaddingAndSampleTimes(t *testing.T) {
	n := newNem(t)
	n.open()
	// 1.5 s of speech, 1 s of silence, 1.8 s of speech: 68800 samples, 53
	// whole frames and a 960-sample tail that is speech (not zeros, so its
	// padding shows).
	pcm := cat(tone(1500, 5000, 0), level(1000, 0), tone(1800, 5000, 0))
	n.feed(pcm)
	segs := n.close()

	recs := n.srv.Records()
	if len(recs) != 1 {
		t.Fatalf("%d streams opened, want 1", len(recs))
	}
	r := recs[0]
	if r.Auth != "Bearer "+nemToken {
		t.Fatalf("the stream was opened with Authorization %q", r.Auth)
	}
	if len(r.Frames) != 54 {
		t.Fatalf("%d frames, want 54 (53 whole and the padded tail)", len(r.Frames))
	}
	for i, f := range r.Frames {
		if f != 2560 {
			t.Fatalf("frame %d is %d bytes, want 2560", i, f)
		}
	}
	if r.End != 68800 || r.Total != 54*1280 {
		t.Fatalf("end.samples = %d with %d received, want the true 68800 (not the padded %d)", r.End, r.Total, 54*1280)
	}
	if !slices.Equal(r.Last[:960], pcm[68800-960:]) || slices.ContainsFunc(r.Last[960:], func(v int16) bool { return v != 0 }) {
		t.Fatal("the last frame is not the true tail followed by zeros")
	}
	fs := finals(segs)
	if len(fs) != 2 || fs[0].Text != stttest.DefaultLines[0] || fs[1].Text != stttest.DefaultLines[1] {
		t.Fatalf("finals = %+v, want the server's two lines", fs)
	}
	if len(r.Flushes) != 1 {
		t.Fatalf("%d flushes, want 1 (the pause)", len(r.Flushes))
	}
	cut := r.Flushes[0] * 1000 / 16000
	if fs[0].StartMS != 0 || fs[0].EndMS != cut || fs[1].StartMS != cut || fs[1].EndMS != 4300 {
		t.Fatalf("finals at %d–%d and %d–%d ms, want 0–%d and %d–4300 (the true end, not the padded 4320)", fs[0].StartMS, fs[0].EndMS, fs[1].StartMS, fs[1].EndMS, cut, cut)
	}
	ps := kindsOf(segs, stt.Partial)
	if len(ps) == 0 || !strings.HasPrefix(stttest.DefaultLines[0], ps[0].Text) || ps[0].Text == "" || ps[0].Engine != "nemotron" {
		t.Fatalf("partials = %+v, want the first line growing", ps)
	}
	for _, g := range fs {
		if g.Engine != "nemotron" {
			t.Fatalf("a final's engine is %q", g.Engine)
		}
	}
}

// "Pause finalisation" for the nemotron engine: speech, 1 s of silence,
// speech makes the plugin send ONE flush about 700 ms into the silence (and
// the same with a DC offset on every sample), and the first utterance's
// final arrives BEFORE the second utterance is sent.
func TestAFlushIsSentAbout700msIntoThePauseEvenWithADCOffset(t *testing.T) {
	for _, dc := range []float64{0, 6000} {
		n := newNem(t)
		n.open()
		n.feed(cat(tone(1500, 5000, dc), level(1000, int16(dc))))
		first := n.waitSeg(fmt.Sprintf("dc %v: final before the second utterance", dc), 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
		n.feed(tone(1500, 5000, dc))
		n.close()
		r := n.srv.Records()[0]
		if len(r.Flushes) != 1 {
			t.Fatalf("dc %v: %d flushes, want 1", dc, len(r.Flushes))
		}
		// 700 ms after the last speech (1500 ms), at the frame it falls in.
		if at := r.Flushes[0] * 1000 / 16000; at < 2200 || at > 2320 {
			t.Fatalf("dc %v: the flush came when %d ms had been sent, want about 2200", dc, at)
		}
		if first.Text != stttest.DefaultLines[0] {
			t.Fatalf("dc %v: the first final is %+v", dc, first)
		}
	}
}

// 🔴 Rule 1 (design: "Serving Nemotron"): a 503 + Retry-After at stream
// open sends the participant to the utterance engine AT ONCE ("Captions
// delayed": an EngineSwitch to utterance), and Nemotron is tried again only
// after max(30 s, Retry-After): a Retry-After of 60 waits 60 s, one of 10
// waits 30 s. When a stream then opens, the participant moves back.
func TestA503SendsTheParticipantToUtteranceAndRetriesAfterMax30sRetryAfter(t *testing.T) {
	for _, c := range []struct {
		ra            int
		early, onTime time.Duration
	}{{60, 59 * time.Second, 60 * time.Second}, {10, 29 * time.Second, 30 * time.Second}} {
		n := newNem(t)
		n.srv.Set(func(s *stttest.Server) { s.Refuse, s.RetryAfter = 503, c.ra })
		n.open()
		n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
		sw := n.waitSeg("the switch to utterance", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
		if sw.Engine != "utterance" {
			t.Fatalf("Retry-After %d: the first switch is to %q, want utterance", c.ra, sw.Engine)
		}
		fl := n.waitSeg("the fallback's line", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
		if fl.Text != "fallback line" || fl.Engine != "utterance" {
			t.Fatalf("Retry-After %d: the fallback's final is %+v", c.ra, fl)
		}
		if got := n.srv.Refusals(); !slices.Equal(got, []int{503}) {
			t.Fatalf("Retry-After %d: refusals %v, want one 503", c.ra, got)
		}
		n.srv.Set(func(s *stttest.Server) { s.Refuse = 0 })
		n.clock.Set(t0.Add(c.early))
		n.feed(level(80, 0))
		if u := n.srv.Upgrades(); u != 0 || len(n.srv.Refusals()) != 1 {
			t.Fatalf("Retry-After %d: Nemotron was tried again at %s (upgrades %d, refusals %v), want not before %s",
				c.ra, c.early, u, n.srv.Refusals(), c.onTime)
		}
		n.clock.Set(t0.Add(c.onTime))
		n.feed(level(80, 0))
		if u := n.srv.Upgrades(); u != 1 {
			t.Fatalf("Retry-After %d: Nemotron was not tried again at %s (upgrades %d)", c.ra, c.onTime, u)
		}
		back := n.waitSeg("the switch back", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch && g.Engine == "nemotron" })
		_ = back
		// Back on Nemotron: the next utterance is the server's.
		n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
		n.waitSeg("a Nemotron final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final && g.Engine == "nemotron" })
		n.close()
	}
}

// "STT at capacity": with the server full at 8, a ninth participant goes to
// the utterance engine while the first eight stay on nemotron; when a slot
// frees, the ninth is back within one 30 s retry.
func TestTheNinthParticipantFallsBackAndReturnsWhenASlotFrees(t *testing.T) {
	n := newNem(t)
	var streams []*nem
	for i := 0; i < 9; i++ {
		m := &nem{t: t, srv: n.srv, eng: n.eng, done: n.done}
		m.open()
		streams = append(streams, m)
		eventually(t, "stream "+fmt.Sprint(i+1)+" decided", func() bool { return n.srv.Upgrades()+len(n.srv.Refusals()) == i+1 })
	}
	if n.srv.Upgrades() != 8 || !slices.Equal(n.srv.Refusals(), []int{503}) {
		t.Fatalf("upgrades %d, refusals %v; want 8 and one 503", n.srv.Upgrades(), n.srv.Refusals())
	}
	ninth := streams[8]
	ninth.waitSeg("the ninth's switch", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch && g.Engine == "utterance" })
	for _, m := range streams[:8] {
		if len(kindsOf(m.got(), stt.EngineSwitch)) != 0 {
			t.Fatal("one of the first eight left nemotron")
		}
	}
	streams[0].close()
	eventually(t, "the slot freed", func() bool { return n.srv.Active() == 7 })
	n.clock.Set(t0.Add(30 * time.Second))
	ninth.feed(level(80, 0))
	ninth.waitSeg("the ninth's return", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch && g.Engine == "nemotron" })
	for _, m := range streams[1:] {
		m.close()
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("not within 5 s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dropped cuts the live stream and waits until the engine has seen it (the
// caption in progress is cleared: an empty partial).
func (n *nem) dropped(cut func()) {
	n.t.Helper()
	before := len(kindsOf(n.got(), stt.Partial))
	cut()
	n.waitSeg("the drop", 5*time.Second, func(stt.Segment) bool {
		ps := kindsOf(n.got(), stt.Partial)
		return len(ps) > before && ps[len(ps)-1].Text == ""
	})
}

// 🔴 Rule 2: a mid-stream drop (the connection cut after the upgrade)
// reconnects at most 3 times, at 1, 2 and 4 s; when all 3 fail to connect,
// the participant goes to the utterance engine (rule 1) with a gap marker
// over the audio no engine took.
func TestAMidStreamDropReconnectsThreeTimesThenFallsBackWithAGap(t *testing.T) {
	n := newNem(t)
	n.open()
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	first := n.waitSeg("the first final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
	n.srv.Set(func(s *stttest.Server) { s.Hangup = true })
	n.dropped(n.srv.DropAll)
	// Audio keeps coming; each chunk is handled at the clock's time.
	at := func(d time.Duration, want int) {
		t.Helper()
		n.clock.Set(t0.Add(d))
		n.feed(level(80, 0))
		if got := n.srv.Hangups(); got != want {
			t.Fatalf("at +%s: %d reconnects tried, want %d", d, got, want)
		}
	}
	at(0, 0)
	at(999*time.Millisecond, 0)
	at(time.Second, 1)
	at(2999*time.Millisecond, 1)
	at(3*time.Second, 2)
	at(6999*time.Millisecond, 2)
	if len(kindsOf(n.got(), stt.EngineSwitch)) != 0 {
		t.Fatal("the participant fell back before the third reconnect failed")
	}
	at(7*time.Second, 3)
	sw := n.waitSeg("the fallback", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
	if sw.Engine != "utterance" {
		t.Fatalf("switched to %q, want utterance", sw.Engine)
	}
	// Only silence followed the last final (no flush unanswered, no
	// utterance in progress): the gap starts at the drop, 2000 ms.
	gaps := kindsOf(n.got(), stt.Gap)
	if len(gaps) != 1 || gaps[0].StartMS != 2000 || gaps[0].EndMS <= gaps[0].StartMS {
		t.Fatalf("gaps = %+v, want one from the drop (2000 ms) to the fallback (the last final ended at %d)", gaps, first.EndMS)
	}
	// No fourth reconnect: the next try is rule 1's, 30 s on.
	at(11*time.Second, 3)
	at(36*time.Second, 3)
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	n.waitSeg("the fallback's line", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final && g.Engine == "utterance" })
	n.close()
}

// A drop that reconnects: the gap covers the audio between the last final
// and the new connection, and the new connection's times are placed at ITS
// first sample (the server counts from zero again). An error close
// (bad_frame, 1003) is a drop too, and is logged by its code.
func TestAReconnectedStreamsTimesStartAtItsFirstSample(t *testing.T) {
	n := newNem(t)
	n.open()
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	first := n.waitSeg("the first final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
	n.dropped(func() { n.srv.CloseAll("bad_frame", 1003) })
	n.feed(level(400, 0)) // lost: 2000–2400 ms
	n.clock.Set(t0.Add(time.Second))
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0))) // the new connection takes it from 2400 ms
	second := n.waitSeg("the second final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final && g.StartMS > first.EndMS })
	n.close()
	if n.srv.Upgrades() != 2 {
		t.Fatalf("%d upgrades, want 2", n.srv.Upgrades())
	}
	gaps := kindsOf(n.got(), stt.Gap)
	if len(gaps) != 1 || gaps[0].StartMS != 2000 || gaps[0].EndMS != 2400 {
		t.Fatalf("gaps = %+v, want 2000–2400 (the drop to the reconnect; the last final ended at %d)", gaps, first.EndMS)
	}
	if second.StartMS != 2400 || second.EndMS < 3400 {
		t.Fatalf("the reconnected stream's final is %d–%d ms, want from 2400 (its first sample)", second.StartMS, second.EndMS)
	}
	if !strings.Contains(n.log.String(), "bad_frame") {
		t.Fatalf("the log does not name the error: %s", n.log.String())
	}
	if len(kindsOf(n.got(), stt.EngineSwitch)) != 0 {
		t.Fatal("a reconnect that worked fell back")
	}
}

// 🔴 The stream token is never logged, nor carried by an error: a refused
// stream (401: a wrong token; 503) and a drop each log a line naming the
// status or code (the positive control), and no line holds the token.
func TestTheStreamTokenIsNeverLogged(t *testing.T) {
	n := newNem(t)
	n.srv.Set(func(s *stttest.Server) { s.Token = "a-different-token" })
	n.open()
	n.feed(level(80, 0))
	n.waitSeg("the fallback", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
	n.srv.Set(func(s *stttest.Server) { s.Token, s.Refuse = nemToken, 503 })
	n.clock.Set(t0.Add(30 * time.Second))
	n.feed(level(80, 0))
	n.srv.Set(func(s *stttest.Server) { s.Refuse = 0 })
	n.clock.Set(t0.Add(60 * time.Second))
	n.feed(level(80, 0))
	n.waitSeg("the return", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch && g.Engine == "nemotron" })
	n.dropped(func() { n.srv.CloseAll("overloaded", 1013) })
	n.close()
	logs := n.log.String()
	for _, want := range []string{"401", "503", "overloaded"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("the log never names %q (the positive control): %s", want, logs)
		}
	}
	if strings.Contains(logs, nemToken) {
		t.Fatalf("the log carries the stream token: %s", logs)
	}
	if !slices.Equal(n.srv.Refusals(), []int{401, 503}) {
		t.Fatalf("refusals %v, want 401 then 503", n.srv.Refusals())
	}
}

// The silent-start check (design: "Silent start and empty-output check"):
// speech energy and no words for 20 s raises silent_start; a stuck non-zero
// level for 20 s (no energy after the DC filter) raises dead_input; and
// neither is raised by digital silence, by speech that has words, or by 19
// s of wordless speech (the boundary's other side).
func TestTheSilentStartCheck(t *testing.T) {
	for _, c := range []struct {
		name  string
		pcm   []int16
		lines []string
		want  []string
	}{
		{"21 s of speech and no words", tone(21000, 5000, 0), []string{""}, []string{stt.FlagSilentStart}},
		{"19 s of speech and no words", tone(19000, 5000, 0), []string{""}, nil},
		{"21 s of a stuck level", level(21000, 12000), nil, []string{stt.FlagDeadInput}},
		{"21 s of a stuck negative level", level(21000, -9000), nil, []string{stt.FlagDeadInput}},
		{"19 s of a stuck level", level(19000, 12000), nil, nil},
		{"21 s of digital silence", level(21000, 0), nil, nil},
		{"21 s of speech with words", tone(21000, 5000, 0), nil, nil},
	} {
		n := newNem(t)
		if c.lines != nil {
			n.srv.Set(func(s *stttest.Server) { s.Lines = c.lines })
		}
		n.open()
		n.feed(c.pcm)
		var got []string
		for _, g := range kindsOf(n.close(), stt.Flag) {
			got = append(got, g.Text)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: flags %q, want %q", c.name, got, c.want)
		}
	}
}

// When the ingest stops delivering audio (the publisher went away) the
// stream is ENDED cleanly before the server's 30 s idle close (a padded last
// frame, `end`, its final kept), never kept open with invented silence; the
// next audio opens a new connection whose times continue at the track's
// own sample count.
func TestAQuietIngestEndsTheStreamAndTheNextAudioReopensIt(t *testing.T) {
	n := newNem(t)
	n.eng.IdleEnd = 150 * time.Millisecond
	n.open()
	n.feed(tone(1000, 5000, 0)) // no pause: only `end` finalises it
	eventually(t, "the idle end", func() bool { r := n.srv.Records(); return len(r) == 1 && r[0].End == 16000 })
	n.waitSeg("the end's final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final && g.EndMS == 1000 })
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	second := n.waitSeg("the new connection's final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final && g.StartMS >= 1000 })
	n.close()
	if n.srv.Upgrades() != 2 || second.StartMS != 1000 {
		t.Fatalf("upgrades %d, the second final at %d ms; want 2 and 1000", n.srv.Upgrades(), second.StartMS)
	}
	if r := n.srv.Records()[0]; r.Close != 1000 {
		t.Fatalf("the first stream closed with %d, want the server's 1000 after `end` (never its idle 1001)", r.Close)
	}
}

// 🔴 A 1013 close (`overloaded`: the server's GPU is not keeping up) is a
// refusal, not a drop: the participant goes to the utterance engine at once
// and Nemotron is tried again only after 30 s. Reconnecting instead would
// let a server that accepts and then sheds every stream keep the
// participant reconnecting forever (six sheds: seven upgrades, never a
// fallback).
func TestAnOverloadedCloseSendsTheParticipantToTheFallback(t *testing.T) {
	n := newNem(t)
	n.open()
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	n.waitSeg("the first final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
	n.dropped(func() { n.srv.CloseAll("overloaded", 1013) })
	sw := n.waitSeg("the fallback", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
	if sw.Engine != "utterance" {
		t.Fatalf("switched to %q", sw.Engine)
	}
	for _, d := range []time.Duration{time.Second, 3 * time.Second, 7 * time.Second, 29 * time.Second} {
		n.clock.Set(t0.Add(d))
		n.feed(level(80, 0))
		if u := n.srv.Upgrades(); u != 1 {
			t.Fatalf("at +%s: %d upgrades, want 1 (no reconnect after an overloaded close)", d, u)
		}
	}
	n.clock.Set(t0.Add(30 * time.Second))
	n.feed(level(80, 0))
	if u := n.srv.Upgrades(); u != 2 {
		t.Fatalf("at +30s: %d upgrades, want 2 (rule 1's retry)", u)
	}
	n.close()
}

// The reconnect budget comes back only with a connection that delivered a
// final: connections closed again before any final (here bad_frame every
// time, as a plugin bug would) use it up, 3 reconnects at 1, 2 and 4 s,
// and the fourth drop sends the participant to the fallback.
func TestStreamsThatDropBeforeAFinalUseUpTheReconnects(t *testing.T) {
	n := newNem(t)
	n.open()
	n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
	n.waitSeg("the first final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
	at := time.Duration(0)
	for i, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		n.dropped(func() { n.srv.CloseAll("bad_frame", 1003) })
		at += wait
		n.clock.Set(t0.Add(at))
		n.feed(level(80, 0))
		if u := n.srv.Upgrades(); u != i+2 {
			t.Fatalf("reconnect %d at +%s: %d upgrades, want %d", i+1, at, u, i+2)
		}
		if len(kindsOf(n.got(), stt.EngineSwitch)) != 0 {
			t.Fatalf("fell back after %d drops, want after 4", i+1)
		}
	}
	n.dropped(func() { n.srv.CloseAll("bad_frame", 1003) })
	sw := n.waitSeg("the fallback after the fourth drop", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
	if sw.Engine != "utterance" {
		t.Fatalf("switched to %q", sw.Engine)
	}
	at += 8 * time.Second
	n.clock.Set(t0.Add(at))
	n.feed(level(80, 0))
	if u := n.srv.Upgrades(); u != 4 {
		t.Fatalf("%d upgrades after the fallback, want 4 (no fifth before rule 1's 30 s)", u)
	}
	n.close()
}

// Where a drop's gap starts: words may have been lost only in audio no final
// covers that held speech. After 10 minutes of silence it starts at the drop
// (not at the last final, ten minutes back); mid-utterance at that
// utterance's start; with a flush still unanswered at the last final.
func TestADropsGapStartsWhereWordsMayHaveBeenLost(t *testing.T) {
	t.Run("after 10 minutes of silence", func(t *testing.T) {
		n := newNem(t)
		n.open()
		n.feed(cat(tone(1000, 5000, 0), level(1000, 0)))
		n.waitSeg("the first final", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.Final })
		n.feed(level(600000, 0))
		n.dropped(n.srv.DropAll)
		n.feed(level(400, 0)) // lost: no connection until the reconnect
		n.clock.Set(t0.Add(time.Second))
		n.feed(level(80, 0))
		gaps := kindsOf(n.close(), stt.Gap)
		// The drop is at 602000 ms; the reconnect at 602400.
		if len(gaps) != 1 || gaps[0].StartMS != 602000 || gaps[0].EndMS != 602400 {
			t.Fatalf("gaps = %+v, want one from the drop (602000 ms) to the reconnect (602400)", gaps)
		}
	})
	t.Run("mid-utterance", func(t *testing.T) {
		n := newNem(t)
		n.open()
		n.feed(cat(level(2000, 0), tone(1200, 5000, 0)))
		n.dropped(n.srv.DropAll)
		n.clock.Set(t0.Add(time.Second))
		n.feed(level(80, 0))
		gaps := kindsOf(n.close(), stt.Gap)
		// The utterance began at 2000 ms, its pre-roll 200 ms before.
		if len(gaps) != 1 || gaps[0].StartMS != 1800 {
			t.Fatalf("gaps = %+v, want one from the utterance's start (1800 ms)", gaps)
		}
	})
	t.Run("a flush unanswered", func(t *testing.T) {
		n := newNem(t)
		n.srv.Set(func(s *stttest.Server) { s.FinalDelay = 2 * time.Second })
		n.open()
		n.feed(cat(level(1000, 0), tone(1000, 5000, 0), level(1000, 0)))
		eventually(t, "the flush", func() bool { r := n.srv.Records(); return len(r) == 1 && len(r[0].Flushes) == 1 })
		n.dropped(n.srv.DropAll)
		n.clock.Set(t0.Add(time.Second))
		n.feed(level(80, 0))
		gaps := kindsOf(n.close(), stt.Gap)
		if len(gaps) != 1 || gaps[0].StartMS != 0 {
			t.Fatalf("gaps = %+v, want one from the last confirmed point (0 ms: the flushed utterance's final never came)", gaps)
		}
	})
}

// failEngine is a fallback that cannot open a stream.
type failEngine struct{}

func (failEngine) Name() string { return "utterance" }
func (failEngine) Open(context.Context, stt.TrackRef) (stt.Stream, error) {
	return nil, fmt.Errorf("no STT service configured")
}

// A fallback that fails to open is said in the log (the track is then not
// transcribed until Nemotron takes it again), not passed over in silence.
func TestAFallbackThatFailsToOpenIsLogged(t *testing.T) {
	n := newNem(t)
	n.eng.Fallback = failEngine{}
	n.srv.Set(func(s *stttest.Server) { s.Refuse = 503 })
	n.open()
	n.feed(level(80, 0))
	n.waitSeg("the switch", 5*time.Second, func(g stt.Segment) bool { return g.Kind == stt.EngineSwitch })
	n.close()
	if !strings.Contains(n.log.String(), "the fallback engine did not open a stream (no STT service configured)") {
		t.Fatalf("the log does not say the fallback failed: %s", n.log.String())
	}
}

// finalsSent is the fake server's finals on its first stream, as
// (text, lead, whether lead was present).
func finalsSent(t *testing.T, srv *stttest.Server) [][3]string {
	t.Helper()
	var out [][3]string
	for _, m := range srv.Records()[0].Sent {
		var f struct {
			Type string  `json:"type"`
			Text string  `json:"text"`
			Lead *string `json:"lead"`
		}
		if json.Unmarshal([]byte(m), &f) != nil || f.Type != "final" {
			continue
		}
		lead, has := "", "absent"
		if f.Lead != nil {
			lead, has = *f.Lead, "present"
		}
		out = append(out, [3]string{f.Text, lead, has})
	}
	return out
}

// Late closing marks, as the server measured them: a sentence's mark is
// decoded only when the NEXT speech begins, so the fake sends it as the
// next final's `lead` (every final carries `lead`; no partial shows the
// mark), and the engine hands it on as that Final's Lead, its Text clean. A
// mark still held when the stream ends comes with the end's final, which
// has no text: a Final with only a Lead.
func TestLateMarksComeAsTheNextFinalsLead(t *testing.T) {
	n := newNem(t)
	n.srv.Set(func(s *stttest.Server) {
		s.Lines = []string{"Can everyone hear me clearly?", "Alice will send it, Friday."}
	})
	n.open()
	n.feed(cat(tone(1500, 5000, 0), level(1000, 0), tone(1500, 5000, 0), level(1000, 0)))
	segs := n.close()

	want := [][3]string{{"Can everyone hear me clearly", "", "present"}, {"Alice will send it, Friday", "?", "present"}, {"", ".", "present"}}
	if got := finalsSent(t, n.srv); !slices.Equal(got, want) {
		t.Fatalf("the server's finals %q, want %q", got, want)
	}
	for _, m := range n.srv.Records()[0].Sent {
		if strings.Contains(m, `"partial"`) && (strings.ContainsAny(m, "?") || strings.Contains(m, "lead")) {
			t.Fatalf("a partial with a late mark or a lead: %s", m)
		}
	}
	var got [][2]string
	for _, g := range finals(segs) {
		got = append(got, [2]string{g.Text, g.Lead})
	}
	if w := [][2]string{{"Can everyone hear me clearly", ""}, {"Alice will send it, Friday", "?"}, {"", "."}}; !slices.Equal(got, w) {
		t.Fatalf("the engine's finals %q, want %q", got, w)
	}
}

// An older server sends no `lead`: the late mark opens the next final's
// text (". Thanks"), and its captions may open with it too. The engine
// moves the marks to Lead, so the line and the caption are clean.
func TestAnOlderServersLeadingMarksBecomeTheLead(t *testing.T) {
	n := newNem(t)
	n.srv.Set(func(s *stttest.Server) { s.LeadInText, s.Lines = true, []string{"We should start.", "? Thanks"} })
	n.open()
	n.feed(cat(tone(1500, 5000, 0), level(1000, 0), tone(1500, 5000, 0), level(1000, 0)))
	segs := n.close()
	if got := finalsSent(t, n.srv); len(got) < 2 || got[1] != [3]string{". ? Thanks", "", "absent"} {
		t.Fatalf("the older server's finals %q", got)
	}
	var got [][2]string
	for _, g := range finals(segs) {
		got = append(got, [2]string{g.Text, g.Lead})
	}
	if w := [][2]string{{"We should start", ""}, {"Thanks", ".?"}}; !slices.Equal(got, w) {
		t.Fatalf("the engine's finals %q, want %q", got, w)
	}
	for _, g := range kindsOf(segs, stt.Partial) {
		if strings.HasPrefix(g.Text, "?") || strings.HasPrefix(g.Text, ".") {
			t.Fatalf("a caption opens with a mark: %q", g.Text)
		}
	}
}
