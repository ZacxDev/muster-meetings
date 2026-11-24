package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
)

// Run starts the server's background work and returns when ctx ends: the
// knock notifier, and the reconcile loop every ReconcileEvery.
func (s *Server) Run(ctx context.Context) {
	defer s.bgStop()
	go func() { <-ctx.Done(); s.bgStop() }()
	go s.knocker.Run(ctx)
	s.runContacts(ctx)
	// Once at start: a restart may have missed webhooks.
	s.Reconcile(ctx)
	// And every transcribed track lost its egress's stream.
	s.resumeTranscription(ctx)
	// A recording waiting to retry lost its loop; the sweeper starts.
	s.resumeRecording(ctx)
	s.runRetention(ctx)
	// The analysis' workers and the notes' outbox (only when configured).
	s.runAI(ctx)
	s.runMail(ctx)
	t := time.NewTicker(s.cfg.ReconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reconcile(ctx)
		}
	}
}

// handleWebhook is LiveKit's webhook, on the internal listener. 🔴 It acts
// on nothing until the request verifies (livekit.Keys.VerifyWebhook: the
// Authorization JWT is signed by the API secret and carries the body's
// SHA-256); anything else is 401. Then:
//
//   - participant_joined: a participant that may not be in the room
//     (mayStay: not the owner's, not an admitted guest's, not LiveKit's own
//     Egress, or the meeting has ended) is removed at once. That is the
//     backstop that ends a removed guest's still-valid token (design:
//     "Removed guest cannot rejoin").
//   - participant_left: nothing. A dropped connection is not a revocation;
//     the guest reconnects (design: "an admitted control guest who
//     reconnects is not" disconnected).
//   - room_finished: the room sat empty for its timeout (or was deleted):
//     the meeting ends and its link dies.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	ev, err := s.cfg.LiveKit.Keys().VerifyWebhook(r, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("webhook: refused: does not verify")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	ctx := r.Context()
	room := ""
	if ev.Room != nil {
		room = ev.Room.Name
	}
	switch ev.Event {
	case "participant_joined":
		if ev.Participant == nil || room == "" {
			break
		}
		s.admitOrRemove(ctx, room, *ev.Participant)
	case "participant_left":
		// 🔴 Not a revocation: a connection that drops (a network blink, a
		// reload) must not cost an admitted guest their admission. They
		// reconnect with a fresh token from /events. An admission ends only by
		// the owner's Remove, the guest's own Leave (/j/{code}/leave), or the
		// meeting's end.
	case "track_published":
		if ev.Participant == nil || ev.Track == nil || room == "" {
			break
		}
		s.onScreenShare(ctx, room, ev.Participant.Identity, *ev.Track)
		s.onTrackPublished(ctx, room, ev.Participant.Identity, *ev.Track)
	case "track_unpublished":
		if ev.Track != nil {
			s.onScreenShareEnded(ctx, *ev.Track)
			s.onTrackUnpublished(ctx, *ev.Track)
		}
	case "egress_updated":
		// A room composite's file began (ACTIVE, with its file's start):
		// recording.go. A track egress's update changes nothing.
		if ev.EgressInfo != nil {
			_, _ = s.onRecordingEgress(ctx, *ev.EgressInfo)
		}
	case "egress_ended":
		// A recording part's, else a track egress's.
		if ev.EgressInfo != nil {
			if owned, err := s.onRecordingEgress(ctx, *ev.EgressInfo); err == nil && !owned {
				s.onEgressEnded(ctx, *ev.EgressInfo)
			}
		}
	case "room_finished":
		if room == "" {
			break
		}
		if id, ended, err := s.store.EndRoom(ctx, room, "LiveKit: room_finished (the room was empty for its timeout)", s.cfg.Now()); err != nil {
			s.cfg.Log.Printf("webhook: room_finished: %v", err)
		} else if ended {
			s.hub.Changed(id)
			s.knocker.Kick(id)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// mayStay is the rule the participant_joined webhook and the reconcile loop
// both apply to a participant LiveKit reports in room (one function, so the
// two paths cannot disagree):
//
//   - LiveKit's own Egress worker (Kind EGRESS) may stay while the room's
//     meeting is live. It is how a track's audio reaches the transcript: the
//     plugin's StartTrackEgress makes Egress join the room as a participant
//     to subscribe to the track, and removing it ends that egress
//     (PARTICIPANT_REMOVED, then egress_ended EGRESS_FAILED). Kind is set by
//     the LiveKit server from the join token's `kind` claim, which only a
//     holder of the API secret can sign and which no token the plugin mints
//     carries; the identity is NOT the test (an "EG_…" identity of any
//     other kind is judged like anyone else).
//   - Everyone else, every other server-side kind included (INGRESS, SIP,
//     AGENT: the plugin starts none of them, and each would be a speaker or
//     listener the owner never admitted), is store.MayBeInRoom: the owner,
//     or an admitted guest, of a live meeting.
func (s *Server) mayStay(ctx context.Context, room string, p livekit.ParticipantInfo) (bool, error) {
	if p.Egress() {
		return s.store.RoomLive(ctx, room, s.cfg.Now())
	}
	return s.store.MayBeInRoom(ctx, room, p.Identity, s.cfg.Now())
}

// admitOrRemove removes p from room unless it may be there (mayStay). When
// the room is an ended meeting's (a client re-created it with a token it
// still held), the room is deleted too.
func (s *Server) admitOrRemove(ctx context.Context, room string, p livekit.ParticipantInfo) {
	ok, err := s.mayStay(ctx, room, p)
	if err != nil {
		s.cfg.Log.Printf("webhook: the admission check failed: %v", err)
		return
	}
	if ok {
		return
	}
	if err := s.cfg.LiveKit.RemoveParticipant(ctx, room, p.Identity); err != nil {
		s.cfg.Log.Printf("webhook: removing an identity that is not admitted: %v (the reconcile loop retries)", err)
	} else {
		s.cfg.Log.Printf("webhook: removed an identity that is not admitted (participant kind %q)", p.Kind)
	}
	s.deleteIfEnded(ctx, room)
}

// deleteIfEnded deletes room when it is one of the plugin's meetings and
// that meeting is no longer live. A room no meeting has is left alone: it is
// not the plugin's.
func (s *Server) deleteIfEnded(ctx context.Context, room string) {
	if _, _, err := s.store.MeetingOfRoom(ctx, room); err != nil {
		return
	}
	live, err := s.store.RoomLive(ctx, room, s.cfg.Now())
	if err != nil || live {
		return
	}
	if err := s.cfg.LiveKit.DeleteRoom(ctx, room); err != nil {
		s.cfg.Log.Printf("reconcile: deleting an ended meeting's room: %v", err)
	}
}

// Reconcile is one pass of the loop that does not trust the webhook (design:
// "A webhook can be lost, so a reconcile loop also calls ListParticipants for
// each live meeting every 15 s and removes any identity not admitted"):
//
//   - for every live meeting, every participant in its room that may not
//     be there (mayStay) is removed;
//   - every room LiveKit holds that belongs to a meeting no longer live is
//     deleted. That ends the calls of meetings ended by any path (End
//     meeting whose DeleteRoom failed, the 24 h cap, an uninstall, a person
//     muster no longer lets the app act for), and of a room a held token
//     re-created after the end.
//
// A LiveKit failure is logged and retried on the next pass.
func (s *Server) Reconcile(ctx context.Context) {
	now := s.cfg.Now()
	rooms, err := s.store.LiveRooms(ctx, now)
	if err != nil {
		s.cfg.Log.Printf("reconcile: %v", err)
		return
	}
	for _, room := range rooms {
		ps, err := s.cfg.LiveKit.ListParticipants(ctx, room)
		if err != nil {
			s.cfg.Log.Printf("reconcile: LiveKit: %v", err)
			return
		}
		for _, p := range ps {
			s.admitOrRemove(ctx, room, p)
		}
	}
	all, err := s.cfg.LiveKit.ListRooms(ctx)
	if err != nil {
		s.cfg.Log.Printf("reconcile: LiveKit: %v", err)
		return
	}
	for _, rm := range all {
		if strings.HasPrefix(rm.Name, "meeting-") {
			s.deleteIfEnded(ctx, rm.Name)
		}
	}
	s.reconcileRecordings(ctx)
}
