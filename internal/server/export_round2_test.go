package server

import (
	"testing"
	"time"
)

// SetIngestHooksForTest sets the hooks between an ingest stream's token
// check and its registration, and inside egressLoop's gap step (nil clears
// either); both are cleared when t ends.
func SetIngestHooksForTest(t *testing.T, beforeRegister, gapCheck func(track string)) {
	testHookBeforeRegister, testHookGapCheck = beforeRegister, gapCheck
	t.Cleanup(func() { testHookBeforeRegister, testHookGapCheck = nil, nil })
}

// ConnNonceForTest is the nonce of track's registered ingest stream, "" for
// none.
func (s *Server) ConnNonceForTest(track string) string {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	if lc := s.tr.conns[track]; lc != nil {
		return lc.nonce
	}
	return ""
}

// RestartWaitForTest is restartWait.
func (s *Server) RestartWaitForTest(track string) time.Duration { return s.restartWait(track) }

// RestartCountedForTest says track has a restart count.
func (s *Server) RestartCountedForTest(track string) bool {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	_, ok := s.tr.restarts[track]
	return ok
}
