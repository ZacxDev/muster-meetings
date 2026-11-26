package store_test

// The meeting's agent (migration 9, agent.go): one agent per meeting, the
// ask's delta cursor (all lines on an agent's first ask, only the new ones
// after), one outstanding ask, a reply bound to that ask, and every MCP
// write re-checking its token.

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// agentFixture is a live meeting of subA's, its session repository, and two
// MCP repositories of subA's (two tokens).
type agentFixture struct {
	s      *store.Store
	owner  *store.OwnerRepo
	id     string
	room   string
	m1, m2 *store.OwnerRepo
	tid1   string
}

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	s, _ := open(t)
	r := owner(t, s, subA, t0)
	c, err := r.CreateMeeting(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	f := agentFixture{s: s, owner: r, id: c.Meeting.ID, room: store.RoomName(c.Meeting.ID)}
	for i, dst := range []**store.OwnerRepo{&f.m1, &f.m2} {
		tok, mt := mintMCP(t, r, t0)
		got, err := s.LookupMCPToken(ctx, tok, t0)
		if err != nil {
			t.Fatal(err)
		}
		*dst = s.ForMCP(got)
		if i == 0 {
			f.tid1 = mt.ID
		}
	}
	if _, err := s.UpsertTrack(ctx, f.room, "TR_agent_own", store.OwnerIdentity, "Ada", t0); err != nil {
		t.Fatal(err)
	}
	return f
}

// line stores one line said in the meeting.
func (f agentFixture) line(t *testing.T, at int64, text string) int64 {
	t.Helper()
	id, err := f.s.AddSegment(ctx, "TR_agent_own", store.SegText, at, at+400, text, "fake")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func texts(segs []store.Segment) []string {
	out := []string{}
	for _, g := range segs {
		out = append(out, g.Text)
	}
	return out
}

// 🔴 One agent per meeting, told apart by its key: while it is in, a join
// is refused (with nothing of the agent in) from a second token, from the
// same token under another name, AND from the same token under the same
// name (a second session sharing the token): only its key resumes it, and
// only through its own token. Once it leaves, another may join.
func TestAMeetingTakesOneAgent(t *testing.T) {
	f := newAgentFixture(t)
	a, already, err := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	if err != nil || already || len(a.Key) != 22 {
		t.Fatalf("first join = %+v %v %v", a, already, err)
	}
	if b, _, err := f.m2.JoinAgent(ctx, f.id, "Reviewer", "", t0); !errors.Is(err, store.ErrAgentTaken) || b != (store.Agent{}) {
		t.Errorf("a second agent's join = %+v, %v; want ErrAgentTaken and nothing of the agent in", b, err)
	}
	if _, _, err := f.m1.JoinAgent(ctx, f.id, "Builder two", "", t0); !errors.Is(err, store.ErrAgentTaken) {
		t.Errorf("the same token under another name = %v; want ErrAgentTaken", err)
	}
	if b, _, err := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0); !errors.Is(err, store.ErrAgentTaken) || b.Key != "" {
		t.Errorf("the same token and name without the key = %+v, %v; want ErrAgentTaken, no key", b, err)
	}
	if _, _, err := f.m2.JoinAgent(ctx, f.id, "Builder", a.Key, t0); !errors.Is(err, store.ErrAgentTaken) {
		t.Errorf("the key through another token = %v; want ErrAgentTaken", err)
	}
	again, already, err := f.m1.JoinAgent(ctx, f.id, "Builder", a.Key, t0)
	if err != nil || !already || again.Key != a.Key {
		t.Errorf("a join with the key = %+v %v %v; want the same agent, already", again, already, err)
	}
	var n int
	if err := f.s.QueryRowForTest(`SELECT count(*) FROM meeting_agents`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d agent rows (%v), want 1", n, err)
	}
	if err := f.m1.LeaveAgent(ctx, f.id, a.Key, t0); err != nil {
		t.Fatal(err)
	}
	if b, _, err := f.m2.JoinAgent(ctx, f.id, "Reviewer", "", t0); err != nil || b.Key == a.Key {
		t.Errorf("a join after the first left = %+v, %v", b, err)
	}
	// An owner session has no token to bind.
	if _, _, err := f.owner.JoinAgent(ctx, f.id, "Builder", "", t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("an owner session's join = %v; want ErrNoSession", err)
	}
}

// 🔴 The delta cursor: the agent's first ask carries every line so far, the
// next only those said since, each with its question; a new agent starts
// again from the first line.
func TestAnAskCarriesTheLinesSinceTheAgentsLastAsk(t *testing.T) {
	f := newAgentFixture(t)
	a, _, err := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	f.line(t, 1_000, "Kickoff of the widget plan.")
	f.line(t, 2_000, "Budget is twelve units.")
	k1, err := f.owner.AskAgent(ctx, f.id, "What is the budget?", t0)
	if err != nil || !k1.First {
		t.Fatalf("first ask = %+v %v; want first", k1, err)
	}
	// A line said after the ask is not in it.
	f.line(t, 3_000, "Ship by the fourth.")
	got, segs, err := f.m1.NextAsk(ctx, f.id, a.Key, t0)
	if err != nil || got.ID != k1.ID || got.Question != "What is the budget?" || !got.First {
		t.Fatalf("first wait = %+v %v", got, err)
	}
	if w := []string{"Kickoff of the widget plan.", "Budget is twelve units."}; !slices.Equal(texts(segs), w) {
		t.Errorf("first ask's lines = %q, want %q", texts(segs), w)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k1.ID, "Twelve units.", t0); err != nil {
		t.Fatal(err)
	}
	f.line(t, 4_000, "Review on Monday.")
	k2, err := f.owner.AskAgent(ctx, f.id, "", t0)
	if err != nil || k2.First {
		t.Fatalf("second ask = %+v %v; want not first", k2, err)
	}
	got, segs, err = f.m1.NextAsk(ctx, f.id, a.Key, t0)
	if err != nil || got.ID != k2.ID || got.Question != "" || got.First {
		t.Fatalf("second wait = %+v %v", got, err)
	}
	if w := []string{"Ship by the fourth.", "Review on Monday."}; !slices.Equal(texts(segs), w) {
		t.Errorf("second ask's lines = %q, want only the new %q", texts(segs), w)
	}
	// A third ask with nothing said since carries no line.
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k2.ID, "Noted.", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.AskAgent(ctx, f.id, "Anything else?", t0); err != nil {
		t.Fatal(err)
	}
	if _, segs, err = f.m1.NextAsk(ctx, f.id, a.Key, t0); err != nil || len(segs) != 0 {
		t.Errorf("third wait = %q %v; want no line", texts(segs), err)
	}
	// Another agent sees everything on its first ask.
	if err := f.owner.RemoveAgent(ctx, f.id, t0); err != nil {
		t.Fatal(err)
	}
	b, _, err := f.m2.JoinAgent(ctx, f.id, "Reviewer", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := f.owner.AskAgent(ctx, f.id, "", t0); err != nil || !k.First {
		t.Fatalf("the new agent's first ask = %+v %v", k, err)
	}
	if _, segs, err = f.m2.NextAsk(ctx, f.id, b.Key, t0); err != nil || len(segs) != 4 {
		t.Errorf("the new agent's first ask's lines = %q %v; want all 4", texts(segs), err)
	}
}

// One ask outstanding at a time; no agent, no ask; nothing asked is
// ErrNoAsk; an ask is not the agent's to fetch with another key.
func TestOneAskIsOutstandingAtATime(t *testing.T) {
	f := newAgentFixture(t)
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); !errors.Is(err, store.ErrNoAgent) {
		t.Errorf("ask with no agent = %v; want ErrNoAgent", err)
	}
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	if _, _, err := f.m1.NextAsk(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoAsk) {
		t.Errorf("wait with nothing asked = %v; want ErrNoAsk", err)
	}
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.AskAgent(ctx, f.id, "again", t0); !errors.Is(err, store.ErrAskOutstanding) {
		t.Errorf("a second ask while one is open = %v; want ErrAskOutstanding", err)
	}
	if _, _, err := f.m1.NextAsk(ctx, f.id, "x"+a.Key[1:], t0); !errors.Is(err, store.ErrNoAgent) {
		t.Errorf("wait with another key = %v; want ErrNoAgent", err)
	}
	if _, _, err := f.m2.NextAsk(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoAgent) {
		t.Errorf("wait with the key through another token = %v; want ErrNoAgent", err)
	}
	// Delivered and unanswered: still outstanding, delivered again.
	k, _, err := f.m1.NextAsk(ctx, f.id, a.Key, t0)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := f.m1.NextAsk(ctx, f.id, a.Key, t0); err != nil || again.ID != k.ID {
		t.Errorf("a second wait before the answer = %+v %v; want the same ask", again, err)
	}
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); !errors.Is(err, store.ErrAskOutstanding) {
		t.Errorf("an ask while one is delivered = %v; want ErrAskOutstanding", err)
	}
}

// 🔴 A reply is bound to the agent's outstanding ask: a made-up ask id, an
// ask cancelled by Remove, and another agent's ask are ErrNoAsk; a second,
// different reply is ErrAnswered; the same reply again changes nothing.
func TestAReplyIsBoundToTheOpenAsk(t *testing.T) {
	f := newAgentFixture(t)
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	k, err := f.owner.AskAgent(ctx, f.id, "Status?", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k.ID+7, "Wrong ask.", t0); !errors.Is(err, store.ErrNoAsk) {
		t.Errorf("a reply to a made-up ask = %v; want ErrNoAsk", err)
	}
	got, unchanged, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k.ID, "All green.", t0.Add(90*time.Second))
	if err != nil || unchanged || got.State != store.AskAnswered {
		t.Fatalf("the reply = %+v %v %v", got, unchanged, err)
	}
	if _, unchanged, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k.ID, "All green.", t0); err != nil || !unchanged {
		t.Errorf("the same reply again = %v %v; want unchanged", unchanged, err)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k.ID, "Actually red.", t0); !errors.Is(err, store.ErrAnswered) {
		t.Errorf("a second reply = %v; want ErrAnswered", err)
	}
	feed, err := f.s.AgentFeed(ctx, f.id)
	if err != nil || feed.Ask != nil || len(feed.Replies) != 1 || feed.Replies[0].Text != "All green." || feed.Replies[0].Name != "Builder" ||
		feed.Replies[0].Question != "Status?" || feed.Replies[0].AtMS != 90_000 {
		t.Errorf("feed = %+v %v", feed, err)
	}
	// Removed: the outstanding ask is cancelled, and a reply to it refused.
	k2, err := f.owner.AskAgent(ctx, f.id, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owner.RemoveAgent(ctx, f.id, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k2.ID, "Too late.", t0); !errors.Is(err, store.ErrNoAgent) {
		t.Errorf("a reply after Remove = %v; want ErrNoAgent", err)
	}
	b, _, _ := f.m2.JoinAgent(ctx, f.id, "Reviewer", "", t0)
	if _, _, err := f.m2.AnswerAsk(ctx, f.id, b.Key, k2.ID, "Not mine.", t0); !errors.Is(err, store.ErrNoAsk) {
		t.Errorf("a reply to the removed agent's ask = %v; want ErrNoAsk", err)
	}
	// Another meeting of the same owner, its own agent and an open ask: the
	// first meeting's agent cannot answer it with its id.
	c2, err := f.owner.CreateMeeting(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m1.JoinAgent(ctx, c2.Meeting.ID, "Builder", "", t0); err != nil {
		t.Fatal(err)
	}
	k3, err := f.owner.AskAgent(ctx, c2.Meeting.ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m2.AnswerAsk(ctx, f.id, b.Key, k3.ID, "Crossed wires.", t0); !errors.Is(err, store.ErrNoAsk) {
		t.Errorf("a reply to another meeting's ask = %v; want ErrNoAsk", err)
	}
	if feed, _ := f.s.AgentFeed(ctx, c2.Meeting.ID); feed.Ask == nil || len(feed.Replies) != 0 {
		t.Errorf("the other meeting's ask after a crossed reply = %+v", feed)
	}
}

// 🔴 Every agent write through MCP re-checks its token: revoked, the join,
// the wait, the reply and the leave are ErrNoSession.
func TestAnAgentsWritesRecheckItsToken(t *testing.T) {
	f := newAgentFixture(t)
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	k, err := f.owner.AskAgent(ctx, f.id, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owner.RevokeMCPToken(ctx, f.tid1, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m1.JoinAgent(ctx, f.id, "Builder", a.Key, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("join with a revoked token = %v; want ErrNoSession", err)
	}
	if _, _, err := f.m1.NextAsk(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("wait with a revoked token = %v; want ErrNoSession", err)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, k.ID, "Leaked.", t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("reply with a revoked token = %v; want ErrNoSession", err)
	}
	if err := f.m1.LeaveAgent(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoSession) {
		t.Errorf("leave with a revoked token = %v; want ErrNoSession", err)
	}
	if feed, _ := f.s.AgentFeed(ctx, f.id); len(feed.Replies) != 0 {
		t.Errorf("a reply was stored through a revoked token: %+v", feed.Replies)
	}
}

// 🔴 The AI switch: an agent joins only a meeting whose AI switch is on, and
// an ask made before it went off is cancelled, not delivered; an ended
// meeting takes no join and no ask; another owner's meeting is ErrNotFound.
func TestAnAgentNeedsTheAISwitchAndALiveMeeting(t *testing.T) {
	off := newAgentFixture(t)
	if _, err := off.owner.SetSwitch(ctx, off.id, store.SwitchAI, false, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := off.m1.JoinAgent(ctx, off.id, "Builder", "", t0); !errors.Is(err, store.ErrAIOff) {
		t.Fatalf("join with AI off = %v; want ErrAIOff", err)
	}
	f := newAgentFixture(t)
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	f.line(t, 1_000, "Something said.")
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.SetSwitch(ctx, f.id, store.SwitchAI, false, t0); err != nil {
		t.Fatal(err)
	}
	if _, segs, err := f.m1.NextAsk(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoAsk) || len(segs) != 0 {
		t.Errorf("wait after AI off = %q, %v; want ErrNoAsk and no line", texts(segs), err)
	}
	// The cancelled ask takes no reply from the agent still in.
	var cancelled int64
	if err := f.s.QueryRowForTest(`SELECT id FROM agent_asks WHERE state = 'cancelled'`).Scan(&cancelled); err != nil {
		t.Fatalf("no cancelled ask: %v", err)
	}
	if _, _, err := f.m1.AnswerAsk(ctx, f.id, a.Key, cancelled, "Answered anyway.", t0); !errors.Is(err, store.ErrNoAsk) {
		t.Errorf("a reply to the cancelled ask = %v; want ErrNoAsk", err)
	}
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); !errors.Is(err, store.ErrAIOff) {
		t.Errorf("ask with AI off = %v; want ErrAIOff", err)
	}
	// Ended; and another owner's.
	g := newAgentFixture(t)
	if _, err := g.owner.End(ctx, g.id, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.m1.JoinAgent(ctx, g.id, "Builder", "", t0); !errors.Is(err, store.ErrGone) {
		t.Errorf("join of an ended meeting = %v; want ErrGone", err)
	}
	other := owner(t, g.s, subB, t0)
	tok, _ := mintMCP(t, other, t0)
	mt, _ := g.s.LookupMCPToken(ctx, tok, t0)
	if _, _, err := g.s.ForMCP(mt).JoinAgent(ctx, g.id, "Intruder", "", t0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another owner's join = %v; want ErrNotFound", err)
	}
}

// 🔴 An ask cancelled before delivery (the AI switch went off) moves no
// cursor: the agent's next ask carries the lines it never received, as its
// first.
func TestACancelledAskDoesNotMoveTheCursor(t *testing.T) {
	f := newAgentFixture(t)
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	f.line(t, 1_000, "Line A.")
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.SetSwitch(ctx, f.id, store.SwitchAI, false, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m1.NextAsk(ctx, f.id, a.Key, t0); !errors.Is(err, store.ErrNoAsk) {
		t.Fatalf("wait after AI off = %v; want ErrNoAsk (cancelled)", err)
	}
	if _, err := f.owner.SetSwitch(ctx, f.id, store.SwitchAI, true, t0); err != nil {
		t.Fatal(err)
	}
	f.line(t, 2_000, "Line B.")
	k, err := f.owner.AskAgent(ctx, f.id, "", t0)
	if err != nil || !k.First {
		t.Fatalf("the ask after a cancelled one = %+v %v; want first", k, err)
	}
	got, segs, err := f.m1.NextAsk(ctx, f.id, a.Key, t0)
	if w := []string{"Line A.", "Line B."}; err != nil || !got.First || !slices.Equal(texts(segs), w) {
		t.Errorf("its lines = %q (first %v, %v); want %q", texts(segs), got.First, err, w)
	}
}

// 🔴 An agent not seen (no join, no wait) since the cut-off is taken out and
// its outstanding ask cancelled; one seen since is not.
func TestAnIdleAgentIsTakenOut(t *testing.T) {
	f := newAgentFixture(t)
	a, _, _ := f.m1.JoinAgent(ctx, f.id, "Builder", "", t0)
	if _, err := f.owner.AskAgent(ctx, f.id, "", t0); err != nil {
		t.Fatal(err)
	}
	if err := f.m1.TouchAgent(ctx, f.id, a.Key, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if gone, err := f.s.ExpireIdleAgent(ctx, f.id, t0.Add(30*time.Second), t0.Add(2*time.Minute)); err != nil || gone {
		t.Fatalf("an agent seen after the cut-off: taken out %v (%v); want kept", gone, err)
	}
	if feed, _ := f.s.AgentFeed(ctx, f.id); feed.Agent == nil {
		t.Fatal("positive control: the agent is not in")
	}
	if gone, err := f.s.ExpireIdleAgent(ctx, f.id, t0.Add(61*time.Second), t0.Add(3*time.Minute)); err != nil || !gone {
		t.Fatalf("an agent not seen since the cut-off: taken out %v (%v); want taken out", gone, err)
	}
	feed, err := f.s.AgentFeed(ctx, f.id)
	if err != nil || feed.Agent != nil || feed.Ask != nil {
		t.Errorf("the feed after the idle agent was taken out = %+v %v; want no agent, no ask", feed, err)
	}
	if _, _, err := f.m2.JoinAgent(ctx, f.id, "Reviewer", "", t0.Add(3*time.Minute)); err != nil {
		t.Errorf("a join after the idle agent was taken out = %v", err)
	}
}
