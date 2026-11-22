package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The guest card (muster's docs/design/contacts.md, "The guest card"):
// after the meeting ends for an admitted guest, by End or by their own
// Leave, their page shows an optional card asking for their name and email,
// which go to the meeting's owner through the contacts plugin.
//
// 🔴 Only with contacts configured (Config.Contacts). Without it no card
// route is mounted (a 404 like any unknown path), no card is ever minted,
// and the ended event and the Leave answer are byte for byte what they were
// before this file existed (TestNoContactsURLChangesNothing).
//
//	POST /card       {"id", "name", "email"} → 200 {"message"}: the card is spent and its submission waits in the outbox
//	POST /card/skip  {"id"}                  → 204: the card is spent; nothing is sent anywhere
//
// Both take the card id in the body (never a URL: noQuery), spend it once,
// and answer a spent, unknown or expired id with the same 404.

// The card's sentences.
const (
	// CardExpired is a card id that is not live: spent, unknown or expired.
	CardExpired = "This form has expired."
	// CardTooMany is past a card limit.
	CardTooMany = "Too many tries. Wait a few minutes and try again."
	// CardHost is the owner's name in the card's sentences when muster gave
	// none.
	CardHost = "the host"
)

// CardSentMessage is the ONE answer to every accepted card: the same body
// whatever contacts later makes of it (a new contact, a merge, dropped as
// not accepting, or not reachable at all), since meetings answers from its
// own outbox before contacts is ever called (design: merge rule 5, "the
// guest's answer never says which happened").
func CardSentMessage(owner string) string {
	if owner == "" {
		owner = CardHost
	}
	if strings.HasSuffix(owner, ".") {
		return "Sent to " + owner
	}
	return "Sent to " + owner + "."
}

// The card limits (design: "Abuse and rate limits").
const (
	// CardPostsPerAddress per CardWindow from one source address.
	CardPostsPerAddress = 10
	// CardPostsPerMeeting per CardWindow on one meeting's cards: the most a
	// meeting's admitted guests (at most store.MaxAdmittedGuests at once)
	// plausibly need, with room for typos.
	CardPostsPerMeeting = 30
	CardWindow          = 10 * time.Minute
	// cardBody is the largest card request.
	cardBody = 2048
)

// contactsSide is the server's contacts state; nil when not configured.
type contactsSide struct {
	client    *contacts.Client
	accepting *contacts.Accepting
	outbox    *contacts.Outbox
	byAddr    *lobby.Limiter
	byMeeting *keyLimiter
}

func newContactsSide(c *contacts.Client, st *store.Store, cfg Config) *contactsSide {
	return &contactsSide{client: c, accepting: contacts.NewAccepting(c, cfg.Log, cfg.Now), outbox: contacts.NewOutbox(c, st, cfg.Log, cfg.Now),
		byAddr: lobby.NewLimiterOf(CardPostsPerAddress, CardWindow), byMeeting: &keyLimiter{max: CardPostsPerMeeting, window: CardWindow}}
}

// touchContacts asks contacts, in the background and at most once a minute
// per owner, whether it accepts this owner's guests: on a knock and while
// an admitted guest's stream is open, so an answer is cached before the
// meeting ends. A no-op without contacts. It never waits.
func (s *Server) touchContacts(subject string) {
	if s.contacts == nil {
		return
	}
	s.contacts.accepting.Refresh(subject)
}

// cardPayload is the card a guest's page draws.
type cardPayload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// offerCard mints participant pid's card when contacts is configured, the
// cached answer says contacts accepts the meeting's owner, and the store's
// card predicate holds (an admitted guest, never the owner, whose meeting
// ended by End or their Leave, offered none before). nil otherwise.
func (s *Server) offerCard(ctx context.Context, subject, pid string) *cardPayload {
	if s.contacts == nil || !s.contacts.accepting.Get(subject) {
		return nil
	}
	off, ok, err := s.store.OfferCard(ctx, pid, s.cfg.Now())
	if err != nil {
		s.cfg.Log.Printf("card: %v", err)
		return nil
	}
	if !ok {
		return nil
	}
	return &cardPayload{ID: off.ID, Name: off.TypedName, Owner: off.OwnerName}
}

// endedPayload is a guest's "ended" event: its message, and the card when
// one is offered. Without a card it is the event as it always was.
func (s *Server) endedPayload(ctx context.Context, g store.Guest) map[string]any {
	out := map[string]any{"message": GuestEnded}
	if c := s.offerCard(ctx, g.Subject, g.ParticipantID); c != nil {
		out["card"] = c
	}
	return out
}

// cardIn reads a card request's body: at most cardBody bytes, JSON, no
// field it does not name.
func cardIn(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cardBody))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(v)
	}
	if err != nil {
		guestErr(w, http.StatusBadRequest, `Send {"id", "name", "email"}, or {"id"} to skip.`)
		return false
	}
	return true
}

// cardLimit counts one card post from r's address and on meeting, and
// writes the 429 when either is past its limit.
func (s *Server) cardLimit(w http.ResponseWriter, r *http.Request, meetingID string) bool {
	now := s.cfg.Now()
	if !s.contacts.byAddr.Allow(s.clientAddr(r), now) || !s.contacts.byMeeting.Allow(meetingID, now) {
		w.Header().Set("Retry-After", "600")
		guestErr(w, http.StatusTooManyRequests, CardTooMany)
		return false
	}
	return true
}

// handleCard is Send: the fields are checked (a 400 names the field and
// spends nothing, so the page keeps what was typed), then the card is spent
// and its submission written to the outbox in one transaction, and the
// guest is answered CardSentMessage. 🔴 Nothing here calls contacts: the
// outbox worker delivers later, so the answer cannot depend on contacts.
func (s *Server) handleCard(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	var in struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if !cardIn(w, r, &in) {
		return
	}
	now := s.cfg.Now()
	c, err := s.store.CardByID(r.Context(), in.ID, now)
	if errors.Is(err, store.ErrNotFound) {
		// Counted against the address all the same: guessing ids costs.
		if s.cardLimit(w, r, "") {
			guestErr(w, http.StatusNotFound, CardExpired)
		}
		return
	}
	if err != nil {
		s.cfg.Log.Printf("card: %v", err)
		guestErr(w, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
		return
	}
	if !s.cardLimit(w, r, c.MeetingID) {
		return
	}
	name, err := lobby.CleanName(in.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": lobby.ErrName.Error(), "field": "name"})
		return
	}
	email, err := contacts.CleanEmail(in.Email)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": contacts.ErrEmail.Error(), "field": "email"})
		return
	}
	spent, err := s.store.SpendCard(r.Context(), in.ID, store.CardSent, now, func(c store.Card) (store.OutboxItem, error) {
		return contacts.SubmissionItem(c, name, email)
	})
	if errors.Is(err, store.ErrNotFound) {
		guestErr(w, http.StatusNotFound, CardExpired)
		return
	}
	if err != nil {
		s.cfg.Log.Printf("card: %v", err)
		guestErr(w, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
		return
	}
	s.contacts.outbox.Kick()
	writeJSON(w, http.StatusOK, map[string]any{"message": CardSentMessage(spent.OwnerName)})
}

// handleCardSkip is Skip: the card is spent and nothing is written
// anywhere but its state.
func (s *Server) handleCardSkip(w http.ResponseWriter, r *http.Request) {
	if !noQuery(w, r) {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !cardIn(w, r, &in) {
		return
	}
	now := s.cfg.Now()
	c, err := s.store.CardByID(r.Context(), in.ID, now)
	meeting := ""
	if err == nil {
		meeting = c.MeetingID
	}
	if !s.cardLimit(w, r, meeting) {
		return
	}
	if _, err := s.store.SpendCard(r.Context(), in.ID, store.CardSkipped, now, nil); errors.Is(err, store.ErrNotFound) {
		guestErr(w, http.StatusNotFound, CardExpired)
		return
	} else if err != nil {
		s.cfg.Log.Printf("card: %v", err)
		guestErr(w, http.StatusInternalServerError, "Something went wrong. Try again in a moment.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// keyLimiter is lobby.Limiter keyed by a string (a meeting id): max posts
// per window. "" is never limited here (an unknown card has no meeting; the
// address limit still counts it).
type keyLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	seen   map[string][]time.Time
}

func (l *keyLimiter) Allow(key string, now time.Time) bool {
	if key == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	if len(l.seen) > 10000 {
		for k, ts := range l.seen {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.window {
				delete(l.seen, k)
			}
		}
	}
	keep := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		l.seen[key] = keep
		return false
	}
	l.seen[key] = append(keep, now)
	return true
}

// runContacts is the contacts side's background work, at Run's start. With
// contacts configured: the outbox worker. Without: the outbox is emptied
// (design: "on boot any rows left in contacts_outbox are deleted (the URL
// was removed; their PII has nowhere to go)"), and nothing else runs.
func (s *Server) runContacts(ctx context.Context) {
	if s.contacts == nil {
		if n, err := s.store.ClearOutbox(ctx); err != nil {
			s.cfg.Log.Printf("contacts: clearing the outbox: %v", err)
		} else if n > 0 {
			s.cfg.Log.Printf("contacts: not configured: %d undelivered outbox row(s) deleted", n)
		}
		return
	}
	go s.contacts.outbox.Run(ctx)
}

// Outbox is the outbox worker (tests); nil without contacts.
func (s *Server) Outbox() *contacts.Outbox {
	if s.contacts == nil {
		return nil
	}
	return s.contacts.outbox
}

// Accepting is the accepting cache (tests); nil without contacts.
func (s *Server) Accepting() *contacts.Accepting {
	if s.contacts == nil {
		return nil
	}
	return s.contacts.accepting
}
