package items

import (
	"context"
	"errors"
	"time"
)

// Loop is the worker's state for one meeting: where its windows are
// anchored, the items it has kept, and its run of failures. One Tick per
// window period: cmd/items-eval drives it on the meeting's own clock, and
// phase 4b's worker will drive it on the wall clock, so the evaluation
// measures exactly the rules that ship.
//
// The rules, per tick that makes a call:
//
//   - a success keeps the guarded items, moves the anchor to the window's
//     newest segment, and clears the failures;
//   - a retryable failure (429, 5xx, timeout) keeps the anchor, so the next
//     window covers the gap, and Wait is the backoff;
//   - a schema failure spends its window (the anchor moves: the same input
//     would fail the same way) and Wait is 0;
//   - a fatal failure stops the loop: no tick calls again.
//
// 🔴 One rule for "Action items paused": EVERY failed call counts, whatever
// its class, and only a success clears the count; Paused is 3 in a row.
type Loop struct {
	Adapter Adapter
	// Raw keeps every proposal and skips the guards. Only the evaluation's
	// duplicator control sets it (to show the scorer can count a
	// duplicate); nothing that ships does.
	Raw bool

	last    int64
	fails   int
	wait    time.Duration
	kept    []Item
	stopped error
}

// Tick runs the window at meeting time nowMS over the meeting's final
// segments. called is false, and no call is made, when no final segment is
// newer than the anchor, or once the loop has stopped.
func (l *Loop) Tick(ctx context.Context, finals []Segment, nowMS int64) (st Step, called bool) {
	if l.stopped != nil {
		return Step{}, false
	}
	segs, ok := Select(finals, nowMS, l.last)
	if !ok {
		return Step{}, false
	}
	w := Window{Segments: segs, Existing: append([]Item{}, l.kept...)}
	if l.Raw {
		t0 := time.Now()
		res, err := l.Adapter.Propose(ctx, w)
		st = Step{Window: w, Result: res, Kept: res.Items, Elapsed: time.Since(t0), Err: err}
	} else {
		st = Run(ctx, l.Adapter, w)
	}
	if st.Err == nil {
		l.fails, l.wait = 0, 0
		l.kept = append(l.kept, st.Kept...)
		l.last = LastID(segs, l.last)
		return st, true
	}
	l.fails++
	switch ClassOf(st.Err) {
	case Retry:
		var e *Error
		var after time.Duration
		if errors.As(st.Err, &e) {
			after = e.RetryAfter
		}
		l.wait = Backoff(l.fails, after)
	case Schema:
		l.wait = 0
		l.last = LastID(segs, l.last)
	default:
		l.wait = 0
		l.stopped = st.Err
	}
	return st, true
}

// Resume restores a loop the worker kept across a restart: the anchor
// (the last segment a successful call covered) and the items kept so far,
// so the next window covers only what came after, not the whole meeting.
func (l *Loop) Resume(lastID int64, kept []Item) {
	l.last = lastID
	l.kept = append([]Item{}, kept...)
}

// Anchor is the last segment a successful call covered (what Resume takes).
func (l *Loop) Anchor() int64 { return l.last }

// Items is what the loop has kept, in order.
func (l *Loop) Items() []Item { return append([]Item{}, l.kept...) }

// Failures is the failed calls since the last success.
func (l *Loop) Failures() int { return l.fails }

// Paused is "Action items paused": PausedAfter failures in a row.
func (l *Loop) Paused() bool { return l.fails >= PausedAfter }

// Wait is how long to hold off before the next call: the backoff after a
// retryable failure, else 0.
func (l *Loop) Wait() time.Duration { return l.wait }

// Stopped is the fatal error that stopped the loop, nil while it runs.
func (l *Loop) Stopped() error { return l.stopped }
