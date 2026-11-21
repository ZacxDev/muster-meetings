package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

var both = store.Notice{Record: true, AI: true}

func knock(t *testing.T, s *store.Store, c store.Created, name string, at time.Time) store.Knocked {
	t.Helper()
	k, err := s.Knock(ctx, c.JoinCode, name, both, at)
	if err != nil {
		t.Fatalf("knock %s: %v", name, err)
	}
	return k
}

// 🔴 The lobby state machine (design: "a table of every (state, event)
// pair: waiting → admitted / denied / ended; admitted → removed / left /
// ended; nothing leaves ended"). The expectation is written out here, not
// derived from the code's table, so a mutant widening any one row is red.
func TestTheLobbyStateMachine(t *testing.T) {
	states := []string{store.StWaiting, store.StAdmitted, store.StDenied, store.StRemoved, store.StLeft, store.StEnded}
	events := []store.Event{store.EvAdmit, store.EvDeny, store.EvRemove, store.EvLeave, store.EvEnd}
	want := map[string]string{
		"waiting/admit":   "admitted",
		"waiting/deny":    "denied",
		"waiting/end":     "ended",
		"admitted/remove": "removed",
		"admitted/leave":  "left",
		"admitted/end":    "ended",
	}
	for _, st := range states {
		for _, ev := range events {
			got, ok := store.Transition(st, ev)
			key := st + "/" + string(ev)
			if w, allowed := want[key]; allowed != ok || got != w {
				t.Errorf("%s: (%q, %v), want (%q, %v)", key, got, ok, w, allowed)
			}
		}
	}
}

// The same machine, as the database applies it: an owner's admit, deny or
// remove changes only a row in a state the table allows, and the rest is
// ErrState with the row unchanged.
func TestMovesTheMachineRefusesChangeNoRow(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	c, _ := o.CreateMeeting(ctx, t0)
	k := knock(t, s, c, "Grace H.", t0)
	mv := func(ev store.Event) error { _, err := o.Move(ctx, c.Meeting.ID, k.ParticipantID, ev, t0); return err }
	if err := mv(store.EvRemove); !errors.Is(err, store.ErrState) {
		t.Errorf("removing a waiting guest: %v, want ErrState", err)
	}
	if err := mv(store.EvDeny); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []store.Event{store.EvAdmit, store.EvDeny, store.EvRemove} {
		if err := mv(ev); !errors.Is(err, store.ErrState) {
			t.Errorf("%s on a denied guest: %v, want ErrState", ev, err)
		}
	}
	g, _ := s.GuestByParticipant(ctx, k.ParticipantID, t0)
	if g.State != store.StDenied {
		t.Errorf("the denied guest is now %q", g.State)
	}
	for _, ev := range []store.Event{store.EvLeave, store.EvEnd} {
		if _, err := o.Move(ctx, c.Meeting.ID, k.ParticipantID, ev, t0); err == nil || errors.Is(err, store.ErrState) {
			t.Errorf("%s is not an owner's move, got %v", ev, err)
		}
	}
}

// 🔴 A ticket opens only its own meeting: the code and the ticket must name
// one row.
func TestATicketBelongsToItsOwnMeetingOnly(t *testing.T) {
	s, _ := open(t)
	a := mk(t, s, subA, t0)
	b := mk(t, s, subB, t0)
	k := knock(t, s, a, "Grace H.", t0)
	if g, err := s.GuestByTicket(ctx, a.JoinCode, k.Ticket, t0); err != nil || g.MeetingID != a.Meeting.ID {
		t.Fatalf("the ticket on its own meeting: %+v, %v", g, err)
	}
	if g, err := s.GuestByTicket(ctx, b.JoinCode, k.Ticket, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's ticket on B's link = %+v, %v; want ErrNotFound", g, err)
	}
}

// Tickets and identities: 128 bits, 22 base64url characters, drawn fresh
// each knock, the ticket stored only hashed.
func TestTicketsAreRandomAndStoredOnlyHashed(t *testing.T) {
	s, p := open(t)
	seen := map[string]bool{}
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	var last store.Knocked
	for i := 0; i < 300; i++ {
		c := mk(t, s, subA, t0)
		k := knock(t, s, c, "Grace H.", t0)
		if !shape.MatchString(k.Ticket) || seen[k.Ticket] {
			t.Fatalf("ticket %q is not 22 base64url characters, or was drawn twice", k.Ticket)
		}
		seen[k.Ticket] = true
		last = k
	}
	if bytes.Contains(dbBytes(t, p), []byte(last.Ticket)) {
		t.Fatal("the ticket itself is in the database")
	}
}

// 🔴 "the 21st waiting knock on a meeting is 429; the 20th passes". A guest
// no longer waiting frees a place.
func TestTheTwentyFirstWaitingKnockIsRefused(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	c, _ := o.CreateMeeting(ctx, t0)
	var first store.Knocked
	for i := 1; i <= store.MaxWaiting; i++ {
		k, err := s.Knock(ctx, c.JoinCode, fmt.Sprintf("Guest %d", i), both, t0)
		if err != nil {
			t.Fatalf("knock %d: %v", i, err)
		}
		if i == 1 {
			first = k
		}
		if k.Waiting != i {
			t.Fatalf("knock %d reports %d waiting", i, k.Waiting)
		}
	}
	if _, err := s.Knock(ctx, c.JoinCode, "Guest 21", both, t0); !errors.Is(err, store.ErrLobbyFull) {
		t.Fatalf("the 21st waiting knock: %v, want ErrLobbyFull", err)
	}
	if _, err := o.Move(ctx, c.Meeting.ID, first.ParticipantID, store.EvDeny, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Knock(ctx, c.JoinCode, "Guest 21", both, t0); err != nil {
		t.Fatalf("a knock after one waiting guest was denied: %v", err)
	}
}

// 🔴 "Notice at knock": a knock whose notice is not the meeting's current
// one is refused with the current one; the current one is taken.
func TestAKnockMustCarryTheCurrentNotice(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	c, _ := o.CreateMeeting(ctx, t0)
	if _, err := o.SetSwitch(ctx, c.Meeting.ID, store.SwitchRecord, false, t0); err != nil {
		t.Fatal(err)
	}
	var mm *store.NoticeMismatch
	if _, err := s.Knock(ctx, c.JoinCode, "Grace H.", both, t0); !errors.As(err, &mm) || mm.Current != (store.Notice{Record: false, AI: true}) {
		t.Fatalf("a knock with the old notice: %v, want a mismatch naming {record: false, ai: true}", err)
	}
	if _, err := s.Knock(ctx, c.JoinCode, "Grace H.", store.Notice{Record: false, AI: true}, t0); err != nil {
		t.Fatalf("a knock with the current notice: %v", err)
	}
}

// 🔴 "Defaults and toggles": a new meeting has both on; either goes off at
// any time; back on is refused once a guest has knocked: waiting, admitted,
// denied, removed or left. Before any knock it is allowed (the positive
// control). Each case is its own meeting.
func TestASwitchGoesBackOnOnlyBeforeAnyGuestKnocked(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	fresh := func() store.Created {
		c, err := o.CreateMeeting(ctx, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Meeting.Record || !c.Meeting.AI {
			t.Fatalf("a new meeting: record %v, ai %v; want both on", c.Meeting.Record, c.Meeting.AI)
		}
		return c
	}
	for _, sw := range []store.Switch{store.SwitchRecord, store.SwitchAI} {
		c := fresh()
		if _, err := o.SetSwitch(ctx, c.Meeting.ID, sw, false, t0); err != nil {
			t.Fatal(err)
		}
		if n, err := o.SetSwitch(ctx, c.Meeting.ID, sw, true, t0); err != nil || n != both {
			t.Fatalf("%s back on before any knock: %+v, %v", sw, n, err)
		}
		for _, after := range []string{"waiting", "admitted", "denied", "removed", "left"} {
			c := fresh()
			if _, err := o.SetSwitch(ctx, c.Meeting.ID, sw, false, t0); err != nil {
				t.Fatal(err)
			}
			k, err := s.Knock(ctx, c.JoinCode, "Grace H.", store.Notice{Record: sw != store.SwitchRecord, AI: sw != store.SwitchAI}, t0)
			if err != nil {
				t.Fatal(err)
			}
			switch after {
			case "admitted", "removed", "left":
				mustMove(t, o, c, k, store.EvAdmit)
				if after == "removed" {
					mustMove(t, o, c, k, store.EvRemove)
				}
				if after == "left" {
					if _, ok, err := s.MarkLeft(ctx, store.RoomName(c.Meeting.ID), identityOf(t, s, k), t0); err != nil || !ok {
						t.Fatalf("left: %v %v", ok, err)
					}
				}
			case "denied":
				mustMove(t, o, c, k, store.EvDeny)
			}
			if _, err := o.SetSwitch(ctx, c.Meeting.ID, sw, true, t0); !errors.Is(err, store.ErrSwitchLocked) {
				t.Errorf("%s back on with a guest %s: %v, want ErrSwitchLocked", sw, after, err)
			}
			if n, err := o.SetSwitch(ctx, c.Meeting.ID, sw, false, t0); err != nil || (sw == store.SwitchRecord && n.Record) || (sw == store.SwitchAI && n.AI) {
				t.Errorf("%s off with a guest %s: %+v, %v", sw, after, n, err)
			}
		}
	}
}

func mustMove(t *testing.T, o *store.OwnerRepo, c store.Created, k store.Knocked, ev store.Event) store.Moved {
	t.Helper()
	m, err := o.Move(ctx, c.Meeting.ID, k.ParticipantID, ev, t0)
	if err != nil {
		t.Fatalf("%s: %v", ev, err)
	}
	return m
}

func identityOf(t *testing.T, s *store.Store, k store.Knocked) string {
	t.Helper()
	g, err := s.GuestByParticipant(ctx, k.ParticipantID, t0)
	if err != nil {
		t.Fatal(err)
	}
	return g.Identity
}

// 🔴 MayBeInRoom, the one admission predicate, over every case: the owner
// and an admitted guest of a live meeting may; nobody else, and nobody at
// all once the meeting ended or is 24 h old.
func TestMayBeInRoom(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	c, _ := o.CreateMeeting(ctx, t0)
	other, _ := owner(t, s, subB, t0).CreateMeeting(ctx, t0)
	room := store.RoomName(c.Meeting.ID)
	ids := map[string]string{}
	for _, st := range []string{"waiting", "admitted", "denied", "removed", "left"} {
		k := knock(t, s, c, "Guest "+st, t0)
		ids[st] = identityOf(t, s, k)
		switch st {
		case "admitted", "removed", "left":
			mustMove(t, o, c, k, store.EvAdmit)
		}
		switch st {
		case "denied":
			mustMove(t, o, c, k, store.EvDeny)
		case "removed":
			mustMove(t, o, c, k, store.EvRemove)
		case "left":
			s.MarkLeft(ctx, room, ids[st], t0)
		}
	}
	may := func(room, id string, at time.Time) bool {
		ok, err := s.MayBeInRoom(ctx, room, id, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	want := map[string]bool{"waiting": false, "admitted": true, "denied": false, "removed": false, "left": false}
	for st, id := range ids {
		if got := may(room, id, t0); got != want[st] {
			t.Errorf("a %s guest: %v, want %v", st, got, want[st])
		}
	}
	if !may(room, store.OwnerIdentity, t0) {
		t.Error("the owner may not be in their own room")
	}
	for name, id := range map[string]string{"unknown": "guest-AAAAAAAAAAAAAAAAAAAAAA", "empty": ""} {
		if may(room, id, t0) {
			t.Errorf("an %s identity may be in the room", name)
		}
	}
	if may(store.RoomName(other.Meeting.ID), ids["admitted"], t0) {
		t.Error("an admitted guest may be in ANOTHER meeting's room")
	}
	if may(room, ids["admitted"], c.Meeting.CreatedAt.Add(store.LinkLifetime)) || may(room, store.OwnerIdentity, c.Meeting.CreatedAt.Add(store.LinkLifetime)) {
		t.Error("someone may be in the room of a meeting 24 h old")
	}
	if _, err := o.End(ctx, c.Meeting.ID, t0); err != nil {
		t.Fatal(err)
	}
	if may(room, ids["admitted"], t0) || may(room, store.OwnerIdentity, t0) {
		t.Error("someone may be in an ended meeting's room")
	}
}

// End moves everyone waiting or admitted to ended; the link is gone.
func TestEndEndsTheLobbyAndTheLink(t *testing.T) {
	s, _ := open(t)
	o := owner(t, s, subA, t0)
	c, _ := o.CreateMeeting(ctx, t0)
	w := knock(t, s, c, "Grace H.", t0)
	a := knock(t, s, c, "Linus T.", t0)
	mustMove(t, o, c, a, store.EvAdmit)
	room, err := o.End(ctx, c.Meeting.ID, t0)
	if err != nil || room != store.RoomName(c.Meeting.ID) {
		t.Fatalf("End = %q, %v", room, err)
	}
	for _, k := range []store.Knocked{w, a} {
		if g, _ := s.GuestByParticipant(ctx, k.ParticipantID, t0); g.State != store.StEnded || g.MeetingLive {
			t.Errorf("after End a guest is %q, live %v", g.State, g.MeetingLive)
		}
	}
	if _, err := s.MeetingByCode(ctx, c.JoinCode, t0); !errors.Is(err, store.ErrGone) {
		t.Errorf("the link after End: %v, want ErrGone", err)
	}
	if _, err := s.Knock(ctx, c.JoinCode, "Ada", both, t0); !errors.Is(err, store.ErrGone) {
		t.Errorf("a knock after End: %v, want ErrGone", err)
	}
	if _, err := o.End(ctx, c.Meeting.ID, t0); !errors.Is(err, store.ErrGone) {
		t.Errorf("ending twice: %v, want ErrGone", err)
	}
}

// 🔴 Ownership: another person cannot read the lobby of, move a guest in,
// switch or end a meeting that is not theirs; every one is ErrNotFound and
// changes nothing.
func TestAnotherPersonCannotTouchTheLobby(t *testing.T) {
	s, _ := open(t)
	a := owner(t, s, subA, t0)
	b := owner(t, s, subB, t0)
	c, _ := a.CreateMeeting(ctx, t0)
	k := knock(t, s, c, "Grace H.", t0)
	if _, err := b.Lobby(ctx, c.Meeting.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B reading A's lobby: %v", err)
	}
	for _, ev := range []store.Event{store.EvAdmit, store.EvDeny, store.EvRemove} {
		if _, err := b.Move(ctx, c.Meeting.ID, k.ParticipantID, ev, t0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("B: %s on A's guest: %v, want ErrNotFound", ev, err)
		}
	}
	if _, err := b.SetSwitch(ctx, c.Meeting.ID, store.SwitchAI, false, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B switching A's meeting: %v", err)
	}
	if _, err := b.End(ctx, c.Meeting.ID, t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("B ending A's meeting: %v", err)
	}
	if g, _ := s.GuestByParticipant(ctx, k.ParticipantID, t0); g.State != store.StWaiting || !g.MeetingLive || !g.Notice.AI {
		t.Errorf("after B's attempts A's guest is %q, meeting live %v, ai %v", g.State, g.MeetingLive, g.Notice.AI)
	}
}
