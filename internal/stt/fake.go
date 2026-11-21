package stt

import (
	"context"
	"strings"
	"sync"
)

// FakeEngine is the `fake` engine (tests only: the plugin refuses it unless
// MEETINGS_TEST_MODE=1). It endpoints with the real endpointer, so its
// segments' times are the real ones, and gives each utterance the next line
// of its script: a partial (the line's first word) when the utterance
// begins, and the whole line as its final when it ends. Each stream starts at
// the script's first line.
type FakeEngine struct {
	Script []string
}

// DefaultScript is the fake's lines when none are configured.
var DefaultScript = []string{"I will send the budget to Sam by Friday.", "Let's meet again next week.", "That works for me."}

// NewFake is the fake engine; an empty script is DefaultScript.
func NewFake(script []string) *FakeEngine {
	var s []string
	for _, l := range script {
		if l = strings.TrimSpace(l); l != "" {
			s = append(s, l)
		}
	}
	if len(s) == 0 {
		s = DefaultScript
	}
	return &FakeEngine{Script: s}
}

// Name is "fake".
func (*FakeEngine) Name() string { return "fake" }

// Open starts a stream.
func (f *FakeEngine) Open(ctx context.Context, track TrackRef) (Stream, error) {
	return &fakeStream{f: f, ep: NewEndpointer(), segs: make(chan Segment, 256), ctx: ctx}, nil
}

type fakeStream struct {
	f    *FakeEngine
	ep   *Endpointer
	segs chan Segment
	ctx  context.Context
	line int
	mu   sync.Mutex
	done bool
}

func (s *fakeStream) Segments() <-chan Segment { return s.segs }

func (s *fakeStream) emit(evs []VADEvent) {
	for _, ev := range evs {
		text := s.f.Script[s.line%len(s.f.Script)]
		var seg Segment
		if ev.Began {
			first, _, _ := strings.Cut(text, " ")
			seg = Segment{Kind: Partial, StartMS: ms(ev.Start), EndMS: ms(ev.Start), Text: first}
		} else {
			seg = Segment{Kind: Final, StartMS: ev.Utterance.StartMS(), EndMS: ev.Utterance.EndMS(), Text: text}
			s.line++
		}
		select {
		case s.segs <- seg:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *fakeStream) Write(pcm []int16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return ErrClosed
	}
	s.emit(s.ep.Push(pcm))
	return nil
}

func (s *fakeStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return nil
	}
	s.done = true
	s.emit(s.ep.Flush())
	close(s.segs)
	return nil
}
