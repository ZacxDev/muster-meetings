package server_test

// 🔴 No model, no mail server, no change. With no OPENROUTER_API_KEY (the
// items and vision models off) and no MEETINGS_SMTP_ADDR, the plugin
// behaves exactly as before the post-call feature: the owner's and a
// guest's notice, the AI switch's answers, the guest's ended event and its
// card (contacts configured), the card's answer, End's answer, and every
// route the feature adds (owner-authenticated and not), are compared with
// goldens TAKEN AT THE PRE-CHANGE COMMIT (5ce6f50; passed again on b0af057,
// main after #16, before any change): this file uses only what
// that commit had, and was run there with -update-noai to write
// testdata/noai.golden.json. An equality to the base cannot be red at the
// base; the mutation rows "no AI: …" supply the failing case.

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateNoAI = flag.Bool("update-noai", false, "rewrite testdata/noai.golden.json (run at the pre-change commit only)")

func noAISurface(t *testing.T) map[string]any {
	e, _ := newCardEnv(t)
	c := e.meeting(ada)
	norm := func(a answer) answer {
		a.Body = strings.ReplaceAll(a.Body, c.ID, "{meeting}")
		return a
	}
	out := map[string]any{}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("owner token = %d %s", resp.StatusCode, raw)
	}
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	var snap struct {
		Notice json.RawMessage `json:"notice"`
	}
	json.Unmarshal([]byte(owner.want("meeting").Data), &snap)
	out["owner_notice"] = string(snap.Notice)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": false})
	out["ai off"] = norm(answerOf(resp, raw))
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": true})
	out["ai on"] = norm(answerOf(resp, raw))
	tk := e.knock(c.Code, grace)
	e.admit(c, grace)
	ev := e.guestEvents(c.Code, tk)
	out["guest_meeting_event"] = ev.want("meeting").Data
	if a := e.server.Accepting(); a != nil {
		a.Wait()
	}
	tok := strings.Repeat("A", 43)
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/meetings/" + c.ID + "/analysis"},
		{"POST", "/api/meetings/" + c.ID + "/reprocess"},
		{"POST", "/api/meetings/" + c.ID + "/analysis/runs/1/current"},
		{"GET", "/api/meetings/" + c.ID + "/drafts"},
		{"PATCH", "/api/meetings/" + c.ID + "/drafts/" + tok},
		{"POST", "/api/meetings/" + c.ID + "/drafts/send"},
		{"POST", "/api/meetings/" + c.ID + "/agent-token"},
		{"GET", "/api/agent/meetings/" + c.ID + "/timeline"},
		{"GET", "/api/settings/email"}, {"PUT", "/api/settings/email"},
		{"GET", "/e/" + tok}, {"POST", "/e/" + tok},
	} {
		var body any
		if p.method != "GET" {
			body = map[string]string{}
		}
		resp, raw := e.do(p.method, p.path, c.Sess, body)
		out[p.method+" "+strings.ReplaceAll(p.path, c.ID, "{meeting}")+" (owner)"] = norm(answerOf(resp, raw))
		resp, raw = e.do(p.method, p.path, "", body)
		out[p.method+" "+strings.ReplaceAll(p.path, c.ID, "{meeting}")+" (no session)"] = norm(answerOf(resp, raw))
	}
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	out["end"] = norm(answerOf(resp, raw))
	rawEnded := ev.want("ended").Data
	var ended endedEv
	if err := json.Unmarshal([]byte(rawEnded), &ended); err != nil || ended.Card == nil {
		t.Fatalf("ended = %s (%v): no card", rawEnded, err)
	}
	out["guest_ended_event"] = strings.ReplaceAll(rawEnded, ended.Card.ID, "{card}")
	resp, raw = e.do("POST", "/card", "", map[string]string{"id": ended.Card.ID, "name": grace, "email": "grace.h@example.test"})
	out["card"] = answerOf(resp, raw)
	return out
}

func TestNoModelAndNoMailChangeNothing(t *testing.T) {
	got := noAISurface(t)
	b, _ := json.MarshalIndent(got, "", "  ")
	b = append(b, '\n')
	const golden = "testdata/noai.golden.json"
	if *updateNoAI {
		if err := os.WriteFile(golden, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(want) {
		t.Errorf("with no model and no mail server the surface changed from the pre-change commit's:\ngot:\n%s\nwant (%s):\n%s", b, golden, want)
	}
}
