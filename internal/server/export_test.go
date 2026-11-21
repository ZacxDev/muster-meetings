package server

import (
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
