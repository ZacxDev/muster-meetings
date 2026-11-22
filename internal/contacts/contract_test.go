package contacts_test

// The contacts API contract, from meetings' side. The contract is the
// contacts plugin's own (github.com/ZacxDev/muster-contacts/contactsapi,
// pinned in go.mod), never a copy:
//
//   - TestContractRunsAgainstTheFake runs contactsapi.RunContract (every case
//     of the plugin's routes.json) against internal/contactstest, so the fake
//     every server and e2e test talks to IS the contract.
//   - TestContractTheSubmissionMeetingsSends: the body meetings builds decodes
//     strictly into contactsapi.Submission, passes its Check, is keyed by
//     KeyForCard, and the fake takes it.
//   - TestContractTheRequestsMeetingsSends: method, path and headers.
//   - TestContractEveryAnswerMeetingsHandles: every status the contract can
//     answer a submission has a defined outbox action.
//   - TestContractTheFieldTable: meetings' card fields (server.CardFields)
//     against the plugin's fields.json lives in internal/server.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/contactstest"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

const contractToken = "ctk_contract_fixture_token_0123456789abcd"

func TestContractRunsAgainstTheFake(t *testing.T) {
	f := contactstest.New(contractToken)
	defer f.Close()
	c, err := contactsapi.LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	ran := contactsapi.RunContract(t, contactsapi.Target{URL: f.URL, Token: contractToken, Given: f.Given})
	// Every case runs: the fake supports every precondition, so a skip here
	// would be a case the fake is not held to.
	if ran != len(c.Cases) || ran < 90 {
		t.Fatalf("%d of %d contract cases ran", ran, len(c.Cases))
	}
}

// fixtureCard is a card as the store hands it over, built in another zone
// first so a builder that forgot to convert to UTC is seen.
func fixtureCard() store.Card {
	zone := time.FixedZone("UTC+5", 5*3600)
	at := func(m int) *time.Time { x := time.Date(2031, 3, 14, 15, m, 0, 0, time.UTC).In(zone); return &x }
	return store.Card{CardID: strings.Repeat("c", 43), ParticipantID: "p4rT1c1pAnT0aaaa", Identity: "guest-Zm9vYmFyYmF6cXV4MDEyMzQ1Ng",
		TypedName: "Wren O.", AdmittedAt: at(3), LeftAt: at(42), MeetingID: "mEeT1nG0id00aaaa", Title: "Quarterly planning",
		CreatedAt: *at(0), StartedAt: at(1), EndedAt: at(42), Subject: "Q29udHJhY3RPd25lcjAxAA", OwnerName: "Owner Name Not Sent"}
}

func TestContractTheSubmissionMeetingsSends(t *testing.T) {
	card := fixtureCard()
	it, err := contacts.SubmissionItem(card, "Wren Okafor", "wren.okafor@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if it.Key != contactsapi.KeyForCard(card.CardID) || it.Path != contactsapi.RouteSubmissions || it.Kind != "submission" {
		t.Errorf("key %q path %q kind %q", it.Key, it.Path, it.Kind)
	}
	var sub contactsapi.Submission
	dec := json.NewDecoder(bytes.NewReader(it.Body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sub); err != nil {
		t.Fatalf("the body is not contactsapi.Submission: %v: %s", err, it.Body)
	}
	if _, _, _, _, err := sub.Check(); err != nil {
		t.Fatalf("contactsapi refuses the body: %v: %s", err, it.Body)
	}
	want := `"meeting":{"id":"mEeT1nG0id00aaaa","title":"Quarterly planning","started_at":"2031-03-14T15:01:00Z","ended_at":"2031-03-14T15:42:00Z"}`
	if !bytes.Contains(it.Body, []byte(want)) || !bytes.Contains(it.Body, []byte(`"joined_at":"2031-03-14T15:03:00Z"`)) {
		t.Errorf("times not RFC 3339 UTC: %s", it.Body)
	}
	if bytes.Contains(it.Body, []byte(card.OwnerName)) {
		t.Errorf("the body carries the owner's display name: %s", it.Body)
	}
	// The fake (held to the contract above) takes it.
	f := contactstest.New(contractToken)
	defer f.Close()
	a, err := contacts.New(f.URL, contractToken).Post(context.Background(), it.Path, it.Key, it.Body)
	if err != nil || a.Status != http.StatusAccepted {
		t.Fatalf("the fake answered %d, %v", a.Status, err)
	}
	// An untitled meeting goes with its default label; a long one is cut to
	// the contract's limit and still passes its check.
	card.Title = ""
	it, _ = contacts.SubmissionItem(card, "Wren Okafor", "wren.okafor@example.test")
	if !bytes.Contains(it.Body, []byte(`"title":"Meeting on 14 Mar 2031, 15:00 UTC"`)) {
		t.Errorf("an untitled meeting's body: %s", it.Body)
	}
	card.Title = strings.Repeat("é", contactsapi.MaxTitleRunes+7)
	if it, err = contacts.SubmissionItem(card, "Wren Okafor", "wren.okafor@example.test"); err != nil ||
		!bytes.Contains(it.Body, []byte(`"title":"`+strings.Repeat("é", contactsapi.MaxTitleRunes)+`"`)) {
		t.Errorf("a long title: %v %s", err, it.Body)
	}
}

func TestContractTheRequestsMeetingsSends(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	card := fixtureCard()
	it, err := contacts.SubmissionItem(card, "Wren Okafor", "wren.okafor@example.test")
	if err != nil {
		t.Fatal(err)
	}
	cl := contacts.New(srv.URL, contractToken)
	if _, _, err := cl.Accepting(context.Background(), card.Subject); err == nil {
		t.Error("a 202 to the owners check is not an answer; want an error")
	}
	if _, err := cl.Post(context.Background(), it.Path, it.Key, it.Body); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d requests", len(got))
	}
	for i, want := range []struct{ method, path, key, ct string }{
		{"GET", contactsapi.RouteOwner + card.Subject, "", ""},
		{"POST", contactsapi.RouteSubmissions, contactsapi.KeyForCard(card.CardID), "application/json"},
	} {
		r := got[i]
		if r.Method != want.method || r.URL.Path != want.path || r.URL.RawQuery != "" {
			t.Errorf("request %d: %s %s?%s, want %s %s", i, r.Method, r.URL.Path, r.URL.RawQuery, want.method, want.path)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer "+contractToken {
			t.Errorf("request %d: Authorization %q", i, a)
		}
		if k := r.Header.Get(contactsapi.IdempotencyHeader); k != want.key {
			t.Errorf("request %d: Idempotency-Key %q, want %q", i, k, want.key)
		}
		if ct := r.Header.Get("Content-Type"); ct != want.ct {
			t.Errorf("request %d: Content-Type %q, want %q", i, ct, want.ct)
		}
	}
	if !bytes.Equal(bodies[1], it.Body) {
		t.Errorf("the posted body changed in flight")
	}
}

// Every status the contract answers a submission with has a defined outbox
// action, and it is the design's: 202 delivered; 400, 409, 413 refused for
// good; 401 and 429 retried. A status the contract gains that this table
// does not name fails here.
func TestContractEveryAnswerMeetingsHandles(t *testing.T) {
	c, err := contactsapi.LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]contacts.Action{202: contacts.Delivered, 400: contacts.Refused, 409: contacts.Refused, 413: contacts.Refused,
		401: contacts.Retry, 429: contacts.Retry}
	seen := map[int]bool{}
	for _, cs := range c.Cases {
		for _, st := range cs.Steps {
			if st.Method != http.MethodPost || st.Path != contactsapi.RouteSubmissions {
				continue
			}
			seen[st.Status] = true
			w, ok := want[st.Status]
			if !ok {
				t.Errorf("the contract answers a submission %d (%s): the outbox's action for it is not decided here", st.Status, cs.Name)
				continue
			}
			if got := contacts.Classify(contacts.Answer{Status: st.Status}, nil); got != w {
				t.Errorf("status %d: the outbox would take action %d, want %d", st.Status, got, w)
			}
		}
	}
	for s := range want {
		if !seen[s] {
			t.Errorf("status %d is in this table but the contract never answers it", s)
		}
	}
	for _, s := range []int{500, 502, 503} {
		if contacts.Classify(contacts.Answer{Status: s}, nil) != contacts.Retry {
			t.Errorf("%d is not retried", s)
		}
	}
	if contacts.Classify(contacts.Answer{}, context.DeadlineExceeded) != contacts.Retry {
		t.Error("no answer is not retried")
	}
}
