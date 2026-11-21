package owners

import (
	"context"
	"testing"
	"time"
)

// SetSleepForTest replaces Boot's wait for muster's Retry-After until t ends.
func SetSleepForTest(t *testing.T, fn func(context.Context, time.Duration) error) {
	old := sleep
	sleep = fn
	t.Cleanup(func() { sleep = old })
}
