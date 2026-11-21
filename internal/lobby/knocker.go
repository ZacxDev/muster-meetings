package lobby

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
)

// Notifier is muster's knock notification (core seam 1e, as a server app
// acting for the meeting's owner: muster.Client's Notify and Withdraw).
type Notifier interface {
	Notify(ctx context.Context, subject, ref string, count int) (bool, error)
	Withdraw(ctx context.Context, subject, ref string) (bool, error)
}

// LobbyCount is who owns a meeting and how many guests wait on it now. A
// meeting that no longer exists is ("", 0, nil).
type LobbyCount func(ctx context.Context, meetingID string) (subject string, waiting int, err error)

// MaxKnockCount is the most a knock's count says (muster: 1 to 99).
const MaxKnockCount = 99

// Knocker sends the knock notification to a meeting's owner (design: "When
// the plugin sends it: only while no owner session holds that meeting's
// event stream open, on the first knock and when the count rises"), and
// withdraws it when the lobby empties or the owner opens the meeting.
//
// Every decision re-reads the lobby, and decisions for all meetings run one
// at a time on one goroutine (Run), so a burst of knocks makes one call per
// rise and never two racing ones. The ref is the meeting id. A failure is
// logged and never touches the lobby: the guest's row stays, and the owner
// sees it when they open the meeting.
type Knocker struct {
	n     Notifier
	hub   *Hub
	count LobbyCount
	log   *log.Logger
	now   func() time.Time

	mu      sync.Mutex
	state   map[string]*knockState
	pending map[string]bool
	queue   chan string
}

type knockState struct {
	sent    int       // the count of the last knock muster took; 0: none since the lobby was last empty
	out     bool      // a knock is on the owner's devices (muster answered 202)
	retryAt time.Time // muster's Retry-After
}

// NewKnocker returns a knocker; start it with Run. It registers itself with
// hub, so an owner opening a meeting's stream withdraws its knock.
func NewKnocker(n Notifier, hub *Hub, count LobbyCount, logger *log.Logger, now func() time.Time) *Knocker {
	if now == nil {
		now = time.Now
	}
	k := &Knocker{n: n, hub: hub, count: count, log: logger, now: now,
		state: map[string]*knockState{}, pending: map[string]bool{}, queue: make(chan string, 1024)}
	hub.OnOwnerWatching(k.Kick)
	return k
}

// Kick asks the knocker to look at meetingID again: after a knock, an
// admit, a deny, an end, or the owner opening the meeting. It never blocks.
func (k *Knocker) Kick(meetingID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending[meetingID] {
		return
	}
	select {
	case k.queue <- meetingID:
		k.pending[meetingID] = true
	default:
		k.log.Printf("knock: the queue is full; a lobby change is not notified")
	}
}

// Run decides each kicked meeting until ctx ends.
func (k *Knocker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-k.queue:
			k.mu.Lock()
			delete(k.pending, id)
			k.mu.Unlock()
			k.decide(ctx, id)
		}
	}
}

func (k *Knocker) decide(ctx context.Context, id string) {
	subject, waiting, err := k.count(ctx, id)
	if err != nil {
		k.log.Printf("knock: reading the lobby: %v", err)
		return
	}
	k.mu.Lock()
	s := k.state[id]
	if s == nil {
		s = &knockState{}
		k.state[id] = s
	}
	k.mu.Unlock()
	defer func() {
		if !s.out && s.sent == 0 {
			k.mu.Lock()
			delete(k.state, id)
			k.mu.Unlock()
		}
	}()
	switch {
	case waiting == 0:
		// The lobby emptied (everyone admitted or denied, or the meeting
		// ended): nothing is waiting, so nothing should be on a phone.
		k.withdraw(ctx, subject, id, s)
		s.sent = 0
	case k.hub.OwnerWatching(id):
		// The owner is looking at the meeting: the row is in front of them.
		k.withdraw(ctx, subject, id, s)
	case waiting > s.sent:
		if k.now().Before(s.retryAt) {
			return // a retry is already scheduled for then
		}
		queued, err := k.n.Notify(ctx, subject, id, min(waiting, MaxKnockCount))
		if err == nil {
			s.sent = waiting
			s.out = s.out || queued
			return
		}
		if e := muster.AsError(err); e != nil && e.Kind == muster.KindRateLimited {
			// One per meeting per 30 s on muster's side: try again then, with
			// whatever the count is by that time.
			s.retryAt = k.now().Add(e.RetryAfter)
			time.AfterFunc(e.RetryAfter, func() { k.Kick(id) })
			return
		}
		// 404: muster cannot reach this person now (their grant ended); 503:
		// push is not set up. The lobby row stays either way.
		k.log.Printf("knock: muster did not take the knock: %s", kindOf(err))
	}
}

func (k *Knocker) withdraw(ctx context.Context, subject, id string, s *knockState) {
	if !s.out || subject == "" {
		s.out = false
		return
	}
	if _, err := k.n.Withdraw(ctx, subject, id); err != nil {
		k.log.Printf("knock: muster did not withdraw the knock: %s", kindOf(err))
		return
	}
	s.out = false
}

func kindOf(err error) string {
	if e := muster.AsError(err); e != nil {
		return string(e.Kind)
	}
	return "error"
}
