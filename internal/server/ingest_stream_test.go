package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// 🔴 "The ingest never pings" (design; spike S2: Egress never answers a
// ping, and with a 20 s ping interval and a 20 s pong timeout every stream
// died at exactly 40 s). An Egress-like client that only writes, in real
// time, for 45 s: the server sends it NOTHING (no ping, no close), and the
// stream is still alive at the end (its next utterance becomes a line).
func TestTheIngestNeverPingsAWriteOnlyClient(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.meeting(ada)
	e.started(c)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	a := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	_, cl := dialStatus(t, e.egressOf(a, 1).URL)
	cl.Watch()
	quiet := egresstest.Silence(1000)
	for i := 0; i < 45; i++ {
		if err := cl.Stream(quiet, 10*time.Millisecond); err != nil {
			t.Fatalf("after %d s the stream refused a write: %v", i, err)
		}
		if got := cl.Received(); len(got) > 0 {
			t.Fatalf("after %d s the server sent a write-only client %d bytes (first %#x): a ping or a close", i, len(got), got[0])
		}
	}
	if err := cl.Stream(speech, 0); err != nil {
		t.Fatalf("after 45 s the stream refused a write: %v", err)
	}
	if ls := segmentsOf(t, owner, 1); ls[0].Text != fakeScript[0] {
		t.Fatalf("after 45 s the line is %q", ls[0].Text)
	}
	if got := cl.Received(); len(got) > 0 {
		t.Fatalf("the server sent %d bytes", len(got))
	}
}

// 🔴 A guest's stream carries the transcript only while they are admitted:
// once removed, their stream ends, and no later line reaches it.
func TestARemovedGuestGetsNoMoreOfTheTranscript(t *testing.T) {
	e := newEnv(t)
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	ticket, _ := e.admitGuest(c, grace)
	guest := e.guestEvents(c.Code, ticket)
	guest.want("meeting")
	resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/participants/"+e.pidAny(c, grace), c.Sess, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("remove = %d %s", resp.StatusCode, raw)
	}
	guest.want("removed")
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	e.publish(c, store.OwnerIdentity, "MICROPHONE")
	segmentsOf(t, owner, 2)
	if strings.Contains(guest.text(), fakeScript[0]) {
		t.Fatalf("a removed guest's stream carried a line:\n%s", guest.text())
	}
	if resp, _ := e.do("POST", "/j/"+c.Code+"/events", "", map[string]string{"ticket": ticket}); resp.StatusCode != 403 {
		t.Fatalf("the removed guest's /events = %d, want 403", resp.StatusCode)
	}
}
