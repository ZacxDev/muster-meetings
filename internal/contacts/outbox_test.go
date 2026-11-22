package contacts_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/contactstest"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Fixtures: an invented guest, pairwise distinct from everything the code
// or the fake names.
const (
	fxToken = "ctk_outbox_fixture_token_9f8e7d6c5b4a3210"
	fxOwner = "T3V0Ym94T3duZXIwMDAwMQ"
	fxName  = "Ottoline Q. Brandvold"
	fxEmail = "ottoline.brandvold@example.test"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{t: time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)} }
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meetings.db"), []byte("outbox test link key 0123456789a"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// item is a submission row for card cardID, holding the fixture guest.
func item(t *testing.T, cardID string) store.OutboxItem {
	t.Helper()
	sub := contacts.Submission{Owner: fxOwner, CardID: cardID, Meeting: contacts.Meeting{ID: "mEeT1nG0id00bbbb", Title: "Outbox test"},
		Participant: contacts.Participant{ID: cardID, Identity: "guest-x", TypedName: "Otto"}, Name: fxName, Email: fxEmail}
	b, err := sub.Body()
	if err != nil {
		t.Fatal(err)
	}
	return store.OutboxItem{Key: sub.Key(), Kind: "submission", Path: contacts.PathSubmissions, Body: b}
}

type rig struct {
	st  *store.Store
	clk *clock
	log *syncBuf
	ob  *contacts.Outbox
}

func newRig(t *testing.T, base string) *rig {
	r := &rig{st: openStore(t), clk: newClock(), log: &syncBuf{}}
	r.ob = contacts.NewOutbox(contacts.New(base, fxToken), r.st, log.New(r.log, "", 0), r.clk.Now)
	return r
}

func (r *rig) enqueue(t *testing.T, cardID string) {
	t.Helper()
	if err := r.st.EnqueueOutbox(context.Background(), item(t, cardID), r.clk.Now()); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) count(t *testing.T) int {
	t.Helper()
	n, err := r.st.CountOutbox(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// 🔴 Each class of answer leads to the design's keep or delete, through the
// real worker and the real store.
func TestTheOutboxKeepsOrDeletesByTheAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		mode   contactstest.Mode
		status int
		keep   bool
	}{
		{"202 delivered", contactstest.Up, 0, false},
		{"404 not accepting", contactstest.Status, 404, false},
		{"400 refused", contactstest.Status, 400, false},
		{"409 refused", contactstest.Status, 409, false},
		{"401 retried", contactstest.Status, 401, true},
		{"429 retried", contactstest.Status, 429, true},
		{"500 retried", contactstest.Status, 500, true},
		{"503 retried", contactstest.Status, 503, true},
		{"no answer (a black hole) retried", contactstest.BlackHole, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := contactstest.New(fxToken)
			defer f.Close()
			f.SetMode(c.mode, c.status, "")
			r := newRig(t, f.URL)
			r.enqueue(t, "cardAAAAAAAAAAAA")
			start := time.Now()
			r.ob.Pass(context.Background())
			if el := time.Since(start); el > contacts.CallTimeout+2*time.Second {
				t.Errorf("one pass took %s: the call timeout is %s", el, contacts.CallTimeout)
			}
			if got := r.count(t) == 1; got != c.keep {
				t.Fatalf("after one pass the row is kept = %v, want %v (log: %s)", got, c.keep, r.log.String())
			}
			if c.keep {
				due, _ := r.st.DueOutbox(context.Background(), r.clk.Now(), 10)
				if len(due) != 0 {
					t.Errorf("a retried row is due again at once")
				}
				r.clk.Add(contacts.BackoffFirst)
				due, _ = r.st.DueOutbox(context.Background(), r.clk.Now(), 10)
				if len(due) != 1 || due[0].Attempts != 1 {
					t.Errorf("a retried row is not due after %s with one attempt counted: %+v", contacts.BackoffFirst, due)
				}
			}
		})
	}
}

func TestTheOutboxBacksOffFrom5sDoublingTo10Minutes(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 320 * time.Second, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := contacts.Backoff(i+1, contacts.Answer{}); got != w {
			t.Errorf("attempt %d: %s, want %s", i+1, got, w)
		}
	}
	if got := contacts.Backoff(1, contacts.Answer{Status: 429, RetryAfter: 15 * time.Minute}); got != 15*time.Minute {
		t.Errorf("a 429's longer Retry-After: %s", got)
	}
	if got := contacts.Backoff(1, contacts.Answer{Status: 429, RetryAfter: 9 * time.Hour}); got != contacts.RetryAfterMax {
		t.Errorf("a huge Retry-After: %s, want the cap %s", got, contacts.RetryAfterMax)
	}
	if got := contacts.Backoff(5, contacts.Answer{Status: 429, RetryAfter: time.Second}); got != 80*time.Second {
		t.Errorf("a shorter Retry-After shortens the backoff: %s", got)
	}
}

// 🔴 A row 72 h old is deleted undelivered and counted; one a minute
// younger is attempted.
func TestTheOutboxDropsARowAfter72Hours(t *testing.T) {
	f := contactstest.New(fxToken)
	defer f.Close()
	f.SetMode(contactstest.Status, 503, "")
	r := newRig(t, f.URL)
	r.enqueue(t, "cardOLDOLDOLDOLD")
	r.clk.Add(time.Minute)
	r.enqueue(t, "cardYOUNGYOUNGYO")
	r.clk.Add(store.OutboxLifetime - time.Minute)
	r.ob.Pass(context.Background())
	if n := r.count(t); n != 1 {
		t.Fatalf("%d rows, want the younger one alone", n)
	}
	if r.ob.Dropped.Load() != 1 || !strings.Contains(r.log.String(), "1 row(s) dropped undelivered after 72h0m0s") {
		t.Errorf("dropped %d; log %q", r.ob.Dropped.Load(), r.log.String())
	}
	if n := f.Count("POST", contacts.PathSubmissions); n != 1 {
		t.Errorf("%d attempts, want 1 (the younger row's)", n)
	}
}

// 🔴 A retry after a lost answer is harmless: contacts got the first post,
// its answer never reached meetings, the row is retried with the same key
// and body, and contacts holds exactly one submission.
func TestARetryAfterALostAnswerMakesExactlyOneSubmission(t *testing.T) {
	f := contactstest.New(fxToken)
	defer f.Close()
	target, _ := url.Parse(f.URL)
	rp := httputil.NewSingleHostReverseProxy(target)
	var lose atomic.Bool
	lose.Store(true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lose.Load() {
			// Forward, then drop the answer on the floor: the connection
			// is cut before a byte of it reaches meetings.
			b, _ := io.ReadAll(r.Body)
			req, _ := http.NewRequest(r.Method, f.URL+r.URL.Path, bytes.NewReader(b))
			req.Header = r.Header.Clone()
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
			lose.Store(false)
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		rp.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	r := newRig(t, proxy.URL)
	r.enqueue(t, "cardLOSTLOSTLOST")
	r.ob.Pass(context.Background())
	if r.count(t) != 1 || len(f.Got()) != 1 {
		t.Fatalf("after the lost answer: %d rows, %d stored; want the row kept and contacts holding it", r.count(t), len(f.Got()))
	}
	r.clk.Add(contacts.BackoffFirst)
	r.ob.Pass(context.Background())
	if r.count(t) != 0 {
		t.Fatalf("the retry was not delivered: %s", r.log.String())
	}
	if n := len(f.Got()); n != 1 {
		t.Errorf("contacts holds %d submissions, want exactly 1", n)
	}
	reqs := f.Requests()
	if len(reqs) != 2 || reqs[0].Key != reqs[1].Key || !bytes.Equal(reqs[0].Body, reqs[1].Body) {
		t.Errorf("the retry is not the same key and body: %+v", reqs)
	}
}

// 🔴 Ordering: due rows go oldest first, and one row's failure does not
// hold back those after it.
func TestTheOutboxDeliversInOrderAndOneFailureHoldsNothingBack(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("Idempotency-Key")
		mu.Lock()
		order = append(order, k)
		mu.Unlock()
		if k == "card:cardBBBBBBBBBBBB" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	r := newRig(t, srv.URL)
	for _, id := range []string{"cardCCCCCCCCCCCC", "cardBBBBBBBBBBBB", "cardAAAAAAAAAAAA"} {
		r.enqueue(t, id)
		r.clk.Add(time.Millisecond)
	}
	if n := r.ob.Pass(context.Background()); n != 2 {
		t.Errorf("delivered %d, want 2", n)
	}
	want := []string{"card:cardCCCCCCCCCCCC", "card:cardBBBBBBBBBBBB", "card:cardAAAAAAAAAAAA"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("order %v, want %v (insertion order)", order, want)
	}
	due, _ := r.st.DueOutbox(context.Background(), r.clk.Now().Add(time.Hour), 10)
	if len(due) != 1 || due[0].Key != "card:cardBBBBBBBBBBBB" {
		t.Errorf("left %+v, want the failed row alone", due)
	}
}

// 🔴 No PII in logs: every class of answer is logged without the guest's
// name, email, the owner or the token; with a positive control that the
// same buffer shows the fixture when it IS written.
func TestTheOutboxLogsNoPII(t *testing.T) {
	f := contactstest.New(fxToken)
	defer f.Close()
	r := newRig(t, f.URL)
	for i, st := range []int{0, 404, 400, 409, 401, 429, 500} {
		if st == 0 {
			f.SetMode(contactstest.Up, 0, "")
		} else {
			f.SetMode(contactstest.Status, st, "")
		}
		r.enqueue(t, "cardPII0000000"+string(rune('a'+i))+"x")
		r.ob.Pass(context.Background())
		r.clk.Add(time.Hour)
	}
	out := r.log.String()
	if strings.Count(out, "contacts: outbox:") < 6 {
		t.Fatalf("the log has too few outbox lines to judge: %q", out)
	}
	for _, s := range []string{fxName, fxEmail, "ottoline", "brandvold", fxOwner, fxToken, "cardPII"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(s)) {
			t.Errorf("the log holds %q: %s", s, out)
		}
	}
	// Positive control: the buffer and the check see the fixture once it is
	// there.
	log.New(r.log, "", 0).Printf("control %s", fxEmail)
	if !strings.Contains(r.log.String(), fxEmail) {
		t.Fatal("the positive control is not seen: the check is blind")
	}
}

// The accepting cache: nothing until an answer; 200 then true, 404 false; a
// failure keeps the last answer; asked at most once per TTL; past
// AcceptingStale with no fresh answer, false.
func TestTheAcceptingCache(t *testing.T) {
	f := contactstest.New(fxToken)
	defer f.Close()
	clk := newClock()
	buf := &syncBuf{}
	a := contacts.NewAccepting(contacts.New(f.URL, fxToken), log.New(buf, "", 0), clk.Now)
	if a.Get(fxOwner) {
		t.Fatal("accepting before any answer")
	}
	a.Refresh(fxOwner)
	a.Wait()
	if !a.Get(fxOwner) {
		t.Fatal("not accepting after a 200")
	}
	a.Refresh(fxOwner) // within the TTL: no call
	a.Wait()
	if n := f.Count("GET", contacts.PathOwners); n != 1 {
		t.Errorf("%d owners checks within the TTL, want 1", n)
	}
	f.SetMode(contactstest.Status, 503, "")
	clk.Add(contacts.AcceptingTTL)
	a.Refresh(fxOwner)
	a.Wait()
	if !a.Get(fxOwner) {
		t.Error("a failed refresh dropped the last answer")
	}
	f.SetMode(contactstest.Status, 401, "")
	clk.Add(contacts.AcceptingTTL)
	a.Refresh(fxOwner)
	a.Wait()
	if !strings.Contains(buf.String(), "contacts: unauthorised") || strings.Contains(buf.String(), fxToken) {
		t.Errorf("a 401's log line: %q", buf.String())
	}
	clk.Add(contacts.AcceptingStale)
	if a.Get(fxOwner) {
		t.Error("an answer older than AcceptingStale is still used")
	}
	f.SetMode(contactstest.Up, 0, "")
	f.SetAccepting(fxOwner, false)
	a.Refresh(fxOwner)
	a.Wait()
	if a.Get(fxOwner) {
		t.Error("accepting after a 404")
	}
}

func TestConfigureIsBothOrNeither(t *testing.T) {
	const tok = "ctk_configure_fixture_0123456789abcdef"
	for _, c := range []struct {
		url, token, muster string
		ok, configured     bool
	}{
		{"", "", "mpl_x", true, false},
		{"  ", " ", "mpl_x", true, false},
		{"http://muster-contacts:8081", "", "mpl_x", false, false},
		{"", tok, "mpl_x", false, false},
		{"http://muster-contacts:8081", tok, "mpl_x", true, true},
		{"https://contacts.example.test", tok, "mpl_x", true, true},
		{"http://muster-contacts:8081/v1", tok, "mpl_x", false, false},
		{"http://muster-contacts:8081?x=1", tok, "mpl_x", false, false},
		{"ftp://muster-contacts", tok, "mpl_x", false, false},
		{"http://*.example.test", tok, "mpl_x", false, false},
		{"http://muster-contacts:8081", "short", "mpl_x", false, false},
		{"http://muster-contacts:8081", tok + " x", "mpl_x", false, false},
		{"http://muster-contacts:8081", tok, tok, false, false},
	} {
		cl, err := contacts.Configure(c.url, c.token, c.muster)
		if (err == nil) != c.ok || (cl != nil) != c.configured {
			t.Errorf("Configure(%q, %q) = %v, %v; want ok %v configured %v", c.url, c.token, cl != nil, err, c.ok, c.configured)
		}
		if err != nil && strings.Contains(err.Error(), tok) {
			t.Errorf("the error shows the token: %v", err)
		}
	}
	if _, err := contacts.Configure("http://a:1", "", ""); err != contacts.ErrHalfConfigured {
		t.Errorf("one alone: %v", err)
	}
}
