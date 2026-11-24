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

// Replay runs meeting m window by window through a, as the worker would:
// a tick every 10 s of meeting time, items.Select's window (anchored on the
// last segment a successful call covered), items.Run's guards, and the
// meeting's items so far as the window's context.
func Replay(ctx context.Context, a items.Adapter, m *Meeting, o ReplayOpts) MeetingRun {
	run := MeetingRun{Meeting: m, MeetingID: m.ID, DurationMS: m.DurationMS(), Emitted: []items.Item{}}
	tl := m.Timeline()
	every := items.DefaultEvery.Milliseconds()
	var last int64
	fails := 0
	for t := every; t <= m.DurationMS()+every*extraTicks; t += every {
		segs, ok := items.Select(tl, t, last)
		if !ok {
			continue
		}
		w := items.Window{Segments: segs, Existing: append([]items.Item{}, run.Emitted...)}
		var st items.Step
		if o.NoGuards {
			t0 := time.Now()
			res, err := a.Propose(ctx, w)
			st = items.Step{Window: w, Result: res, Kept: res.Items, Elapsed: time.Since(t0), Err: err}
		} else {
			st = items.Run(ctx, a, w)
		}
		wr := WindowRun{Seq: len(run.Windows), AtMS: t, Proposed: len(st.Result.Items), Kept: len(st.Kept), Elapsed: st.Elapsed,
			Provider: st.Result.Provider, Model: st.Result.Model, CostUSD: st.Result.Usage.CostUSD, CostKnown: st.Result.Usage.CostKnown,
			Rewritten: st.Result.Rewritten}
		for _, s := range segs {
			wr.Segments = append(wr.Segments, s.ID)
		}
		for _, d := range st.Dropped {
			if wr.Dropped == nil {
				wr.Dropped = map[string]int{}
			}
			wr.Dropped[d.Reason]++
		}
		if st.Err != nil {
			c := items.ClassOf(st.Err)
			wr.Class, wr.Error = c.String(), st.Err.Error()
			var de *items.DetailError
			if o.Detail != nil && errors.As(st.Err, &de) {
				o.Detail(m.ID, wr.Seq, de.Detail)
			}
			run.Windows = append(run.Windows, wr)
			switch c {
			case items.Retry:
				// The next window covers the gap: the anchor stays.
				fails++
				var e *items.Error
				var after time.Duration
				if errors.As(st.Err, &e) {
					after = e.RetryAfter
				}
				if o.Sleep != nil {
					o.Sleep(items.Backoff(fails, after))
				}
				continue
			case items.Schema:
				// The window is spent: its answer was unusable.
				fails = 0
				last = items.LastID(segs, last)
				continue
			default:
				run.Fatal = st.Err.Error()
				return run
			}
		}
		fails = 0
		run.Windows = append(run.Windows, wr)
		run.Emitted = append(run.Emitted, st.Kept...)
		last = items.LastID(segs, last)
	}
	return run
}
