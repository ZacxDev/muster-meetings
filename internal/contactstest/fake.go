// Package contactstest is a fake of the contacts plugin's internal API, as
// muster's docs/design/contacts.md specifies it ("The contacts API") and as
// internal/contacts/testdata/contract.json pins it for this repository:
// bearer auth, per-owner accepting, submissions with Idempotency-Key, the
// per-owner cap, strict bodies. internal/contacts's contract test runs every
// case of the contract against it, so a test or the e2e harness that talks
// to this fake talks to the contract.
//
// It is NOT the contacts plugin: it keeps what it was sent in memory and
// makes no contact. When the plugin's own server is available, the contract
// test runs against it instead (CONTACTS_CONTRACT_URL).
package contactstest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
)

// Mode is how the fake answers.
type Mode int

const (
	// Up answers as the contract says.
	Up Mode = iota
	// BlackHole accepts every connection and never answers (a hung peer).
	BlackHole
	// Status answers every request with Fake.ForceStatus.
	Status
)

// Received is one submission the fake accepted.
type Received struct {
	Key  string
	Body contacts.Submission
}

// Fake is the fake contacts API.
type Fake struct {
	*httptest.Server
	Token string

	mu sync.Mutex
	// mode, forced: see Mode.
	mode        Mode
	forceStatus int
	retryAfter  string
	// notAccepting holds the owners answered 404 (everyone else is accepted:
	// a person who never opened Contacts is, design: "every owner works").
	notAccepting map[string]bool
	// dailyCap is submissions per owner before 429 (design: 200).
	dailyCap int
	perOwner map[string]int
	idem     map[string]idemEntry
	got      []Received
	requests []Request
	hung     []net.Conn
}

// Request is one request the fake saw: method, path and headers that matter.
type Request struct {
	Method, Path, Auth, Key, ContentType string
	Body                                 []byte
}

type idemEntry struct {
	sum    [32]byte
	status int
	body   []byte
}

// New starts a fake with token.
func New(token string) *Fake {
	f := &Fake{Token: token, notAccepting: map[string]bool{}, dailyCap: 200, perOwner: map[string]int{}, idem: map[string]idemEntry{}}
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

// SetMode changes how the fake answers. status is used by Status;
// retryAfter is sent with it when not "".
func (f *Fake) SetMode(m Mode, status int, retryAfter string) {
	f.mu.Lock()
	f.mode, f.forceStatus, f.retryAfter = m, status, retryAfter
	f.mu.Unlock()
}

// SetAccepting sets whether owner's writes are accepted.
func (f *Fake) SetAccepting(owner string, ok bool) {
	f.mu.Lock()
	f.notAccepting[owner] = !ok
	f.mu.Unlock()
}

// SetDailyCap sets the per-owner submission cap.
func (f *Fake) SetDailyCap(n int) { f.mu.Lock(); f.dailyCap = n; f.mu.Unlock() }

// Got is every submission accepted, in order (a replayed key once).
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

// Count is how many requests to path the fake saw (prefix match).
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

func errBody(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Key: r.Header.Get("Idempotency-Key"),
		ContentType: r.Header.Get("Content-Type"), Body: raw})
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
		errBody(w, force, http.StatusText(force))
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(f.Token)) != 1 {
		errBody(w, http.StatusUnauthorized, "unauthorised")
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, contacts.PathOwners):
		owner := strings.TrimPrefix(r.URL.Path, contacts.PathOwners)
		if !validOwner(owner) {
			errBody(w, http.StatusNotFound, "not accepting")
			return
		}
		f.mu.Lock()
		no := f.notAccepting[owner]
		f.mu.Unlock()
		if no {
			errBody(w, http.StatusNotFound, "not accepting")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"accepting":true}`+"\n")
	case r.Method == http.MethodPost && r.URL.Path == contacts.PathSubmissions:
		f.submission(w, r, raw)
	default:
		errBody(w, http.StatusNotFound, "not found")
	}
}

func validOwner(s string) bool {
	if len(s) != 22 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (f *Fake) submission(w http.ResponseWriter, r *http.Request, raw []byte) {
	if len(raw) > 4<<10 {
		errBody(w, http.StatusBadRequest, "body too large")
		return
	}
	var s contacts.Submission
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		errBody(w, http.StatusBadRequest, "body")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || key != s.Key() {
		errBody(w, http.StatusBadRequest, "Idempotency-Key")
		return
	}
	if !validOwner(s.Owner) {
		errBody(w, http.StatusBadRequest, "owner")
		return
	}
	if _, err := lobby.CleanName(s.Name); err != nil {
		errBody(w, http.StatusBadRequest, "name")
		return
	}
	if e, err := contacts.CleanEmail(s.Email); err != nil || e != s.Email {
		errBody(w, http.StatusBadRequest, "email")
		return
	}
	sum := sha256.Sum256(raw)
	f.mu.Lock()
	defer f.mu.Unlock()
	ik := s.Owner + "\x00" + key
	if prev, ok := f.idem[ik]; ok {
		if prev.sum != sum {
			errBody(w, http.StatusConflict, "idempotency key reused with another body")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(prev.status)
		_, _ = w.Write(prev.body)
		return
	}
	if f.notAccepting[s.Owner] {
		errBody(w, http.StatusNotFound, "not accepting")
		return
	}
	if f.perOwner[s.Owner] >= f.dailyCap {
		w.Header().Set("Retry-After", "3600")
		errBody(w, http.StatusTooManyRequests, "too many submissions")
		return
	}
	f.perOwner[s.Owner]++
	f.got = append(f.got, Received{Key: key, Body: s})
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	body, _ := json.Marshal(map[string]string{"receipt": base64.RawURLEncoding.EncodeToString(b)})
	body = append(body, '\n')
	f.idem[ik] = idemEntry{sum: sum, status: http.StatusAccepted, body: body}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(body)
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
