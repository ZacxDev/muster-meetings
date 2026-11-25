package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// cancelOn is the LiveKit client's transport: on the first call to the
// Twirp method it names it cancels the End request's context (the client
// going away mid-teardown) before sending the call on, so a call made on
// that context fails as it would have.
type cancelOn struct {
	method string
	mu     sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOn) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/"+c.method) {
		c.mu.Lock()
		if c.cancel != nil && !c.fired {
			c.fired = true
			c.cancel()
		}
		c.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// 🔴 Regression: End answered 500 for a meeting it had ended when the
// request's context was cancelled during the teardown (the room's deletion
// failed with "context canceled", and so did the read for the answer). Once
// the meeting has ended, End answers 200 with it, and the egress is stopped
// and the room deleted anyway. The context is cancelled at the first
// egress stop, and (another run) at the room's deletion.
func TestEndAnswers200AndTearsDownWhenTheRequestIsCancelledMidTeardown(t *testing.T) {
	for _, method := range []string{"livekit.Egress/StopEgress", "livekit.RoomService/DeleteRoom"} {
		t.Run(method, func(t *testing.T) {
			hook := &cancelOn{method: method}
			e := newEnvOpts(t, nil, func(c *server.Config) { c.LiveKit.HTTP.Transport = hook })
			c := e.meeting(ada)
			// The owner's token makes the room, as joining does; the
			// owner's microphone gets a transcription egress.
			e.started(c)
			e.egressOf(e.publish(c, store.OwnerIdentity, "MICROPHONE"), 1)
			if _, ok := e.lk.Room(c.Room); !ok {
				t.Fatal("no room before End (the fixture of this test)")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook.mu.Lock()
			hook.cancel = cancel
			hook.mu.Unlock()
			req := httptest.NewRequestWithContext(ctx, "POST", "/api/meetings/"+c.ID+"/end", nil)
			req.Header.Set("Authorization", "Bearer "+c.Sess)
			rec := httptest.NewRecorder()
			e.server.ServeHTTP(rec, req)

			hook.mu.Lock()
			fired := hook.fired
			hook.mu.Unlock()
			if !fired || ctx.Err() == nil {
				t.Fatalf("the request's context was never cancelled at %s: this test reproduces nothing", method)
			}
			var b struct {
				Meeting struct {
					State string `json:"state"`
				} `json:"meeting"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &b)
			if rec.Code != http.StatusOK || b.Meeting.State != "ended" {
				t.Errorf("End with the request cancelled mid-teardown = %d %s, want 200 with the ended meeting", rec.Code, rec.Body.String())
			}
			if n := e.lk.CountCalls("StopEgress"); n == 0 {
				t.Errorf("the transcription egress was never stopped: its stop ran on the cancelled request context")
			}
			if _, ok := e.lk.Room(c.Room); ok {
				t.Errorf("the room survived End: its deletion ran on the cancelled request context")
			}
			if got := e.log.String(); strings.Contains(got, "context canceled") || strings.Contains(got, "stopping an egress") {
				t.Errorf("the teardown failed a LiveKit call:\n%s", got)
			}
		})
	}
}
