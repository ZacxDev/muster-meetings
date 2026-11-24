package items

import (
	"context"
	"time"
)

// DefaultEvery is the window period (MEETINGS_ITEMS_EVERY).
const DefaultEvery = 10 * time.Second

// Lookback is how far back a window reaches.
const Lookback = 90 * time.Second

// Select is the window for a tick at meeting time nowMS (design: "The
// worker"): the final segments that ended in the last 90 s, and every one
// after lastID (the last segment a successful call covered), however old,
// so a window after failures covers the gap. finals are the meeting's final
// segments, in id order, that have ended by nowMS. ok is false when there
// is no segment after lastID: no new segment, no call.
func Select(finals []Segment, nowMS, lastID int64) (window []Segment, ok bool) {
	from := nowMS - Lookback.Milliseconds()
	for _, s := range finals {
		if s.EndMS > nowMS {
			continue
		}
		if s.ID > lastID {
			ok = true
		}
		if s.EndMS > from || s.ID > lastID {
			window = append(window, s)
		}
	}
	if !ok {
		return nil, false
	}
	return window, true
}

// Step is one window through the worker: the adapter's proposal, then the
// guards. A failed call keeps nothing and answers the error (its class says
// whether the next window retries the gap).
type Step struct {
	Window  Window
	Result  Result
	Kept    []Item
	Dropped []Dropped
	Elapsed time.Duration
	Err     error
}

// Run calls a on w and guards the answer (at most MaxPerWindow kept).
func Run(ctx context.Context, a Adapter, w Window) Step {
	return run(ctx, a, w, MaxPerWindow)
}

// run is Run keeping at most limit.
func run(ctx context.Context, a Adapter, w Window, limit int) Step {
	t0 := time.Now()
	res, err := a.Propose(ctx, w)
	st := Step{Window: w, Result: res, Elapsed: time.Since(t0), Err: err}
	if err != nil {
		return st
	}
	st.Kept, st.Dropped = GuardCap(w, res.Items, limit)
	return st
}

// LastID is the newest segment in segs (the anchor after a successful call).
func LastID(segs []Segment, prev int64) int64 {
	for _, s := range segs {
		if s.ID > prev {
			prev = s.ID
		}
	}
	return prev
}
