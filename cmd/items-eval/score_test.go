package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// The scoring fixture, with every expected number computed by hand below.
// Meeting B reuses segment ids from meeting A on purpose: matching is in the
// same meeting only.
//
//	A: LA1 [1 2] Ada, LA2 [5] Bo, LA3 [7] Cy      B: LB1 [1] Dee
//	A emitted: e1 [2] "ada"  → LA1, assignee right (case ignored)
//	           e2 [1] "Bo"   → LA1 again (a duplicate), assignee wrong
//	           e3 [9] "Ada"  → nothing
//	           e4 [5 7] "Cy" → LA2 and LA3, right (Cy is LA3's)
//	B emitted: e5 [5] "Dee"  → nothing (5 is a label only in A)
//
//	labels 4, matched LA1 LA2 LA3 = 3        recall    3/4 = 0.75
//	emitted 5, matching e1 e2 e4 = 3         precision 3/5 = 0.60
//	labels with >1 match: LA1 = 1            dup rate  1/4 = 0.25
//	matching with the right assignee e1 e4   assignee  2/3
//	calls 100 300 2000 (schema) | 400 (retry) 5000 ms
//	  nearest rank: p50 = 3rd of 5 = 400 ms, p95 = 5th = 5 s
//	schema 1/5 = 0.20; cost 0.001+0.002+0.003+0.004 = 0.010 (the retry
//	reported none and is not counted); 30 + 30 min = 1 h → $0.010 / h
func scoringFixture() []MeetingRun {
	a := &Meeting{ID: "A", Labels: []Label{
		{ID: "LA1", Assignee: "Ada", Segments: []int64{1, 2}},
		{ID: "LA2", Assignee: "Bo", Segments: []int64{5}},
		{ID: "LA3", Assignee: "Cy", Segments: []int64{7}},
	}}
	b := &Meeting{ID: "B", Labels: []Label{{ID: "LB1", Assignee: "Dee", Segments: []int64{1}}}}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	return []MeetingRun{
		{Meeting: a, MeetingID: "A", DurationMS: 30 * 60 * 1000,
			Emitted: []items.Item{
				{Assignee: "ada", Segments: []int64{2}},
				{Assignee: "Bo", Segments: []int64{1}},
				{Assignee: "Ada", Segments: []int64{9}},
				{Assignee: "Cy", Segments: []int64{5, 7}},
			},
			Windows: []WindowRun{
				{Elapsed: ms(100), Provider: "P1", CostUSD: 0.001, CostKnown: true},
				{Elapsed: ms(300), Provider: "P1", CostUSD: 0.002, CostKnown: true, Rewritten: 2},
				{Elapsed: ms(2000), Provider: "P2", CostUSD: 0.003, CostKnown: true, Class: "schema"},
			}},
		{Meeting: b, MeetingID: "B", DurationMS: 30 * 60 * 1000,
			Emitted: []items.Item{{Assignee: "Dee", Segments: []int64{5}}},
			Windows: []WindowRun{
				{Elapsed: ms(400), Class: "retry"},
				{Elapsed: ms(5000), Provider: "P1", CostUSD: 0.004, CostKnown: true},
			}},
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestScoreAgainstAHandComputedFixture(t *testing.T) {
	m := Score(scoringFixture())
	if m.Labels != 4 || m.Emitted != 5 || m.MatchedLabels != 3 || m.MatchingEmitted != 3 || m.DupLabels != 1 || m.AssigneeRight != 2 {
		t.Errorf("counts = labels %d emitted %d matched %d matching %d dup %d assignee %d; want 4 5 3 3 1 2",
			m.Labels, m.Emitted, m.MatchedLabels, m.MatchingEmitted, m.DupLabels, m.AssigneeRight)
	}
	if !near(m.Recall, 0.75) {
		t.Errorf("recall = %v, want 0.75", m.Recall)
	}
	if !near(m.Precision, 0.60) {
		t.Errorf("precision = %v, want 0.60 (an item matching in another meeting does not count)", m.Precision)
	}
	if !near(m.DupRate, 0.25) {
		t.Errorf("duplicate rate = %v, want 0.25", m.DupRate)
	}
	if !near(m.Assignee, 2.0/3.0) {
		t.Errorf("assignee accuracy = %v, want 2/3", m.Assignee)
	}
	if m.Calls != 5 || m.SchemaFailures != 1 || m.RetryFailures != 1 || !near(m.SchemaRate, 0.20) {
		t.Errorf("calls %d schema %d retry %d rate %v, want 5 1 1 0.20", m.Calls, m.SchemaFailures, m.RetryFailures, m.SchemaRate)
	}
	if m.P50 != 400*time.Millisecond || m.P95 != 5*time.Second {
		t.Errorf("p50 %v p95 %v, want 400ms 5s", m.P50, m.P95)
	}
	if !m.CostKnown || !near(m.CostUSD, 0.010) || !near(m.Hours, 1) || !near(m.CostPerHour, 0.010) {
		t.Errorf("cost %v known %v hours %v per hour %v, want 0.010 true 1 0.010", m.CostUSD, m.CostKnown, m.Hours, m.CostPerHour)
	}
	if m.Providers["P1"] != 3 || m.Providers["P2"] != 1 || m.Rewritten != 2 {
		t.Errorf("providers %v rewritten %d", m.Providers, m.Rewritten)
	}
}

func TestACostOpenRouterDidNotReportFailsTheGate(t *testing.T) {
	runs := scoringFixture()
	runs[1].Windows[1].CostKnown = false
	m := Score(runs)
	for _, g := range Gates(m, items.AdapterChatJSON) {
		if g.Name == "cost per meeting-hour" && (g.Pass || g.Value != "unknown") {
			t.Errorf("an unreported cost: gate %+v, want it failed as unknown", g)
		}
	}
}

func TestGatesAtTheirBars(t *testing.T) {
	ok := Metrics{Recall: 0.80, Precision: 0.80, DupRate: 0.05, Assignee: 0.85, Calls: 100, P95: 4 * time.Second, SchemaRate: 0.01, CostKnown: true, CostPerHour: 0.05}
	if g := Gates(ok, items.AdapterChatJSON); !Passes(g) {
		t.Errorf("every metric exactly at its bar must pass: %+v", g)
	}
	bad := map[string]func(*Metrics){
		"recall":                func(m *Metrics) { m.Recall = 0.79 },
		"precision":             func(m *Metrics) { m.Precision = 0.79 },
		"duplicate rate":        func(m *Metrics) { m.DupRate = 0.06 },
		"assignee accuracy":     func(m *Metrics) { m.Assignee = 0.84 },
		"latency p95":           func(m *Metrics) { m.P95 = 4*time.Second + time.Millisecond },
		"schema failures":       func(m *Metrics) { m.SchemaRate = 0.011 },
		"cost per meeting-hour": func(m *Metrics) { m.CostPerHour = 0.051 },
		"fatal failures":        func(m *Metrics) { m.FatalFailures = 1 },
	}
	for name, f := range bad {
		m := ok
		f(&m)
		gs := Gates(m, items.AdapterChatJSON)
		if Passes(gs) {
			t.Errorf("%s just past its bar: the run passed", name)
		}
		for _, g := range gs {
			if g.Name != name && !g.Pass {
				t.Errorf("%s past its bar also failed %s", name, g.Name)
			}
		}
	}
	m := ok
	m.Assignee = 0.5
	if !Passes(Gates(m, items.AdapterExtract)) {
		t.Error("assignee accuracy is reported, not a gate, for extract")
	}
}

func TestWorstIsEachMetricsWorstRun(t *testing.T) {
	w := Worst([]Metrics{
		{Recall: 0.9, Precision: 0.7, DupRate: 0.01, P95: time.Second, CostPerHour: 0.02, CostKnown: true},
		{Recall: 0.8, Precision: 0.95, DupRate: 0.03, P95: 3 * time.Second, CostPerHour: 0.01, CostKnown: true},
		{Recall: 0.85, Precision: 0.9, DupRate: 0.02, P95: 2 * time.Second, CostPerHour: 0.03, CostKnown: false},
	})
	if w.Recall != 0.8 || w.Precision != 0.7 || w.DupRate != 0.03 || w.P95 != 3*time.Second || w.CostPerHour != 0.03 || w.CostKnown {
		t.Errorf("worst = %+v", w)
	}
}

func TestChoose(t *testing.T) {
	s := func(model string, pass bool, recall, cost float64) Scored {
		return Scored{Model: model, Pass: pass, Worst: Metrics{Recall: recall, CostPerHour: cost}}
	}
	cases := []struct {
		ss   []Scored
		want string
	}{
		{[]Scored{s("scaledown/extract", true, 0.85, 0.005), s("deepseek/x", true, 0.92, 0.03)}, "deepseek/x"},
		{[]Scored{s("scaledown/extract", true, 0.90, 0.005), s("deepseek/x", true, 0.90, 0.03)}, "scaledown/extract"},
		{[]Scored{s("deepseek/x", true, 0.90, 0.03), s("scaledown/extract", true, 0.90, 0.005)}, "scaledown/extract"},
		{[]Scored{s("scaledown/extract", true, 0.81, 0.005), s("deepseek/x", false, 0.99, 0.03)}, "scaledown/extract"},
		{[]Scored{s("scaledown/extract", false, 0.95, 0.005), s("deepseek/x", false, 0.99, 0.03)}, "off"},
	}
	for i, c := range cases {
		if got := Choose(c.ss); got != c.want {
			t.Errorf("case %d: Choose = %s, want %s", i, got, c.want)
		}
	}
}

func TestTablesNameTheModelTheProviderAndTheChoice(t *testing.T) {
	m := Score(scoringFixture())
	res := Result{Date: "2026-11-24", Choice: "off", Models: []Scored{{Model: "deepseek/deepseek-v4.1-flash-1017", Adapter: items.AdapterChatJSON,
		Prefs: map[string]any{"data_collection": "deny"}, Runs: []Metrics{m}, Worst: m, Gates: Gates(m, items.AdapterChatJSON)}}}
	out := Tables(res)
	for _, want := range []string{"# Action-item evaluation, 2026-11-24", "`deepseek/deepseek-v4.1-flash-1017` (chat_json)", `{"data_collection":"deny"}`,
		"P1 (3), P2 (1)", "| recall | 0.75 | ≥ 0.80 | **no** |", "**Choice:** `off`."} {
		if !strings.Contains(out, want) {
			t.Errorf("the table has no %q:\n%s", want, out)
		}
	}
}

func TestARewrittenSpanInTheNonASCIIMeetingFailsExtract(t *testing.T) {
	runs := scoringFixture()
	runs[1].Meeting.Unicode = true // meeting B; A's 2 rewrites are not in a non-ASCII meeting
	runs[1].Windows[1].Rewritten = 1
	m := Score(runs)
	if m.RewrittenUnicode != 1 || m.Rewritten != 3 {
		t.Fatalf("rewritten %d, in the non-ASCII meeting %d; want 3 and 1", m.Rewritten, m.RewrittenUnicode)
	}
	gate := func(adapter string) Gate {
		for _, g := range Gates(m, adapter) {
			if g.Name == "rewritten spans, non-ASCII meeting" {
				return g
			}
		}
		t.Fatal("no rewritten-span gate")
		return Gate{}
	}
	if g := gate(items.AdapterExtract); g.Pass || !g.Gating {
		t.Errorf("a rewritten span in the non-ASCII meeting: extract gate %+v, want a failing gate", g)
	}
	if g := gate(items.AdapterChatJSON); g.Gating {
		t.Errorf("chat_json has no offsets: gate %+v, want it reported, not gating", g)
	}
}
