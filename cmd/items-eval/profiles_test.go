package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// A recap segment two labels cite tells neither apart: an item citing its
// own statement and the recap matches its own label only. By hand:
//
//	R: RA [1 9] Ada, RB [4 9] Bo (9 is the recap)   own: RA {1}, RB {4}
//	   e1 [1 9] → RA; e2 [4 9] → RB; e3 [9] → nothing
//	S: SA [5 6], SB [5 6] (every segment shared: each keeps all)
//	   e4 [6] → SA and SB
//
//	labels 4, matched 4 → recall 1; emitted 4, matching e1 e2 e4 → 0.75;
//	no label matched twice → duplicates 0; assignee right on e1, e2 and e4
//	(no assignee on either side) → 3.
//
// Matched on any shared segment instead, e1, e2 and e3 each match RA and RB:
// duplicates 2/4, precision 1.
func TestARecapSegmentMatchesNoLabelItRestates(t *testing.T) {
	r := &Meeting{ID: "R", Labels: []Label{{ID: "RA", Assignee: "Ada", Segments: []int64{1, 9}}, {ID: "RB", Assignee: "Bo", Segments: []int64{4, 9}}}}
	s := &Meeting{ID: "S", Labels: []Label{{ID: "SA", Segments: []int64{5, 6}}, {ID: "SB", Segments: []int64{5, 6}}}}
	m := Score([]MeetingRun{
		{Meeting: r, Emitted: []items.Item{{Assignee: "Ada", Segments: []int64{1, 9}}, {Assignee: "Bo", Segments: []int64{4, 9}}, {Segments: []int64{9}}}},
		{Meeting: s, Emitted: []items.Item{{Segments: []int64{6}}}},
	})
	if m.MatchedLabels != 4 || m.MatchingEmitted != 3 || m.DupLabels != 0 || m.AssigneeRight != 3 {
		t.Errorf("matched %d, matching %d, duplicated %d, assignee right %d; want 4 3 0 3: a recap segment matched a label it only restates",
			m.MatchedLabels, m.MatchingEmitted, m.DupLabels, m.AssigneeRight)
	}
}

// fakeOR is a fake OpenRouter answering each request with the next of
// answers (a full chat completion body), recording the bodies.
type fakeOR struct {
	mu      sync.Mutex
	bodies  []map[string]any
	answers []string
}

func (f *fakeOR) serve(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		_, _ = io.WriteString(w, f.answers[min(n, len(f.answers)-1)])
	}))
	t.Cleanup(s.Close)
	return s
}

func completion(content any, finish string, completionTokens, reasoning int, cost float64) string {
	b, _ := json.Marshal(map[string]any{"model": "deepseek/deepseek-v4.1-flash-1017", "provider": "FakeProvider",
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": finish}},
		"usage": map[string]any{"prompt_tokens": 700, "completion_tokens": completionTokens, "cost": cost,
			"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}}})
	return string(b)
}

// handMeeting is five 60 s segments (ends 60.0, 121.2, 182.4, 243.6 and
// 304.8 s: 1.2 s apart) and three labels.
func handMeeting() *Meeting {
	m := &Meeting{ID: "H", Speakers: []Speaker{{Name: "Ada"}, {Name: "Bo"}, {Name: "Cy"}},
		Segments: []CSeg{
			{ID: 1, Speaker: "Ada", Text: "Bo, can you send the invoice by Friday?", Dur: 60},
			{ID: 2, Speaker: "Bo", Text: "Yes, I'll send it.", Dur: 60},
			{ID: 3, Speaker: "Cy", Text: "I'll book the room.", Dur: 60},
			{ID: 4, Speaker: "Ada", Text: "We could maybe repaint the office.", Dur: 60},
			{ID: 5, Speaker: "Bo", Text: "And I will call the vendor tomorrow.", Dur: 60},
		},
		Labels: []Label{
			{ID: "H1", Assignee: "Bo", Segments: []int64{1, 2}},
			{ID: "H2", Assignee: "Cy", Segments: []int64{3}},
			{ID: "H3", Assignee: "Bo", Segments: []int64{5}},
		}}
	m.build()
	return m
}

// The post-call pass against a fake OpenRouter, scored by hand. The meeting
// fits one chunk, so the pass is one request; its first answer is cut off at
// max_tokens (a schema failure that still reports its cost), the retry
// answers six items, and the guards keep three:
//
//	"Send the invoice" Bo [1 2]           → H1, assignee right       kept
//	"Send the invoice to the client" [2]  → duplicate-citation       dropped
//	"Book the room" Ada [3]               → H2, assignee wrong       kept
//	"Repaint the office" [4]              → no label (a hypothetical) kept
//	"Wire the money" Mallory [5]          → bad-owner                dropped
//	"Call the vendor" [5 99]              → out-of-window            dropped
//
//	labels 3, matched H1 H2 → recall 2/3; emitted 3, matching 2 → precision
//	2/3; duplicates 0; assignee 1/2. Calls 2, schema 1 → 50%. Cost $0.0003 +
//	$0.0005 = $0.0008, known, over 304.8 s → $0.0008 × 3600/304.8 =
//	$0.0094488… per meeting-hour.
func TestThePostCallPassScoredByHandAgainstAFakeOpenRouter(t *testing.T) {
	answer := `{"items":[` +
		`{"task":"Send the invoice","owner":"Bo","due":"Friday","segment_ids":[1,2]},` +
		`{"task":"Send the invoice to the client","owner":"Bo","due":"","segment_ids":[2]},` +
		`{"task":"Book the room","owner":"Ada","due":"","segment_ids":[3]},` +
		`{"task":"Repaint the office","owner":"","due":"","segment_ids":[4]},` +
		`{"task":"Wire the money","owner":"Mallory","due":"","segment_ids":[5]},` +
		`{"task":"Call the vendor","owner":"Bo","due":"tomorrow","segment_ids":[5,99]}]}`
	f := &fakeOR{answers: []string{completion(nil, "length", 8192, 8000, 0.0003), completion(answer, "stop", 300, 120, 0.0005)}}
	srv := f.serve(t)
	p, _ := items.ParsePrefs(`{"order":["deepinfra/fp8"],"allow_fallbacks":false}`, items.AdapterChatJSON)
	a, err := items.New(items.PostCallConfig(items.Config{Model: "deepseek/deepseek-v4.1-flash-1017", Adapter: items.AdapterChatJSON, Prefs: p,
		APIKey: "sk-test", BaseURL: srv.URL + "/api/v1"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := handMeeting()
	if m.DurationMS() != 304800 {
		t.Fatalf("the fixture's meeting is %d ms, want 304800", m.DurationMS())
	}
	run := ReplayPostCall(context.Background(), a, m, ReplayOpts{})
	if len(f.bodies) != 2 || f.bodies[1]["max_tokens"] != float64(8192) {
		t.Fatalf("%d requests, max_tokens %v: want 2 (the cut-off answer retried) at 8192", len(f.bodies), f.bodies[len(f.bodies)-1]["max_tokens"])
	}
	if w := run.Windows[0]; w.Class != "schema" || w.FinishReason != "length" || w.CompletionTokens != 8192 || w.ReasoningTokens != 8000 || !w.CostKnown || w.Provider != "FakeProvider" {
		t.Errorf("the cut-off call is recorded as %+v: want schema, length, 8192 tokens (8000 reasoning), its cost and provider kept", w)
	}
	sc := Score([]MeetingRun{run})
	got := fmt.Sprintf("recall %.4f precision %.4f dup %.2f assignee %.2f calls %d schema %.2f cost %.4f known %v per-hour %.7f",
		sc.Recall, sc.Precision, sc.DupRate, sc.Assignee, sc.Calls, sc.SchemaRate, sc.CostUSD, sc.CostKnown, sc.CostPerHour)
	want := "recall 0.6667 precision 0.6667 dup 0.00 assignee 0.50 calls 2 schema 0.50 cost 0.0008 known true per-hour 0.0094488"
	if got != want {
		t.Errorf("the post-call pass scored\n got %s\nwant %s", got, want)
	}
}

// A model passing only the post-call profile (p95 30 s: over live's 10 s,
// under post-call's 60 s) is chosen for postcall and `off` for live; a
// model judged only live is never read for postcall.
func TestEachProfileChoosesOnItsOwnVerdicts(t *testing.T) {
	slow := Metrics{Recall: 0.85, Precision: 0.93, DupRate: 0.03, Assignee: 1, Calls: 144, P95: 30 * time.Second, SchemaRate: 0.005, CostKnown: true, CostPerHour: 0.02}
	fast := slow
	fast.Recall, fast.P95 = 0.99, 2*time.Second
	dups := slow
	dups.DupRate, dups.P95 = 0.41, time.Second
	ds, sd, other := "deepseek/deepseek-v4.1-flash-1017", "scaledown/extract", "other/model-0101"
	run := []Scored{
		NewScored(ds, items.AdapterChatJSON, profileLive, nil, []Metrics{slow}, []Metrics{slow}),
		NewScored(ds, items.AdapterChatJSON, profilePostCall, nil, []Metrics{slow}, []Metrics{slow}),
		NewScored(sd, items.AdapterExtract, profileLive, nil, []Metrics{dups}, []Metrics{dups}),
		NewScored(sd, items.AdapterExtract, profilePostCall, nil, []Metrics{dups}, []Metrics{dups}),
	}
	if run[0].Pass || !run[1].Pass || run[2].Pass || run[3].Pass {
		t.Fatalf("the fixture: deepseek live %v postcall %v, scaledown live %v postcall %v; want only deepseek postcall passing",
			run[0].Pass, run[1].Pass, run[2].Pass, run[3].Pass)
	}
	if got := Choose(run, profileLive); got != "off" {
		t.Errorf("live = %s, want off: the only passing verdict is post-call's", got)
	}
	if got := Choose(run, profilePostCall); got != ds {
		t.Errorf("postcall = %s, want %s", got, ds)
	}
	// A better model judged live only: chosen live, never for postcall.
	run = append(run, NewScored(other, items.AdapterChatJSON, profileLive, nil, []Metrics{fast}, []Metrics{fast}))
	if got := Choose(run, profileLive); got != other {
		t.Errorf("live with a passing live model = %s, want %s", got, other)
	}
	if got := Choose(run, profilePostCall); got != ds {
		t.Errorf("postcall with a better live-only model = %s, want %s: a live verdict was read for postcall", got, ds)
	}
}

// The post-call controls gate the run as the live ones do. m01 (2.44
// minutes: a cap of 10) is given 11 labels, in both its variants: the live
// oracle still reaches every one, a window at a time, but the post-call
// oracle proposes all 11 in its one call and the meeting's cap drops one, so
// only the post-call oracle fails, and no model may be called.
func TestAFailedPostCallControlStopsTheRunBeforeAnyModel(t *testing.T) {
	const m02 = `  - {id: m01-2, text: "Renew the staging certificate", assignee: Dev, due: "this morning", segments: [105, 106, 114, 115], cases: [cross_speaker, restated]}`
	more := `  - {id: m01-2, text: "Renew the staging certificate", assignee: Dev, due: "this morning", segments: [105, 106], cases: [cross_speaker]}
  - {id: m01-2b, text: "Confirm the certificate renewal", assignee: Dev, segments: [114, 115], cases: [cross_speaker]}`
	for i, seg := range []int{99, 100, 101, 104, 107, 112, 116} {
		more += fmt.Sprintf("\n  - {id: m01-x%d, text: \"Extra label %c%c%c\", assignee: Ada, segments: [%d]}", i, 'k'+i, 'q'+i, 'c'+i, seg)
	}
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	for _, src := range files {
		b, _ := os.ReadFile(src)
		if n := filepath.Base(src); n == "m01-standup.yaml" || n == "m01-standup.stt.yaml" {
			if !bytes.Contains(b, []byte(m02)) {
				t.Fatalf("%s has no label m01-2 as the fixture expects", n)
			}
			b = bytes.Replace(b, []byte(m02), []byte(more), 1)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ms, err := LoadCorpus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := Lint(ms); len(p) > 0 || len(ms[0].Labels) != 11 || items.PostCallCap(ms[0].Timeline()) != 10 {
		t.Fatalf("the fixture: lint %v, %d labels, cap %d; want a clean lint, 11 labels, a cap of 10", p, len(ms[0].Labels), items.PostCallCap(ms[0].Timeline()))
	}
	f := &fakeRouter{}
	srv := f.serve(t)
	var so, se bytes.Buffer
	code := run([]string{"-corpus", dir, "-runs", "1", "-out", filepath.Join(t.TempDir(), "r.json"), "-base", srv.URL + "/api/v1", "-models", "scaledown/extract"},
		func(k string) string {
			if k == "OPENROUTER_API_KEY" {
				return "sk-test"
			}
			return ""
		}, &so, &se)
	live, post, _ := strings.Cut(so.String(), "controls, postcall:")
	if strings.Contains(live, "FAIL") || !strings.Contains(post, "oracle                         FAIL") {
		t.Fatalf("the fixture: want every live control passing and the post-call oracle failing:\n%s", so.String())
	}
	if code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), "a control did not score as stated") {
		t.Errorf("a failing post-call oracle: exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
}

func TestProfilesFlag(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"live,postcall", "[live postcall]"},
		{" postcall , live,postcall", "[postcall live]"},
		{"postcall", "[postcall]"},
		{"live,batch", `-profiles: unknown profile "batch" (want live, postcall)`},
		{" , ", "-profiles: no profile"},
	} {
		p, err := parseProfiles(c.in)
		got := fmt.Sprint(p)
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("parseProfiles(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// -profiles postcall runs the post-call pass only: one request per meeting
// per corpus (every corpus meeting fits one chunk), each at the post-call
// max_tokens, and the choice for postcall alone; an unknown profile stops
// the run before any request.
func TestARunOfOneProfile(t *testing.T) {
	f := &fakeRouter{}
	srv := f.serve(t)
	env := func(k string) string {
		if k == "OPENROUTER_API_KEY" {
			return "sk-test"
		}
		return ""
	}
	args := func(prof string) []string {
		return []string{"-corpus", corpusDir, "-runs", "1", "-out", filepath.Join(t.TempDir(), "r.json"), "-base", srv.URL + "/api/v1",
			"-models", "deepseek/deepseek-v4.1-flash-1017", "-profiles", prof,
			"-prefs", `deepseek/deepseek-v4.1-flash-1017={"order":["deepinfra/fp8"],"allow_fallbacks":false}`}
	}
	var so, se bytes.Buffer
	if code := run(args("live,batch"), env, &so, &se); code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), `unknown profile "batch"`) {
		t.Fatalf("an unknown profile: exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
	so.Reset()
	se.Reset()
	if code := run(args("postcall"), env, &so, &se); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	meetings := len(load(t)) + len(loadSTT(t))
	if len(f.bodies) != meetings {
		t.Errorf("%d requests, want one per meeting per corpus (%d)", len(f.bodies), meetings)
	}
	for _, b := range f.bodies {
		if b["max_tokens"] != float64(8192) {
			t.Fatalf("a post-call request at max_tokens %v, want 8192", b["max_tokens"])
		}
	}
	out := so.String()
	if strings.Contains(out, "controls, live") || !strings.Contains(out, "controls, postcall, stt corpus:") ||
		!strings.Contains(out, "**Choices:** postcall `off`.") || strings.Contains(out, "Profile `live`") {
		t.Errorf("stdout of a postcall-only run:\n%s", out)
	}
}
