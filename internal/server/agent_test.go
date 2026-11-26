package server_test

// The meeting's agent (agent.go) through POST /mcp and the owner app: join,
// wait (pending, an ask, ended), say, and everyone's live panel; the
// owner's question stays on the owner's page; one agent per meeting; and no
// word of a question or an answer in the log.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// agentEnv is an env whose meeting_agent_wait gives up after 300 ms.
func agentEnv(t *testing.T) *env {
	return newEnvOpts(t, nil, func(c *server.Config) { c.AgentWait = 300 * time.Millisecond })
}

type agentWait struct {
	Status   string `json:"status"`
	AskID    int64  `json:"ask_id"`
	First    bool   `json:"first"`
	Question string `json:"question"`
	Lines    []struct {
		OffsetMS int64  `json:"offset_ms"`
		Speaker  string `json:"speaker"`
		Text     string `json:"text"`
	} `json:"lines"`
}

func (w agentWait) texts() []string {
	out := []string{}
	for _, l := range w.Lines {
		out = append(out, l.Speaker+": "+l.Text)
	}
	return out
}

type agentFeed struct {
	Agent *struct {
		Name string `json:"name"`
	} `json:"agent"`
	Ask *struct {
		ID       int64  `json:"id"`
		State    string `json:"state"`
		Question string `json:"question"`
	} `json:"ask"`
	Replies []struct {
		Name     string `json:"name"`
		Question string `json:"question"`
		Text     string `json:"text"`
	} `json:"replies"`
}

// agentSeen is every agent event's data each stream has delivered to a
// test (read on the test's goroutine only: the stream's raw text is written
// by its reader).
var agentSeen = map[*stream]*strings.Builder{}

// agentUntil reads agent events until ok holds of one.
func agentUntil(t *testing.T, s *stream, what string, ok func(agentFeed) bool) agentFeed {
	t.Helper()
	if agentSeen[s] == nil {
		agentSeen[s] = &strings.Builder{}
		t.Cleanup(func() { delete(agentSeen, s) })
	}
	for {
		ev := s.want("agent")
		agentSeen[s].WriteString(ev.Data + "\n")
		var f agentFeed
		if err := json.Unmarshal([]byte(ev.Data), &f); err != nil {
			t.Fatal(err)
		}
		if ok(f) {
			return f
		}
		t.Logf("agent event, not yet %s: %s", what, ev.Data)
	}
}

func (e *env) agentJoin(tok, id, name string) string {
	e.t.Helper()
	var v struct {
		Key string `json:"agent_key"`
	}
	e.ok(tok, "meeting_agent_join", map[string]any{"id": id, "name": name}, &v)
	return v.Key
}

func (e *env) agentWaitFor(tok, id, key string) agentWait {
	e.t.Helper()
	var w agentWait
	e.ok(tok, "meeting_agent_wait", map[string]any{"id": id, "agent_key": key}, &w)
	return w
}

func (e *env) ask(c call, question string) (int, string) {
	e.t.Helper()
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/agent/ask", c.Sess, map[string]any{"question": question})
	return resp.StatusCode, string(raw)
}

// The whole loop: the agent joins; a wait with nothing asked is pending;
// the owner asks; the wait answers every line so far and the question; the
// agent says; the owner and a guest both see the answer, live, under its
// name, and only the owner's copy holds the question; the next ask carries
// only the lines since; End answers the wait "ended"; the timeline keeps
// the answers.
func TestAnAgentAnswersTheOwnersAskForEveryone(t *testing.T) {
	e := agentEnv(t)
	c := e.meeting(ada)
	e.started(c)
	ticket, _ := e.admitGuest(c, grace)
	tok, _ := e.mintMCP(c.Sess)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	guest := e.guestEvents(c.Code, ticket)
	owner.want("meeting")
	e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 2_000, "The widget plan starts now.")
	e.say(c, "guest-x", grace+" (guest)", 2_500, 3_000, "Budget is twelve units.")

	key := e.agentJoin(tok, c.ID, "Widget builder")
	agentUntil(t, owner, "the agent in", func(f agentFeed) bool { return f.Agent != nil && f.Agent.Name == "Widget builder" })
	agentUntil(t, guest, "the agent in", func(f agentFeed) bool { return f.Agent != nil && f.Agent.Name == "Widget builder" })

	if w := e.agentWaitFor(tok, c.ID, key); w.Status != "pending" {
		t.Fatalf("a wait with nothing asked = %+v, want pending", w)
	}
	const question = "Whose is the budget line?"
	if code, raw := e.ask(c, question); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	if code, raw := e.ask(c, "again"); code != 409 {
		t.Errorf("a second ask while one is outstanding = %d %s, want 409", code, raw)
	}
	w := e.agentWaitFor(tok, c.ID, key)
	if want := []string{"Ada L.: The widget plan starts now.", "Grace H. (guest): Budget is twelve units."}; w.Status != "ask" || !w.First || w.Question != question || !slices.Equal(w.texts(), want) {
		t.Fatalf("the first ask = %+v; want first, the question and %q", w, want)
	}
	if w.Lines[0].OffsetMS != 1_000 {
		t.Errorf("offset_ms = %d, want 1000", w.Lines[0].OffsetMS)
	}
	const answer = "The budget line is Grace's, at twelve units."
	e.ok(tok, "meeting_agent_say", map[string]any{"id": c.ID, "agent_key": key, "ask_id": w.AskID, "text": answer}, nil)
	of := agentUntil(t, owner, "the answer", func(f agentFeed) bool { return len(f.Replies) == 1 })
	if r := of.Replies[0]; r.Text != answer || r.Name != "Widget builder" || r.Question != question || of.Ask != nil {
		t.Errorf("the owner's feed = %+v", of)
	}
	gf := agentUntil(t, guest, "the answer", func(f agentFeed) bool { return len(f.Replies) == 1 })
	if r := gf.Replies[0]; r.Text != answer || r.Name != "Widget builder" || r.Question != "" {
		t.Errorf("the guest's feed = %+v; want the answer without the question", gf)
	}

	// 🔴 The question never reaches a guest; the positive control: it is on
	// the owner's stream. The next ask carries the line said since, not one
	// said after it.
	e.say(c, store.OwnerIdentity, "Ada L.", 4_000, 5_000, "Ship it Friday.")
	if code, raw := e.ask(c, "Second private question?"); code != 201 {
		t.Fatalf("second ask = %d %s", code, raw)
	}
	e.say(c, store.OwnerIdentity, "Ada L.", 6_000, 7_000, "Said after the ask.")
	agentUntil(t, guest, "the second ask", func(f agentFeed) bool { return f.Ask != nil })
	agentUntil(t, owner, "the second ask", func(f agentFeed) bool { return f.Ask != nil && f.Ask.Question == "Second private question?" })
	w2 := e.agentWaitFor(tok, c.ID, key)
	if want := []string{"Ada L.: Ship it Friday."}; w2.Status != "ask" || w2.First || !slices.Equal(w2.texts(), want) {
		t.Errorf("the second ask = %+v; want only the line since, %q", w2, want)
	}
	if g := agentSeen[guest].String(); strings.Contains(g, "private question") || strings.Contains(g, "budget line?") {
		t.Errorf("a guest's stream holds the owner's question:\n%s", g)
	}
	if !strings.Contains(agentSeen[owner].String(), "Second private question?") {
		t.Fatal("positive control: the owner's stream has no question")
	}

	// A reply to an ask that is not the open one is refused.
	if a := e.tool(tok, "meeting_agent_say", map[string]any{"id": c.ID, "agent_key": key, "ask_id": w.AskID, "text": "Different."}); !a.IsError {
		t.Errorf("a second, different answer to the first ask = %s; want refused", a.text())
	}
	e.ok(tok, "meeting_agent_say", map[string]any{"id": c.ID, "agent_key": key, "ask_id": w2.AskID, "text": "Friday works."}, nil)

	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	if w := e.agentWaitFor(tok, c.ID, key); w.Status != "ended" {
		t.Errorf("a wait after End = %+v, want ended", w)
	}
	var tl struct {
		AgentReplies []struct {
			Text     string `json:"text"`
			Question string `json:"question"`
		} `json:"agent_replies"`
	}
	e.ok(tok, "meeting_timeline", map[string]any{"id": c.ID}, &tl)
	if len(tl.AgentReplies) != 2 || tl.AgentReplies[0].Text != answer || tl.AgentReplies[1].Text != "Friday works." || tl.AgentReplies[0].Question != question {
		t.Errorf("the timeline's agent_replies = %+v", tl.AgentReplies)
	}
}

// 🔴 One agent per meeting through the door: a second session's join (its
// own token) is refused and names the agent in; Remove agent ends the
// first's wait ("ended") and lets the second join.
func TestASecondAgentIsRefusedUntilTheFirstIsRemoved(t *testing.T) {
	e := agentEnv(t)
	c := e.meeting(ada)
	e.started(c)
	t1, _ := e.mintMCP(c.Sess)
	t2, _ := e.mintMCP(c.Sess)
	k1 := e.agentJoin(t1, c.ID, "Builder")
	a := e.tool(t2, "meeting_agent_join", map[string]any{"id": c.ID, "name": "Reviewer"})
	if !a.IsError || !strings.Contains(a.text(), "Another agent (Builder) is already in this meeting") {
		t.Fatalf("a second agent's join = %v %q; want refused naming Builder", a.IsError, a.text())
	}
	if again := e.agentJoin(t1, c.ID, "Builder"); again != k1 {
		t.Errorf("a retried join answered another key")
	}
	if resp, raw := e.do("DELETE", "/api/meetings/"+c.ID+"/agent", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("remove = %d %s", resp.StatusCode, raw)
	}
	if w := e.agentWaitFor(t1, c.ID, k1); w.Status != "ended" {
		t.Errorf("the removed agent's wait = %+v, want ended", w)
	}
	if k2 := e.agentJoin(t2, c.ID, "Reviewer"); k2 == "" || k2 == k1 {
		t.Errorf("the second agent's join after Remove = %q", k2)
	}
	if resp, _ := e.do("DELETE", "/api/meetings/"+c.ID+"/agent", c.Sess, nil); resp.StatusCode != 200 {
		t.Errorf("remove the second = %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "/api/meetings/"+c.ID+"/agent", c.Sess, nil); resp.StatusCode != 404 {
		t.Errorf("remove with none in = %d, want 404", resp.StatusCode)
	}
}

// A wait that is waiting when the owner asks answers the ask at once, not
// at its deadline.
func TestAWaitingAgentGetsTheAskAtOnce(t *testing.T) {
	e := newEnvOpts(t, nil, func(c *server.Config) { c.AgentWait = 20 * time.Second; c.StreamRecheck = time.Hour })
	c := e.meeting(ada)
	e.started(c)
	tok, _ := e.mintMCP(c.Sess)
	key := e.agentJoin(tok, c.ID, "Builder")
	got := make(chan agentWait, 1)
	go func() {
		var w agentWait
		a := e.tool(tok, "meeting_agent_wait", map[string]any{"id": c.ID, "agent_key": key})
		_ = json.Unmarshal([]byte(a.text()), &w)
		got <- w
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if code, raw := e.ask(c, "Now?"); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	select {
	case w := <-got:
		if w.Status != "ask" || w.Question != "Now?" {
			t.Errorf("the wait = %+v", w)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("the wait answered %s after the ask: want at once", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting agent was not woken by the ask")
	}
}

// 🔴 An agent needs the meeting's AI switch on (its guests were told the
// transcript goes to an AI service only then); and a wait already waiting
// when its token is revoked delivers nothing: the ask that wakes it is
// refused with the revoked-token answer.
func TestAnAgentNeedsAIOnAndALiveToken(t *testing.T) {
	e := newEnvOpts(t, nil, func(c *server.Config) { c.AgentWait = 20 * time.Second })
	c := e.meeting(ada)
	e.started(c)
	tok, tid := e.mintMCP(c.Sess)
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]any{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("AI off = %d %s", resp.StatusCode, raw)
	}
	if a := e.tool(tok, "meeting_agent_join", map[string]any{"id": c.ID, "name": "Builder"}); !a.IsError || !strings.Contains(a.text(), "AI switch is off") {
		t.Errorf("join with AI off = %v %q; want refused", a.IsError, a.text())
	}
	d := e.meeting(ada)
	e.started(d)
	key := e.agentJoin(tok, d.ID, "Builder")
	got := make(chan toolAns, 1)
	go func() { got <- e.tool(tok, "meeting_agent_wait", map[string]any{"id": d.ID, "agent_key": key}) }()
	time.Sleep(200 * time.Millisecond)
	if resp, raw := e.do("DELETE", "/api/settings/mcp/"+tid, c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("revoke = %d %s", resp.StatusCode, raw)
	}
	if code, raw := e.ask(d, "Secret plans?"); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	select {
	case a := <-got:
		if !a.IsError || !strings.Contains(a.text(), "This token was revoked") || strings.Contains(a.text(), "Secret plans") {
			t.Errorf("the revoked token's wait = %v %q; want the revoked-token refusal", a.IsError, a.text())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never answered")
	}
}

// 🔴 No word of a question, an answer or the agent's name reaches the log;
// the log names the tools, the meeting and the outcome.
func TestAgentWordsNeverReachTheLog(t *testing.T) {
	const name, question, answer = "Zorblatt helper", "Where is the Quibblequack file?", "The Snorkelwump draft holds it."
	e := agentEnv(t)
	c := e.meeting(ada)
	e.started(c)
	tok, _ := e.mintMCP(c.Sess)
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	key := e.agentJoin(tok, c.ID, name)
	if code, raw := e.ask(c, question); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	w := e.agentWaitFor(tok, c.ID, key)
	e.ok(tok, "meeting_agent_say", map[string]any{"id": c.ID, "agent_key": key, "ask_id": w.AskID, "text": answer}, nil)
	agentUntil(t, owner, "the answer", func(f agentFeed) bool { return len(f.Replies) == 1 })
	all := agentSeen[owner].String()
	for _, word := range []string{"Zorblatt", "Quibblequack", "Snorkelwump"} {
		if !strings.Contains(all, word) {
			t.Fatalf("positive control: the owner's stream has no %q", word)
		}
	}
	lg := e.log.String()
	for _, word := range []string{"Zorblatt", "Quibblequack", "Snorkelwump", key} {
		if strings.Contains(lg, word) {
			t.Errorf("the log holds %q:\n%s", word, lg)
		}
	}
	for _, want := range []string{"mcp: tool meeting_agent_join meeting " + c.ID + ": ok in ", "mcp: tool meeting_agent_wait meeting " + c.ID + ": ok (ask) in ",
		"mcp: tool meeting_agent_say meeting " + c.ID + ": ok in "} {
		if !strings.Contains(lg, want) {
			t.Errorf("the log has no %q:\n%s", want, lg)
		}
	}
}
