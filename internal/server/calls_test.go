package server_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Guest names: pairwise distinct, and no word the code or the fake names.
const (
	grace = "Grace H."
	linus = "Linus T."
)

var bothOn = map[string]bool{"record": true, "ai": true, "transcribed": true}

// call is one owner's open meeting, as the tests drive it.
type call struct {
	ID, Code, Room, Sess, Subject string
}

// meeting opens the app for p and makes a New meeting.
func (e *env) meeting(p mustertest.Person) call {
	e.t.Helper()
	sess := e.enter(p)
	c := e.create(sess)
	code := c.JoinURL[strings.LastIndex(c.JoinURL, "/")+1:]
	return call{ID: c.ID, Code: code, Room: store.RoomName(c.ID), Sess: sess, Subject: e.subject(p)}
}

// knockFrom knocks on code as name from the address addr (X-Forwarded-For).
func (e *env) knockFrom(code, name string, notice map[string]bool, addr string) (*http.Response, map[string]any) {
	e.t.Helper()
	resp, raw := e.do("POST", "/j/"+code+"/knock", "", map[string]any{"name": name, "notice": notice}, "X-Forwarded-For", addr)
	var b map[string]any
	_ = json.Unmarshal(raw, &b)
	return resp, b
}

// knock knocks and returns the ticket.
func (e *env) knock(code, name string) string {
	e.t.Helper()
	resp, b := e.knockFrom(code, name, bothOn, "198.51.100.7")
	tk, _ := b["ticket"].(string)
	if resp.StatusCode != 201 || tk == "" {
		e.t.Fatalf("knock as %s = %d %v", name, resp.StatusCode, b)
	}
	return tk
}

// pid is the lobby row id of the guest called name.
func (e *env) pid(c call, name string) string {
	e.t.Helper()
	lob, err := e.store.ForSubject(c.Subject).Lobby(context.Background(), c.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range lob {
		if p.Name == name {
			return p.ID
		}
	}
	e.t.Fatalf("%s is not in the lobby of %s", name, c.ID)
	return ""
}

func (e *env) admit(c call, name string) {
	e.t.Helper()
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, name)+"/admit", c.Sess, nil); resp.StatusCode != 200 {
		e.t.Fatalf("admit %s = %d %s", name, resp.StatusCode, raw)
	}
}

// sseEvent is one event of a stream.
type sseEvent struct {
	Name string
	Data string
}

// stream is an open event stream.
type stream struct {
	t      *testing.T
	status int
	events chan sseEvent
	raw    *strings.Builder
	done   chan struct{}
	cancel func()
}

// open starts method path with bearer and body, and reads its events.
func (e *env) open(method, path, bearer string, body any, hdr ...string) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(ctx, method, e.srv.URL+path, rd)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	s := &stream{t: e.t, status: resp.StatusCode, events: make(chan sseEvent, 64), raw: &strings.Builder{}, done: make(chan struct{}), cancel: cancel}
	e.t.Cleanup(cancel)
	go func() {
		defer close(s.done)
		defer close(s.events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			s.raw.WriteString(line + "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.Data = line[6:]
			case line == "":
				if ev.Name != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	return s
}

// next is the next event within d, or ok false (the stream ended, or none
// came).
func (s *stream) next(d time.Duration) (sseEvent, bool) {
	select {
	case ev, ok := <-s.events:
		return ev, ok
	case <-time.After(d):
		return sseEvent{}, false
	}
}

// want reads events until one named name, failing on timeout.
func (s *stream) want(name string) sseEvent {
	s.t.Helper()
	for {
		ev, ok := s.next(5 * time.Second)
		if !ok {
			s.t.Fatalf("no %q event (the stream so far: %q)", name, s.text())
		}
		if ev.Name == name {
			return ev
		}
	}
}

// text is everything the stream sent, once it has ended (or after a short
// wait).
func (s *stream) text() string {
	select {
	case <-s.done:
	case <-time.After(200 * time.Millisecond):
	}
	return s.raw.String()
}

func (e *env) wait(code, ticket string) *stream {
	return e.open("POST", "/j/"+code+"/wait", "", map[string]string{"ticket": ticket})
}

func (e *env) guestEvents(code, ticket string) *stream {
	return e.open("POST", "/j/"+code+"/events", "", map[string]string{"ticket": ticket})
}

// admitted waits for the admitted event and returns its token answer.
func admittedToken(t *testing.T, s *stream) map[string]string {
	t.Helper()
	ev := s.want("admitted")
	var a map[string]string
	if err := json.Unmarshal([]byte(ev.Data), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// jwtShape finds anything shaped like a JWT in a text.
var jwtShape = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ`)

// ---- 🔴 no token before admission; the grants ----

// "No token before admission" and "Grants, exactly", end to end: a knock
// answers only a ticket; a waiting ticket's stream carries no token; the
// admit is what makes the token, and it is exactly the guest grant for THIS
// meeting's room, for this guest's identity and name, for 2 minutes.
func TestNoGuestTokenBeforeAdmissionAndItsGrantIsExact(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	resp, raw := e.do("POST", "/j/"+c.Code+"/knock", "", map[string]any{"name": "  " + grace + "  ", "notice": bothOn}, "X-Forwarded-For", "198.51.100.7")
	var kb map[string]any
	json.Unmarshal(raw, &kb)
	if resp.StatusCode != 201 || len(kb) != 1 || kb["ticket"] == nil {
		t.Fatalf("knock = %d %s: want 201 with the ticket and nothing else", resp.StatusCode, raw)
	}
	ticket := kb["ticket"].(string)
	w := e.wait(c.Code, ticket)
	first := w.want("waiting")
	if !strings.Contains(first.Data, "This meeting is recorded and transcribed.") {
		t.Errorf("waiting event = %s: want the current notice", first.Data)
	}
	if ev, ok := w.next(300 * time.Millisecond); ok {
		t.Fatalf("a waiting ticket's stream sent %q %s before any admit", ev.Name, ev.Data)
	}
	if jwtShape.MatchString(w.raw.String()) || len(e.lk.RTCs()) != 0 {
		t.Fatalf("a waiting ticket's stream carries a token: %q", w.raw.String())
	}
	e.admit(c, grace)
	a := admittedToken(t, w)
	if a["livekit_url"] != e.lk.WSURL() || a["ice"] != "relay" || !strings.HasPrefix(a["identity"], "guest-") {
		t.Errorf("admitted = %v", a)
	}
	claims, err := livekittest.Keys.Verify(a["token"], e.clock.Now())
	if err != nil {
		t.Fatalf("the admitted token does not verify: %v", err)
	}
	got, _ := json.Marshal(claims.Video)
	want, _ := json.Marshal(livekit.GuestGrant(c.Room))
	if string(got) != string(want) {
		t.Errorf("the guest's grant =\n%s\nwant\n%s", got, want)
	}
	if claims.Subject != a["identity"] || claims.Name != grace+" (guest)" || claims.Expires-claims.NotBefore != 120 {
		t.Errorf("claims: sub %q name %q life %d s", claims.Subject, claims.Name, claims.Expires-claims.NotBefore)
	}
	// The token joins the room (LiveKit's own check, in the fake).
	if id, room, err := e.lk.Join(a["token"]); err != nil || id != a["identity"] || room != c.Room {
		t.Errorf("joining with the token: %s %s %v", id, room, err)
	}
}

// The owner's token: the owner grant, the "owner" identity, the person's
// display name; 410 once ended; another person's meeting is 404; LiveKit
// down is 503.
func TestTheOwnersTokenIsTheOwnerGrant(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil)
	var a map[string]string
	json.Unmarshal(raw, &a)
	if resp.StatusCode != 200 || a["identity"] != store.OwnerIdentity {
		t.Fatalf("token = %d %s", resp.StatusCode, raw)
	}
	claims, err := livekittest.Keys.Verify(a["token"], e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(claims.Video)
	want, _ := json.Marshal(livekit.OwnerGrant(c.Room))
	if string(got) != string(want) || claims.Name != ada.DisplayName || claims.Subject != store.OwnerIdentity {
		t.Errorf("owner claims: %s, name %q, sub %q", got, claims.Name, claims.Subject)
	}
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/token", e.enter(bea), nil); resp.StatusCode != 404 {
		t.Errorf("Bea asking for a token to Ada's meeting = %d, want 404", resp.StatusCode)
	}
	e.lk.SetDown(true)
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 503 || !strings.Contains(string(raw), "Calls are unavailable") {
		t.Errorf("token with LiveKit down = %d %s", resp.StatusCode, raw)
	}
	e.lk.SetDown(false)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 410 {
		t.Errorf("a token for an ended meeting = %d, want 410", resp.StatusCode)
	}
}

// New meeting makes the room first: with LiveKit down it says "Calls are
// unavailable" and stores no meeting.
func TestNewMeetingMakesTheRoomOrNothing(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	e.lk.SetDown(true)
	resp, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
	if resp.StatusCode != 503 || !strings.Contains(string(raw), "Calls are unavailable") {
		t.Fatalf("New meeting with LiveKit down = %d %s", resp.StatusCode, raw)
	}
	if list, _, _ := e.store.ForSubject(e.subject(ada)).ListMeetings(context.Background(), nil, 10); len(list) != 0 {
		t.Fatalf("a failed New meeting stored %d meetings", len(list))
	}
	e.lk.SetDown(false)
	c := e.create(sess)
	if rm, ok := e.lk.Room(store.RoomName(c.ID)); !ok || rm.EmptyTimeout != 600 || rm.MaxParticipants != 8 {
		t.Fatalf("the room = %+v, %v", rm, ok)
	}
}

// ---- 🔴 a ticket opens only its own meeting ----

func TestATicketCannotWaitOnOrGetATokenForAnotherMeeting(t *testing.T) {
	e := newEnv(t)
	a := e.meeting(ada)
	b := e.meeting(bea)
	ticket := e.knock(a.Code, grace)
	e.knock(b.Code, linus)
	for _, path := range []string{"/wait", "/events"} {
		if resp, raw := e.do("POST", "/j/"+b.Code+path, "", map[string]string{"ticket": ticket}); resp.StatusCode != 404 {
			t.Errorf("A's ticket on B's link %s = %d %s, want 404", path, resp.StatusCode, raw)
		}
	}
	e.admit(a, grace)
	// Admitted on A: still nothing on B, and A's own wait gives A's room.
	if resp, raw := e.do("POST", "/j/"+b.Code+"/wait", "", map[string]string{"ticket": ticket}); resp.StatusCode != 404 || jwtShape.Match(raw) {
		t.Errorf("A's admitted ticket on B's link /wait = %d %s, want 404 and no token", resp.StatusCode, raw)
	}
	tok := admittedToken(t, e.wait(a.Code, ticket))
	claims, _ := livekittest.Keys.Verify(tok["token"], e.clock.Now())
	if claims.Video == nil || claims.Video.Room != a.Room {
		t.Errorf("the token's room = %+v, want A's %s", claims.Video, a.Room)
	}
}

// ---- 🔴 a guest reaches no owner route ----

// "a guest calling the owner's /api/meetings/{id}/token with their ticket
// gets 401", for every owner route, waiting and admitted, with the ticket as
// the bearer and in the body; and nothing changed.
func TestAGuestReachesNoOwnerRoute(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	waiting := e.knock(c.Code, grace)
	admitted := e.knock(c.Code, linus)
	e.admit(c, linus)
	pid := e.pid(c, grace)
	routes := [][2]string{
		{"GET", "/api/meetings"}, {"POST", "/api/meetings"}, {"GET", "/api/meetings/" + c.ID},
		{"POST", "/api/meetings/" + c.ID + "/token"}, {"POST", "/api/meetings/" + c.ID + "/record"}, {"POST", "/api/meetings/" + c.ID + "/ai"},
		{"GET", "/api/meetings/" + c.ID + "/events"}, {"POST", "/api/meetings/" + c.ID + "/lobby/" + pid + "/admit"},
		{"POST", "/api/meetings/" + c.ID + "/lobby/" + pid + "/deny"}, {"DELETE", "/api/meetings/" + c.ID + "/participants/" + pid},
		{"POST", "/api/meetings/" + c.ID + "/end"},
	}
	for _, tk := range []string{waiting, admitted} {
		for _, r := range routes {
			body := map[string]any{"ticket": tk, "on": true}
			if resp, raw := e.do(r[0], r[1], tk, body); resp.StatusCode != 401 {
				t.Errorf("%s %s with a guest's ticket as the bearer = %d %s, want 401", r[0], r[1], resp.StatusCode, raw)
			}
			if resp, _ := e.do(r[0], r[1], "", body); resp.StatusCode != 401 {
				t.Errorf("%s %s with a guest's ticket in the body = %d, want 401", r[0], r[1], resp.StatusCode)
			}
		}
	}
	if e.meetingState(c.Subject, c.ID) != "open" || e.pid(c, grace) != pid {
		t.Fatal("a guest's calls changed the meeting or the lobby")
	}
}

// ---- 🔴 a denied guest gets no token ----

func TestADeniedGuestGetsNoToken(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	ticket := e.knock(c.Code, grace)
	w := e.wait(c.Code, ticket)
	w.want("waiting")
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, grace)+"/deny", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("deny = %d %s", resp.StatusCode, raw)
	}
	if ev := w.want("denied"); !strings.Contains(ev.Data, "You weren't admitted.") {
		t.Errorf("denied = %s", ev.Data)
	}
	if txt := w.text(); jwtShape.MatchString(txt) || strings.Contains(txt, "event: admitted") {
		t.Fatalf("the denied guest's stream: %q", txt)
	}
	// Asking again changes nothing: denied at once, no token.
	again := e.wait(c.Code, ticket)
	if ev := again.want("denied"); jwtShape.MatchString(again.text()) {
		t.Fatalf("a denied ticket waiting again: %s / %q", ev.Name, again.text())
	}
	if resp, _ := e.do("POST", "/j/"+c.Code+"/events", "", map[string]string{"ticket": ticket}); resp.StatusCode != 403 {
		t.Errorf("a denied ticket's event stream = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pidAny(c, grace)+"/admit", c.Sess, nil); resp.StatusCode != 409 {
		t.Errorf("admitting a denied guest = %d, want 409", resp.StatusCode)
	}
}

// openDB opens the plugin's database a second time, for a test that must
// see a row no route shows.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sha256sum(b []byte) [32]byte { return sha256.Sum256(b) }

// pidAny is a guest's row id in any state.
func (e *env) pidAny(c call, name string) string {
	e.t.Helper()
	db := openDB(e.t, e.dbPath)
	var id string
	if err := db.QueryRow(`SELECT id FROM participants WHERE meeting_id = ? AND display_name = ?`, c.ID, name).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// ---- 🔴 removal: revoked, kicked, and kept out ----

// "Removed guest cannot rejoin (Y2)": removal marks the identity removed,
// calls RemoveParticipant, and kills the ticket; a rejoin with the token the
// guest still holds is removed at once on participant_joined, while an
// admitted guest's join is left alone (the positive control).
func TestARemovedGuestIsRevokedKickedAndKeptOut(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	gt := e.knock(c.Code, grace)
	lt := e.knock(c.Code, linus)
	e.admit(c, grace)
	e.admit(c, linus)
	gtok, ltok := admittedToken(t, e.wait(c.Code, gt)), admittedToken(t, e.wait(c.Code, lt))
	gid, lid := gtok["identity"], ltok["identity"]
	for _, tk := range []string{gtok["token"], ltok["token"]} {
		if _, _, err := e.lk.Join(tk); err != nil {
			t.Fatal(err)
		}
	}
	gev := e.guestEvents(c.Code, gt)
	gev.want("meeting")
	resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/participants/"+e.pid(c, grace), c.Sess, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"disconnected":true`) {
		t.Fatalf("remove = %d %s", resp.StatusCode, raw)
	}
	if in := e.lk.InRoom(c.Room); strings.Contains(strings.Join(in, ","), gid) || !strings.Contains(strings.Join(in, ","), lid) {
		t.Fatalf("after the removal the room holds %v: want Linus and not Grace", in)
	}
	gev.want("removed")
	for _, path := range []string{"/wait", "/events"} {
		if resp, raw := e.do("POST", "/j/"+c.Code+path, "", map[string]string{"ticket": gt}); resp.StatusCode != 403 || jwtShape.Match(raw) {
			t.Errorf("the removed ticket's %s = %d %s, want 403", path, resp.StatusCode, raw)
		}
	}
	// The guest reconnects with the token they still hold: LiveKit lets them
	// in (it is valid for two minutes), and the webhook throws them out.
	if _, _, err := e.lk.Join(gtok["token"]); err != nil {
		t.Fatal(err)
	}
	if st, err := e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, gid); err != nil || st != 200 {
		t.Fatalf("webhook = %d %v", st, err)
	}
	if strings.Contains(strings.Join(e.lk.InRoom(c.Room), ","), gid) {
		t.Fatalf("the removed guest is back in the LiveKit room after rejoining with the old token: %v", e.lk.InRoom(c.Room))
	}
	before := e.lk.CountCalls("RemoveParticipant " + c.Room + " " + lid)
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, lid)
	if e.lk.CountCalls("RemoveParticipant "+c.Room+" "+lid) != before || !strings.Contains(strings.Join(e.lk.InRoom(c.Room), ","), lid) {
		t.Fatal("an admitted guest's join was removed (the positive control)")
	}
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, store.OwnerIdentity)
	if e.lk.CountCalls("RemoveParticipant "+c.Room+" "+store.OwnerIdentity) != 0 {
		t.Fatal("the owner's join was removed")
	}
}

// 🔴 A dropped connection is not a revocation (design: "an admitted control
// guest who reconnects is not" disconnected): after participant_left the
// guest is still admitted, their /events stream gives a fresh token, and a
// rejoin with it is left alone.
func TestAnAdmittedGuestWhoDropsReconnectsWithoutKnocking(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	gt := e.knock(c.Code, grace)
	e.admit(c, grace)
	tok := admittedToken(t, e.wait(c.Code, gt))
	e.lk.Join(tok["token"])
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_left", c.Room, tok["identity"])
	ev := e.guestEvents(c.Code, gt)
	tev := ev.want("token")
	var fresh map[string]string
	json.Unmarshal([]byte(tev.Data), &fresh)
	if claims, err := livekittest.Keys.Verify(fresh["token"], e.clock.Now()); err != nil || claims.Subject != tok["identity"] {
		t.Fatalf("the reconnect token from /events: %v, %+v; want a valid token for the same identity", err, claims)
	}
	if _, _, err := e.lk.Join(fresh["token"]); err != nil {
		t.Fatal(err)
	}
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, tok["identity"])
	if !slices.Contains(e.lk.InRoom(c.Room), tok["identity"]) {
		t.Fatalf("an admitted guest who dropped and reconnected was removed: the room holds %v", e.lk.InRoom(c.Room))
	}
	e.server.Reconcile(context.Background())
	if !slices.Contains(e.lk.InRoom(c.Room), tok["identity"]) {
		t.Fatal("the reconcile loop removed the reconnected guest")
	}
}

// The guest's own Leave spends the ticket: they are dropped from the room,
// their streams are refused, and their old token is thrown out on a rejoin.
func TestAGuestWhoLeavesMustKnockAgain(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	gt := e.knock(c.Code, grace)
	e.admit(c, grace)
	tok := admittedToken(t, e.wait(c.Code, gt))
	e.lk.Join(tok["token"])
	if resp, raw := e.do("POST", "/j/"+c.Code+"/leave", "", map[string]string{"ticket": gt}); resp.StatusCode != 204 {
		t.Fatalf("leave = %d %s", resp.StatusCode, raw)
	}
	if slices.Contains(e.lk.InRoom(c.Room), tok["identity"]) {
		t.Fatal("Leave did not drop the guest from the room")
	}
	for _, p := range []string{"/wait", "/events"} {
		if resp, _ := e.do("POST", "/j/"+c.Code+p, "", map[string]string{"ticket": gt}); resp.StatusCode != 403 {
			t.Errorf("a left ticket's %s = %d, want 403", p, resp.StatusCode)
		}
	}
	e.lk.Join(tok["token"])
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, tok["identity"])
	if len(e.lk.InRoom(c.Room)) != 0 {
		t.Errorf("a guest who left is back with their old token: %v", e.lk.InRoom(c.Room))
	}
}

// ---- 🔴 the event-stream caps ----

// One stream per ticket: a second /wait for the same ticket closes the
// first, so a ticket can never hold more than one slot.
func TestOneStreamPerTicket(t *testing.T) {
	e := newEnv(t)
	e.server.SetStreamCapsForTest(2, 100)
	c := e.meeting(ada)
	ticket := e.knock(c.Code, grace)
	first := e.wait(c.Code, ticket)
	first.want("waiting")
	var last *stream
	for i := 0; i < 5; i++ { // five more, under a cap of two per address
		last = e.wait(c.Code, ticket)
		if last.status != 200 {
			t.Fatalf("stream %d of one ticket = %d: a replaced stream still holds its slot", i+2, last.status)
		}
		last.want("waiting")
	}
	select {
	case <-first.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the first stream of the ticket stayed open after a newer one replaced it")
	}
}

// Per client address, across the guests' and the owner's streams: past the
// cap 429 with Retry-After; another address is not affected; a closed
// stream frees its place.
func TestStreamsPerAddressAreCapped(t *testing.T) {
	e := newEnv(t)
	e.server.SetStreamCapsForTest(3, 100)
	c := e.meeting(ada)
	var open []*stream
	for i := 0; i < 3; i++ {
		tk := e.knock(c.Code, fmt.Sprintf("Guest %d", i))
		s := e.open("POST", "/j/"+c.Code+"/wait", "", map[string]string{"ticket": tk}, "X-Forwarded-For", "198.51.100.40")
		if s.status != 200 {
			t.Fatalf("stream %d from one address = %d", i+1, s.status)
		}
		open = append(open, s)
	}
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil, "X-Forwarded-For", "198.51.100.40")
	if owner.status != 429 {
		t.Fatalf("the 4th stream from one address (the owner's) = %d, want 429", owner.status)
	}
	if other := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil, "X-Forwarded-For", "203.0.113.41"); other.status != 200 {
		t.Fatalf("a stream from another address = %d, want 200", other.status)
	}
	open[0].cancel()
	<-open[0].done
	eventually(t, 5*time.Second, "a closed stream frees its place", func() bool {
		s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil, "X-Forwarded-For", "198.51.100.40")
		s.cancel()
		return s.status == 200
	})
}

// In all: past MaxStreams every address gets 429.
func TestStreamsAreCappedInAll(t *testing.T) {
	e := newEnv(t)
	e.server.SetStreamCapsForTest(100, 3)
	c := e.meeting(ada)
	for i := 0; i < 3; i++ {
		if s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil, "X-Forwarded-For", fmt.Sprintf("198.51.100.%d", 50+i)); s.status != 200 {
			t.Fatalf("stream %d = %d", i+1, s.status)
		}
	}
	if s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil, "X-Forwarded-For", "203.0.113.60"); s.status != 429 {
		t.Fatalf("the 4th stream in all, from a fresh address = %d, want 429", s.status)
	}
}

// ---- 🔴 X-Forwarded-For with no trusted proxy ----

// A proxy nobody declared: warned once, loudly, and /healthz says so; the
// header is not believed.
func TestXForwardedForWithoutTrustedProxiesIsWarnedOnce(t *testing.T) {
	e := newEnv(t)
	e.server.SetTrustedProxiesForTest(nil)
	health := func() string {
		resp, err := http.Get(e.internal.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	c := e.meeting(ada)
	e.do("POST", "/j/"+c.Code+"/knock", "", map[string]any{"name": grace, "notice": bothOn}) // no header: no warning (the control)
	if strings.Contains(e.log.String(), "WARNING") || strings.Contains(health(), "warning") {
		t.Fatal("a warning with no X-Forwarded-For seen")
	}
	for i := 0; i < 3; i++ {
		e.knockFrom(c.Code, fmt.Sprintf("Guest %d", i), bothOn, "198.51.100.70")
	}
	if n := strings.Count(e.log.String(), "WARNING: a request carried X-Forwarded-For but MEETINGS_TRUSTED_PROXIES is unset"); n != 1 {
		t.Fatalf("the warning was logged %d times, want once", n)
	}
	if h := health(); !strings.Contains(h, "ok\nwarning: a request carried X-Forwarded-For") {
		t.Fatalf("/healthz = %q: want ok and the warning", h)
	}
	// Not believed: all of them count against the peer, so the limit binds
	// at 10 whatever the header says.
	for i := 0; i < 10; i++ {
		e.knockFrom(c.Code, fmt.Sprintf("Spread %d", i), bothOn, fmt.Sprintf("203.0.113.%d", 100+i))
	}
	if resp, _ := e.knockFrom(c.Code, "Spread 11", bothOn, "203.0.113.200"); resp.StatusCode != 429 {
		t.Fatalf("with no trusted proxy, a forged X-Forwarded-For got past the per-address limit: %d", resp.StatusCode)
	}
}

// ---- 🔴 MEETINGS_CLIENT_IP_HEADER ----

// knockVia knocks on code as name with the header pairs hdr.
func (e *env) knockVia(code, name string, hdr ...string) int {
	e.t.Helper()
	resp, _ := e.do("POST", "/j/"+code+"/knock", "", map[string]any{"name": name, "notice": bothOn}, hdr...)
	return resp.StatusCode
}

// The client-address header (e.g. CF-Connecting-IP) is the knock limit's
// address only when the peer is a trusted proxy; then X-Forwarded-For is
// not read. From any other peer a spoofed header is ignored, and every
// knock counts against the peer.
func TestTheClientIPHeaderIsBelievedOnlyFromATrustedPeer(t *testing.T) {
	e := newEnv(t) // the test server's peer, loopback, is a trusted proxy
	e.server.SetClientIPHeaderForTest("CF-Connecting-IP")

	// One header address behind many X-Forwarded-For addresses: the limit binds.
	one := e.meeting(ada)
	for i := 1; i <= 10; i++ {
		if st := e.knockVia(one.Code, fmt.Sprintf("Header %d", i), "CF-Connecting-IP", "203.0.113.9", "X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i)); st != 201 {
			t.Fatalf("knock %d from one CF-Connecting-IP = %d", i, st)
		}
	}
	if st := e.knockVia(one.Code, "Header 11", "CF-Connecting-IP", "203.0.113.9", "X-Forwarded-For", "192.0.2.111"); st != 429 {
		t.Fatalf("trusted peer: the 11th knock from one CF-Connecting-IP = %d, want 429 (X-Forwarded-For was read)", st)
	}

	// Many header addresses behind one X-Forwarded-For address: none binds.
	many := e.meeting(bea)
	for i := 1; i <= 11; i++ {
		if st := e.knockVia(many.Code, fmt.Sprintf("Spread %d", i), "CF-Connecting-IP", fmt.Sprintf("198.51.100.%d", 20+i), "X-Forwarded-For", "192.0.2.200"); st != 201 {
			t.Fatalf("trusted peer: knock %d with its own CF-Connecting-IP and a shared X-Forwarded-For = %d, want 201", i, st)
		}
	}

	// An untrusted peer: the header is a spoof, ignored; the peer is counted.
	e.server.SetTrustedProxiesForTest([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	spoof := e.meeting(ada)
	for i := 1; i <= 10; i++ {
		e.knockVia(spoof.Code, fmt.Sprintf("Spoof %d", i), "CF-Connecting-IP", fmt.Sprintf("203.0.113.%d", 100+i))
	}
	if st := e.knockVia(spoof.Code, "Spoof 11", "CF-Connecting-IP", "203.0.113.211"); st != 429 {
		t.Fatalf("an untrusted peer's CF-Connecting-IP was believed: the 11th knock from one peer = %d, want 429", st)
	}
}

// 🔴 "2 → 1 → 2 pushes again": the last pushed count follows the count
// down, so a rise after a fall is a rise.
func TestAKnockCountThatFallsAndRisesIsPushedAgain(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	pushes := func() []mustertest.Push {
		e.server.SettleKnocksForTest(t)
		return e.fake.Pushes()
	}
	e.knock(c.Code, grace)
	if p := pushes(); len(p) != 1 {
		t.Fatalf("after the first knock: %+v", p)
	}
	e.clock.Add(31 * time.Second)
	e.knockFrom(c.Code, linus, bothOn, "203.0.113.2")
	if p := pushes(); len(p) != 2 || p[1].Count != 2 {
		t.Fatalf("after two knocks: %+v", p)
	}
	e.clock.Add(31 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, linus)+"/deny", c.Sess, nil)
	if p := pushes(); len(p) != 2 {
		t.Fatalf("a fall pushed: %+v", p)
	}
	e.clock.Add(31 * time.Second)
	e.knockFrom(c.Code, "Ines W.", bothOn, "203.0.113.3")
	if p := pushes(); len(p) != 3 || p[2].Kind != "knock" || p[2].Count != 2 {
		t.Fatalf("2 → 1 → 2 did not push again: %+v", p)
	}
}

// ---- 🔴 the webhook ----

// "Webhook: a valid signature passes; a tampered body, a wrong secret and no
// header are 401", and a refused webhook acts on nothing: an identity that
// is not admitted stays until a VALID webhook names it.
func TestTheWebhookActsOnlyOnAVerifiedRequest(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	intruder := "guest-Intruder00000000000"
	e.lk.Put(c.Room, intruder)
	url := e.internal.URL + "/livekit/webhook"
	body := []byte(fmt.Sprintf(`{"event":"participant_joined","room":{"name":%q},"participant":{"identity":%q}}`, c.Room, intruder))
	good, _ := livekittest.Keys.SignWebhook(body, e.clock.Now())
	other, _ := livekit.Keys{Key: livekittest.Keys.Key, Secret: "another-secret-entirely-0000000000"}.SignWebhook(body, e.clock.Now())
	tampered := []byte(strings.Replace(string(body), "participant_joined", "room_finished___", 1))
	for name, req := range map[string][2]string{
		"no header":       {string(body), ""},
		"a wrong secret":  {string(body), other},
		"a tampered body": {string(tampered), good},
	} {
		st, err := livekittest.PostWebhook(url, []byte(req[0]), req[1])
		if err != nil || st != 401 {
			t.Errorf("%s: %d %v, want 401", name, st, err)
		}
	}
	if !strings.Contains(strings.Join(e.lk.InRoom(c.Room), ","), intruder) || e.lk.CountCalls("RemoveParticipant") != 0 {
		t.Fatal("a refused webhook was acted on")
	}
	if e.meetingState(c.Subject, c.ID) != "open" {
		t.Fatal("a refused webhook ended the meeting")
	}
	if st, _ := livekittest.PostWebhook(url, body, good); st != 200 {
		t.Fatalf("the valid webhook = %d", st)
	}
	if strings.Contains(strings.Join(e.lk.InRoom(c.Room), ","), intruder) {
		t.Fatal("the valid webhook did not remove the identity that is not admitted (the positive control)")
	}
}

// room_finished (the room sat empty for 10 minutes) ends the meeting and
// kills the link.
func TestRoomFinishedEndsTheMeeting(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	if st, _ := e.lk.Webhook(e.internal.URL+"/livekit/webhook", "room_finished", c.Room, ""); st != 200 {
		t.Fatal(st)
	}
	if e.meetingState(c.Subject, c.ID) != "ended" {
		t.Fatal("room_finished left the meeting open")
	}
	if resp, _ := e.do("GET", "/j/"+c.Code, "", nil); resp.StatusCode != 410 {
		t.Errorf("the link after room_finished = %d, want 410", resp.StatusCode)
	}
}

// ---- 🔴 ending ----

// "ending a meeting kills the link (410), deletes the room and ends every
// token": the link, a knock and a wait are 410; a waiting guest's stream and
// an admitted guest's stream both say ended; the room is deleted; a client
// that re-creates the room with a token it still holds is thrown out and the
// room deleted again; the owner gets no new token.
func TestEndingAMeetingKillsTheLinkDeletesTheRoomAndEndsEveryToken(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	wt := e.knock(c.Code, grace)
	at := e.knock(c.Code, linus)
	e.admit(c, linus)
	tok := admittedToken(t, e.wait(c.Code, at))
	e.lk.Join(tok["token"])
	ws := e.wait(c.Code, wt)
	ws.want("waiting")
	gs := e.guestEvents(c.Code, at)
	gs.want("meeting")
	_, otRaw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil)
	var ot map[string]string
	json.Unmarshal(otRaw, &ot)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	ws.want("ended")
	gs.want("ended")
	if _, ok := e.lk.Room(c.Room); ok {
		t.Fatal("the room survived End meeting")
	}
	if resp, _ := e.do("GET", "/j/"+c.Code, "", nil); resp.StatusCode != 410 {
		t.Errorf("the link after End = %d, want 410", resp.StatusCode)
	}
	if resp, _ := e.knockFrom(c.Code, "Ada", bothOn, "203.0.113.4"); resp.StatusCode != 410 {
		t.Errorf("a knock after End = %d, want 410", resp.StatusCode)
	}
	for _, tk := range []string{wt, at} {
		if resp, _ := e.do("POST", "/j/"+c.Code+"/wait", "", map[string]string{"ticket": tk}); resp.StatusCode != 410 {
			t.Errorf("a wait after End = %d, want 410", resp.StatusCode)
		}
	}
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 410 {
		t.Errorf("the owner's token after End = %d, want 410", resp.StatusCode)
	}
	// The admitted guest's token is still within its two minutes: LiveKit
	// (auto-create) lets it make the room again; the plugin throws it out.
	if _, _, err := e.lk.Join(tok["token"]); err != nil {
		t.Fatal(err)
	}
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, tok["identity"])
	if _, ok := e.lk.Room(c.Room); ok {
		t.Fatalf("a guest's held token re-created the ended meeting's room and it survived: %v", e.lk.InRoom(c.Room))
	}
	// The owner's held token too: nobody may be in an ended meeting's room.
	if _, _, err := e.lk.Join(ot["token"]); err != nil {
		t.Fatal(err)
	}
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, store.OwnerIdentity)
	if _, ok := e.lk.Room(c.Room); ok {
		t.Fatalf("the owner's held token re-created the ended meeting's room and it survived: %v", e.lk.InRoom(c.Room))
	}
}

// Both pages' policies, pinned whole (design: "Every page sets its own
// strict CSP"): their own script, style and images; their own API and
// LiveKit's signalling origin, wss and https, and nothing else; framed by
// muster's origin (owner) or by nobody (guest).
func TestThePagesPoliciesArePinned(t *testing.T) {
	e := newEnv(t)
	host := strings.TrimPrefix(e.lk.WSURL(), "ws://")
	rest := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self' ws://" + host + " http://" + host +
		"; media-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors "
	resp, _ := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
	if got := resp.Header.Get("Content-Security-Policy"); got != rest+musterOrigin {
		t.Errorf("the owner page's CSP =\n%s\nwant\n%s", got, rest+musterOrigin)
	}
	c := e.meeting(bea)
	resp, _ = e.do("GET", "/j/"+c.Code, "", nil)
	if got := resp.Header.Get("Content-Security-Policy"); got != rest+"'none'" {
		t.Errorf("the guest page's CSP =\n%s\nwant\n%s", got, rest+"'none'")
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("the guest page's X-Frame-Options = %q", got)
	}
	if got := resp.Header.Get("Permissions-Policy"); got != "camera=(self), microphone=(self), display-capture=(self)" {
		t.Errorf("the guest page's Permissions-Policy = %q", got)
	}
	if got := server.RTCOrigins("wss://meet-rtc.example.test"); got != "wss://meet-rtc.example.test https://meet-rtc.example.test" {
		t.Errorf("RTCOrigins(wss) = %q", got)
	}
}

// After 24 hours the link is dead even if nobody ended the meeting.
func TestTheLinkIs410After24Hours(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	ticket := e.knock(c.Code, grace)
	e.clock.Add(store.LinkLifetime)
	if resp, _ := e.do("GET", "/j/"+c.Code, "", nil); resp.StatusCode != 410 {
		t.Errorf("the page at 24 h = %d", resp.StatusCode)
	}
	if resp, _ := e.knockFrom(c.Code, linus, bothOn, "203.0.113.4"); resp.StatusCode != 410 {
		t.Errorf("a knock at 24 h = %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/j/"+c.Code+"/wait", "", map[string]string{"ticket": ticket}); resp.StatusCode != 410 {
		t.Errorf("a wait at 24 h = %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/j/AAAAAAAAAAAAAAAAAAAAAA", "", nil); resp.StatusCode != 404 {
		t.Errorf("an unknown code = %d, want 404 (not 410)", resp.StatusCode)
	}
}

// ---- 🔴 the consent notice ----

// "Notice at knock": the guest page shows the notice for the switches as
// they are; a knock carrying another one is 409 with the current one, and
// the current one is taken.
func TestAKnockWithAStaleNoticeIs409(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	_, page := e.do("GET", "/j/"+c.Code, "", nil)
	for _, want := range []string{"This meeting is recorded and transcribed.", "sent to an AI service (OpenRouter)", `data-record="true"`, `data-ai="true"`} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("the guest page lacks %q", want)
		}
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("record off = %d %s", resp.StatusCode, raw)
	}
	resp, b := e.knockFrom(c.Code, grace, bothOn, "198.51.100.7")
	n, _ := b["notice"].(map[string]any)
	if resp.StatusCode != 409 || n == nil || n["record"] != false || n["ai"] != true || !strings.Contains(fmt.Sprint(n["text"]), "This meeting is transcribed.") {
		t.Fatalf("a knock with the stale notice = %d %v: want 409 naming the current one", resp.StatusCode, b)
	}
	if resp, b := e.knockFrom(c.Code, grace, map[string]bool{"record": false, "ai": true, "transcribed": true}, "198.51.100.7"); resp.StatusCode != 201 {
		t.Fatalf("a knock with the current notice = %d %v", resp.StatusCode, b)
	}
	// And now it can only stay off.
	if resp, _ := e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": true}); resp.StatusCode != 409 {
		t.Errorf("record back on after a knock = %d, want 409", resp.StatusCode)
	}
	if resp, raw := e.do("POST", "/j/"+c.Code+"/knock", "", map[string]any{"name": linus}); resp.StatusCode != 400 {
		t.Errorf("a knock with no notice = %d %s, want 400", resp.StatusCode, raw)
	}
}

// "Notice is current": the admitted guest's stream says what is on NOW: a
// switch turned off after they knocked, and after their stream opened.
func TestTheAdmittedGuestSeesTheCurrentNotice(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	ticket := e.knock(c.Code, grace)
	e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": false})
	e.admit(c, grace)
	s := e.guestEvents(c.Code, ticket)
	if ev := s.want("meeting"); !strings.Contains(ev.Data, `"badge":"Transcribing · AI"`) {
		t.Fatalf("the admitted guest's notice = %s, want the current one (no Recording)", ev.Data)
	}
	e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": false})
	if ev := s.want("meeting"); !strings.Contains(ev.Data, `"badge":"Transcribing"`) {
		t.Fatalf("after AI off the admitted guest's notice = %s", ev.Data)
	}
}

// ---- 🔴 knock limits ----

func TestKnockLimits(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	// Per address: the 10th passes, the 11th in 10 minutes is 429.
	for i := 1; i <= 10; i++ {
		if resp, b := e.knockFrom(c.Code, fmt.Sprintf("Same place %d", i), bothOn, "198.51.100.7"); resp.StatusCode != 201 {
			t.Fatalf("knock %d from one address = %d %v", i, resp.StatusCode, b)
		}
	}
	if resp, _ := e.knockFrom(c.Code, "Same place 11", bothOn, "198.51.100.7"); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the 11th knock from one address = %d, want 429 with Retry-After", resp.StatusCode)
	}
	// Per meeting: 20 waiting; the 21st is 429 from a fresh address.
	for i := 11; i <= 20; i++ {
		if resp, b := e.knockFrom(c.Code, fmt.Sprintf("Elsewhere %d", i), bothOn, fmt.Sprintf("203.0.113.%d", i)); resp.StatusCode != 201 {
			t.Fatalf("waiting knock %d = %d %v", i, resp.StatusCode, b)
		}
	}
	if resp, b := e.knockFrom(c.Code, "Elsewhere 21", bothOn, "192.0.2.21"); resp.StatusCode != 429 || !strings.Contains(fmt.Sprint(b), "already waiting") {
		t.Fatalf("the 21st waiting knock = %d %v, want 429", resp.StatusCode, b)
	}
}

// The call holds 8 people: the owner and 7 guests.
func TestTheCallIsFullAtEight(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	for i := 1; i <= 8; i++ {
		e.knockFrom(c.Code, fmt.Sprintf("Guest %d", i), bothOn, fmt.Sprintf("203.0.113.%d", i))
	}
	for i := 1; i <= 7; i++ {
		e.admit(c, fmt.Sprintf("Guest %d", i))
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, "Guest 8")+"/admit", c.Sess, nil); resp.StatusCode != 429 {
		t.Fatalf("the 8th guest's admit = %d %s, want 429", resp.StatusCode, raw)
	}
}

// ---- 🔴 the guest name ----

// The name is cleaned on the way in (trimmed; control and bidi characters
// refused, so no row is stored), and every page shows names as text.
func TestTheGuestNameIsCheckedAndShownAsText(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	for name, n := range map[string]string{"right-to-left override": "Grace\u202eH", "a newline": "Grace\nH", "41 characters": strings.Repeat("g", 41), "blank": "   "} {
		if resp, b := e.knockFrom(c.Code, n, bothOn, "198.51.100.8"); resp.StatusCode != 400 {
			t.Errorf("%s: knock = %d %v, want 400", name, resp.StatusCode, b)
		}
	}
	if lob, _ := e.store.ForSubject(c.Subject).Lobby(context.Background(), c.ID); len(lob) != 0 {
		t.Fatalf("refused names were stored: %+v", lob)
	}
	e.knockFrom(c.Code, "  <b>Grace</b>  ", bothOn, "198.51.100.8")
	ev := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil).want("meeting")
	if !strings.Contains(ev.Data, `"name":"\u003cb\u003eGrace\u003c/b\u003e"`) {
		t.Fatalf("the owner's lobby = %s: want the trimmed name, as a JSON string", ev.Data)
	}
	// The owner's display name on the guest page is escaped.
	e.fake.AddPerson(mustertest.Person{ID: 1618033988, Email: "phi@example.test", DisplayName: `<img src=x onerror=alert(1)>`})
	m := e.meeting(mustertest.Person{ID: 1618033988})
	_, page := e.do("GET", "/j/"+m.Code, "", nil)
	if strings.Contains(string(page), "<img src=x") || !strings.Contains(string(page), "&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatalf("the guest page renders the owner's name as markup:\n%s", page)
	}
}

// Every script this plugin serves writes text with textContent, never as
// markup: a guest's name and the owner's are text a stranger can choose.
// (The vendored LiveKit client is not ours and is not scanned.)
func TestNoPageScriptInterpretsMarkup(t *testing.T) {
	dir := filepath.Join("web", "static")
	files, _ := filepath.Glob(filepath.Join(dir, "*.js"))
	if len(files) < 3 {
		t.Fatalf("found %d scripts in %s (want app.js, call.js, guest.js): this scan would prove nothing", len(files), dir)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment", "DOMParser", "eval(", "new Function"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
		if !strings.Contains(string(b), "textContent") {
			t.Errorf("%s never uses textContent: is this the right file?", f)
		}
	}
}

// ---- 🔴 no ticket in a URL or a log ----

func TestNoTicketCodeOrNameInAURLOrALog(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	ticket := e.knock(c.Code, grace)
	for _, path := range []string{"/knock", "/wait", "/events"} {
		resp, raw := e.do("POST", "/j/"+c.Code+path+"?ticket="+ticket, "", map[string]any{"ticket": ticket, "name": linus, "notice": bothOn})
		if resp.StatusCode != 400 {
			t.Errorf("%s with ?ticket= = %d %s, want 400", path, resp.StatusCode, raw)
		}
	}
	e.admit(c, grace)
	tok := admittedToken(t, e.wait(c.Code, ticket))
	e.do("DELETE", "/api/meetings/"+c.ID+"/participants/"+e.pidAny(c, grace), c.Sess, nil)
	e.lk.Webhook(e.internal.URL+"/livekit/webhook", "participant_joined", c.Room, tok["identity"])
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	got := e.log.String()
	for _, want := range []string{"http POST /j/{code}/knock 201", "http POST /j/{code}/wait 200", "/end 200"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log lacks %q (so this scan proves nothing):\n%s", want, got)
		}
	}
	for what, secret := range map[string]string{"ticket": ticket, "join code": c.Code, "guest name": grace, "LiveKit token": tok["token"][:40],
		"LiveKit secret": livekittest.Keys.Secret, "identity": tok["identity"]} {
		if strings.Contains(got, secret) {
			t.Errorf("the log carries the %s:\n%s", what, got)
		}
	}
}

// ---- 🔴 the knock notification ----

// "Knock push timing": a push only while no owner session holds the
// meeting's event stream, on the first knock and when the count rises; the
// owner opening the meeting withdraws it, and so does the lobby emptying.
func TestTheKnockIsPushedOnlyWhileTheOwnerIsNotLooking(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	pushes := func() []mustertest.Push {
		e.server.SettleKnocksForTest(t)
		return e.fake.Pushes()
	}
	tag := fmt.Sprintf("app-%d-%s", app.PluginID, c.ID)
	e.knock(c.Code, grace)
	if p := pushes(); len(p) != 1 || p[0].UserID != ada.ID || p[0].Kind != "knock" || p[0].Count != 1 || p[0].Tag != tag {
		t.Fatalf("after the first knock: %+v, want one knock to Ada, count 1, tag %s", p, tag)
	}
	e.clock.Add(31 * time.Second) // past muster's one-per-30-s for the ref
	e.knockFrom(c.Code, linus, bothOn, "203.0.113.2")
	if p := pushes(); len(p) != 2 || p[1].Count != 2 {
		t.Fatalf("after the count rose to 2: %+v, want a second knock with count 2", p)
	}
	// A deny lowers the count: no push.
	e.clock.Add(31 * time.Second)
	e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, linus)+"/deny", c.Sess, nil)
	if p := pushes(); len(p) != 2 {
		t.Fatalf("after a deny: %+v, want no new push", p)
	}
	// The owner opens the meeting: the knock is withdrawn, and a knock while
	// they look sends nothing.
	s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	s.want("meeting")
	if p := pushes(); len(p) != 3 || p[2].Kind != "resolved" || p[2].Tag != tag {
		t.Fatalf("after the owner opened the meeting: %+v, want the knock withdrawn", p)
	}
	e.clock.Add(31 * time.Second)
	e.knockFrom(c.Code, "Ines W.", bothOn, "203.0.113.3")
	s.want("meeting")
	if p := pushes(); len(p) != 3 {
		t.Fatalf("a knock while the owner watches pushed: %+v", p)
	}
	s.cancel()
	<-s.done
	// The client has stopped reading; the server's handler unregisters the
	// owner from the hub when it returns, a moment later. Wait for THAT, or
	// the next knock can still see the owner watching.
	eventually(t, 5*time.Second, "the server no longer counts the owner as watching", func() bool { return !e.server.OwnerWatchingForTest(c.ID) })
	// The owner looks away; a new knock raises the count past what was sent.
	e.clock.Add(31 * time.Second)
	e.knockFrom(c.Code, "Omar K.", bothOn, "203.0.113.5")
	if p := pushes(); len(p) != 4 || p[3].Kind != "knock" || p[3].Count != 3 {
		t.Fatalf("a knock with the owner away: %+v, want a knock with count 3", p)
	}
	// Everyone is let in: the lobby empties and the knock is withdrawn.
	for _, n := range []string{grace, "Ines W.", "Omar K."} {
		e.admit(c, n)
	}
	if p := pushes(); len(p) != 5 || p[4].Kind != "resolved" {
		t.Fatalf("after the lobby emptied: %+v, want the knock withdrawn", p)
	}
	for _, p := range e.fake.Pushes() {
		if p.UserID != ada.ID {
			t.Fatalf("a push went to someone else: %+v", p)
		}
	}
}

// ---- 🔴 the reconcile loop ----

// "Reconcile": with the webhook lost, an identity not admitted is removed on
// the next pass, and the owner and an admitted guest stay (the positive
// control). A room left behind by an ended meeting is deleted.
func TestTheReconcileLoopRemovesWhoIsNotAdmittedAndDeletesEndedRooms(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	gt := e.knock(c.Code, grace)
	e.admit(c, grace)
	tok := admittedToken(t, e.wait(c.Code, gt))
	e.lk.Join(tok["token"])
	e.lk.Put(c.Room, store.OwnerIdentity)
	e.lk.Put(c.Room, "guest-Unknown000000000000")
	e.server.Reconcile(context.Background())
	if got := strings.Join(e.lk.InRoom(c.Room), ","); got != tok["identity"]+",owner" {
		t.Fatalf("after one pass the room holds %q, want the admitted guest and the owner", got)
	}
	// End meeting while LiveKit is down: the room stays until a pass.
	e.lk.SetDown(true)
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	e.lk.SetDown(false)
	if _, ok := e.lk.Room(c.Room); !ok {
		t.Fatal("the fixture: the room should have survived the failed delete")
	}
	e.server.Reconcile(context.Background())
	if _, ok := e.lk.Room(c.Room); ok {
		t.Fatal("the reconcile pass left an ended meeting's room")
	}
	// A room that is not the plugin's is left alone.
	e.lk.Put("someone-elses-room", "x")
	e.lk.Put("meeting-NotOurs00000000", "x")
	e.server.Reconcile(context.Background())
	if _, ok := e.lk.Room("meeting-NotOurs00000000"); !ok {
		t.Fatal("the reconcile pass deleted a room no meeting of the plugin's has")
	}
}

// ---- ownership on the call routes ----

func TestAnotherPersonCannotDriveTheCall(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	e.knock(c.Code, grace)
	pid := e.pid(c, grace)
	bs := e.enter(bea)
	for _, r := range [][2]string{
		{"POST", "/api/meetings/" + c.ID + "/token"}, {"POST", "/api/meetings/" + c.ID + "/record"}, {"POST", "/api/meetings/" + c.ID + "/ai"},
		{"GET", "/api/meetings/" + c.ID + "/events"}, {"POST", "/api/meetings/" + c.ID + "/lobby/" + pid + "/admit"},
		{"POST", "/api/meetings/" + c.ID + "/lobby/" + pid + "/deny"}, {"DELETE", "/api/meetings/" + c.ID + "/participants/" + pid},
		{"POST", "/api/meetings/" + c.ID + "/end"},
	} {
		if resp, raw := e.do(r[0], r[1], bs, map[string]bool{"on": false}); resp.StatusCode != 404 {
			t.Errorf("Bea: %s %s = %d %s, want 404", r[0], r[1], resp.StatusCode, raw)
		}
	}
	m, _ := e.store.ForSubject(c.Subject).Meeting(context.Background(), c.ID)
	if m.State != "open" || !m.Record || !m.AI || e.pid(c, grace) != pid {
		t.Fatalf("Bea's calls changed Ada's meeting: %+v", m)
	}
}

// The owner's stream: the snapshot at once, a knock as it happens, and
// locked once a guest knocked.
func TestTheOwnersStreamShowsTheLobbyLive(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	if ev := s.want("meeting"); !strings.Contains(ev.Data, `"lobby":[]`) || !strings.Contains(ev.Data, `"locked":false`) {
		t.Fatalf("first snapshot = %s", ev.Data)
	}
	e.knock(c.Code, grace)
	ev := s.want("meeting")
	if !strings.Contains(ev.Data, `"name":"`+grace+`"`) || !strings.Contains(ev.Data, `"state":"waiting"`) || !strings.Contains(ev.Data, `"locked":true`) {
		t.Fatalf("after a knock = %s", ev.Data)
	}
	e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	if ev := s.want("meeting"); !strings.Contains(ev.Data, `"state":"ended"`) {
		t.Fatalf("after End = %s", ev.Data)
	}
}

// 🔴 An owner's event stream ends with the session: once the person's grant
// expires (their sessions deleted, their meeting kept for the guests), a
// stream opened before stops sending the lobby, which holds guests' names.
func TestTheOwnersStreamEndsWithTheSession(t *testing.T) {
	e := newEnv(t)
	c := e.meeting(ada)
	s := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	s.want("meeting")
	if _, err := e.store.EndSessions(context.Background(), []string{c.Subject}, e.clock.Now(), e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	e.knock(c.Code, grace)
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream outlived its session; it sent: %q", s.raw.String())
	}
	if strings.Contains(s.text(), grace) {
		t.Fatalf("the stream sent a guest's name after its session ended: %q", s.text())
	}
	if e.meetingState(c.Subject, c.ID) != "open" {
		t.Fatal("the fixture: the meeting should still be open")
	}
}

// The vendored LiveKit client is the pinned release, byte for byte
// (scripts/vendor-livekit-client.sh), and is served from this origin.
func TestTheVendoredLiveKitClientIsThePinnedOne(t *testing.T) {
	e := newEnv(t)
	resp, js := e.do("GET", "/static/vendor/livekit-client-2.25.0.umd.js", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("the vendored client = %d", resp.StatusCode)
	}
	if got := fmt.Sprintf("%x", sha256sum(js)); got != "2a9c687ae2ec2036f1221f2b664cde6f306a62fd1d153b198bc8eebe2c4c060a" {
		t.Fatalf("the vendored client's sha256 = %s: not the pinned 2.25.0", got)
	}
}
