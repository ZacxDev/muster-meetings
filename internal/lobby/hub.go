package lobby

import "sync"

// Hub tells the streams watching a meeting that something about it changed.
// It carries no data: a watcher re-reads the database, so what it sends is
// never older than the change that woke it. It also counts the owner's open
// event streams per meeting: "the owner is looking at it" for the knock
// notification (design: "only while no owner session holds that meeting's
// event stream open").
type Hub struct {
	mu     sync.Mutex
	topics map[string]*topic
	// onOwner, when set, runs (outside the lock) each time an owner stream
	// opens on a meeting: the knocker withdraws the meeting's knock.
	onOwner func(meetingID string)
}

type topic struct {
	subs   map[chan struct{}]bool // the channel, and whether it is an owner's
	owners int
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{topics: map[string]*topic{}} }

// OnOwnerWatching sets the function run each time an owner stream opens.
func (h *Hub) OnOwnerWatching(fn func(meetingID string)) {
	h.mu.Lock()
	h.onOwner = fn
	h.mu.Unlock()
}

// Watch subscribes to meetingID's changes. The channel holds at most one
// pending wake-up (changes coalesce). owner says this is the owner's event
// stream. Call stop when done.
func (h *Hub) Watch(meetingID string, owner bool) (wake <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	t := h.topics[meetingID]
	if t == nil {
		t = &topic{subs: map[chan struct{}]bool{}}
		h.topics[meetingID] = t
	}
	t.subs[ch] = owner
	if owner {
		t.owners++
	}
	fn := h.onOwner
	h.mu.Unlock()
	if owner && fn != nil {
		fn(meetingID)
	}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if t := h.topics[meetingID]; t != nil {
				if t.subs[ch] {
					t.owners--
				}
				delete(t.subs, ch)
				if len(t.subs) == 0 {
					delete(h.topics, meetingID)
				}
			}
		})
	}
}

// Changed wakes every watcher of meetingID.
func (h *Hub) Changed(meetingID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.topics[meetingID]; t != nil {
		for ch := range t.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// OwnerWatching says an owner event stream is open on meetingID.
func (h *Hub) OwnerWatching(meetingID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[meetingID]
	return t != nil && t.owners > 0
}
