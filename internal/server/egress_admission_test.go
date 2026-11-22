package server_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// 🔴 LiveKit's own Egress worker joins the room as a participant (kind
// EGRESS) to subscribe to the track the plugin asked it to stream. The
// admission rule must not remove it, or no track egress ever streams and
// nothing is transcribed; and it must key on the kind LiveKit reports, never
// on the identity, so an EG_-looking identity of any other kind is judged
// like anyone else.

// egressID is an identity shaped like LiveKit's egress ids (Egress joins
// under its egress id); impostor is the same shape on a STANDARD participant.
const (
	egressID = "EG_Vh94mbLbVAdKq"
	impostor = "EG_impostor00000"
)

func (e *env) joinedWebhook(room, identity string, kind livekit.Kind) {
	e.t.Helper()
	if st, err := e.lk.ParticipantJoined(e.lk.WebhookURL, room, identity, kind); err != nil || st != 200 {
		e.t.Fatalf("participant_joined for %s (%s) = %d, %v", identity, kind, st, err)
	}
}

// The webhook path: participant_joined for LiveKit's Egress is not acted on;
// for a STANDARD participant with an EG_ identity that is not admitted, and
// for every other server-side kind, it removes them (the positive controls).
func TestTheJoinWebhookKeepsLiveKitsEgressAndRemovesAnEGIdentityOfAnotherKind(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.lk.PutKind(c.Room, egressID, livekit.KindEgress)
	e.joinedWebhook(c.Room, egressID, livekit.KindEgress)
	if !slices.Contains(e.lk.InRoom(c.Room), egressID) || e.lk.CountCalls("RemoveParticipant "+c.Room+" "+egressID) != 0 {
		t.Fatalf("participant_joined removed LiveKit's Egress worker (kind EGRESS): the room holds %v", e.lk.InRoom(c.Room))
	}

	e.lk.PutKind(c.Room, impostor, livekit.KindStandard)
	e.joinedWebhook(c.Room, impostor, livekit.KindStandard)
	if slices.Contains(e.lk.InRoom(c.Room), impostor) {
		t.Fatalf("participant_joined kept a STANDARD participant with an EG_ identity that is not admitted: the room holds %v", e.lk.InRoom(c.Room))
	}
	// The plugin starts no ingress, SIP call or agent: each would be a
	// speaker or listener the owner never admitted.
	for _, k := range []livekit.Kind{livekit.KindIngress, livekit.KindSIP, livekit.KindAgent} {
		id := "x-" + strings.ToLower(string(k)) + "-1"
		e.lk.PutKind(c.Room, id, k)
		e.joinedWebhook(c.Room, id, k)
		if slices.Contains(e.lk.InRoom(c.Room), id) {
			t.Errorf("participant_joined kept a participant of kind %s that is not admitted", k)
		}
	}
	if !slices.Contains(e.lk.InRoom(c.Room), egressID) {
		t.Fatalf("the Egress worker is gone after the other joins: %v", e.lk.InRoom(c.Room))
	}

	// An Egress in an ENDED meeting's room is no reason to keep the room:
	// it is removed and the room deleted, as for anyone else.
	e.lk.SetDown(true)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	e.lk.SetDown(false)
	if _, ok := e.lk.Room(c.Room); !ok {
		t.Fatal("the fixture: the room should have survived the failed delete")
	}
	e.joinedWebhook(c.Room, egressID, livekit.KindEgress)
	if _, ok := e.lk.Room(c.Room); ok {
		t.Fatalf("an Egress joining an ended meeting's room kept the room: %v", e.lk.InRoom(c.Room))
	}
}

// The reconcile path: a listed Egress worker stays, a listed STANDARD
// participant with an EG_ identity that is not admitted is removed, and the
// owner stays (the positive control). No webhook is involved: Put and
// PutKind post none.
func TestTheReconcileLoopKeepsLiveKitsEgressAndRemovesAnEGIdentityOfAnotherKind(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.lk.PutKind(c.Room, egressID, livekit.KindEgress)
	e.lk.PutKind(c.Room, impostor, livekit.KindStandard)
	e.lk.PutKind(c.Room, "x-agent-1", livekit.KindAgent)
	e.lk.Put(c.Room, store.OwnerIdentity)
	e.server.Reconcile(context.Background())
	got := strings.Join(e.lk.InRoom(c.Room), ",")
	if !strings.Contains(got, egressID) {
		t.Fatalf("the reconcile loop removed LiveKit's Egress worker (kind EGRESS): the room holds %q", got)
	}
	if want := egressID + ",owner"; got != want {
		t.Fatalf("after one pass the room holds %q, want %q (the EG_ impostor and the agent removed)", got, want)
	}
}

// End to end through the fake's Egress, which joins the room as the real one
// does (kind EGRESS, its egress id as the identity) and posts
// participant_joined before it dials the ingest: the owner's microphone is
// transcribed and the Egress is never removed. This is the live failure:
// before the fix the plugin removed Egress ~2 ms after it joined, the
// egress ended EGRESS_FAILED, and the restart loop never got audio through.
func TestATranscribedTracksEgressWorkerIsNotRemovedFromTheRoom(t *testing.T) {
	e := newEnv(t)
	e.lk.EgressAudio = func(room, identity, track string) []byte { return speech }
	e.lk.EgressPace = time.Millisecond
	c := e.meeting(ada)
	e.started(c)
	track := e.publish(c, store.OwnerIdentity, "MICROPHONE")
	g := e.egressOf(track, 1)
	eventually(t, 5*time.Second, "the first egress streamed or was removed", func() bool {
		for _, x := range e.lk.Egresses() {
			if x.ID == g.ID {
				return x.Streamed || x.Removed
			}
		}
		return false
	})
	for _, x := range e.lk.Egresses() {
		if x.Removed {
			t.Fatalf("the plugin removed LiveKit's Egress worker %s from the room; calls: %v", x.ID, e.lk.Calls())
		}
	}
	if n := e.lk.CountCalls("RemoveParticipant"); n != 0 {
		t.Fatalf("RemoveParticipant was called %d times during a transcribed call: %v", n, e.lk.Calls())
	}
	if !slices.Contains(e.lk.InRoom(c.Room), g.ID) {
		t.Fatalf("the active egress's worker is not in the room: %v", e.lk.InRoom(c.Room))
	}
}

// The real server's bodies through the real admission code: the
// participant_joined webhooks livekit-server 1.13.11 sent for an
// egress-kind participant and for a STANDARD one with an EG_ identity
// (internal/livekit/testdata, recorded from a real run; see the livekit
// package's TestTheParticipantKindIsReadFromTheRealServersWebhookAndList),
// re-signed for the env and posted as LiveKit posts them. Only the room name
// is changed to the env's meeting's.
func TestTheRealServersEgressWebhookBodyIsNotRemoved(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	for _, tc := range []struct {
		file, identity string
		kind           livekit.Kind
		kept           bool
	}{
		{"real-webhook-participant-joined-egress.json", "EG_capture0001", livekit.KindEgress, true},
		{"real-webhook-participant-joined-standard.json", "EG_impostor01", livekit.KindStandard, false},
	} {
		raw, err := os.ReadFile("../livekit/testdata/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(raw), `"name":"meeting-capture"`) != 1 {
			t.Fatalf("%s: the fixture's room name is not where the test expects it", tc.file)
		}
		body := []byte(strings.Replace(string(raw), `"name":"meeting-capture"`, `"name":"`+c.Room+`"`, 1))
		e.lk.PutKind(c.Room, tc.identity, tc.kind)
		auth, err := livekittest.Keys.SignWebhook(body, e.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		if st, err := livekittest.PostWebhook(e.lk.WebhookURL, body, auth); err != nil || st != 200 {
			t.Fatalf("%s: %d %v", tc.file, st, err)
		}
		if in := slices.Contains(e.lk.InRoom(c.Room), tc.identity); in != tc.kept {
			t.Errorf("%s: after the real server's participant_joined, %s in the room = %v, want %v", tc.file, tc.identity, in, tc.kept)
		}
	}
}
