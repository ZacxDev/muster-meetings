package stt

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Transcriber is what the utterance engine sends each utterance to: the
// STT service (Client), or a test's fake.
type Transcriber interface {
	Transcribe(ctx context.Context, pcm []int16) (string, error)
}

// The utterance engine's limits (design: "Three engines, one interface" and
// "Serving Nemotron", rule 3).
const (
	// MaxInFlight is how many utterances the whole plugin transcribes at
	// once: it shares stt-cohere's GPU and threadpool with dictation.
	MaxInFlight = 4
	// QueueAudio is how much audio one participant's queue holds; past it the
	// oldest utterance is dropped with a gap marker.
	QueueAudio = 30 * time.Second
	// CallTimeout bounds one transcription (muster's own client allows a
	// minute for two minutes of audio; an utterance is at most 15 s).
	CallTimeout = 30 * time.Second
)

// UtteranceEngine sends each endpointed utterance to the STT service.
type UtteranceEngine struct {
	t   Transcriber
	sem chan struct{}
}

// NewUtterance is the utterance engine over t.
func NewUtterance(t Transcriber) *UtteranceEngine {
	return &UtteranceEngine{t: t, sem: make(chan struct{}, MaxInFlight)}
}

// Name is "utterance".
func (*UtteranceEngine) Name() string { return "utterance" }

// Open starts a stream: an endpointer, a queue, and one worker.
func (e *UtteranceEngine) Open(ctx context.Context, track TrackRef) (Stream, error) {
	s := &uttStream{e: e, ep: NewEndpointer(), segs: make(chan Segment, 64)}
	s.cond = sync.NewCond(&s.mu)
	ctx, s.cancel = context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		s.dead = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}()
	go s.work(ctx)
	return s, nil
}

// item is one queued utterance, or a gap marker for one that was dropped.
type item struct {
	u   Utterance
	gap bool
}

type uttStream struct {
	e      *UtteranceEngine
	ep     *Endpointer
	segs   chan Segment
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []item
	queued  int64 // samples of audio queued
	closed  bool  // Close was called: drain, then end
	dead    bool  // the context ended: end now
	wclosed bool  // Write is refused
}

func (s *uttStream) Segments() <-chan Segment { return s.segs }

func (s *uttStream) Write(pcm []int16) error {
	s.mu.Lock()
	if s.wclosed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.mu.Unlock()
	for _, ev := range s.ep.Push(pcm) {
		if ev.Utterance != nil {
			s.enqueue(*ev.Utterance)
		}
	}
	return nil
}

// enqueue adds u, first dropping the oldest queued utterances (each
// replaced by a gap marker in its place) until it fits in QueueAudio.
func (s *uttStream) enqueue(u Utterance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := int64(QueueAudio/time.Millisecond) * Rate / 1000
	for i := 0; s.queued+int64(len(u.PCM)) > limit && i < len(s.queue); i++ {
		if s.queue[i].gap {
			continue
		}
		s.queued -= int64(len(s.queue[i].u.PCM))
		s.queue[i] = item{u: Utterance{Start: s.queue[i].u.Start, End: s.queue[i].u.End}, gap: true}
	}
	s.queue = append(s.queue, item{u: u})
	s.queued += int64(len(u.PCM))
	s.cond.Broadcast()
}

// next is the next queued item, waiting for one; ok false when the stream is
// over.
func (s *uttStream) next() (item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.queue) == 0 && !s.closed && !s.dead {
		s.cond.Wait()
	}
	if s.dead || len(s.queue) == 0 {
		return item{}, false
	}
	it := s.queue[0]
	s.queue = s.queue[1:]
	s.queued -= int64(len(it.u.PCM))
	return it, true
}

func (s *uttStream) work(ctx context.Context) {
	defer close(s.segs)
	defer s.cancel()
	for {
		it, ok := s.next()
		if !ok {
			return
		}
		seg, emit := s.transcribe(ctx, it)
		if !emit {
			continue
		}
		select {
		case s.segs <- seg:
		case <-ctx.Done():
			return
		}
	}
}

// transcribe turns one item into its segment: a gap stays a gap; a constant
// signal is nothing; the service's text is a final (nothing when it heard
// nothing); a failure is a gap over the utterance.
func (s *uttStream) transcribe(ctx context.Context, it item) (Segment, bool) {
	if it.gap {
		return Segment{Kind: Gap, StartMS: it.u.StartMS(), EndMS: it.u.EndMS()}, true
	}
	if constant(it.u.PCM) {
		return Segment{}, false
	}
	select {
	case s.e.sem <- struct{}{}:
	case <-ctx.Done():
		return Segment{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, CallTimeout)
	text, err := s.e.t.Transcribe(cctx, it.u.PCM)
	cancel()
	<-s.e.sem
	if err != nil {
		return Segment{Kind: Gap, StartMS: it.u.StartMS(), EndMS: it.u.EndMS()}, true
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Segment{}, false
	}
	return Segment{Kind: Final, StartMS: it.u.StartMS(), EndMS: it.u.EndMS(), Text: text}, true
}

func (s *uttStream) Close() error {
	for _, ev := range s.ep.Flush() {
		if ev.Utterance != nil {
			s.enqueue(*ev.Utterance)
		}
	}
	s.mu.Lock()
	s.closed, s.wclosed = true, true
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

// constant says every sample is the same: digital silence, or a stuck
// level (stt-cohere's own check, app.py: such audio makes the model invent
// text).
func constant(pcm []int16) bool {
	for _, v := range pcm {
		if v != pcm[0] {
			return false
		}
	}
	return true
}
