package items

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

// The post-call pass (phase 4b's, after End: one pass over the whole meeting
// feeding the owner-approved emails). cmd/items-eval replays PostCall itself,
// so what it measures is what ships.
const (
	// PostCallChunkChars is the most speech (characters of segment text) one
	// call reads. The corpus speaks 580 characters a minute (14,631 over
	// 25.2 meeting-minutes), so this is about 83 minutes: an hour-long
	// meeting is ONE call, and only a longer one is split. At about four
	// characters a token it is about 12k tokens of input.
	PostCallChunkChars = 48000
	// PostCallMaxTokens is a post-call chat_json answer's max_tokens: twice a
	// live window's (ChatJSONMaxTokens), since one answer lists a whole
	// meeting's items. At the ~80 tokens a second the first paid run's
	// truncated calls imply (1024 tokens in ~13 s) it is ~100 s of writing,
	// inside PostCallTimeout.
	PostCallMaxTokens = 8192
	// PostCallTimeout bounds one post-call request: twice the 60 s latency
	// bar, so a slow call is measured as slow rather than cut off as a retry.
	PostCallTimeout = 2 * time.Minute
	// PostCallAttempts is how many times one chunk is tried. There is no
	// next window after End to cover a failed chunk, so a retryable or a
	// schema failure is tried again (a retryable one after Backoff); a fatal
	// one stops the pass.
	PostCallAttempts = 3
	// PostCallItemsPerMinute and PostCallMinItems are the meeting's cap
	// (PostCallCap). The corpus is denser than a real meeting by design (3
	// to 5 items in about two minutes each); its densest meeting, m12, has 4
	// labels in 1.67 minutes, 2.4 a minute. 3 a minute is above that, so the
	// cap never trims a meeting the corpus calls plausible; it bounds a
	// runaway or an injected list (180 for an hour) that an owner would
	// otherwise be asked to approve item by item.
	PostCallItemsPerMinute = 3
	PostCallMinItems       = 10
)

// PostCallCap is the most items the post-call pass keeps for a meeting of
// these segments: PostCallItemsPerMinute per minute of it, at least
// PostCallMinItems.
func PostCallCap(segs []Segment) int {
	if len(segs) == 0 {
		return PostCallMinItems
	}
	minutes := float64(segs[len(segs)-1].EndMS-segs[0].StartMS) / 60000
	return max(PostCallMinItems, int(math.Ceil(PostCallItemsPerMinute*minutes)))
}

// Chunks splits a meeting's final segments (in id order) into the post-call
// pass's calls: each at most PostCallChunkChars of speech (and at least one
// segment no earlier chunk had), and each after the first starting with
// the previous chunk's last 90 s (Lookback, the live window's reach; at
// most half the budget), so a request in one chunk and its acceptance in the
// next are read together once.
func Chunks(segs []Segment) [][]Segment {
	var out [][]Segment
	from, next := 0, 0
	for next < len(segs) {
		end, n := from, 0
		for end < len(segs) && (end <= next || n+utf8.RuneCountInString(segs[end].Text) <= PostCallChunkChars) {
			n += utf8.RuneCountInString(segs[end].Text)
			end++
		}
		out = append(out, append([]Segment{}, segs[from:end]...))
		next = end
		if end == len(segs) {
			break
		}
		cut := segs[end-1].EndMS - Lookback.Milliseconds()
		from, n = end, 0
		for from > 0 && segs[from-1].EndMS > cut && n+utf8.RuneCountInString(segs[from-1].Text) <= PostCallChunkChars/2 {
			from--
			n += utf8.RuneCountInString(segs[from].Text)
		}
	}
	return out
}

// PostCallOpts are the post-call pass's hooks.
type PostCallOpts struct {
	// Sleep waits out a backoff; nil waits on the clock, or until ctx ends.
	Sleep func(time.Duration)
	// OnCall sees every call: its chunk's index and the call as Run saw it.
	OnCall func(chunk int, st Step)
}

// PostCall is the post-call pass over a meeting's final segments (in id
// order): each chunk (Chunks) through a, then the SAME guards as a live
// window (GuardCap): every citation in the chunk, a known owner, and no
// duplicate of an item kept from an earlier chunk, which the model is also
// shown as the window's current items. It keeps at most PostCallCap items
// for the meeting, and stops calling once it has them.
//
// A chunk that fails is tried PostCallAttempts times (see there). A fatal
// failure stops the pass; a chunk that fails every attempt is skipped and
// the pass goes on. Either way the items kept so far are answered with the
// error (its class is the last failure's), so the caller can tell a
// complete pass from a partial one.
func PostCall(ctx context.Context, a Adapter, segs []Segment, o PostCallOpts) ([]Item, error) {
	limit := PostCallCap(segs)
	chunks := Chunks(segs)
	var kept []Item
	failed := 0
	var last error
	for i, ch := range chunks {
		if len(kept) >= limit {
			break
		}
		for attempt := 1; ; attempt++ {
			st := run(ctx, a, Window{Segments: ch, Existing: append([]Item{}, kept...)}, limit-len(kept))
			if o.OnCall != nil {
				o.OnCall(i, st)
			}
			if st.Err == nil {
				kept = append(kept, st.Kept...)
				break
			}
			c := ClassOf(st.Err)
			if c != Retry && c != Schema {
				return kept, fmt.Errorf("items: the post-call pass stopped at chunk %d of %d: %w", i+1, len(chunks), st.Err)
			}
			if attempt >= PostCallAttempts {
				failed++
				last = st.Err
				break
			}
			if c == Retry {
				var e *Error
				var after time.Duration
				if errors.As(st.Err, &e) {
					after = e.RetryAfter
				}
				if err := wait(ctx, o.Sleep, Backoff(attempt, after)); err != nil {
					return kept, fmt.Errorf("items: the post-call pass stopped at chunk %d of %d: %w", i+1, len(chunks), err)
				}
			}
		}
	}
	if failed > 0 {
		return kept, fmt.Errorf("items: the post-call pass lost %d of %d chunks after %d attempts each: %w", failed, len(chunks), PostCallAttempts, last)
	}
	return kept, nil
}

func wait(ctx context.Context, sleep func(time.Duration), d time.Duration) error {
	if sleep != nil {
		sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
