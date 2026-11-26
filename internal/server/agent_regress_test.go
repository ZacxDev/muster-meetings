package server_test

// Regressions from the audit of the meeting's agent (PR #20 at 0704d8f):
// each was red there.

import (
	"slices"
	"strings"
	"testing"
)

// 🔴 The agent_key is what tells two sessions apart: a join while an agent
// is in, without that agent's key, is refused even with the same token and
// the same name (two Claude Code sessions sharing one token), and the
// refusal hands over neither the key nor the agent's name; a join WITH the
// key resumes the same agent.
func TestAJoinWithoutTheAgentsKeyIsRefused(t *testing.T) {
	e := agentEnv(t)
	c := e.meeting(ada)
	e.started(c)
	tok, _ := e.mintMCP(c.Sess)
	key := e.agentJoin(tok, c.ID, "Builder")
	second := e.tool(tok, "meeting_agent_join", map[string]any{"id": c.ID, "name": "Builder"})
	if !second.IsError || strings.Contains(second.text(), key) || strings.Contains(second.text(), "Builder") {
		t.Fatalf("a second session's join with the same token and name = %v %q; want refused, with no key and no name", second.IsError, second.text())
	}
	if !strings.Contains(second.text(), "An agent is already in this meeting") {
		t.Errorf("the refusal says %q", second.text())
	}
	var v struct {
		Key     string `json:"agent_key"`
		Already bool   `json:"already"`
	}
	e.ok(tok, "meeting_agent_join", map[string]any{"id": c.ID, "name": "Builder", "agent_key": key}, &v)
	if v.Key != key || !v.Already {
		t.Errorf("a join with the agent's key = %+v; want the same key, already", v)
	}
}

// 🔴 An ask cancelled before it was delivered (the AI switch went off) does
// not move the agent's cursor: the next ask carries every line the agent
// never received, as its first.
func TestACancelledAskLosesNoLines(t *testing.T) {
	e := agentEnv(t)
	c := e.meeting(ada)
	e.started(c)
	tok, _ := e.mintMCP(c.Sess)
	key := e.agentJoin(tok, c.ID, "Builder")
	e.say(c, "owner", "Ada L.", 1_000, 2_000, "Line A before the cancelled ask.")
	if code, raw := e.ask(c, ""); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]any{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("AI off = %d %s", resp.StatusCode, raw)
	}
	if w := e.agentWaitFor(tok, c.ID, key); w.Status != "pending" {
		t.Fatalf("the wait after AI off = %+v, want pending (the ask cancelled)", w)
	}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]any{"on": true}); resp.StatusCode != 200 {
		t.Fatalf("AI on = %d %s", resp.StatusCode, raw)
	}
	e.say(c, "owner", "Ada L.", 3_000, 4_000, "Line B after it.")
	if code, raw := e.ask(c, ""); code != 201 {
		t.Fatalf("ask = %d %s", code, raw)
	}
	w := e.agentWaitFor(tok, c.ID, key)
	if want := []string{"Ada L.: Line A before the cancelled ask.", "Ada L.: Line B after it."}; w.Status != "ask" || !w.First || !slices.Equal(w.texts(), want) {
		t.Errorf("the ask after a cancelled one = %+v; want first and %q", w, want)
	}
}
