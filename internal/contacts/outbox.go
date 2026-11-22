package contacts

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The outbox's backoff (design: "5 s doubling to 10 minutes").
const (
	BackoffFirst = 5 * time.Second
	BackoffMax   = 10 * time.Minute
	// RetryAfterMax caps a 429's Retry-After.
	RetryAfterMax = time.Hour
	// pollEvery is the worker's longest sleep with nothing due sooner.
	pollEvery = 30 * time.Second
	// batch is how many due rows one pass takes.
	batch = 50
)

// Action is what the outbox does with a row after one attempt.
type Action int

const (
	// Retry keeps the row, due again after the backoff.
	Retry Action = iota
	// Delivered deletes it: contacts took it (2xx).
	Delivered
	// NotAccepting deletes it: 404, the owner hid Contacts, was revoked or
	// turned collection off. It will never be accepted.
	NotAccepting
	// Refused deletes it and logs the event name: 400, 409 or another 4xx,
	// a request contacts will never take.
	Refused
)

// Classify is the design's table ("Failure behaviour"): 2xx and 404 delete
// the row; 400 and 409 (and any other 4xx but 401 and 429) delete it and
// are logged; 401, 429, 5xx, any other status and no answer at all retry.
func Classify(a Answer, err error) Action {
	if err != nil {
		return Retry
	}
	switch s := a.Status; {
	case s >= 200 && s < 300:
		return Delivered
	case s == http.StatusNotFound:
		return NotAccepting
	case s == http.StatusUnauthorized, s == http.StatusTooManyRequests:
		return Retry
	case s >= 400 && s < 500:
		return Refused
	}
	return Retry
}

// Backoff is the wait after the attempts-th failed attempt (1 is the
// first): BackoffFirst doubling to BackoffMax, or a longer Retry-After.
func Backoff(attempts int, a Answer) time.Duration {
	d := BackoffFirst
	for i := 1; i < attempts && d < BackoffMax; i++ {
		d *= 2
	}
	d = min(d, BackoffMax)
	if a.RetryAfter > d {
		d = min(a.RetryAfter, RetryAfterMax)
	}
	return d
}

// Outbox delivers contacts_outbox, oldest first, off every request's path.
type Outbox struct {
	c     *Client
	st    *store.Store
	log   *log.Logger
	now   func() time.Time
	kick  chan struct{}
	after func(time.Duration) <-chan time.Time

	// Dropped counts rows deleted undelivered after store.OutboxLifetime
	// (the design's meetings_contacts_outbox_dropped_total: meetings has no
	// metrics endpoint, so it is logged and readable here).
	Dropped atomic.Int64
}

// NewOutbox is the worker for c over st. now is the clock (time.Now when
// nil).
func NewOutbox(c *Client, st *store.Store, logger *log.Logger, now func() time.Time) *Outbox {
	if now == nil {
		now = time.Now
	}
	return &Outbox{c: c, st: st, log: logger, now: now, kick: make(chan struct{}, 1), after: time.After}
}

// Kick wakes the worker (a row was just written). It never blocks.
func (o *Outbox) Kick() {
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

// Run delivers until ctx ends: a pass at once, then whenever a row falls
// due or Kick is called.
func (o *Outbox) Run(ctx context.Context) {
	for {
		o.Pass(ctx)
		wait := pollEvery
		if next, ok, err := o.st.NextOutbox(ctx); err == nil && ok {
			if d := next.Sub(o.now()); d < wait {
				wait = max(d, 50*time.Millisecond)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-o.kick:
		case <-o.after(wait):
		}
	}
}

// Pass is one sweep: rows past store.OutboxLifetime are dropped, then every
// due row is attempted once, in insertion order. A row that fails is
// rescheduled and the pass goes on to the next: one row's retries never hold
// back another's. It returns how many rows it delivered.
func (o *Outbox) Pass(ctx context.Context) int {
	if n, err := o.st.DropExpiredOutbox(ctx, o.now()); err != nil {
		o.log.Printf("contacts: outbox: %v", err)
	} else if n > 0 {
		o.Dropped.Add(n)
		o.log.Printf("contacts: outbox: %d row(s) dropped undelivered after %s (meetings_contacts_outbox_dropped_total %d)", n, store.OutboxLifetime, o.Dropped.Load())
	}
	rows, err := o.st.DueOutbox(ctx, o.now(), batch)
	if err != nil {
		o.log.Printf("contacts: outbox: %v", err)
		return 0
	}
	delivered := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			return delivered
		}
		a, err := o.c.Post(ctx, r.Path, r.Key, r.Body)
		// 🔴 Only the kind, the row's own id and the status are logged:
		// never the body (a name and an email), the key or the owner.
		switch Classify(a, err) {
		case Delivered:
			delivered++
			o.done(ctx, r)
		case NotAccepting:
			o.log.Printf("contacts: outbox: %s %d not accepting (404); dropped", r.Kind, r.ID)
			o.done(ctx, r)
		case Refused:
			o.log.Printf("contacts: outbox: %s %d refused (%d); dropped", r.Kind, r.ID, a.Status)
			o.done(ctx, r)
		default:
			wait := Backoff(r.Attempts+1, a)
			if err := o.st.RetryOutbox(ctx, r.ID, o.now().Add(wait)); err != nil {
				o.log.Printf("contacts: outbox: %v", err)
			}
			what := "no answer"
			if err == nil {
				what = http.StatusText(a.Status)
			}
			o.log.Printf("contacts: outbox: %s %d not delivered (%s); retry in %s", r.Kind, r.ID, what, wait)
		}
	}
	return delivered
}

func (o *Outbox) done(ctx context.Context, r store.OutboxRow) {
	if err := o.st.DeleteOutbox(ctx, r.ID); err != nil {
		o.log.Printf("contacts: outbox: %v", err)
	}
}
