package main

import (
	"context"
	"errors"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// WindowRun is one tick that made a call.
type WindowRun struct {
	Seq      int            `json:"seq"`
	AtMS     int64          `json:"at_ms"`
	Segments []int64        `json:"segments"`
	Proposed int            `json:"proposed"`
	Kept     int            `json:"kept"`
	Dropped  map[string]int `json:"dropped,omitempty"`
	Elapsed  time.Duration  `json:"elapsed_ns"`
	// Class is "" for a success, else the error's class.
	Class     string  `json:"class,omitempty"`
	Error     string  `json:"error,omitempty"`
	Provider  string  `json:"provider,omitempty"`
	Model     string  `json:"model,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	CostKnown bool    `json:"cost_known"`
	Rewritten int     `json:"rewritten,omitempty"`
	// What OpenRouter said about the answer, failed or not: why it stopped,
	// and its tokens (reasoning counts against max_tokens).
	FinishReason     string `json:"finish_reason,omitempty"`
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	ReasoningTokens  int    `json:"reasoning_tokens,omitempty"`
}

// windowRun records one call.
func windowRun(seq int, atMS int64, st items.Step, m *Meeting, o ReplayOpts) WindowRun {
	u := st.Result.Usage
	wr := WindowRun{Seq: seq, AtMS: atMS, Proposed: len(st.Result.Items), Kept: len(st.Kept), Elapsed: st.Elapsed,
		Provider: st.Result.Provider, Model: st.Result.Model, CostUSD: u.CostUSD, CostKnown: u.CostKnown,
		Rewritten: st.Result.Rewritten, FinishReason: st.Result.FinishReason,
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, ReasoningTokens: u.ReasoningTokens}
	for _, s := range st.Window.Segments {
		wr.Segments = append(wr.Segments, s.ID)
	}
	for _, d := range st.Dropped {
		if wr.Dropped == nil {
			wr.Dropped = map[string]int{}
		}
		wr.Dropped[d.Reason]++
	}
	if st.Err != nil {
		wr.Class, wr.Error = items.ClassOf(st.Err).String(), st.Err.Error()
		var de *items.DetailError
		if o.Detail != nil && errors.As(st.Err, &de) {
			o.Detail(m.ID, wr.Seq, de.Detail)
		}
	}
	return wr
}

// MeetingRun is one meeting replayed through one adapter.
type MeetingRun struct {
	Meeting    *Meeting     `json:"-"`
	MeetingID  string       `json:"meeting"`
	Windows    []WindowRun  `json:"windows"`
	Emitted    []items.Item `json:"emitted"`
	DurationMS int64        `json:"duration_ms"`
	// Fatal is the first fatal error: the meeting stopped there.
	Fatal string `json:"fatal,omitempty"`
}

// ReplayOpts are the replay's knobs.
type ReplayOpts struct {
	// NoGuards scores the adapter's raw proposals (the duplicator control
	// only: everything that ships goes through the guards).
	NoGuards bool
	// Sleep waits out a backoff (nil: no wait). The replay's clock is the
	// meeting's, so a wait changes only how hard OpenRouter is pressed.
	Sleep func(time.Duration)
	// Detail receives a failed call's upstream message (nil: dropped).
	Detail func(meeting string, seq int, detail string)
}

// extraTicks is how many ticks past the last segment the replay keeps
// trying, as the live worker would, to cover segments a failed call left.
const extraTicks = 6

// Replay runs meeting m window by window through a, as the worker will:
// items.Loop (the worker's own anchor, guards, failure and backoff rules),
// ticked every 10 s of the meeting's clock.
func Replay(ctx context.Context, a items.Adapter, m *Meeting, o ReplayOpts) MeetingRun {
	run := MeetingRun{Meeting: m, MeetingID: m.ID, DurationMS: m.DurationMS(), Emitted: []items.Item{}}
	tl := m.Timeline()
	every := items.DefaultEvery.Milliseconds()
	l := &items.Loop{Adapter: a, Raw: o.NoGuards}
	for t := every; t <= m.DurationMS()+every*extraTicks; t += every {
		st, called := l.Tick(ctx, tl, t)
		if !called {
			continue
		}
		run.Windows = append(run.Windows, windowRun(len(run.Windows), t, st, m, o))
		if err := l.Stopped(); err != nil {
			run.Fatal = err.Error()
			break
		}
		if w := l.Wait(); w > 0 && o.Sleep != nil {
			o.Sleep(w)
		}
	}
	run.Emitted = append(run.Emitted, l.Items()...)
	return run
}

// ReplayPostCall runs meeting m through the post-call pass, as phase 4b will
// after End: items.PostCall itself over the meeting's final segments, every
// call recorded (at the meeting's end, its clock), its items the emitted
// ones. NoGuards does not apply: the pass has no unguarded mode.
func ReplayPostCall(ctx context.Context, a items.Adapter, m *Meeting, o ReplayOpts) MeetingRun {
	run := MeetingRun{Meeting: m, MeetingID: m.ID, DurationMS: m.DurationMS(), Emitted: []items.Item{}}
	sleep := o.Sleep
	if sleep == nil {
		// The replay's nil is no wait; PostCall's would wait on the clock.
		sleep = func(time.Duration) {}
	}
	got, err := items.PostCall(ctx, a, m.Timeline(), items.PostCallOpts{
		Sleep: sleep,
		OnCall: func(_ int, st items.Step) {
			run.Windows = append(run.Windows, windowRun(len(run.Windows), m.DurationMS(), st, m, o))
		},
	})
	if items.ClassOf(err) == items.Fatal {
		run.Fatal = err.Error()
	}
	run.Emitted = append(run.Emitted, got...)
	return run
}
