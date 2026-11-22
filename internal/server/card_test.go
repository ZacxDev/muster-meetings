package server_test

// The guest card (muster's docs/design/contacts.md, "The guest card",
// "Failure behaviour"), against the fake contacts API (internal/contactstest,
// which internal/contacts's contract test holds to the contract).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/contactstest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Card fixtures: an invented guest, distinct from every other fixture and
// from any constant the code names.
const (
	cToken = "ctk_server_card_fixture_7a6b5c4d3e2f1a0b"
	cEmail = "imogen.vasquez-thorne@example.test"
	cName  = "Imogen V-T."
)

// newCardEnv is an env with contacts configured against a fake.
func newCardEnv(t *testing.T) (*env, *contactstest.Fake) {
	t.Helper()
	f := contactstest.New(cToken)
	t.Cleanup(f.Close)
	e := newEnvOpts(t, nil, func(c *server.Config) { c.Contacts = contacts.New(f.URL, cToken) })
	return e, f
}

// inCall knocks as name, is admitted, and opens their event stream; it
// returns once the stream has run (so the accepting cache was asked) and
// the answer is in.
func (e *env) inCall(c call, name string) (string, *stream) {
	e.t.Helper()
	tk := e.knock(c.Code, name)
	e.admit(c, name)
	s := e.guestEvents(c.Code, tk)
	s.want("meeting")
	if a := e.server.Accepting(); a != nil {
		a.Wait()
	}
	return tk, s
}

func (e *env) end(c call) time.Duration {
	e.t.Helper()
	start := time.Now()
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		e.t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	return time.Since(start)
}

type endedEv struct {
	Message string `json:"message"`
	Card    *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
	} `json:"card"`
}

func endedOf(t *testing.T, s *stream) endedEv {
	t.Helper()
	var ev endedEv
	if err := json.Unmarshal([]byte(s.want("ended").Data), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// cardStates is every participant's card_state, by name, read from the
// database itself.
func (e *env) cardStates(meetingID string) map[string]string {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?mode=ro")
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT display_name, coalesce(card_state, '') FROM participants WHERE meeting_id = ?`, meetingID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, s string
		rows.Scan(&n, &s)
		out[n] = s
	}
	return out
}

func (e *env) outboxRows() int {
	e.t.Helper()
	n, err := e.store.CountOutbox(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- 🔴 when the card shows: each condition alone false, and all true ----

func TestCardEligibilityAllTrue(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	e.end(c)
	ev := endedOf(t, s)
	if ev.Message != server.GuestEnded || ev.Card == nil || len(ev.Card.ID) != 43 || ev.Card.Name != cName || ev.Card.Owner != ada.DisplayName {
		t.Fatalf("ended = %+v, want the card with the typed name and the owner's", ev)
	}
	if st := e.cardStates(c.ID)[cName]; st != store.CardOffered {
		t.Errorf("card_state %q, want offered", st)
	}
}

func TestCardEligibilityNotAccepting(t *testing.T) {
	e, f := newCardEnv(t)
	c := e.meeting(ada)
	f.SetAccepting(c.Subject, false)
	_, s := e.inCall(c, cName)
	e.end(c)
	if ev := endedOf(t, s); ev.Card != nil {
		t.Fatalf("a card while contacts says not accepting: %+v", ev)
	}
	if st := e.cardStates(c.ID)[cName]; st != "" {
		t.Errorf("card_state %q, want none minted", st)
	}
}

func TestCardEligibilityNoAnswerYet(t *testing.T) {
	e, f := newCardEnv(t)
	f.SetMode(contactstest.Status, 503, "")
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	e.end(c)
	if ev := endedOf(t, s); ev.Card != nil {
		t.Fatalf("a card with no answer from contacts ever: %+v", ev)
	}
}

func TestCardEligibilityNeverAdmitted(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName) // warms the cache for this owner
	tk := e.knock(c.Code, grace)
	w := e.wait(c.Code, tk)
	w.want("waiting")
	e.end(c)
	endedOf(t, s)
	var ev endedEv
	json.Unmarshal([]byte(w.want("ended").Data), &ev)
	if ev.Card != nil {
		t.Fatalf("a waiting guest got a card: %+v", ev)
	}
	if st := e.cardStates(c.ID)[grace]; st != "" {
		t.Errorf("a never-admitted guest's card_state %q", st)
	}
	// And the store itself refuses them, whatever asks.
	if _, ok, _ := e.store.OfferCard(context.Background(), e.pidAny(c, grace), time.Now()); ok {
		t.Error("a never-admitted guest was offered a card by the store")
	}
}

func TestCardEligibilityRemovedOrDenied(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	_, rs := e.inCall(c, grace)
	if resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/participants/"+e.pid(c, grace), c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("remove = %d %s", resp.StatusCode, raw)
	}
	if ev := rs.want("removed"); strings.Contains(ev.Data, "card") {
		t.Fatalf("a removed guest's event carries a card: %s", ev.Data)
	}
	e.knock(c.Code, linus)
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, linus)+"/deny", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("deny = %d %s", resp.StatusCode, raw)
	}
	e.end(c)
	endedOf(t, s)
	got := e.cardStates(c.ID)
	if got[grace] != "" || got[linus] != "" || got[cName] != store.CardOffered {
		t.Errorf("card states %v: want none for the removed and the denied, one for the guest at End", got)
	}
	// And the store itself refuses them, whatever asks.
	for _, n := range []string{grace, linus} {
		if _, ok, _ := e.store.OfferCard(context.Background(), e.pidAny(c, n), time.Now()); ok {
			t.Errorf("OfferCard minted a card for %s", n)
		}
	}
}

// 🔴 Never the owner: a participants row of kind owner, admitted and ended,
// gets no card from the store's predicate.
func TestCardIsNeverOfferedToTheOwner(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	db, err := sql.Open("sqlite", "file:"+e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO participants (id, meeting_id, kind, display_name, identity, ticket_sha256, state, knocked_at, admitted_at)
		VALUES ('ownerRow00000000', ?, 'owner', ?, 'owner-row-identity', 'owner-row-ticket-hash', 'admitted', 1, 1)`, c.ID, ada.DisplayName); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.end(c)
	endedOf(t, s)
	if st := e.cardStates(c.ID)[ada.DisplayName]; st != "" {
		t.Fatalf("the owner's row was offered a card (%q)", st)
	}
	if _, ok, err := e.store.OfferCard(context.Background(), "ownerRow00000000", time.Now()); ok || err != nil {
		t.Fatalf("OfferCard(owner) = %v, %v; want no card", ok, err)
	}
	// The control: the same row as a guest IS offered one, so the case
	// reaches the kind check and nothing earlier refuses it.
	db2, _ := sql.Open("sqlite", "file:"+e.dbPath)
	defer db2.Close()
	db2.Exec(`UPDATE participants SET kind = 'guest' WHERE id = 'ownerRow00000000'`)
	db2.Close()
	if _, ok, err := e.store.OfferCard(context.Background(), "ownerRow00000000", time.Now()); !ok || err != nil {
		t.Fatalf("control: the same row as a guest = %v, %v; want a card", ok, err)
	}
}

// ---- the Leave path, and once only ----

func TestCardOnLeaveAndOnlyOnce(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	tk, _ := e.inCall(c, cName)
	resp, raw := e.do("POST", "/j/"+c.Code+"/leave", "", map[string]string{"ticket": tk})
	var b struct {
		Card *struct{ ID, Name, Owner string } `json:"card"`
	}
	json.Unmarshal(raw, &b)
	if resp.StatusCode != 200 || b.Card == nil || len(b.Card.ID) != 43 || b.Card.Name != cName || b.Card.Owner != ada.DisplayName {
		t.Fatalf("leave = %d %s: want 200 with the card", resp.StatusCode, raw)
	}
	// A second Leave: the ticket is spent (409), no second card.
	if resp, raw := e.do("POST", "/j/"+c.Code+"/leave", "", map[string]string{"ticket": tk}); resp.StatusCode != 409 || strings.Contains(string(raw), "card") {
		t.Errorf("a second leave = %d %s", resp.StatusCode, raw)
	}
	e.end(c)
	// 🔴 Once per guest per meeting: End offers no second card to one who
	// already had one, whatever asks.
	if _, ok, _ := e.store.OfferCard(context.Background(), e.pidAny(c, cName), time.Now()); ok {
		t.Fatal("a second card was minted for the same guest")
	}
	// Spent once: Skip, then Skip and Send again are the expired answer.
	if resp, raw := e.do("POST", "/card/skip", "", map[string]string{"id": b.Card.ID}); resp.StatusCode != 204 {
		t.Fatalf("skip = %d %s", resp.StatusCode, raw)
	}
	for _, p := range []struct {
		path string
		body map[string]string
	}{{"/card/skip", map[string]string{"id": b.Card.ID}}, {"/card", map[string]string{"id": b.Card.ID, "name": cName, "email": cEmail}}} {
		if resp, raw := e.do("POST", p.path, "", p.body); resp.StatusCode != 404 || !strings.Contains(string(raw), server.CardExpired) {
			t.Errorf("%s after a skip = %d %s", p.path, resp.StatusCode, raw)
		}
	}
}

// ---- 🔴 spending: Send, Skip, twice, expired, unknown ----

func TestCardSendWritesOneOutboxRowAndSkipWritesNone(t *testing.T) {
	e, f := newCardEnv(t)
	f.SetMode(contactstest.Status, 503, "") // hold delivery: the rows stay to be read
	c := e.meeting(ada)
	// Warm the cache while contacts answers, then hold delivery.
	f.SetMode(contactstest.Up, 0, "")
	_, s1 := e.inCall(c, cName)
	_, s2 := e.inCall(c, grace)
	f.SetMode(contactstest.Status, 503, "")
	e.end(c)
	send, skip := endedOf(t, s1).Card, endedOf(t, s2).Card
	if send == nil || skip == nil {
		t.Fatal("no cards")
	}
	if resp, raw := e.do("POST", "/card/skip", "", map[string]string{"id": skip.ID}); resp.StatusCode != 204 {
		t.Fatalf("skip = %d %s", resp.StatusCode, raw)
	}
	if n := e.outboxRows(); n != 0 {
		t.Fatalf("a skip wrote %d outbox rows", n)
	}
	resp, raw := e.do("POST", "/card", "", map[string]string{"id": send.ID, "name": "  " + cName + "  ", "email": cEmail})
	if resp.StatusCode != 200 || string(raw) != `{"message":"Sent to Ada L."}`+"\n" {
		t.Fatalf("send = %d %q", resp.StatusCode, raw)
	}
	if n := e.outboxRows(); n != 1 {
		t.Fatalf("%d outbox rows after one send, want 1", n)
	}
	if resp, raw := e.do("POST", "/card", "", map[string]string{"id": send.ID, "name": cName, "email": cEmail}); resp.StatusCode != 404 || !strings.Contains(string(raw), server.CardExpired) {
		t.Errorf("a second send = %d %s", resp.StatusCode, raw)
	}
	if n := e.outboxRows(); n != 1 {
		t.Errorf("%d outbox rows after a second send, want still 1", n)
	}
	got := e.cardStates(c.ID)
	if got[cName] != store.CardSent || got[grace] != store.CardSkipped {
		t.Errorf("card states %v", got)
	}
	// Delivered once contacts is back: exactly the fields the guest sent.
	f.SetMode(contactstest.Up, 0, "")
	e.clock.Add(time.Hour)
	e.server.Outbox().Pass(context.Background())
	recv := f.Got()
	if len(recv) != 1 || recv[0].Body.Name != cName || recv[0].Body.Email != cEmail || recv[0].Body.Owner != e.subject(ada) ||
		recv[0].Body.Meeting.ID != c.ID || recv[0].Body.Participant.TypedName != cName || recv[0].Key != "card:"+publicCardID(send.ID) || recv[0].Body.CardID != publicCardID(send.ID) {
		t.Fatalf("contacts got %+v", recv)
	}
	if e.outboxRows() != 0 {
		t.Error("the delivered row is still in the outbox")
	}
}

func TestCardExpiredUnknownAndMalformedAreOneAnswer(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	e.end(c)
	card := endedOf(t, s).Card
	want := `{"error":"` + server.CardExpired + `"}` + "\n"
	for _, id := range []string{strings.Repeat("Q", 43), "short", ""} {
		if resp, raw := e.do("POST", "/card", "", map[string]string{"id": id, "name": cName, "email": cEmail}); resp.StatusCode != 404 || string(raw) != want {
			t.Errorf("id %q = %d %s", id, resp.StatusCode, raw)
		}
	}
	e.clock.Add(store.CardLifetime)
	if resp, raw := e.do("POST", "/card", "", map[string]string{"id": card.ID, "name": cName, "email": cEmail}); resp.StatusCode != 404 || string(raw) != want {
		t.Errorf("an expired card = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.do("POST", "/card?id="+card.ID, "", map[string]string{"id": card.ID}); resp.StatusCode != 400 {
		t.Errorf("a card id in the query = %d, want 400", resp.StatusCode)
	}
}

// ---- validation, and the card keeps its state on a 400 ----

func TestCardValidationNamesTheFieldAndSpendsNothing(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	e.end(c)
	card := endedOf(t, s).Card
	for _, x := range []struct{ name, email, field string }{
		{"", cEmail, "name"},
		{strings.Repeat("n", 41), cEmail, "name"},
		{cName, "", "email"},
		{cName, "a@example.test, b@example.test", "email"},
		{cName, "Imogen <" + cEmail + ">", "email"},
		{cName, "no-at-sign.example.test", "email"},
	} {
		resp, raw := e.do("POST", "/card", "", map[string]string{"id": card.ID, "name": x.name, "email": x.email})
		var b map[string]string
		json.Unmarshal(raw, &b)
		if resp.StatusCode != 400 || b["field"] != x.field {
			t.Errorf("(%q, %q) = %d %s, want 400 naming %s", x.name, x.email, resp.StatusCode, raw, x.field)
		}
	}
	if resp, raw := e.do("POST", "/card", "", `{"id":"`+card.ID+`","name":"x","email":"x@example.test","phone":"1"}`); resp.StatusCode != 400 {
		t.Errorf("an unknown field = %d %s", resp.StatusCode, raw)
	}
	if n := e.outboxRows(); n != 0 {
		t.Fatalf("a refused card wrote %d rows", n)
	}
	if resp, raw := e.do("POST", "/card", "", map[string]string{"id": card.ID, "name": cName, "email": cEmail}); resp.StatusCode != 200 {
		t.Fatalf("the valid send after the refusals = %d %s: the card was spent by a refusal", resp.StatusCode, raw)
	}
}

// ---- 🔴 the uniform answer ----

// The answer to Send is the same bytes whether contacts will take it,
// refuse it as not accepting, or is down: it never depends on contacts.
func TestCardAnswerIsUniformWhateverContactsDoes(t *testing.T) {
	e, f := newCardEnv(t)
	var cards []string
	var meetings []call
	for i, n := range []string{"Wren A.", "Wren B.", "Wren C."} {
		c := e.meeting(ada)
		_, s := e.inCall(c, n)
		e.end(c)
		cd := endedOf(t, s).Card
		if cd == nil {
			t.Fatalf("meeting %d: no card", i)
		}
		cards, meetings = append(cards, cd.ID), append(meetings, c)
	}
	var answers []string
	for i, mode := range []func(){
		func() { f.SetMode(contactstest.Up, 0, ""); f.SetAccepting(e.subject(ada), true) },
		func() { f.SetMode(contactstest.Up, 0, ""); f.SetAccepting(e.subject(ada), false) },
		func() { f.SetMode(contactstest.BlackHole, 0, "") },
	} {
		mode()
		// The cache learns the new state before the send.
		e.clock.Add(contacts.AcceptingTTL)
		e.server.Accepting().Refresh(e.subject(ada))
		e.server.Accepting().Wait()
		resp, raw := e.do("POST", "/card", "", map[string]string{"id": cards[i], "name": cName, "email": fmt.Sprintf("u%d.%s", i, cEmail)})
		answers = append(answers, fmt.Sprintf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), raw))
	}
	if answers[0] != answers[1] || answers[1] != answers[2] || !strings.HasPrefix(answers[0], "200 application/json {\"message\":\"Sent to Ada L.\"}") {
		t.Fatalf("the answers differ by what contacts does:\n%s", strings.Join(answers, "\n"))
	}
}

// ---- limits ----

func TestCardLimitPerAddress(t *testing.T) {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	_, s := e.inCall(c, cName)
	e.end(c)
	card := endedOf(t, s).Card
	bad := map[string]string{"id": card.ID, "name": cName, "email": "not an email"}
	for i := 0; i < server.CardPostsPerAddress; i++ {
		if resp, _ := e.do("POST", "/card", "", bad, "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 400 {
			t.Fatalf("post %d from one address = %d", i+1, resp.StatusCode)
		}
	}
	if resp, raw := e.do("POST", "/card", "", bad, "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 429 || !strings.Contains(string(raw), server.CardTooMany) {
		t.Fatalf("post %d from one address = %d %s, want 429", server.CardPostsPerAddress+1, resp.StatusCode, raw)
	}
	// Another address is not limited by the first one's posts.
	if resp, _ := e.do("POST", "/card", "", bad, "X-Forwarded-For", "198.51.100.10"); resp.StatusCode != 400 {
		t.Fatalf("a fresh address = %d, want 400 (its own bucket)", resp.StatusCode)
	}
}

// ---- 🔴 no PII in logs ----

func TestTheCardLogsNoPII(t *testing.T) {
	e, f := newCardEnv(t)
	c := e.meeting(ada)
	tk, _ := e.inCall(c, cName)
	_, raw := e.do("POST", "/j/"+c.Code+"/leave", "", map[string]string{"ticket": tk})
	var b struct{ Card struct{ ID string } }
	json.Unmarshal(raw, &b)
	e.do("POST", "/card", "", map[string]string{"id": b.Card.ID, "name": cName, "email": "bad email"})
	e.do("POST", "/card", "", map[string]string{"id": b.Card.ID, "name": cName, "email": cEmail})
	for _, st := range []int{400, 503} {
		f.SetMode(contactstest.Status, st, "")
		e.clock.Add(time.Hour)
		e.server.Outbox().Pass(context.Background())
	}
	out := e.log.String()
	if !strings.Contains(out, "http POST /card 200") || !strings.Contains(out, "contacts: outbox: submission") {
		t.Fatalf("the log does not show the card's requests or the outbox: is it the log? %q", out)
	}
	for _, s := range []string{cName, cEmail, "imogen", "vasquez", b.Card.ID, cToken} {
		if s != "" && strings.Contains(strings.ToLower(out), strings.ToLower(s)) {
			t.Errorf("the log holds %q", s)
		}
	}
	e.log.Write([]byte("control " + cEmail + "\n"))
	if !strings.Contains(e.log.String(), cEmail) {
		t.Fatal("the positive control is not seen")
	}
}

// ---- 🔴 contacts down: meetings unaffected ----

// With contacts black-holed (a listener that never answers), knock, admit,
// the guest's stream, End and the card's Send each finish well inside the
// 3 s call timeout: nothing on their path waits for contacts. A due outbox
// row against the black hole does not hold up End either.
func TestContactsDownLeavesTheCallAndEndAsFast(t *testing.T) {
	e, f := newCardEnv(t)
	c := e.meeting(ada)
	// A card offered while contacts answered, so Send is exercised too.
	_, s0 := e.inCall(c, grace)
	f.SetMode(contactstest.BlackHole, 0, "")
	if err := e.store.EnqueueOutbox(context.Background(), store.OutboxItem{Key: "card:heldRow000000000", Kind: "submission", Path: contacts.PathSubmissions, Body: []byte(`{}`)}, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.server.Outbox().Kick()
	const limit = time.Second
	e.clock.Add(contacts.AcceptingTTL) // every refresh from here goes to the black hole
	start := time.Now()
	tk := e.knock(c.Code, cName)
	if d := time.Since(start); d > limit {
		t.Errorf("knock took %s with contacts down", d)
	}
	start = time.Now()
	e.admit(c, cName)
	if d := time.Since(start); d > limit {
		t.Errorf("admit took %s with contacts down", d)
	}
	start = time.Now()
	s := e.guestEvents(c.Code, tk)
	s.want("token")
	if d := time.Since(start); d > limit {
		t.Errorf("the guest's stream took %s to start with contacts down", d)
	}
	if d := e.end(c); d > limit {
		t.Errorf("End took %s with contacts down", d)
	}
	endedOf(t, s)
	card := endedOf(t, s0).Card
	if card == nil {
		t.Fatal("no card from the cached answer")
	}
	start = time.Now()
	if resp, raw := e.do("POST", "/card", "", map[string]string{"id": card.ID, "name": grace, "email": cEmail}); resp.StatusCode != 200 {
		t.Fatalf("send = %d %s", resp.StatusCode, raw)
	}
	if d := time.Since(start); d > limit {
		t.Errorf("Send took %s with contacts down", d)
	}
	if n := e.outboxRows(); n != 2 {
		t.Errorf("%d rows waiting, want both kept for contacts' return", n)
	}
}

// ---- 🔴 no URL: the outbox is emptied on boot ----

func TestNoContactsURLEmptiesTheOutboxOnBoot(t *testing.T) {
	e := newEnv(t)
	// A row left by an earlier, configured boot of the same database.
	if err := e.store.EnqueueOutbox(context.Background(), store.OutboxItem{Key: "card:leftOver00000000", Kind: "submission", Path: contacts.PathSubmissions, Body: []byte(`{"email":"` + cEmail + `"}`)}, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if e.server.Outbox() != nil {
		t.Fatal("an outbox worker exists with no contacts URL")
	}
	e.server.RunContactsForTest(context.Background())
	if n := e.outboxRows(); n != 0 {
		t.Fatalf("a boot with no contacts URL kept %d outbox row(s)", n)
	}
	// The control: with contacts configured, the same boot step keeps them.
	ec, f := newCardEnv(t)
	f.SetMode(contactstest.Status, 503, "")
	if err := ec.store.EnqueueOutbox(context.Background(), store.OutboxItem{Key: "card:leftOver00000000", Kind: "submission", Path: contacts.PathSubmissions, Body: []byte(`{}`)}, ec.clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ec.server.RunContactsForTest(context.Background())
	if n := ec.outboxRows(); n != 1 {
		t.Fatalf("control: a configured boot left %d rows, want 1", n)
	}
}

// publicCardID is what meetings names a card to contacts by: the card's
// SHA-256, base64url (never the secret the guest's page holds).
func publicCardID(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// The card's fields are the contacts plugin's rules: every name and email
// of its fields.json is accepted or refused by meetings exactly as the
// table says, and sent as the table's cleaned name and address.
func TestCardFieldsFollowTheContractsFieldTable(t *testing.T) {
	f, err := contactsapi.LoadFields()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Names) < 5 || len(f.Emails) < 20 {
		t.Fatalf("the field table has %d names and %d emails", len(f.Names), len(f.Emails))
	}
	const okEmail, okName = "wren.okafor@example.test", "Wren Okafor"
	for _, n := range f.Names {
		got, _, field := server.CardFields(n.Input, okEmail)
		if n.OK != (field == "") || (n.OK && got != n.Clean) || (!n.OK && field != "name") {
			t.Errorf("name %q: got %q, refused %q; the contract says ok %v as %q", n.Input, got, field, n.OK, n.Clean)
		}
	}
	for _, m := range f.Emails {
		_, got, field := server.CardFields(okName, m.Input)
		if m.OK != (field == "") || (m.OK && got != m.Address) || (!m.OK && field != "email") {
			t.Errorf("email %q: got %q, refused %q; the contract says ok %v as %q", m.Input, got, field, m.OK, m.Address)
		}
	}
}
