package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The call's owner routes (design: "The plugin's HTTP API"):
//
//	POST   /api/meetings/{id}/token                  the owner's LiveKit token
//	POST   /api/meetings/{id}/record {"on"}          Record off any time; on only before any guest knocked
//	POST   /api/meetings/{id}/ai {"on"}              AI action items, the same rule
//	GET    /api/meetings/{id}/events                 the owner's event stream: the meeting, its notice and its lobby
//	POST   /api/meetings/{id}/lobby/{pid}/admit      admit a waiting guest
//	POST   /api/meetings/{id}/lobby/{pid}/deny       deny a waiting guest
//	DELETE /api/meetings/{id}/participants/{pid}     remove an admitted guest
//	POST   /api/meetings/{id}/end                    End meeting
//
// Each goes through owner(), so the meeting is always the session's own
// person's: another person's meeting is the same 404 as none.

// CallsUnavailable is what New meeting and the call say when LiveKit does
// not answer (design: "LiveKit down").
const CallsUnavailable = "Calls are unavailable right now. Try again in a moment."

// liveKitErr marks a failure of the LiveKit server API.
type liveKitErr struct{ err error }

func (e liveKitErr) Error() string { return e.err.Error() }
func (e liveKitErr) Unwrap() error { return e.err }

// tokenAnswer is what a participant connects with.
type tokenAnswer struct {
	LiveKitURL string `json:"livekit_url"`
	Token      string `json:"token"`
	Identity   string `json:"identity"`
	ICE        string `json:"ice"`
}

func (s *Server) meetingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such meeting"})
	case errors.Is(err, store.ErrGone):
		writeJSON(w, http.StatusGone, map[string]any{"error": "this meeting has ended"})
	default:
		s.cfg.Log.Printf("meeting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be read"})
	}
}

// handleOwnerToken is the owner's LiveKit token for their live meeting, with
// the owner grant (livekit.OwnerGrant). It makes sure the room exists first:
// a room LiveKit closed while nobody was in it is made again.
func (s *Server) handleOwnerToken(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	now := s.cfg.Now()
	m, room, err := repo.LiveMeeting(r.Context(), r.PathValue("id"), now)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if _, err := s.cfg.LiveKit.CreateRoom(r.Context(), room); err != nil {
		s.cfg.Log.Printf("token: LiveKit: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": CallsUnavailable})
		return
	}
	name, err := repo.DisplayName(r.Context())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if name == "" {
		name = "Host"
	}
	tok, err := s.cfg.LiveKit.Keys().Mint(livekit.Participant{Identity: store.OwnerIdentity, Name: name}, livekit.OwnerGrant(room), now)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	if err := repo.MarkStarted(r.Context(), m.ID, now); err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenAnswer{LiveKitURL: s.cfg.LiveKitURL, Token: tok, Identity: store.OwnerIdentity, ICE: s.cfg.ICE})
}

// readJSON reads a small JSON body into v; false (and a 400 written) when it
// is not one.
func readJSON(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil || json.Unmarshal(raw, v) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not the JSON this route takes"})
		return false
	}
	return true
}

// handleSwitch turns Record or AI on or off. Off is always allowed; on is
// 409 once a guest has knocked (store.SetSwitch).
func (s *Server) handleSwitch(which store.Switch) ownerHandler {
	return func(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
		var in struct {
			On *bool `json:"on"`
		}
		if !readJSON(w, r, 256, &in) {
			return
		}
		if in.On == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": `send {"on": true} or {"on": false}`})
			return
		}
		id := r.PathValue("id")
		n, err := repo.SetSwitch(r.Context(), id, which, *in.On, s.cfg.Now())
		if errors.Is(err, store.ErrSwitchLocked) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "A guest has already asked to join, so this can only stay off."})
			return
		}
		if err != nil {
			s.meetingErr(w, err)
			return
		}
		if which == store.SwitchRecord && !*in.On {
			// Off any time: the recording stops at once.
			if start, err := s.store.MeetingStart(r.Context(), id); err == nil {
				at := s.sinceStart(start)
				s.stopRecording(r.Context(), id, &at)
			}
		}
		s.hub.Changed(id)
		writeJSON(w, http.StatusOK, map[string]any{"notice": s.noticeFor(n.Record, n.AI)})
	}
}

// handleMove admits, denies or removes one guest. A removal is done in the
// design's order: the identity is marked removed in the database (so the
// participant_joined check and the reconcile loop refuse it from now on, and
// its ticket is dead), then LiveKit's RemoveParticipant drops the
// connection. If LiveKit does not answer, the reconcile loop removes the
// identity within one tick.
func (s *Server) handleMove(ev store.Event) ownerHandler {
	return func(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
		id, pid := r.PathValue("id"), r.PathValue("pid")
		if !store.ValidParticipantID(pid) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such person in this meeting"})
			return
		}
		mv, err := repo.Move(r.Context(), id, pid, ev, s.cfg.Now())
		switch {
		case errors.Is(err, store.ErrCallFull):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": fmt.Sprintf("The call is full: %d people at most.", livekit.MaxParticipants)})
			return
		case errors.Is(err, store.ErrState):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "That person is no longer waiting, or no longer in the call."})
			return
		case errors.Is(err, store.ErrNotFound):
			if _, err := repo.Meeting(r.Context(), id); err != nil {
				s.meetingErr(w, err)
				return
			}
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such person in this meeting"})
			return
		case err != nil:
			s.meetingErr(w, err)
			return
		}
		out := map[string]any{"ok": true}
		if ev == store.EvAdmit {
			// The recording starts with the first admitted guest (design:
			// "Started when the first guest is admitted with Record on");
			// a part already running makes this nothing.
			s.startRecording(id)
		}
		if ev == store.EvRemove {
			err := s.cfg.LiveKit.RemoveParticipant(r.Context(), mv.Room, mv.Identity)
			if err != nil {
				s.cfg.Log.Printf("remove: LiveKit: %v (the reconcile loop retries)", err)
			}
			out["disconnected"] = err == nil
		}
		s.hub.Changed(id)
		s.knocker.Kick(id)
		writeJSON(w, http.StatusOK, out)
	}
}

// handleEnd is End meeting: the meeting ends and its link dies (410), every
// waiting stream is told "ended", and the room is deleted, which disconnects
// everyone. Every token for the room is dead from then on: none of its
// identities passes MayBeInRoom, so a client that reconnects with a token it
// still holds is removed by the participant_joined check, and a room LiveKit
// re-creates for it is deleted by the reconcile loop.
func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	room, err := repo.End(r.Context(), id, s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	s.hub.Changed(id)
	s.knocker.Kick(id)
	s.stopTranscription(r.Context(), id)
	s.stopRecording(r.Context(), id, nil)
	// The post-call pass (only with the analysis configured).
	s.kickPost()
	if err := s.cfg.LiveKit.DeleteRoom(r.Context(), room); err != nil {
		s.cfg.Log.Printf("end: LiveKit: %v (the reconcile loop retries)", err)
	}
	m, err := repo.Meeting(r.Context(), id)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meeting": m})
}

// ownerSnapshot is one event on the owner's stream: the meeting, its notice,
// whether its switches are locked off (a guest has knocked), and its lobby.
type ownerSnapshot struct {
	Meeting store.Meeting       `json:"meeting"`
	Notice  lobby.Notice        `json:"notice"`
	Locked  bool                `json:"locked"`
	Lobby   []store.Participant `json:"lobby"`
}

// sseStart writes an event stream's headers. False when the writer cannot
// stream.
func sseStart(w http.ResponseWriter) (http.Flusher, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming is not supported here"})
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return f, true
}

// sse writes one event. Event names and JSON data never hold a newline that
// would break the frame (json.Marshal escapes them).
func sse(w io.Writer, f http.Flusher, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// stream runs an event stream: send is called once at once, then on every
// change the hub signals for meetingID, every StreamRecheck, and a comment is
// sent every Heartbeat. It stops when send says done, the client goes away,
// or a write fails.
func (s *Server) stream(ctx context.Context, w http.ResponseWriter, f http.Flusher, meetingID string, owner bool, send func() (done bool, err error)) {
	wake, stop := s.hub.Watch(meetingID, owner)
	defer stop()
	recheck := time.NewTicker(s.cfg.StreamRecheck)
	defer recheck.Stop()
	beat := time.NewTicker(s.cfg.Heartbeat)
	defer beat.Stop()
	for {
		done, err := send()
		if done || err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-recheck.C:
		case <-beat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			f.Flush()
			continue
		}
	}
}

// handleOwnerEvents is the owner's event stream (read with fetch, since it
// needs the bearer): a "meeting" event with the whole snapshot at once and on
// every change, and a last one when the meeting ends. While it is open the
// owner is "looking at the meeting": no knock is pushed to their phone, and
// one that was is withdrawn.
func (s *Server) handleOwnerEvents(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	id := r.PathValue("id")
	if _, err := repo.Meeting(r.Context(), id); err != nil {
		s.meetingErr(w, err)
		return
	}
	ctx, release, ok := s.acquireStream(w, r, "")
	if !ok {
		return
	}
	defer release()
	f, ok := sseStart(w)
	if !ok {
		return
	}
	var last []byte
	var lastItems string
	tc := newTCursor()
	s.stream(ctx, w, f, id, true, func() (bool, error) {
		// 🔴 The session is checked again before every send, not only when
		// the stream opened: an expired grant deletes the owner's sessions
		// and keeps the meeting running, and a stream opened before that must
		// stop showing its lobby (the guests' names) then. The page's next
		// request is 401: "Open Meetings from muster again".
		if ok, err := repo.SessionLive(r.Context(), s.cfg.Now()); err != nil || !ok || !s.app.Live() {
			return true, err
		}
		m, err := repo.Meeting(r.Context(), id)
		if err != nil {
			return true, err
		}
		lob, err := repo.Lobby(r.Context(), id)
		if err != nil {
			return true, err
		}
		liveNow := m.State == "open" && s.cfg.Now().Before(m.LinkExpiresAt)
		if !liveNow {
			m.State = "ended"
			lob = []store.Participant{}
		}
		snap := ownerSnapshot{Meeting: m, Notice: s.noticeOf(r.Context(), id, m.Record, m.AI), Locked: len(lob) > 0, Lobby: lob}
		if !snap.Locked && liveNow {
			snap.Locked, err = repo.Knocked(r.Context(), id)
			if err != nil {
				return true, err
			}
		}
		b, _ := json.Marshal(snap)
		if string(b) != string(last) {
			last = b
			if err := sse(w, f, "meeting", snap); err != nil {
				return true, err
			}
		}
		if liveNow {
			// The live action items (only with the analysis configured).
			if err := s.sendItems(r.Context(), w, f, id, &lastItems); err != nil {
				return true, err
			}
			// The transcript, through the owner's repository: only this
			// owner's meeting's lines.
			if err := s.sendTranscript(r.Context(), w, f, id, tc, func(after int64) ([]store.Segment, error) {
				return repo.SegmentsAfter(r.Context(), id, after, store.MaxSegmentsPage)
			}); err != nil {
				return true, err
			}
		}
		return !liveNow, nil
	})
}

// ---- the guests' LiveKit token ----

// guestToken mints an admitted guest's LiveKit token, with the guest grant
// (livekit.GuestGrant) for the meeting's own room. 🔴 It is the only place a
// guest token is minted, and it first asks store.MayBeInRoom, the one
// admission predicate: a waiting, denied, removed or left guest, or anyone
// in an ended meeting, gets none.
func (s *Server) guestToken(ctx context.Context, g store.Guest) (tokenAnswer, bool, error) {
	ok, err := s.store.MayBeInRoom(ctx, g.Room, g.Identity, s.cfg.Now())
	if err != nil || !ok {
		return tokenAnswer{}, false, err
	}
	tok, err := s.cfg.LiveKit.Keys().Mint(livekit.Participant{Identity: g.Identity, Name: guestLabel(g.Name)}, livekit.GuestGrant(g.Room), s.cfg.Now())
	if err != nil {
		return tokenAnswer{}, false, err
	}
	return tokenAnswer{LiveKitURL: s.cfg.LiveKitURL, Token: tok, Identity: g.Identity, ICE: s.cfg.ICE}, true, nil
}

// guestLabel is how a guest is named to everyone: what they typed, then
// "(guest)" (design: "shown … with "(guest)" after it").
func guestLabel(name string) string { return strings.TrimSpace(name) + guestSuffix }
