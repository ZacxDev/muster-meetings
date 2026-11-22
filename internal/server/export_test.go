package server

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// SetBeforeCreateHookForTest sets the hook New meeting runs between the
// owner's admission and the insert (nil clears it), and clears it when t
// ends.
func SetBeforeCreateHookForTest(t *testing.T, fn func()) {
	testHookBeforeCreate = fn
	t.Cleanup(func() { testHookBeforeCreate = nil })
}

// SettleKnocksForTest waits until the knock notifier has decided every
// lobby change made so far.
func (s *Server) SettleKnocksForTest(t *testing.T) {
	t.Helper()
	if !s.knocker.Settle(5 * time.Second) {
		t.Fatal("the knock notifier did not settle in 5 s")
	}
}

// OwnerWatchingForTest says an owner event stream is registered on
// meetingID: the server handler's own state, which outlives the client's
// view of a closed stream until the handler returns.
func (s *Server) OwnerWatchingForTest(meetingID string) bool { return s.hub.OwnerWatching(meetingID) }

// SetStreamCapsForTest sets the per-address and total event-stream caps.
func (s *Server) SetStreamCapsForTest(perAddress, total int) {
	s.cfg.StreamsPerAddress, s.cfg.MaxStreams = perAddress, total
}

// SetTrustedProxiesForTest replaces the trusted proxies.
func (s *Server) SetTrustedProxiesForTest(p []netip.Prefix) { s.cfg.TrustedProxies = p }

// SetClientIPHeaderForTest sets the client-address header's name.
func (s *Server) SetClientIPHeaderForTest(h string) { s.cfg.ClientIPHeader = h }

// AmendLogsForTest is how many meetings hold an amend log (late closing
// marks kept for the event streams).
func (s *Server) AmendLogsForTest() int {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	return len(s.tr.amends)
}

// SetAmendLogGraceForTest sets how long a live meeting's amend log outlives
// its last ingest stream, until t ends.
func SetAmendLogGraceForTest(t *testing.T, d time.Duration) {
	old := amendLogGrace
	amendLogGrace = d
	t.Cleanup(func() { amendLogGrace = old })
}

// RunContactsForTest is Run's contacts step alone (the boot's outbox rule).
func (s *Server) RunContactsForTest(ctx context.Context) { s.runContacts(ctx) }
