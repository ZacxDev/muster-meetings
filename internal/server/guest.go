package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The guest routes (design: "Guests: links, the lobby, admission"). A guest
// has no account and no session: the link's code says which meeting, and
// the ticket a knock answers says which knock. 🔴 A ticket travels only in a
// request body, never in a URL (so never in an access log or a Referer), and
// is held in the guest page's memory only. A route that takes one refuses a
// request with any query string.
//
//	GET  /j/{code}          the guest page; 410 once the meeting has ended or is 24 h old
//	POST /j/{code}/knock    {"name", "notice": {"record", "ai"}} → 201 {"ticket", "notice"}
//	POST /j/{code}/wait     {"ticket"} → an event stream ending in admitted, denied or ended
//	POST /j/{code}/events   {"ticket"} → an admitted guest's event stream: a fresh token, the notice, removed, ended
//	POST /j/{code}/leave    {"ticket"} → the guest leaves; their ticket is spent

// The guest page's sentences.
const (
	GuestNoMeeting = "This link is not a meeting link. Check that you copied all of it."
	GuestEnded     = "This meeting has ended."
	GuestTooMany   = "Too many people asked to join from here. Wait a few minutes and try again."
	GuestLobbyFull = "Too many people are already waiting to join. Try again in a minute."
	GuestNoTicket  = "Ask to join again."
	GuestRemoved   = "You are no longer in this meeting."
)

type guestPage struct {
	Message   string
	OwnerName string
	Notice    lobby.Notice
}

func (s *Server) renderGuest(w http.ResponseWriter, code int, p guestPage) {
	h := w.Header()
	h.Set("Content-Security-Policy", s.guestPageCSP())
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = pageTmpl.ExecuteTemplate(w, "guest.html", p)
}

// handleGuestPage is the link itself. It names whose meeting it is and,
// before anything else, the consent notice for the switches as they are now;
// the knock sends back which notice was shown. The owner's display name is
// rendered as text (html/template escapes it).
func (s *Server) handleGuestPage(w http.ResponseWriter, r *http.Request) {
	g, err := s.store.MeetingByCode(r.Context(), r.PathValue("code"), s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.renderGuest(w, http.StatusNotFound, guestPage{Message: GuestNoMeeting})
		return
	case errors.Is(err, store.ErrGone):
		s.renderGuest(w, http.StatusGone, guestPage{Message: GuestEnded})
		return
	case err != nil:
		s.cfg.Log.Printf("guest page: %v", err)
		s.renderGuest(w, http.StatusInternalServerError, guestPage{Message: "Something went wrong. Try again in a moment."})
		return
	}
	s.renderGuest(w, http.StatusOK, guestPage{OwnerName: g.OwnerName, Notice: s.noticeFor(g.Notice.Record, g.Notice.AI)})
}

// guestErr answers a guest route's refusal as JSON.
func guestErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// noQuery refuses a guest route called with any query string: a ticket must
// never be sent in a URL, and nothing these routes take is.
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		guestErr(w, http.StatusBadRequest, "This route takes no query string: send the ticket in the body.")
		return false
	}
	return true
}

// handleKnock stores a knock: the guest's name, checked (lobby.CleanName),
// and the notice the page showed them ({"record", "ai", "transcribed"}),
// which must be the meeting's current one (409 with the current notice
// otherwise, and the page shows it again before the guest may knock). Limits: 10 knocks per source address per 10
// minutes, 20 waiting per meeting; past either 429. It answers the ticket,
// which the page keeps in memory.
func (s *Server) handleKnock(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	code := r.PathValue("code")
	var in struct {
		Name   string `json:"name"`
		Notice *struct {
			Record      *bool `json:"record"`
			AI          *bool `json:"ai"`
			Transcribed *bool `json:"transcribed"`
		} `json:"notice"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Notice == nil || in.Notice.Record == nil || in.Notice.AI == nil {
		guestErr(w, http.StatusBadRequest, `Send {"name", "notice": {"record", "ai"}}.`)
		return
	}
	now := s.cfg.Now()
	// The meeting first: a dead link says so whatever else is wrong.
	gm, err := s.store.MeetingByCode(r.Context(), code, now)
	if err != nil {
		s.guestMeetingErr(w, err)
		return
	}
	name, err := lobby.CleanName(in.Name)
	if err != nil {
		guestErr(w, http.StatusBadRequest, lobby.ErrName.Error())
		return
	}
	if !s.limiter.Allow(s.clientAddr(r), now) {
		w.Header().Set("Retry-After", "600")
		guestErr(w, http.StatusTooManyRequests, GuestTooMany)
		return
	}
	// 🔴 Whether the meeting is transcribed is part of the notice: a page
	// that showed another answer (or none: a page from before it was) is
	// the same 409 as a switch that changed, with the current notice.
	if in.Notice.Transcribed == nil || *in.Notice.Transcribed != s.Transcribing() {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "The meeting's notice changed. Read it again before you ask to join.",
			"notice": s.noticeFor(gm.Notice.Record, gm.Notice.AI)})
		return
	}
	k, err := s.store.Knock(r.Context(), code, name, store.Notice{Record: *in.Notice.Record, AI: *in.Notice.AI}, now)
	var mismatch *store.NoticeMismatch
	switch {
	case errors.As(err, &mismatch):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "The meeting's notice changed. Read it again before you ask to join.",
			"notice": s.noticeFor(mismatch.Current.Record, mismatch.Current.AI)})
		return
	case errors.Is(err, store.ErrLobbyFull):
		w.Header().Set("Retry-After", "60")
		guestErr(w, http.StatusTooManyRequests, GuestLobbyFull)
		return
	case err != nil:
		s.guestMeetingErr(w, err)
		return
	}
	s.hub.Changed(k.MeetingID)
	s.knocker.Kick(k.MeetingID)
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": k.Ticket})
}

func (s *Server) guestMeetingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		guestErr(w, http.StatusNotFound, GuestNoMeeting)
	case errors.Is(err, store.ErrGone):
		guestErr(w, http.StatusGone, GuestEnded)
	default:
		s.cfg.Log.Printf("guest: %v", err)
		guestErr(w, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
	}
}

// ticketOf reads {"ticket"} and finds the guest it belongs to on this
// link's meeting. It writes the refusal and answers false when there is
// none: a dead link is 410, and an unknown ticket, or one from another
// meeting, is 404.
func (s *Server) ticketOf(w http.ResponseWriter, r *http.Request) (store.Guest, bool) {
	if !noQuery(w, r) {
		return store.Guest{}, false
	}
	var in struct {
		Ticket string `json:"ticket"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512))
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Ticket == "" {
		guestErr(w, http.StatusBadRequest, `Send {"ticket"}.`)
		return store.Guest{}, false
	}
	code := r.PathValue("code")
	if _, err := s.store.MeetingByCode(r.Context(), code, s.cfg.Now()); err != nil {
		s.guestMeetingErr(w, err)
		return store.Guest{}, false
	}
	g, err := s.store.GuestByTicket(r.Context(), code, in.Ticket, s.cfg.Now())
	if errors.Is(err, store.ErrNotFound) {
		guestErr(w, http.StatusNotFound, GuestNoTicket)
		return store.Guest{}, false
	}
	if err != nil {
		s.guestMeetingErr(w, err)
		return store.Guest{}, false
	}
	return g, true
}

// handleWait is the guest's wait: an event stream, read with fetch so the
// ticket stays in the body. It sends "waiting" with the current notice, and
// again whenever the notice changes, then exactly one of:
//
//	admitted {"livekit_url", "token", "identity", "ice"}   the owner admitted them
//	denied                                                the owner denied them
//	ended                                                 the meeting ended
//
// 🔴 A waiting ticket's stream never carries a token: the only token is the
// admitted event's, minted by guestToken, which asks the admission
// predicate. A ticket that was removed or left is dead: 403, so the guest
// must knock again and, if admitted, gets a new identity.
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	g, ok := s.ticketOf(w, r)
	if !ok {
		return
	}
	switch g.State {
	case store.StRemoved, store.StLeft:
		guestErr(w, http.StatusForbidden, GuestRemoved)
		return
	}
	ctx, release, ok := s.acquireStream(w, r, g.ParticipantID)
	if !ok {
		return
	}
	defer release()
	f, ok := sseStart(w)
	if !ok {
		return
	}
	var lastNotice string
	s.stream(ctx, w, f, g.MeetingID, false, func() (bool, error) {
		cur, err := s.store.GuestByParticipant(r.Context(), g.ParticipantID, s.cfg.Now())
		if err != nil {
			return true, err
		}
		if !cur.MeetingLive || cur.State == store.StEnded {
			return true, sse(w, f, "ended", map[string]any{"message": GuestEnded})
		}
		if cur.State == store.StDenied {
			return true, sse(w, f, "denied", map[string]any{"message": "You weren't admitted."})
		}
		// The token's only gate is guestToken's admission predicate: this
		// handler does not decide who is admitted.
		tok, ok, err := s.guestToken(r.Context(), cur)
		if err != nil {
			return true, err
		}
		if ok {
			return true, sse(w, f, "admitted", tok)
		}
		if cur.State == store.StWaiting {
			n := s.noticeFor(cur.Notice.Record, cur.Notice.AI)
			if b, _ := json.Marshal(n); string(b) != lastNotice {
				lastNotice = string(b)
				return false, sse(w, f, "waiting", map[string]any{"notice": n})
			}
			return false, nil
		}
		return true, sse(w, f, "ended", map[string]any{"message": GuestRemoved})
	})
}

// handleGuestEvents is an admitted guest's event stream: "meeting" with the
// CURRENT notice (never the one cached at knock time, design: "the admitted
// page shows the current state from the meeting's event stream"), at once
// and on every change; the live transcript (segment, partials, transcript:
// transcript.go); then "removed" or "ended". Phase 4 adds the action items
// here. A ticket that is not admitted gets no stream (403), and a stream
// whose ticket stops being admitted ends before it sends anything more.
func (s *Server) handleGuestEvents(w http.ResponseWriter, r *http.Request) {
	g, ok := s.ticketOf(w, r)
	if !ok {
		return
	}
	if g.State != store.StAdmitted || !g.MeetingLive {
		guestErr(w, http.StatusForbidden, GuestRemoved)
		return
	}
	ctx, release, ok := s.acquireStream(w, r, g.ParticipantID)
	if !ok {
		return
	}
	defer release()
	f, ok := sseStart(w)
	if !ok {
		return
	}
	var last string
	minted := false
	tc := newTCursor()
	s.stream(ctx, w, f, g.MeetingID, false, func() (bool, error) {
		cur, err := s.store.GuestByParticipant(r.Context(), g.ParticipantID, s.cfg.Now())
		if err != nil {
			return true, err
		}
		if !cur.MeetingLive || cur.State == store.StEnded {
			return true, sse(w, f, "ended", map[string]any{"message": GuestEnded})
		}
		if cur.State != store.StAdmitted {
			return true, sse(w, f, "removed", map[string]any{"message": GuestRemoved})
		}
		// A fresh token at the start of every stream: an admitted guest whose
		// connection dropped opens /events again (the page's Retry) and
		// reconnects without knocking. guestToken asks the admission
		// predicate, so only a still-admitted guest gets one.
		if !minted {
			minted = true
			tok, ok, err := s.guestToken(r.Context(), cur)
			if err != nil {
				return true, err
			}
			if ok {
				if err := sse(w, f, "token", tok); err != nil {
					return true, err
				}
			}
		}
		snap := map[string]any{"notice": s.noticeFor(cur.Notice.Record, cur.Notice.AI)}
		if b, _ := json.Marshal(snap); string(b) != last {
			last = string(b)
			if err := sse(w, f, "meeting", snap); err != nil {
				return true, err
			}
		}
		// The live transcript (decision 6: guests see what the owner sees),
		// of this ticket's own meeting only, and only while admitted (checked
		// above, on every send).
		return false, s.sendTranscript(r.Context(), w, f, cur.MeetingID, tc, func(after int64) ([]store.Segment, error) {
			return s.store.SegmentsAfter(r.Context(), cur.MeetingID, after, store.MaxSegmentsPage)
		})
	})
}

// handleLeave is the guest's own Leave: an admitted guest becomes left, is
// dropped from the room, and their ticket is spent (they knock to come back).
// Only this, a removal, or the meeting's end ends an admission: a connection
// that drops (LiveKit's participant_left) does not, so a guest whose network
// blinked reconnects with a fresh token from /events.
func (s *Server) handleLeave(w http.ResponseWriter, r *http.Request) {
	g, ok := s.ticketOf(w, r)
	if !ok {
		return
	}
	if g.State != store.StAdmitted {
		guestErr(w, http.StatusConflict, GuestRemoved)
		return
	}
	if _, _, err := s.store.MarkLeft(r.Context(), g.Room, g.Identity, s.cfg.Now()); err != nil {
		s.guestMeetingErr(w, err)
		return
	}
	if err := s.cfg.LiveKit.RemoveParticipant(r.Context(), g.Room, g.Identity); err != nil {
		s.cfg.Log.Printf("leave: LiveKit: %v (the reconcile loop retries)", err)
	}
	s.hub.Changed(g.MeetingID)
	w.WriteHeader(http.StatusNoContent)
}
