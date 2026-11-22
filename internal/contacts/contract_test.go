package contacts_test

// The contacts API contract, from meetings' side (muster's
// docs/design/contacts.md, "Phase 1: the API contract, shared by both
// plugins"). testdata/contract.json is meetings' own copy; every test here
// reads only it, so replacing it with the contacts plugin's contract is a
// file copy, and anything the two disagree on turns red here.
//
//   - TestContractCasesAgainstTheFake runs every case against
//     internal/contactstest (or, with CONTACTS_CONTRACT_URL and
//     CONTACTS_CONTRACT_TOKEN set, the state-free cases against a real
//     contacts server), so what the server and e2e tests talk to IS the
//     contract.
//   - TestContractTheSubmissionMeetingsSends: the body meetings builds for
//     the fixture's card equals the contract's body, field for field.
//   - TestContractTheRequestsMeetingsSends: method, path and headers.
//   - TestContractEveryAnswerMeetingsHandles: the outbox's action per status.
//   - TestContractTheFieldTable: the shared validation table.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/contactstest"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

type contract struct {
	Token      string `json:"token"`
	Owner      string `json:"owner"`
	OtherOwner string `json:"other_owner"`
	Submission struct {
		Path string          `json:"path"`
		Key  string          `json:"key"`
		Body json.RawMessage `json:"body"`
	} `json:"submission"`
	Cases []struct {
		Name  string `json:"name"`
		Setup *struct {
			NotAccepting []string `json:"not_accepting"`
			DailyCap     *int     `json:"daily_cap"`
		} `json:"setup"`
		Request struct {
			Method   string         `json:"method"`
			Path     string         `json:"path"`
			Auth     string         `json:"auth"`
			Key      string         `json:"key"`
			Body     string         `json:"body"`
			Override map[string]any `json:"override"`
		} `json:"request"`
		Response struct {
			Status     int            `json:"status"`
			Body       map[string]any `json:"body"`
			Receipt    bool           `json:"receipt"`
			RetryAfter bool           `json:"retry_after"`
			SameAs     string         `json:"same_as"`
		} `json:"response"`
		After *struct {
			Stored int `json:"stored"`
		} `json:"after"`
	} `json:"cases"`
	Outbox []struct {
		Status int    `json:"status"`
		Action string `json:"action"`
	} `json:"outbox"`
	Fields struct {
		Emails []struct {
			In  string  `json:"in"`
			Out *string `json:"out"`
		} `json:"emails"`
		Names []struct {
			In  string  `json:"in"`
			Out *string `json:"out"`
		} `json:"names"`
	} `json:"fields"`
}

func loadContract(t *testing.T) contract {
	t.Helper()
	raw, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var c contract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) < 10 || len(c.Outbox) < 10 || len(c.Fields.Emails) < 10 {
		t.Fatalf("the contract has %d cases, %d outbox rows, %d emails: is it the file it should be?", len(c.Cases), len(c.Outbox), len(c.Fields.Emails))
	}
	return c
}

var receiptRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func TestContractCasesAgainstTheFake(t *testing.T) {
	c := loadContract(t)
	base, token := os.Getenv("CONTACTS_CONTRACT_URL"), os.Getenv("CONTACTS_CONTRACT_TOKEN")
	var fake *contactstest.Fake
	if base == "" {
		fake = contactstest.New(c.Token)
		defer fake.Close()
		base, token = fake.URL, c.Token
	}
	subst := func(s string) string {
		return strings.NewReplacer("{owner}", c.Owner, "{other_owner}", c.OtherOwner, "{submission.key}", c.Submission.Key).Replace(s)
	}
	seen := map[string]string{}
	ran := 0
	for _, tc := range c.Cases {
		if tc.Setup != nil && fake == nil {
			t.Logf("%s: needs state the contract sets up on the fake; skipped against a real server", tc.Name)
			continue
		}
		if tc.Setup != nil {
			for _, o := range tc.Setup.NotAccepting {
				fake.SetAccepting(subst(o), false)
			}
			if tc.Setup.DailyCap != nil {
				fake.SetDailyCap(*tc.Setup.DailyCap)
			}
		}
		var body io.Reader
		if tc.Request.Body != "" {
			if tc.Request.Body != "{submission.body}" {
				t.Fatalf("%s: body %q: only {submission.body} is known", tc.Name, tc.Request.Body)
			}
			var m map[string]any
			if err := json.Unmarshal(c.Submission.Body, &m); err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.Request.Override {
				m[k] = v
			}
			b, _ := json.Marshal(m)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(tc.Request.Method, base+subst(tc.Request.Path), body)
		switch tc.Request.Auth {
		case "token":
			req.Header.Set("Authorization", "Bearer "+token)
		case "wrong":
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", len(token)))
		case "prefix":
			req.Header.Set("Authorization", "Bearer "+token[:len(token)-1])
		case "none":
		default:
			t.Fatalf("%s: auth %q", tc.Name, tc.Request.Auth)
		}
		if tc.Request.Key != "" {
			req.Header.Set("Idempotency-Key", subst(tc.Request.Key))
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.Name, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ran++
		if resp.StatusCode != tc.Response.Status {
			t.Errorf("%s: status %d, want %d (%s)", tc.Name, resp.StatusCode, tc.Response.Status, raw)
		}
		if tc.Response.Body != nil {
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, tc.Response.Body) {
				t.Errorf("%s: body %s, want %v", tc.Name, raw, tc.Response.Body)
			}
		}
		if tc.Response.Receipt {
			var got map[string]any
			_ = json.Unmarshal(raw, &got)
			if r, _ := got["receipt"].(string); !receiptRE.MatchString(r) || len(got) != 1 {
				t.Errorf("%s: body %s, want exactly {\"receipt\": <43 base64url>}", tc.Name, raw)
			}
		}
		if tc.Response.RetryAfter && resp.Header.Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", tc.Name)
		}
		if tc.Response.SameAs != "" && seen[tc.Response.SameAs] != string(raw) {
			t.Errorf("%s: body %s, want the same as %q's %s", tc.Name, raw, tc.Response.SameAs, seen[tc.Response.SameAs])
		}
		seen[tc.Name] = string(raw)
		if tc.After != nil && fake != nil {
			if n := len(fake.Got()); n != tc.After.Stored {
				t.Errorf("%s: %d submissions stored, want %d", tc.Name, n, tc.After.Stored)
			}
		}
		if fake != nil {
			fake.SetAccepting(c.Owner, true)
			fake.SetAccepting(c.OtherOwner, true)
			fake.SetDailyCap(200)
		}
	}
	if ran < 8 {
		t.Fatalf("only %d cases ran", ran)
	}
}

// fixtureCard is the store.Card the contract's submission is about: every
// value the contract's body holds, as meetings reads it from its database.
func fixtureCard(t *testing.T, c contract) (store.Card, string, string) {
	t.Helper()
	var m struct {
		Owner   string `json:"owner"`
		Meeting struct {
			ID        string    `json:"id"`
			Title     string    `json:"title"`
			StartedAt time.Time `json:"started_at"`
			EndedAt   time.Time `json:"ended_at"`
		} `json:"meeting"`
		Participant struct {
			ID        string    `json:"id"`
			Identity  string    `json:"identity"`
			TypedName string    `json:"typed_name"`
			JoinedAt  time.Time `json:"joined_at"`
			LeftAt    time.Time `json:"left_at"`
		} `json:"participant"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(c.Submission.Body, &m); err != nil {
		t.Fatal(err)
	}
	// Times as the store hands them over: UTC, but built in another zone
	// first, so a builder that forgot to convert is seen.
	zone := time.FixedZone("UTC+5", 5*3600)
	at := func(v time.Time) *time.Time { x := v.In(zone); return &x }
	return store.Card{ParticipantID: m.Participant.ID, Identity: m.Participant.Identity, TypedName: m.Participant.TypedName,
		AdmittedAt: at(m.Participant.JoinedAt), LeftAt: at(m.Participant.LeftAt), MeetingID: m.Meeting.ID, Title: m.Meeting.Title,
		CreatedAt: m.Meeting.StartedAt.Add(-time.Hour), StartedAt: at(m.Meeting.StartedAt), EndedAt: at(m.Meeting.EndedAt),
		Subject: m.Owner, OwnerName: "Owner Name Not Sent"}, m.Name, m.Email
}

func TestContractTheSubmissionMeetingsSends(t *testing.T) {
	c := loadContract(t)
	card, name, email := fixtureCard(t, c)
	it, err := contacts.SubmissionItem(card, name, email)
	if err != nil {
		t.Fatal(err)
	}
	if it.Key != c.Submission.Key || it.Path != c.Submission.Path || it.Kind != "submission" {
		t.Errorf("key %q path %q kind %q; want %q %q submission", it.Key, it.Path, it.Kind, c.Submission.Key, c.Submission.Path)
	}
	var got, want any
	if err := json.Unmarshal(it.Body, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(c.Submission.Body, &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the body meetings sends:\n%s\nthe contract's:\n%s", it.Body, c.Submission.Body)
	}
	// 🔴 Never the owner's display name, and never the guest's card secret.
	if bytes.Contains(it.Body, []byte(card.OwnerName)) {
		t.Errorf("the body carries the owner's display name: %s", it.Body)
	}
	// An untitled meeting is sent with its default label; a long title is cut.
	card.Title = ""
	it, _ = contacts.SubmissionItem(card, name, email)
	if !bytes.Contains(it.Body, []byte(`"title":"Meeting on 14 Mar 2031, 14:00 UTC"`)) {
		t.Errorf("an untitled meeting's body: %s", it.Body)
	}
	card.Title = strings.Repeat("é", contacts.MaxTitle+7)
	it, _ = contacts.SubmissionItem(card, name, email)
	if !bytes.Contains(it.Body, []byte(`"title":"`+strings.Repeat("é", contacts.MaxTitle)+`"`)) {
		t.Errorf("a long title is not cut to %d characters: %s", contacts.MaxTitle, it.Body)
	}
}

func TestContractTheRequestsMeetingsSends(t *testing.T) {
	c := loadContract(t)
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
	cl := contacts.New(srv.URL, c.Token)
	if _, _, err := cl.Accepting(context.Background(), c.Owner); err == nil {
		t.Error("a 202 to the owners check is not an answer; want an error")
	}
	if _, err := cl.Post(context.Background(), c.Submission.Path, c.Submission.Key, c.Submission.Body); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d requests", len(got))
	}
	for i, want := range []struct{ method, path, key, ct string }{
		{"GET", "/v1/owners/" + c.Owner, "", ""},
		{"POST", c.Submission.Path, c.Submission.Key, "application/json"},
	} {
		r := got[i]
		if r.Method != want.method || r.URL.Path != want.path || r.URL.RawQuery != "" {
			t.Errorf("request %d: %s %s?%s, want %s %s", i, r.Method, r.URL.Path, r.URL.RawQuery, want.method, want.path)
		}
		if a := r.Header.Get("Authorization"); a != "Bearer "+c.Token {
			t.Errorf("request %d: Authorization %q", i, a)
		}
		if k := r.Header.Get("Idempotency-Key"); k != want.key {
			t.Errorf("request %d: Idempotency-Key %q, want %q", i, k, want.key)
		}
		if ct := r.Header.Get("Content-Type"); ct != want.ct {
			t.Errorf("request %d: Content-Type %q, want %q", i, ct, want.ct)
		}
	}
	if !bytes.Equal(bodies[1], c.Submission.Body) {
		t.Errorf("the posted body changed in flight")
	}
}

func TestContractEveryAnswerMeetingsHandles(t *testing.T) {
	c := loadContract(t)
	names := map[contacts.Action]string{contacts.Retry: "retry", contacts.Delivered: "delivered", contacts.NotAccepting: "not_accepting", contacts.Refused: "refused"}
	for _, o := range c.Outbox {
		var err error
		if o.Status == 0 {
			err = context.DeadlineExceeded
		}
		if got := names[contacts.Classify(contacts.Answer{Status: o.Status}, err)]; got != o.Action {
			t.Errorf("status %d: the outbox would %s, the contract says %s", o.Status, got, o.Action)
		}
	}
}

func TestContractTheFieldTable(t *testing.T) {
	c := loadContract(t)
	for _, e := range c.Fields.Emails {
		got, err := contacts.CleanEmail(e.In)
		switch {
		case e.Out == nil && err == nil:
			t.Errorf("email %q accepted as %q, want refused", e.In, got)
		case e.Out != nil && (err != nil || got != *e.Out):
			t.Errorf("email %q = %q, %v; want %q", e.In, got, err, *e.Out)
		}
	}
	for _, n := range c.Fields.Names {
		got, err := lobby.CleanName(n.In)
		switch {
		case n.Out == nil && err == nil:
			t.Errorf("name %q accepted as %q, want refused", n.In, got)
		case n.Out != nil && (err != nil || got != *n.Out):
			t.Errorf("name %q = %q, %v; want %q", n.In, got, err, *n.Out)
		}
	}
}
