package server

import (
	"context"
	"net/http"
	"net/netip"
	"sync"

	"github.com/ZacxDev/muster-meetings/internal/lobby"
)

// The event-stream caps. An event stream holds a connection and a goroutine
// for as long as the client keeps it open, so each is counted:
//
//   - ONE stream per ticket, across /wait and /events: a new one closes the
//     old (a guest page that reconnects replaces its own stream);
//   - StreamsPerAddress per client address (lobby.ClientAddr, so behind
//     MEETINGS_TRUSTED_PROXIES the client the ingress names), across /wait,
//     /events and the owner's stream;
//   - MaxStreams in all.
//
// Past either of the last two the answer is 429 with Retry-After.
const (
	DefaultStreamsPerAddress = 16
	DefaultMaxStreams        = 2000
)

type streamReg struct {
	mu       sync.Mutex
	perAddr  map[netip.Addr]int
	total    int
	byTicket map[string]*streamEntry
}

type streamEntry struct {
	addr     netip.Addr
	ticket   string
	cancel   context.CancelFunc
	released bool
}

func newStreamReg() *streamReg {
	return &streamReg{perAddr: map[netip.Addr]int{}, byTicket: map[string]*streamEntry{}}
}

// release frees e's slot once. The caller holds the lock.
func (g *streamReg) release(e *streamEntry) {
	if e.released {
		return
	}
	e.released = true
	g.total--
	if g.perAddr[e.addr]--; g.perAddr[e.addr] <= 0 {
		delete(g.perAddr, e.addr)
	}
	if e.ticket != "" && g.byTicket[e.ticket] == e {
		delete(g.byTicket, e.ticket)
	}
}

// acquireStream takes a stream slot for r. ticket is the participant a
// guest stream is for ("" for the owner's): an older stream of the same
// ticket is closed first. It answers the stream's context (cancelled when a
// newer stream of the ticket replaces it) and the function that frees the
// slot; or, past a cap, writes the 429 and answers ok false.
func (s *Server) acquireStream(w http.ResponseWriter, r *http.Request, ticket string) (context.Context, func(), bool) {
	addr := s.clientAddr(r)
	g := s.streams
	g.mu.Lock()
	if old := g.byTicket[ticket]; ticket != "" && old != nil {
		g.release(old)
		old.cancel()
	}
	if g.total >= s.cfg.MaxStreams || g.perAddr[addr] >= s.cfg.StreamsPerAddress {
		g.mu.Unlock()
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many open connections. Try again in a moment."})
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(r.Context())
	e := &streamEntry{addr: addr, ticket: ticket, cancel: cancel}
	g.total++
	g.perAddr[addr]++
	if ticket != "" {
		g.byTicket[ticket] = e
	}
	g.mu.Unlock()
	return ctx, func() {
		g.mu.Lock()
		g.release(e)
		g.mu.Unlock()
		cancel()
	}, true
}

// clientAddr is the address a request is counted against
// (lobby.ClientAddr). With no trusted proxies configured, a request that
// carries X-Forwarded-For says the plugin sits behind a proxy nobody
// declared, so every client is being counted as that proxy: that is logged,
// loudly, once, and /healthz says so from then on.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	if len(s.cfg.TrustedProxies) == 0 && len(r.Header.Values("X-Forwarded-For")) > 0 && s.xffUntrusted.CompareAndSwap(false, true) {
		s.cfg.Log.Printf("WARNING: %s", XFFWarning)
	}
	return lobby.ClientAddr(r, s.cfg.TrustedProxies)
}

// XFFWarning is what the log and /healthz say when X-Forwarded-For arrives
// with MEETINGS_TRUSTED_PROXIES unset.
const XFFWarning = "a request carried X-Forwarded-For but MEETINGS_TRUSTED_PROXIES is unset: every client is counted as the proxy's address for the knock and stream limits; set it to the ingress's CIDRs"
