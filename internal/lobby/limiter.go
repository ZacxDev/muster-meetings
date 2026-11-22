package lobby

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The per-address knock limit (design: "10 knocks per source address per
// 10 minutes").
const (
	KnocksPerAddress = 10
	KnockWindow      = 10 * time.Minute
)

// Limiter counts knocks per source address in a sliding window. It is in
// memory: one replica (design), and a restart forgetting the counts only
// ever lets a few more knocks through, each of which the owner still sees.
type Limiter struct {
	Max    int
	Window time.Duration

	mu   sync.Mutex
	seen map[netip.Addr][]time.Time
}

// NewLimiter is the design's limit.
func NewLimiter() *Limiter { return NewLimiterOf(KnocksPerAddress, KnockWindow) }

// NewLimiterOf is a limiter of max per window, its own bucket (the guest
// card's, beside the knocks').
func NewLimiterOf(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, seen: map[netip.Addr][]time.Time{}}
}

// Allow records a knock from addr at now and says whether it is within the
// limit. A refused knock is not recorded.
func (l *Limiter) Allow(addr netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 10000 {
		l.prune(now)
	}
	ts := l.seen[addr]
	keep := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < l.Window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.Max {
		l.seen[addr] = keep
		return false
	}
	l.seen[addr] = append(keep, now)
	return true
}

func (l *Limiter) prune(now time.Time) {
	for a, ts := range l.seen {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.Window {
			delete(l.seen, a)
		}
	}
}

// ClientAddr is the address a request came from. With no trusted proxies it
// is the connection's peer. When the peer is one of trusted (the ingress in
// front of the plugin):
//
//   - with header empty, it is the right-most X-Forwarded-For entry that is
//     not itself trusted: the first hop a trusted proxy saw;
//   - with header set (MEETINGS_CLIENT_IP_HEADER, e.g. CF-Connecting-IP), it
//     is that header's one address, and X-Forwarded-For is not read. A header
//     that is missing, repeated, a list, or not an address leaves the peer:
//     every such request shares the proxy's count, which can only refuse
//     knocks, never let one through.
//
// A header from a peer that is not trusted is never read, whatever its name.
// An IPv6 address is cut to its /64, so one host cannot spread its knocks
// over its own prefix.
func ClientAddr(r *http.Request, trusted []netip.Prefix, header string) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	addr := peer.Unmap()
	if inAny(addr, trusted) {
		if header != "" {
			if vs := r.Header.Values(header); len(vs) == 1 {
				if a, err := netip.ParseAddr(strings.TrimSpace(vs[0])); err == nil {
					addr = a.Unmap()
				}
			}
		} else {
			addr = forwardedFor(r, addr, trusted)
		}
	}
	if addr.Is6() {
		p, _ := addr.Prefix(64)
		return p.Addr()
	}
	return addr
}

// forwardedFor walks X-Forwarded-For from the right, past every trusted hop,
// starting from the trusted peer addr.
func forwardedFor(r *http.Request, addr netip.Addr, trusted []netip.Prefix) netip.Addr {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = a.Unmap()
		if !inAny(addr, trusted) {
			break
		}
	}
	return addr
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
