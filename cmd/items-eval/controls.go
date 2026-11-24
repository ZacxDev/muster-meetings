package main

import (
	"context"
	"fmt"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// The harness's own controls (design: "The harness's own controls"), run
// before any model. Each is an Adapter replayed through the same code as a
// model, so a control that does not score as stated says the harness, the
// guards or the corpus is broken, and no model is scored.

// nullAdapter emits nothing.
type nullAdapter struct{}

func (nullAdapter) Name() string { return "null" }
func (nullAdapter) Propose(context.Context, items.Window) (items.Result, error) {
	return items.Result{}, nil
}

// labelAdapter emits each of a meeting's labels once, in the first window
// that holds any of its segments, citing the ones in the window: the oracle.
// copies 2 is the duplicator; lie cites a segment outside the window too.
type labelAdapter struct {
	m      *Meeting
	copies int
	lie    bool
	done   map[string]bool
}

func (a *labelAdapter) Name() string { return "labels" }
func (a *labelAdapter) Propose(_ context.Context, w items.Window) (items.Result, error) {
	var res items.Result
	var top int64
	for _, s := range w.Segments {
		top = max(top, s.ID)
	}
	for _, l := range a.m.Labels {
		if a.done[l.ID] {
			continue
		}
		var in []int64
		for _, id := range l.Segments {
			if w.Has(id) {
				in = append(in, id)
			}
		}
		if len(in) == 0 {
			continue
		}
		a.done[l.ID] = true
		if a.lie {
			// One cited segment in the window and one past it: the guard
			// drops an item for ANY citation outside.
			in = []int64{in[0], top + 1000}
		}
		for range max(a.copies, 1) {
			res.Items = append(res.Items, items.Item{Text: l.Text, Assignee: l.Assignee, Due: l.Due, Segments: append([]int64{}, in...)})
		}
	}
	return res, nil
}

// ControlResult is one control's outcome.
type ControlResult struct {
	Name    string  `json:"name"`
	Want    string  `json:"want"`
	Got     string  `json:"got"`
	Pass    bool    `json:"pass"`
	Metrics Metrics `json:"metrics"`
}

func replayAll(ms []*Meeting, mk func(*Meeting) items.Adapter, o ReplayOpts) []MeetingRun {
	var runs []MeetingRun
	for _, m := range ms {
		runs = append(runs, Replay(context.Background(), mk(m), m, o))
	}
	return runs
}

func oracle(copies int, lie bool) func(*Meeting) items.Adapter {
	return func(m *Meeting) items.Adapter {
		return &labelAdapter{m: m, copies: copies, lie: lie, done: map[string]bool{}}
	}
}

// RunControls runs the four controls (the duplicator twice: raw, and
// through the guards) on ms.
func RunControls(ms []*Meeting) []ControlResult {
	var out []ControlResult

	n := Score(replayAll(ms, func(*Meeting) items.Adapter { return nullAdapter{} }, ReplayOpts{}))
	out = append(out, ControlResult{"null", "recall 0", fmt.Sprintf("recall %.2f, %d emitted", n.Recall, n.Emitted),
		n.Recall == 0 && n.Emitted == 0, n})

	o := Score(replayAll(ms, oracle(1, false), ReplayOpts{}))
	out = append(out, ControlResult{"oracle", "recall 1.00 and precision 1.00", fmt.Sprintf("recall %.2f, precision %.2f", o.Recall, o.Precision),
		o.Recall == 1 && o.Precision == 1, o})

	// Scored raw, the duplicator must FAIL the duplicate gate: the scorer
	// can go red on it.
	d := Score(replayAll(ms, oracle(2, false), ReplayOpts{NoGuards: true}))
	out = append(out, ControlResult{"duplicator", "fails the duplicate gate (rate > 0.05)", fmt.Sprintf("duplicate rate %.2f", d.DupRate),
		d.DupRate > barDupRate, d})

	// Through the guards, the same output must lose every second copy: at
	// least half of what it proposed is dropped as a duplicate, and no label
	// is matched twice. (Recall is the oracle's to check.)
	dgr := replayAll(ms, oracle(2, false), ReplayOpts{})
	dg := Score(dgr)
	dprop, ddup := 0, 0
	for _, r := range dgr {
		for _, w := range r.Windows {
			dprop += w.Proposed
			ddup += w.Dropped[items.DropDupText] + w.Dropped[items.DropDupCitation]
		}
	}
	out = append(out, ControlResult{"duplicator through the guards", "duplicate rate 0, at least half dropped as duplicates",
		fmt.Sprintf("duplicate rate %.2f, %d of %d dropped as duplicates", dg.DupRate, ddup, dprop),
		dg.DupRate == 0 && dprop > 0 && 2*ddup >= dprop, dg})

	lr := replayAll(ms, oracle(1, true), ReplayOpts{})
	l := Score(lr)
	proposed, outside := 0, 0
	for _, r := range lr {
		for _, w := range r.Windows {
			proposed += w.Proposed
			outside += w.Dropped[items.DropOutOfWindow]
		}
	}
	out = append(out, ControlResult{"liar", "every item dropped by the guards (out-of-window)",
		fmt.Sprintf("%d proposed, %d dropped out-of-window, %d emitted", proposed, outside, l.Emitted),
		proposed > 0 && outside == proposed && l.Emitted == 0, l})
	return out
}

// ControlsPass reports every control passing.
func ControlsPass(cs []ControlResult) bool {
	for _, c := range cs {
		if !c.Pass {
			return false
		}
	}
	return len(cs) > 0
}
