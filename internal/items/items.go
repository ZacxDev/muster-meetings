// Package items finds action items in a meeting's transcript (design:
// "Action items"). A window of final segments goes to a model through one of
// two adapters, and the model's answer goes through deterministic guards,
// because the model is not trusted to obey:
//
//   - extract: ScaleDown's `scaledown/extract` on OpenRouter, which returns
//     spans of the input by character offset; a span becomes an item citing
//     the segments it overlaps, and the item's text is the input at those
//     offsets, never the model's own `text`;
//   - chat_json: any OpenRouter chat model under a strict JSON schema, which
//     rewrites each item as owner, task and due, citing segment ids.
//
// This package is phase 4a's: the adapters, the guards, the window
// selection and the live loop (Loop), and the post-call pass over the whole
// meeting after End (PostCall), all of which cmd/items-eval replays. It stores nothing; the
// live worker, its table and its panel are phase 4b's.
//
// 🔴 What was said is untrusted text (README: "The transcript"). Every
// adapter fences it as data before a model sees it, and the guards, not the
// model, decide what is kept.
package items

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Segment is one final transcript line, as the worker hands it to a model.
type Segment struct {
	ID      int64  `json:"id" yaml:"id"`
	Speaker string `json:"speaker" yaml:"speaker"` // a display name, without " (guest)"
	Guest   bool   `json:"guest,omitempty" yaml:"guest,omitempty"`
	StartMS int64  `json:"start_ms" yaml:"start_ms"`
	EndMS   int64  `json:"end_ms" yaml:"end_ms"`
	// Text is what was said: untrusted.
	Text string `json:"text" yaml:"text"`
}

// Item is an action item: one an adapter proposes, or one the meeting
// already has.
type Item struct {
	Text     string  `json:"text"`
	Assignee string  `json:"assignee,omitempty"`
	Due      string  `json:"due,omitempty"`
	Segments []int64 `json:"segments"`
}

// Window is one call's input: the final segments the model may cite, and
// the meeting's current items (for context; the guards dedupe against them
// whether or not the model looks).
type Window struct {
	Segments []Segment
	Existing []Item
}

// Has reports whether segment id is in the window.
func (w Window) Has(id int64) bool {
	for _, s := range w.Segments {
		if s.ID == id {
			return true
		}
	}
	return false
}

// Usage is what OpenRouter reported a call cost.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// CostUSD is OpenRouter's own figure; CostKnown is false when the answer
	// carried none (the evaluation fails its cost gate on that, rather than
	// read a missing cost as free).
	CostUSD   float64 `json:"cost_usd"`
	CostKnown bool    `json:"cost_known"`
	// ReasoningTokens is the completion tokens the model spent reasoning
	// (usage.completion_tokens_details.reasoning_tokens; 0 when none was
	// reported). They count against max_tokens.
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Result is one adapter call's answer, before the guards.
type Result struct {
	Items []Item
	// Provider is the provider OpenRouter says answered; Model the slug it
	// says it ran.
	Provider string
	Model    string
	Usage    Usage
	// FinishReason is the first choice's finish_reason ("stop", "length",
	// …; "" when none was given). It and Usage are kept on a failed call
	// whenever OpenRouter answered 200, so a schema failure still says what
	// it cost and why it stopped.
	FinishReason string
	// Rewritten counts extract spans whose `text` was not input[start:end]
	// (S6 saw 1 in 22); a high count on a non-ASCII input would also say the
	// offsets are not in characters.
	Rewritten int
}

// Adapter proposes items for one window.
type Adapter interface {
	// Name is "extract" or "chat_json".
	Name() string
	Propose(ctx context.Context, w Window) (Result, error)
}

// The error classes an adapter answers (wrapped in *Error).
type Class int

const (
	// Retry is a 429, a 5xx, a timeout or a broken connection: back off, and
	// the next window covers the gap.
	Retry Class = iota + 1
	// Schema is an answer that did not parse to the expected shape.
	Schema
	// Fatal is any other refusal (400, 401, 402, 403, 404, …): retrying
	// the same request will not help, and the configuration is wrong.
	Fatal
)

func (c Class) String() string {
	switch c {
	case Retry:
		return "retry"
	case Schema:
		return "schema"
	case Fatal:
		return "fatal"
	}
	return "unknown"
}

// Error is an adapter failure with its class.
type Error struct {
	Class  Class
	Status int // the HTTP status, 0 for none
	// RetryAfter is the server's Retry-After, 0 for none.
	RetryAfter time.Duration
	Msg        string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("items: %s (%d): %s", e.Class, e.Status, e.Msg)
	}
	return fmt.Sprintf("items: %s: %s", e.Class, e.Msg)
}

// ClassOf is err's class, 0 for nil or an error that is not an *Error.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return 0
}

// MaxBackoff caps Backoff.
const MaxBackoff = 60 * time.Second

// PausedAfter is how many failures in a row make the panel say "Action
// items paused" (phase 4b's panel; the count is the design's).
const PausedAfter = 3

// Backoff is the wait after the n-th retryable failure in a row (n ≥ 1):
// 1, 2, 4 … 60 s, and never less than the server's Retry-After.
func Backoff(n int, retryAfter time.Duration) time.Duration {
	if n < 1 {
		n = 1
	}
	d := MaxBackoff
	if n <= 6 {
		d = time.Second << (n - 1)
	}
	if retryAfter > d {
		d = retryAfter
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}
