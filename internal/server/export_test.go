package server

import "testing"

// SetBeforeCreateHookForTest sets the hook New meeting runs between the
// owner's admission and the insert (nil clears it), and clears it when t
// ends.
func SetBeforeCreateHookForTest(t *testing.T, fn func()) {
	testHookBeforeCreate = fn
	t.Cleanup(func() { testHookBeforeCreate = nil })
}
