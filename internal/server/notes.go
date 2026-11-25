package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster-meetings/internal/mail"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The meeting notes by email (decisions 1, 2, 3 and 7): OPTIONAL, only with
// Config.Mail (and Config.AI). After a meeting's post run, one draft per
// recipient: the owner, and every guest who sent the card (its address is
// kept for this; store.KeepCardEmail). 🔴 The owner approves first: nothing
// is queued but by Send, and Send needs the owner's address confirmed by a
// mailed link. Each queued draft is one message to one recipient, sent by
// the outbox with retries; a send whose outcome is unknown (the connection
// broke after the message was handed over) is never retried by itself.
//
//	GET   /api/meetings/{id}/drafts                the drafts that stand, owner first
//	GET   /api/meetings/{id}/drafts/{did}/preview  what that recipient will read (subject, text, theirs and everyone else's)
//	PATCH /api/meetings/{id}/drafts/{did}          {"items"?: [{text, assignee, due}], "removed"?: bool}
//	POST  /api/meetings/{id}/drafts/send           {"ids"?: [...]}: queue every draft (or those)
//	POST  /api/meetings/{id}/drafts/{did}/retry    queue a failed (or unknown) one again
//	GET   /api/settings/email                      {"confirmed", "pending"}
//	PUT   /api/settings/email                      {"email"}: mails a confirmation link
//	GET   /e/{token}                               the confirmation page (changes nothing)
//	POST  /e/{token}                               confirms
//
// 🔴 No log line carries an address, a name, an item or a token.

// DefaultMailBackoff is the wait before each retry of a deferred send; past
// the last, the draft is failed.
var DefaultMailBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour}

// The notes' sentences.
const (
	NotesNeedAddress = "Confirm your email address in Meetings settings before sending notes."
	NotesLocked      = "Those notes were already sent, or are on their way."
	ConfirmTooMany   = "Too many confirmation emails. Wait an hour and try again."
	ConfirmNotSent   = "The confirmation email could not be sent. Try again in a moment."
	AddressInvalid   = "Type one email address, like name@example.com."
)

// The reasons a draft fails before it reaches the mail server.
const (
	PhraseBadAddress = "The mail server cannot take this address. Edit it, or don't send."
	PhraseNotMade    = "The message could not be made."
)

// NotesAddress is the one rule for an address the notes go to: the card,
// an edited draft and the outbox all check it (mail.ValidAddress, which is
// what the message is built with).
func NotesAddress(a string) bool {
	return len(a) <= 254 && strings.Contains(a, "@") && mail.ValidAddress(a)
}

// ConfirmPerHour is the most confirmation emails one owner may ask for in
// an hour.
const ConfirmPerHour = 5

type notesSide struct {
	kick    chan struct{}
	mu      sync.Mutex
	confirm map[string][]time.Time
}

func newNotesSide() *notesSide {
	return &notesSide{kick: make(chan struct{}, 1), confirm: map[string][]time.Time{}}
}

func (s *Server) kickMail() {
	if s.notes == nil {
		return
	}
	select {
	case s.notes.kick <- struct{}{}:
	default:
	}
}

func (s *Server) notesRoutes() {
	s.mux.HandleFunc("GET /api/meetings/{id}/drafts", s.owner(s.handleDrafts))
	s.mux.HandleFunc("GET /api/meetings/{id}/drafts/{did}/preview", s.owner(s.handlePreview))
	s.mux.HandleFunc("PATCH /api/meetings/{id}/drafts/{did}", s.owner(s.handleEditDraft))
	s.mux.HandleFunc("POST /api/meetings/{id}/drafts/send", s.owner(s.handleSend))
	s.mux.HandleFunc("POST /api/meetings/{id}/drafts/{did}/retry", s.owner(s.handleRetryDraft))
	s.mux.HandleFunc("GET /api/settings/email", s.owner(s.handleGetEmail))
	s.mux.HandleFunc("PUT /api/settings/email", s.owner(s.handlePutEmail))
	s.mux.HandleFunc("GET /e/{token}", s.handleConfirmPage)
	s.mux.HandleFunc("POST /e/{token}", s.handleConfirm)
}

// ---- the drafts ----

func (s *Server) handleDrafts(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	ds, err := repo.Drafts(r.Context(), r.PathValue("id"))
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	em, err := repo.Email(r.Context(), s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": ds, "owner_email": em})
}

func (s *Server) draftOf(ctx context.Context, repo *store.OwnerRepo, meetingID, id string) (store.Draft, error) {
	ds, err := repo.Drafts(ctx, meetingID)
	if err != nil {
		return store.Draft{}, err
	}
	for _, d := range ds {
		if d.ID == id {
			return d, nil
		}
	}
	return store.Draft{}, store.ErrNotFound
}

// notesOf is what draft d's recipient reads.
func (s *Server) notesOf(ctx context.Context, repo *store.OwnerRepo, d store.Draft, ownerName string) (mail.Notes, store.Meeting, []store.Segment, error) {
	m, segs, err := repo.Transcript(ctx, d.MeetingID)
	if err != nil {
		return mail.Notes{}, store.Meeting{}, nil, err
	}
	names, err := s.store.Speakers(ctx, d.MeetingID)
	if err != nil {
		return mail.Notes{}, store.Meeting{}, nil, err
	}
	start := m.CreatedAt
	if m.StartedAt != nil {
		start = *m.StartedAt
	}
	n := mail.Notes{OwnerName: ownerName, Title: m.Title, When: start.Format("2 Jan 2006"), RecipientName: d.Name,
		ForOwner: d.Recipient == store.RecipientOwner, Names: names, Attached: true}
	if n.OwnerName == "" {
		n.OwnerName = CardHost
	}
	for _, it := range d.Items {
		n.Items = append(n.Items, mail.NoteItem{Text: it.Text, Assignee: it.Assignee, Due: it.Due})
	}
	if s.cfg.S3 != nil {
		live, until, err := s.store.MailShareLive(ctx, d.ID, s.cfg.Now())
		if err != nil {
			return mail.Notes{}, store.Meeting{}, nil, err
		}
		if live {
			n.VideoURL = s.ShareURL(s.store.MailShareToken(d.ID))
			if until != nil {
				n.VideoUntil = "until " + until.Format("2 Jan 2006")
			}
		}
	}
	return n, m, segs, nil
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	d, err := s.draftOf(r.Context(), repo, r.PathValue("id"), r.PathValue("did"))
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	owner, err := repo.DisplayName(r.Context())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	n, _, _, err := s.notesOf(r.Context(), repo, d, owner)
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	subject, text, _ := mail.Render(n)
	yours, others := mail.Split(n.Items, n.RecipientName, n.Names)
	if yours == nil {
		yours = []mail.NoteItem{}
	}
	if others == nil {
		others = []mail.NoteItem{}
	}
	video := n.VideoURL != ""
	if !video && d.State == store.DraftDraft && s.cfg.S3 != nil {
		parts, _, _, _, err := s.partsOf(r.Context(), repo, d.MeetingID)
		if err != nil {
			s.meetingErr(w, err)
			return
		}
		for _, p := range parts {
			video = video || p.Playable()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": subject, "text": text, "yours": yours, "others": others,
		"from": mail.FromName(owner), "video": video})
}

// The limits of an edited item.
const (
	itemTextMax  = 500
	itemFieldMax = 120
)

func cleanField(v string, maxRunes int) (string, bool) {
	v = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, v))
	return v, utf8.RuneCountInString(v) <= maxRunes
}

func (s *Server) handleEditDraft(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	var in struct {
		Items   *[]store.DraftItem `json:"items"`
		Removed *bool              `json:"removed"`
		Email   *string            `json:"email"`
	}
	if !readJSON(w, r, 64<<10, &in) {
		return
	}
	if in.Items == nil && in.Removed == nil && in.Email == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": `send {"items": [...]}, {"removed": true} and/or {"email": "…"}`})
		return
	}
	if in.Email != nil {
		a := strings.TrimSpace(*in.Email)
		if !NotesAddress(a) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": AddressInvalid, "field": "email"})
			return
		}
		in.Email = &a
	}
	if in.Items != nil {
		if len(*in.Items) > store.MaxDraftItems {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Too many action items."})
			return
		}
		var ok bool
		clean := []store.DraftItem{}
		for _, it := range *in.Items {
			var a, b, c bool
			it.Text, a = cleanField(it.Text, itemTextMax)
			it.Assignee, b = cleanField(it.Assignee, itemFieldMax)
			it.Due, c = cleanField(it.Due, itemFieldMax)
			if ok = a && b && c; !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "An action item is too long."})
				return
			}
			if it.Text != "" {
				clean = append(clean, it)
			}
		}
		in.Items = &clean
	}
	d, err := repo.EditDraft(r.Context(), r.PathValue("id"), r.PathValue("did"), in.Items, in.Removed, in.Email, s.cfg.Now())
	if errors.Is(err, store.ErrNotGuest) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Your own address is the one confirmed in settings."})
		return
	}
	if errors.Is(err, store.ErrDraftLocked) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": NotesLocked})
		return
	}
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"draft": d})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, 8192, &in) {
		return
	}
	now := s.cfg.Now()
	n, err := repo.Approve(r.Context(), r.PathValue("id"), in.IDs, now, func(tx *sql.Tx, d store.Draft) error {
		if s.cfg.S3 == nil {
			return nil
		}
		_, err := s.store.AddMailShare(r.Context(), tx, d, now)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNoOwnerEmail):
		writeJSON(w, http.StatusConflict, map[string]any{"error": NotesNeedAddress})
		return
	case errors.Is(err, store.ErrNoSession):
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	s.kickMail()
	writeJSON(w, http.StatusOK, map[string]any{"queued": n})
}

func (s *Server) handleRetryDraft(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	err := repo.RetryDraft(r.Context(), r.PathValue("id"), r.PathValue("did"), s.cfg.Now())
	if errors.Is(err, store.ErrDraftLocked) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": NotesLocked})
		return
	}
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	s.kickMail()
	writeJSON(w, http.StatusOK, map[string]any{"queued": 1})
}

// ---- the outbox ----

func (s *Server) runMail(ctx context.Context) {
	if s.notes == nil {
		return
	}
	go func() {
		if n, err := s.store.ResetSending(ctx, s.cfg.Now()); err != nil {
			s.cfg.Log.Printf("notes: %v", err)
		} else if n > 0 {
			s.cfg.Log.Printf("notes: %d drafts were being sent when the plugin stopped: marked unknown (the owner may retry)", n)
		}
		for {
			s.MailPass(ctx)
			wait := 30 * time.Second
			if next, ok, err := s.store.NextMail(ctx); err == nil && ok {
				if d := next.Sub(s.cfg.Now()); d < wait {
					wait = max(d, time.Second)
				}
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-s.notes.kick:
			case <-t.C:
			}
			t.Stop()
		}
	}()
}

// runMailRetention drops kept guest addresses, with or without a mail
// server: every one at start when none is configured (nothing would send
// to them), and every hour those MailKeepDays past their meeting.
func (s *Server) runMailRetention(ctx context.Context) {
	go func() {
		s.MailRetention(ctx, true)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.MailRetention(ctx, false)
			}
		}
	}()
}

// MailRetention is one pass of it; boot is the pass at start.
func (s *Server) MailRetention(ctx context.Context, boot bool) {
	if boot && s.notes == nil {
		if err := s.store.ClearMail(ctx); err != nil {
			s.cfg.Log.Printf("notes: clearing kept addresses: %v", err)
		}
	}
	if n, err := s.store.PurgeMail(ctx, s.cfg.Now()); err != nil {
		s.cfg.Log.Printf("notes: purge: %v", err)
	} else if n > 0 {
		s.cfg.Log.Printf("notes: %d guest addresses dropped (%d days after their meeting)", n, store.MailKeepDays)
	}
}

// MailPass sends every due queued draft.
func (s *Server) MailPass(ctx context.Context) {
	for ctx.Err() == nil {
		o, ok, err := s.store.ClaimMail(ctx, s.cfg.Now())
		if err != nil {
			s.cfg.Log.Printf("notes: %v", err)
			return
		}
		if !ok {
			return
		}
		s.sendOne(ctx, o)
	}
}

// compose is a draft's message.
func (s *Server) compose(ctx context.Context, o store.OutMail, to string) ([]byte, error) {
	repo := s.store.ForSubject(o.Subject)
	n, m, segs, err := s.notesOf(ctx, repo, o.Draft, o.OwnerName)
	if err != nil {
		return nil, err
	}
	msg := mail.Message{FromName: mail.FromName(o.OwnerName), FromAddr: s.cfg.Mail.From(), ReplyTo: o.OwnerEmail,
		ToName: o.Name, ToAddr: to, MessageID: o.ID, Date: approvedAt(o),
		Attachments: []mail.Attachment{
			{Name: "meeting-" + m.ID + ".txt", ContentType: "text/plain; charset=utf-8", Data: []byte(TranscriptText(m, segs))},
			{Name: "meeting-" + m.ID + ".vtt", ContentType: "text/vtt; charset=utf-8", Data: []byte(TranscriptVTT(segs))},
		}}
	msg.Subject, msg.Text, msg.HTML = mail.Render(n)
	b, err := mail.Build(msg)
	if errors.Is(err, mail.ErrTooLarge) {
		n.Attached, msg.Attachments = false, nil
		msg.Subject, msg.Text, msg.HTML = mail.Render(n)
		b, err = mail.Build(msg)
	}
	return b, err
}

// approvedAt is when the owner pressed Send for o: the message's Date, so a
// retry is the same message.
func approvedAt(o store.OutMail) time.Time {
	if o.ApprovedAt != nil {
		return *o.ApprovedAt
	}
	return time.Time{}
}

func (s *Server) sendOne(ctx context.Context, o store.OutMail) {
	now := s.cfg.Now()
	to := o.Email
	if o.Recipient == store.RecipientOwner {
		to = o.OwnerEmail
	}
	// 🔴 The owner's confirmed address is the Reply-To of every message:
	// none confirmed (it was replaced since Send), nothing goes.
	if to == "" || o.OwnerEmail == "" || o.ApprovedAt == nil {
		_ = s.store.MailDone(ctx, o.ID, store.DraftFailed, "the address is gone", now)
		s.cfg.Log.Printf("notes: draft %s: failed: no address", o.ID)
		return
	}
	backoff := s.cfg.MailBackoff
	if len(backoff) == 0 {
		backoff = DefaultMailBackoff
	}
	msg, err := s.compose(ctx, o, to)
	if err != nil {
		// 🔴 Never queued forever: an address the mail server cannot take
		// fails the draft at once, with a reason the owner reads (they may
		// edit the address, or not send); any other failure to make the
		// message counts toward the same cap as a deferred send.
		s.cfg.Log.Printf("notes: draft %s: composing: %v", o.ID, err)
		switch {
		case !NotesAddress(to):
			_ = s.store.MailDone(ctx, o.ID, store.DraftFailed, PhraseBadAddress, now)
		case o.Attempts > len(backoff):
			_ = s.store.MailDone(ctx, o.ID, store.DraftFailed, PhraseNotMade, now)
		default:
			_ = s.store.MailRetry(ctx, o.ID, now.Add(backoff[o.Attempts-1]), PhraseNotMade, now)
		}
		return
	}
	err = s.cfg.Mail.Send(ctx, to, msg)
	switch mail.ClassOf(err) {
	case 0:
		if err != nil { // not a mail.Error: treat as temporary
			break
		}
		if err := s.store.MailSent(ctx, o.ID, s.cfg.Now()); err != nil {
			s.cfg.Log.Printf("notes: draft %s: %v", o.ID, err)
		}
		s.cfg.Log.Printf("notes: draft %s of meeting %s: sent (%d bytes)", o.ID, o.MeetingID, len(msg))
		return
	case mail.Permanent:
		_ = s.store.MailDone(ctx, o.ID, store.DraftFailed, err.Error(), s.cfg.Now())
		s.cfg.Log.Printf("notes: draft %s: failed: %v", o.ID, err)
		return
	case mail.Unknown:
		_ = s.store.MailDone(ctx, o.ID, store.DraftUnknown, err.Error(), s.cfg.Now())
		s.cfg.Log.Printf("notes: draft %s: outcome unknown: %v", o.ID, err)
		return
	}
	if o.Attempts > len(backoff) {
		_ = s.store.MailDone(ctx, o.ID, store.DraftFailed, err.Error(), s.cfg.Now())
		s.cfg.Log.Printf("notes: draft %s: failed after %d attempts: %v", o.ID, o.Attempts, err)
		return
	}
	_ = s.store.MailRetry(ctx, o.ID, s.cfg.Now().Add(backoff[o.Attempts-1]), err.Error(), s.cfg.Now())
	s.cfg.Log.Printf("notes: draft %s: deferred (attempt %d): %v", o.ID, o.Attempts, err)
}

// ---- the card's address ----

// notesFor says a meeting's guests get its notes: mail is configured and
// the meeting's AI action items are on (with them off nothing is analysed,
// so there are no notes, and the card does not promise them).
func (s *Server) notesFor(ctx context.Context, meetingID string) bool {
	if s.notes == nil {
		return false
	}
	ai, _, err := s.store.MeetingAI(ctx, meetingID)
	return err == nil && ai
}

// keepCardEmail keeps a guest's card address for the notes (only when the
// card said so: notesFor), and drafts their notes when the meeting's run is
// already done.
func (s *Server) keepCardEmail(ctx context.Context, c store.Card, email string) {
	if !s.notesFor(ctx, c.MeetingID) {
		return
	}
	if err := s.store.KeepCardEmail(ctx, c.ParticipantID, email); err != nil {
		s.cfg.Log.Printf("notes: keeping a card address: %v", err)
		return
	}
	if err := s.store.DraftFor(ctx, c.MeetingID, c.ParticipantID, s.cfg.Now()); err != nil {
		s.cfg.Log.Printf("notes: drafting for a late card: %v", err)
	}
}

// ---- the owner's address ----

func (s *Server) handleGetEmail(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	em, err := repo.Email(r.Context(), s.cfg.Now())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, em)
}

func (s *Server) confirmAllowed(subject string, now time.Time) bool {
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	var keep []time.Time
	for _, t := range s.notes.confirm[subject] {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) >= ConfirmPerHour {
		s.notes.confirm[subject] = keep
		return false
	}
	s.notes.confirm[subject] = append(keep, now)
	return true
}

func (s *Server) handlePutEmail(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	var in struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, 1024, &in) {
		return
	}
	addr := strings.TrimSpace(in.Email)
	if !NotesAddress(addr) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": AddressInvalid, "field": "email"})
		return
	}
	now := s.cfg.Now()
	if !s.confirmAllowed(repo.Subject(), now) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": ConfirmTooMany})
		return
	}
	tok, exp, err := repo.SetPendingEmail(r.Context(), addr, now)
	if errors.Is(err, store.ErrNoSession) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	}
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	owner, _ := repo.DisplayName(r.Context())
	subject, text, html := mail.Confirm(owner, s.cfg.PublicURL+"/e/"+tok, exp.Format("2 Jan 2006 15:04 MST"))
	msg, err := mail.Build(mail.Message{FromName: "Meetings", FromAddr: s.cfg.Mail.From(), ToName: owner, ToAddr: addr, Subject: subject,
		MessageID: "confirm-" + store.Hash(tok)[:24], Date: now, Text: text, HTML: html})
	if err == nil {
		err = s.cfg.Mail.Send(r.Context(), addr, msg)
	}
	if err != nil {
		s.cfg.Log.Printf("notes: the confirmation email: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": ConfirmNotSent})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pending": addr, "expires_at": exp})
}

// confirmPage is the confirmation link's page.
type confirmPage struct {
	Missing bool
	Done    bool
	Address string
	Action  string
}

// confirmCSP is its policy: its own styles, a form posting to itself, no
// script, framed by nobody.
const confirmCSP = "default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (s *Server) renderConfirm(w http.ResponseWriter, code int, p confirmPage) {
	h := w.Header()
	h.Set("Content-Security-Policy", confirmCSP)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := pageTmpl.ExecuteTemplate(w, "confirm.html", p); err != nil {
		s.cfg.Log.Printf("page confirm.html: %v", err)
	}
}

// 🔴 One answer, byte for byte, for every link that confirms nothing: a
// malformed, unknown, used or expired token, by GET and by POST alike.
func (s *Server) confirmMissing(w http.ResponseWriter) {
	s.renderConfirm(w, http.StatusNotFound, confirmPage{Missing: true})
}

func (s *Server) handleConfirmPage(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	addr, err := s.store.PendingByToken(r.Context(), tok, s.cfg.Now())
	if err != nil {
		s.confirmMissing(w)
		return
	}
	s.renderConfirm(w, http.StatusOK, confirmPage{Address: addr, Action: "/e/" + tok})
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	addr, err := s.store.PendingByToken(r.Context(), tok, s.cfg.Now())
	if err == nil {
		err = s.store.ConfirmEmail(r.Context(), tok, s.cfg.Now())
	}
	if err != nil {
		s.confirmMissing(w)
		return
	}
	s.renderConfirm(w, http.StatusOK, confirmPage{Done: true, Address: addr})
}
