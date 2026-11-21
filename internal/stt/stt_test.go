package stt_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/stt"
)

// tone is ms of a 300 Hz sine at amplitude a, plus dc, at 16 kHz.
func tone(ms int, a, dc float64) []int16 {
	n := ms * 16
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(a*math.Sin(2*math.Pi*300*float64(i)/16000) + dc)
	}
	return s
}

// level is ms of a constant level (digital silence at 0; a DC offset else).
func level(ms int, v int16) []int16 {
	s := make([]int16, ms*16)
	for i := range s {
		s[i] = v
	}
	return s
}

func cat(parts ...[]int16) []int16 {
	var out []int16
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// push feeds pcm in 80 ms chunks, as the ingest does, and collects the
// ended utterances and where (in samples consumed) each was cut.
func push(e *stt.Endpointer, pcm []int16) (us []stt.Utterance, at []int64) {
	for i := 0; i < len(pcm); i += 1280 {
		for _, ev := range e.Push(pcm[i:min(i+1280, len(pcm))]) {
			if ev.Utterance != nil {
				us = append(us, *ev.Utterance)
				at = append(at, e.Pos())
			}
		}
	}
	return us, at
}

// "Pause finalisation" (design: plugin unit tests): speech, 1 s of silence,
// speech makes the endpointer end the first utterance about 700 ms into the
// silence, BEFORE the second begins; and the same with a DC offset added to
// every sample (a clipped or offset microphone) still ends it there.
func TestAPauseEndsTheUtteranceAbout700msIntoTheSilenceEvenWithADCOffset(t *testing.T) {
	for _, dc := range []float64{0, 6000} {
		e := stt.NewEndpointer()
		pcm := cat(tone(1500, 5000, dc), level(1000, int16(dc)), tone(1500, 5000, dc))
		us, at := push(e, pcm)
		if len(us) != 1 {
			t.Fatalf("dc %v: %d utterances ended before the stream did, want 1 (the pause must end the first)", dc, len(us))
		}
		// Cut 700 ms after the last speech (1500 ms), ±one 80 ms chunk.
		if cut := at[0] * 1000 / 16000; cut < 2200 || cut > 2300 {
			t.Errorf("dc %v: the first utterance ended when %d ms had been read, want about 2200 (700 ms into the silence)", dc, cut)
		}
		if us[0].StartMS() > 0 || us[0].EndMS() < 1500 || us[0].EndMS() > 1700 {
			t.Errorf("dc %v: the first utterance is %d–%d ms, want 0–about 1600", dc, us[0].StartMS(), us[0].EndMS())
		}
		rest := e.Flush()
		if len(rest) != 1 || rest[0].Utterance == nil || rest[0].Utterance.StartMS() < 2300 || rest[0].Utterance.StartMS() > 2500 {
			t.Errorf("dc %v: the stream's end gave %+v, want the second utterance starting about 2300 ms (200 ms of pre-roll)", dc, rest)
		}
	}
}

// A constant signal (digital silence, or a stuck level) is never speech,
// whatever its level; a short click is no utterance.
func TestSilenceAConstantLevelAndAClickAreNoUtterance(t *testing.T) {
	for name, pcm := range map[string][]int16{
		"digital silence":   level(3000, 0),
		"a constant level":  level(3000, 12000),
		"a 40 ms click":     cat(level(1000, 0), tone(40, 9000, 0), level(2000, 0)),
		"very quiet speech": tone(3000, 300, 0),
	} {
		e := stt.NewEndpointer()
		us, _ := push(e, pcm)
		for _, ev := range e.Flush() {
			if ev.Utterance != nil {
				us = append(us, *ev.Utterance)
			}
		}
		if len(us) != 0 {
			t.Errorf("%s: %d utterances, want none", name, len(us))
		}
	}
}

// An utterance is at most 15 s: 20 s of unbroken speech is cut at 15 s and
// goes on as a second.
func TestAnUtteranceIsCutAt15Seconds(t *testing.T) {
	e := stt.NewEndpointer()
	us, _ := push(e, tone(20000, 5000, 0))
	if len(us) != 1 || us[0].StartMS() != 0 || us[0].EndMS() != 15000 || len(us[0].PCM) != 15*16000 {
		t.Fatalf("20 s of speech: %d utterances (first %d–%d ms), want one cut at 15000 ms", len(us), us[0].StartMS(), us[0].EndMS())
	}
	rest := e.Flush()
	if len(rest) != 1 || rest[0].Utterance.StartMS() != 15000 {
		t.Fatalf("the rest = %+v, want an utterance from 15000 ms", rest)
	}
}

// ---- the utterance engine ----

type fakeSTT struct {
	mu       sync.Mutex
	calls    [][]int16
	inFlight atomic.Int32
	most     atomic.Int32
	gate     chan struct{} // when set, each call waits for one value
	answer   func(n int) (string, error)
}

func (f *fakeSTT) Transcribe(ctx context.Context, pcm []int16) (string, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.most.Load()
		if n <= m || f.most.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, pcm)
	i := len(f.calls)
	f.mu.Unlock()
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.answer != nil {
		return f.answer(i)
	}
	return "line", nil
}

func collect(t *testing.T, s stt.Stream, d time.Duration) []stt.Segment {
	t.Helper()
	var out []stt.Segment
	timeout := time.After(d)
	for {
		select {
		case seg, ok := <-s.Segments():
			if !ok {
				return out
			}
			out = append(out, seg)
		case <-timeout:
			t.Fatalf("the stream did not end within %s (got %+v)", d, out)
		}
	}
}

func writeAll(t *testing.T, s stt.Stream, pcm []int16) {
	t.Helper()
	for i := 0; i < len(pcm); i += 1280 {
		if err := s.Write(pcm[i:min(i+1280, len(pcm))]); err != nil {
			t.Fatal(err)
		}
	}
}

// Each utterance is sent to the STT service as its own request, and comes
// back as one final at the utterance's sample times; an empty answer is
// nothing, and a failure is a gap over the utterance.
func TestTheUtteranceEngineSendsEachUtteranceAndAnswersFinalsAndGaps(t *testing.T) {
	f := &fakeSTT{answer: func(n int) (string, error) {
		switch n {
		case 1:
			return "  Ada speaks first.  ", nil
		case 2:
			return "", nil
		default:
			return "", errors.New("the service is down")
		}
	}}
	e := stt.NewUtterance(f)
	s, _ := e.Open(context.Background(), stt.TrackRef{Track: "TR_x"})
	writeAll(t, s, cat(tone(1000, 5000, 0), level(1000, 0), tone(1000, 5000, 0), level(1000, 0), tone(1000, 5000, 0), level(1000, 0)))
	s.Close()
	segs := collect(t, s, 5*time.Second)
	if len(f.calls) != 3 {
		t.Fatalf("%d requests, want 3 (one per utterance)", len(f.calls))
	}
	if len(segs) != 2 || segs[0].Kind != stt.Final || segs[0].Text != "Ada speaks first." || segs[0].StartMS != 0 ||
		segs[1].Kind != stt.Gap || segs[1].StartMS < 3800 || segs[1].StartMS > 4000 {
		t.Fatalf("segments = %+v, want the final, nothing for the empty answer, and a gap from about 3800 ms", segs)
	}
}

// At most 4 utterances are in flight across the whole plugin, however many
// streams there are (it shares stt-cohere with dictation).
func TestAtMostFourUtterancesAreInFlight(t *testing.T) {
	f := &fakeSTT{gate: make(chan struct{})}
	e := stt.NewUtterance(f)
	var streams []stt.Stream
	for i := 0; i < 7; i++ {
		s, _ := e.Open(context.Background(), stt.TrackRef{Track: "TR_x"})
		writeAll(t, s, cat(tone(1000, 5000, 0), level(1000, 0)))
		streams = append(streams, s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.inFlight.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := f.inFlight.Load(); n != 4 {
		t.Fatalf("%d transcriptions in flight with 7 waiting, want exactly 4", n)
	}
	close(f.gate)
	for _, s := range streams {
		s.Close()
		collect(t, s, 5*time.Second)
	}
	if m := f.most.Load(); m != 4 {
		t.Fatalf("at most %d were ever in flight, want 4", m)
	}
}

// One participant's queue holds 30 s of audio: past it the OLDEST queued
// utterance is dropped and stands as a gap marker in its place, in order.
func TestAFullQueueDropsTheOldestWithAGapMarker(t *testing.T) {
	f := &fakeSTT{gate: make(chan struct{})}
	e := stt.NewUtterance(f)
	s, _ := e.Open(context.Background(), stt.TrackRef{Track: "TR_x"})
	// Five utterances of 9 s each (about 9.3 s with pre-roll and tail): the
	// first is in flight, then four queue, about 37 s > 30 s.
	utt := cat(tone(9000, 5000, 0), level(1000, 0))
	writeAll(t, s, utt)
	for deadline := time.Now().Add(5 * time.Second); f.inFlight.Load() < 1; {
		if time.Now().After(deadline) {
			t.Fatal("the first utterance was never sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		writeAll(t, s, utt)
	}
	s.Close()
	close(f.gate)
	segs := collect(t, s, 5*time.Second)
	var kinds []string
	for _, g := range segs {
		kinds = append(kinds, string(g.Kind))
	}
	// In flight: #1. Queued: #2..#5 is about 37 s: #2 is dropped.
	want := "final gap final final final"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("segments %q, want %q", got, want)
	}
	if segs[1].StartMS < 9800 || segs[1].StartMS > 10000 || segs[1].EndMS < 19000 || segs[1].EndMS > 19200 {
		t.Fatalf("the gap is %d–%d ms, want the second utterance's place (about 9800–19100)", segs[1].StartMS, segs[1].EndMS)
	}
	if len(f.calls) != 4 {
		t.Fatalf("%d requests, want 4 (the dropped one never sent)", len(f.calls))
	}
}

// ---- the fake engine ----

// The fake gives each utterance the next scripted line, at the
// endpointer's own times, and a partial (the line's first word) as it
// begins.
func TestTheFakeEngineScriptsOneLinePerUtterance(t *testing.T) {
	e := stt.NewFake([]string{"First <b>line</b>.", "Second line."})
	s, _ := e.Open(context.Background(), stt.TrackRef{Track: "TR_x"})
	go func() {
		writeAll(t, s, cat(tone(1000, 5000, 0), level(1000, 0), tone(1000, 5000, 0), level(1000, 0)))
		s.Close()
	}()
	segs := collect(t, s, 5*time.Second)
	if len(segs) != 4 || segs[0].Kind != stt.Partial || segs[0].Text != "First" || segs[1].Kind != stt.Final || segs[1].Text != "First <b>line</b>." ||
		segs[2].Kind != stt.Partial || segs[3].Text != "Second line." || segs[3].StartMS < 1800 || segs[3].StartMS > 2000 {
		t.Fatalf("segments = %+v", segs)
	}
}

// ---- the STT service's API: the one muster's dictation calls ----

// The request is muster's dictation's (docs/dictation.md): POST
// /v1/audio/transcriptions, multipart with the audio in a `file` part (a
// 16 kHz mono WAV), `language` en and `punctuation` true, `model` only when
// configured, and the bearer; the answer's `text` is the transcript, and the
// statuses map as muster's client maps them.
func TestTheClientCallsTheSTTServiceAsDictationDoes(t *testing.T) {
	type seen struct {
		path, auth, lang, punct, model, filename string
		hasModel                                 bool
		wav                                      []byte
	}
	got := make(chan seen, 4)
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("not multipart: %v", err)
		}
		f, h, _ := r.FormFile("file")
		var b []byte
		if f != nil {
			b, _ = io.ReadAll(f)
		}
		_, hasModel := r.MultipartForm.Value["model"]
		got <- seen{r.URL.Path, r.Header.Get("Authorization"), r.FormValue("language"), r.FormValue("punctuation"), r.FormValue("model"), h.Filename, hasModel, b}
		w.WriteHeader(status)
		io.WriteString(w, `{"text": "Hello there."}`)
	}))
	defer srv.Close()
	pcm := tone(500, 5000, 0)
	for _, model := range []string{"", "CohereLabs/cohere-transcribe-03-2026"} {
		c, err := stt.NewClient(srv.URL, "stt-fixture-token-91", model, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		text, err := c.Transcribe(context.Background(), pcm)
		s := <-got
		if err != nil || text != "Hello there." || s.path != "/v1/audio/transcriptions" || s.auth != "Bearer stt-fixture-token-91" ||
			s.lang != "en" || s.punct != "true" || s.hasModel != (model != "") || s.model != model || s.filename != "utterance.wav" {
			t.Fatalf("model %q: %q, %v; request %+v", model, text, err, s)
		}
		// A RIFF/WAVE file, PCM, mono, 16 kHz, 16-bit, holding the samples.
		w := s.wav
		if string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" || binary.LittleEndian.Uint16(w[20:]) != 1 ||
			binary.LittleEndian.Uint16(w[22:]) != 1 || binary.LittleEndian.Uint32(w[24:]) != 16000 || binary.LittleEndian.Uint16(w[34:]) != 16 ||
			int(binary.LittleEndian.Uint32(w[40:])) != 2*len(pcm) || int16(binary.LittleEndian.Uint16(w[44+2*100:])) != pcm[100] {
			t.Fatalf("the file part is not the samples as a 16 kHz mono WAV: header %x", w[:44])
		}
	}
	c, _ := stt.NewClient(srv.URL, "stt-fixture-token-91", "", time.Second)
	for code, want := range map[int]error{400: stt.ErrUndecodable, 401: stt.ErrRefused, 403: stt.ErrRefused, 413: stt.ErrTooLong, 503: stt.ErrUnavailable, 422: stt.ErrUnavailable} {
		status = code
		_, err := c.Transcribe(context.Background(), pcm)
		<-got
		if !errors.Is(err, want) {
			t.Errorf("status %d: %v, want %v", code, err, want)
		}
	}
	for _, bad := range []string{"", "stt.example.test:8000", "http://stt.example.test:8000/v1", "ftp://stt.example.test", "http://u:p@stt.example.test"} {
		if _, err := stt.NewClient(bad, "t", "", 0); err == nil {
			t.Errorf("NewClient(%q) accepted it", bad)
		}
	}
	if _, err := stt.NewClient(srv.URL, " ", "", 0); err == nil {
		t.Error("NewClient with no token accepted it")
	}
}
