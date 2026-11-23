// Package contactstest is a fake of the contacts plugin's internal API, built
// to the plugin's own contract (github.com/ZacxDev/muster-contacts/contactsapi,
// pinned in go.mod): its routes, wire types, field checks and error texts.
// internal/contacts's contract test runs contactsapi.RunContract (every case
// of the plugin's routes.json) against it, so a test or the e2e harness that
// talks to this fake talks to the contract.
//
// It is NOT the contacts plugin: it keeps what it was sent in memory. Phase
// 1b's API is GET /v1/owners/{owner} and POST /v1/submissions (202, no
// body); every other path is the 404 of an unknown route.
package contactstest

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
)

// Mode is how the fake answers.
type Mode int

const (
	// Up answers as the contract says.
	Up Mode = iota
	// BlackHole accepts every connection and never answers (a hung peer).
	BlackHole
	// Status answers every request with the forced status.
	Status
)

// Received is one submission the fake accepted and KEPT (one an owner who
// is not accepting sent is answered alike and dropped, as the contract
// says, and is not here).
type Received struct {
	Key  string
	Body contactsapi.Submission
}

// Request is one request the fake saw.
type Request struct {
	Method, Path, Auth, Key, ContentType string
	Body                                 []byte
}

type idemEntry struct {
	sum    [32]byte
	status int
	body   []byte
}

// Fake is the fake contacts API.
type Fake struct {
	*httptest.Server
	Token string

	mu          sync.Mutex
	mode        Mode
	forceStatus int
	retryAfter  string
	// notAccepting: owners answered 404 on the owners check, receipts and
	// attendance, and whose cards are dropped (everyone else is accepted: a
	// person who never opened Contacts is).
	notAccepting map[string]bool
	limited      map[string]bool
	dailyCap     int
	perOwner     map[string]int
	idem         map[string]idemEntry
	got          []Received
	requests     []Request
	hung         []net.Conn
}

// New starts a fake with token.
func New(token string) *Fake {
	f := &Fake{Token: token, notAccepting: map[string]bool{}, limited: map[string]bool{}, dailyCap: 200, perOwner: map[string]int{},
		idem: map[string]idemEntry{}}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.Server.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if st == http.StateNew {
			f.mu.Lock()
			if f.mode == BlackHole {
				f.hung = append(f.hung, c)
			}
			f.mu.Unlock()
		}
	}
	f.Start()
	return f
}

// SetMode changes how the fake answers; status (and retryAfter, when not
// "") are used by Status.
func (f *Fake) SetMode(m Mode, status int, retryAfter string) {
	f.mu.Lock()
	f.mode, f.forceStatus, f.retryAfter = m, status, retryAfter
	f.mu.Unlock()
}

// SetAccepting sets whether owner is accepting.
func (f *Fake) SetAccepting(owner string, ok bool) {
	f.mu.Lock()
	f.notAccepting[owner] = !ok
	f.mu.Unlock()
}

// SetCardLimitReached spends owner's daily cards (the next is 429).
func (f *Fake) SetCardLimitReached(owner string) {
	f.mu.Lock()
	f.limited[owner] = true
	f.mu.Unlock()
}

// Given is contactsapi.Target.Given for this fake: the contract's
// preconditions, for owner.
func (f *Fake) Given(given, owner string) error {
	switch given {
	case contactsapi.GivenOwnerRedeemed, contactsapi.GivenOwnerExpired:
		f.SetAccepting(owner, true)
	case contactsapi.GivenOwnerRevoked, contactsapi.GivenCollectOff:
		f.SetAccepting(owner, false)
	case contactsapi.GivenCardLimitReached:
		f.SetCardLimitReached(owner)
	default:
		return contactsapi.ErrUnsupportedGiven
	}
	return nil
}

// Got is every submission kept, in order (a replayed key once).
func (f *Fake) Got() []Received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Received(nil), f.got...)
}

// Requests is every request seen, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Count is how many requests to a path with this prefix the fake saw.
func (f *Fake) Count(method, pathPrefix string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			n++
		}
	}
	return n
}

// Close also drops black-holed connections.
func (f *Fake) Close() {
	f.mu.Lock()
	for _, c := range f.hung {
		c.Close()
	}
	f.mu.Unlock()
	f.Server.CloseClientConnections()
	f.Server.Close()
}

func answer(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	answer(w, code, contactsapi.ErrorAnswer{Error: msg})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	raw, tooBig := io.ReadAll(http.MaxBytesReader(w, r.Body, contactsapi.MaxBody))
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
		Key: r.Header.Get(contactsapi.IdempotencyHeader), ContentType: r.Header.Get("Content-Type"), Body: raw})
	mode, force, ra := f.mode, f.forceStatus, f.retryAfter
	f.mu.Unlock()
	switch mode {
	case BlackHole:
		<-r.Context().Done()
		return
	case Status:
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		refuse(w, force, http.StatusText(force))
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(f.Token)) != 1 {
		refuse(w, http.StatusUnauthorized, contactsapi.ErrUnauthorised)
		return
	}
	var mbe *http.MaxBytesError
	if errors.As(tooBig, &mbe) {
		refuse(w, http.StatusRequestEntityTooLarge, contactsapi.ErrBodyTooLarge)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, contactsapi.RouteOwner):
		f.owner(w, strings.TrimPrefix(r.URL.Path, contactsapi.RouteOwner))
	case r.Method == http.MethodPost && r.URL.Path == contactsapi.RouteSubmissions:
		f.submission(w, r, raw)
	default:
		refuse(w, http.StatusNotFound, contactsapi.ErrNotFoundRoute)
	}
}

func strict(raw []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func fieldErr(w http.ResponseWriter, err error) {
	var fe contactsapi.FieldError
	if errors.As(err, &fe) {
		answer(w, http.StatusBadRequest, contactsapi.ErrorAnswer{Error: contactsapi.ErrInvalidField, Field: fe.Field})
		return
	}
	refuse(w, http.StatusBadRequest, contactsapi.ErrBadBody)
}

func (f *Fake) owner(w http.ResponseWriter, owner string) {
	if !contactsapi.ValidSubject(owner) {
		refuse(w, http.StatusBadRequest, contactsapi.ErrBadOwner)
		return
	}
	f.mu.Lock()
	no := f.notAccepting[owner]
	f.mu.Unlock()
	if no {
		refuse(w, http.StatusNotFound, contactsapi.ErrNotAccepting)
		return
	}
	answer(w, http.StatusOK, contactsapi.OwnerAnswer{Accepting: true})
}

// replay answers a key seen before: the stored answer for the same body,
// 409 for another. It reports whether it answered.
func (f *Fake) replay(w http.ResponseWriter, owner, key string, sum [32]byte) bool {
	prev, ok := f.idem[owner+"\x00"+key]
	if !ok {
		return false
	}
	if prev.sum != sum {
		refuse(w, http.StatusConflict, contactsapi.ErrKeyReused)
		return true
	}
	if len(prev.body) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(prev.status)
	_, _ = w.Write(prev.body)
	return true
}

func (f *Fake) remember(w http.ResponseWriter, owner, key string, sum [32]byte, status int, v any) {
	var body []byte
	if v != nil {
		body, _ = json.Marshal(v)
		body = append(body, '\n')
		w.Header().Set("Content-Type", "application/json")
	}
	f.idem[owner+"\x00"+key] = idemEntry{sum: sum, status: status, body: body}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (f *Fake) submission(w http.ResponseWriter, r *http.Request, raw []byte) {
	var s contactsapi.Submission
	if !strict(raw, &s) {
		refuse(w, http.StatusBadRequest, contactsapi.ErrBadBody)
		return
	}
	_, address, _, _, err := s.Check()
	if err != nil {
		fieldErr(w, err)
		return
	}
	ikey := r.Header.Get(contactsapi.IdempotencyHeader)
	if ikey != contactsapi.KeyForCard(s.CardID) {
		refuse(w, http.StatusBadRequest, contactsapi.ErrKey)
		return
	}
	sum := sha256.Sum256(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replay(w, s.Owner, ikey, sum) {
		return
	}
	if f.limited[s.Owner] || f.perOwner[s.Owner] >= f.dailyCap {
		w.Header().Set("Retry-After", "3600")
		refuse(w, http.StatusTooManyRequests, contactsapi.ErrTooManyCards)
		return
	}
	// Not accepting: answered exactly as an accepted card (202, no body),
	// and dropped.
	if !f.notAccepting[s.Owner] {
		f.perOwner[s.Owner]++
		s.Email = address
		f.got = append(f.got, Received{Key: ikey, Body: s})
	}
	f.remember(w, s.Owner, ikey, sum, http.StatusAccepted, nil)
}

// Wait polls until cond holds or d passes; it reports whether it held.
func Wait(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
