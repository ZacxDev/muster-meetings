package contacts

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// The accepting cache's times (design: "What meetings sends, and when";
// "Failure behaviour").
const (
	// AcceptingTTL is how long an answer is fresh: the owner is asked at
	// most this often.
	AcceptingTTL = 60 * time.Second
	// AcceptingStale is how long a failed refresh keeps using the last
	// answer. Past it, with no fresh answer, no card is offered.
	AcceptingStale = 24 * time.Hour
	// unauthorisedEvery is how often a 401 is logged.
	unauthorisedEvery = time.Minute
)

// Accepting answers "may meetings offer the card for this owner?" from
// memory, never blocking: a fresh or a stale-but-recent answer, and false
// when it holds none. Refresh asks contacts in the background.
type Accepting struct {
	c   *Client
	log *log.Logger
	now func() time.Time

	mu        sync.Mutex
	m         map[string]*accEntry
	lastUnath time.Time
	// wg counts refreshes in flight (Wait, for tests).
	wg sync.WaitGroup
}

type accEntry struct {
	ok       bool
	have     bool      // an answer was ever received
	at       time.Time // when it was received
	asked    time.Time // when the last ask was sent
	inflight bool
}

// NewAccepting is the cache for c. now is the clock (time.Now when nil).
func NewAccepting(c *Client, logger *log.Logger, now func() time.Time) *Accepting {
	if now == nil {
		now = time.Now
	}
	return &Accepting{c: c, log: logger, now: now, m: map[string]*accEntry{}}
}

// Get is the cached answer for owner: true only when contacts said 200
// within AcceptingStale. It never calls contacts.
func (a *Accepting) Get(owner string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.m[owner]
	return e != nil && e.have && e.ok && a.now().Sub(e.at) < AcceptingStale
}

// Refresh asks contacts about owner in the background, unless an ask was
// sent within AcceptingTTL or one is in flight. It returns at once.
func (a *Accepting) Refresh(owner string) {
	a.mu.Lock()
	e := a.m[owner]
	if e == nil {
		if len(a.m) > 10000 {
			a.pruneLocked()
		}
		e = &accEntry{}
		a.m[owner] = e
	}
	now := a.now()
	if e.inflight || (!e.asked.IsZero() && now.Sub(e.asked) < AcceptingTTL) {
		a.mu.Unlock()
		return
	}
	e.inflight, e.asked = true, now
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		ok, ans, err := a.c.Accepting(context.Background(), owner)
		a.mu.Lock()
		defer a.mu.Unlock()
		e.inflight = false
		if err != nil {
			// The last answer stands (until AcceptingStale). A 401 is the
			// one failure worth a line: a wrong token on one side.
			if ans.Status == http.StatusUnauthorized && a.now().Sub(a.lastUnath) >= unauthorisedEvery {
				a.lastUnath = a.now()
				a.log.Printf("contacts: unauthorised (check MEETINGS_CONTACTS_TOKEN against the contacts plugin's CONTACTS_API_TOKEN)")
			}
			return
		}
		e.ok, e.have, e.at = ok, true, a.now()
	}()
}

// Wait blocks until every refresh in flight has finished (tests).
func (a *Accepting) Wait() { a.wg.Wait() }

func (a *Accepting) pruneLocked() {
	now := a.now()
	for k, e := range a.m {
		if !e.inflight && now.Sub(e.asked) >= AcceptingStale {
			delete(a.m, k)
		}
	}
}
