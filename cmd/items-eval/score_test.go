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
//	A emitted: e1 [2] "ada"  can match LA1
//	           e2 [1] "Bo"   can match LA1
//	           e3 [9] "Ada"  can match nothing
//	           e4 [5 7] "Cy" can match LA2 or LA3
//	B emitted: e5 [5] "Dee"  can match nothing (5 is a label only in A)
//
//	One to one, a maximum matching is 2 pairs. e1 and e2 tie for LA1 (each
//	shares one own segment): the earlier item, e1. e4 ties LA2 and LA3: the
//	earlier label, LA2. e2 is a duplicate of LA1; e3 and e5 false positives.
//
//	labels 4, paired LA1 LA2 = 2             recall    2/4 = 0.50
//	emitted 5, paired e1 e4 = 2              precision 2/5 = 0.40
//	labels a duplicate is charged to: LA1    dup rate  1/4 = 0.25
//	pairs with the right assignee: e1–LA1 ("ada", case ignored); e4 is Cy,
//	LA2 is Bo                                assignee  1/2
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
	if m.Labels != 4 || m.Emitted != 5 || m.MatchedLabels != 2 || m.MatchingEmitted != 2 || m.DupLabels != 1 || m.DupItems != 1 || m.AssigneeRight != 1 {
		t.Errorf("counts = labels %d emitted %d matched %d matching %d dup %d (items %d) assignee %d; want 4 5 2 2 1 (1) 1",
			m.Labels, m.Emitted, m.MatchedLabels, m.MatchingEmitted, m.DupLabels, m.DupItems, m.AssigneeRight)
	}
	if !near(m.Recall, 0.50) {
		t.Errorf("recall = %v, want 0.50", m.Recall)
	}
	if !near(m.Precision, 0.40) {
		t.Errorf("precision = %v, want 0.40 (an item matching in another meeting does not count)", m.Precision)
	}
	if !near(m.DupRate, 0.25) {
		t.Errorf("duplicate rate = %v, want 0.25", m.DupRate)
	}
	if !near(m.Assignee, 0.5) {
		t.Errorf("assignee accuracy = %v, want 1/2", m.Assignee)
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
	for _, g := range Gates(m, items.AdapterChatJSON, profileLive) {
		if g.Name == "cost per meeting-hour" && (g.Pass || g.Value != "unknown") {
			t.Errorf("an unreported cost: gate %+v, want it failed as unknown", g)
		}
	}
}

// Every bar, exactly at it and just past it, under each profile: the
// quality bars are the same for both; the latency bar is 10 s live and 60 s
// post-call, written out here, not read from the code.
func TestGatesAtTheirBars(t *testing.T) {
	for _, pr := range []struct {
		profile string
		p95     time.Duration
		bar     string
	}{{profileLive, 10 * time.Second, "≤ 10s"}, {profilePostCall, 60 * time.Second, "≤ 60s"}} {
		ok := Metrics{Recall: 0.80, Precision: 0.80, DupRate: 0.05, Assignee: 0.85, Calls: 100, P95: pr.p95, SchemaRate: 0.01, CostKnown: true, CostPerHour: 0.05}
		if g := Gates(ok, items.AdapterChatJSON, pr.profile); !Passes(g) {
			t.Errorf("%s: every metric exactly at its bar must pass: %+v", pr.profile, g)
		}
		for _, g := range Gates(ok, items.AdapterChatJSON, pr.profile) {
			if g.Name == "latency p95" && g.Bar != pr.bar {
				t.Errorf("%s: the latency bar reads %q, want %q", pr.profile, g.Bar, pr.bar)
			}
		}
		bad := map[string]func(*Metrics){
			"recall":                func(m *Metrics) { m.Recall = 0.79 },
			"precision":             func(m *Metrics) { m.Precision = 0.79 },
			"duplicate rate":        func(m *Metrics) { m.DupRate = 0.06 },
			"assignee accuracy":     func(m *Metrics) { m.Assignee = 0.84 },
			"latency p95":           func(m *Metrics) { m.P95 = pr.p95 + time.Millisecond },
			"schema failures":       func(m *Metrics) { m.SchemaRate = 0.011 },
			"cost per meeting-hour": func(m *Metrics) { m.CostPerHour = 0.051 },
			"fatal failures":        func(m *Metrics) { m.FatalFailures = 1 },
		}
		for name, f := range bad {
			m := ok
			f(&m)
			gs := Gates(m, items.AdapterChatJSON, pr.profile)
			if Passes(gs) {
				t.Errorf("%s: %s just past its bar: the run passed", pr.profile, name)
			}
			for _, g := range gs {
				if g.Name != name && !g.Pass {
					t.Errorf("%s: %s past its bar also failed %s", pr.profile, name, g.Name)
				}
			}
		}
		m := ok
		m.Assignee = 0.5
		if !Passes(Gates(m, items.AdapterExtract, pr.profile)) {
			t.Errorf("%s: assignee accuracy is reported, not a gate, for extract", pr.profile)
		}
	}
	// The same p95, 30 s, between the two bars: post-call passes, live fails.
	m := Metrics{Recall: 1, Precision: 1, Assignee: 1, Calls: 12, P95: 30 * time.Second, CostKnown: true}
	if Passes(Gates(m, items.AdapterChatJSON, profileLive)) || !Passes(Gates(m, items.AdapterChatJSON, profilePostCall)) {
		t.Errorf("p95 30 s: live passes %v, postcall passes %v; want false and true",
			Passes(Gates(m, items.AdapterChatJSON, profileLive)), Passes(Gates(m, items.AdapterChatJSON, profilePostCall)))
	}
	if Passes(Gates(Metrics{Recall: 1, Precision: 1, Assignee: 1, Calls: 12, P95: 0, CostKnown: true}, items.AdapterChatJSON, "batch")) {
		t.Error("an unknown profile has no latency bar and must fail it")
	}
}

func TestWorstIsEachMetricsWorstRun(t *testing.T) {
	w := Worst([]Metrics{
		{Recall: 0.9, Precision: 0.7, DupRate: 0.01, P95: time.Second, CostPerHour: 0.02, CostKnown: true, Calls: 7},
		{Recall: 0.8, Precision: 0.95, DupRate: 0.03, P95: 3 * time.Second, CostPerHour: 0.01, CostKnown: true, Calls: 3},
		{Recall: 0.85, Precision: 0.9, DupRate: 0.02, P95: 2 * time.Second, CostPerHour: 0.03, CostKnown: false, Calls: 5},
	})
	// Calls: the fewest (3), so a corpus that made no call fails the
	// latency and schema gates even beside one that did.
	if w.Recall != 0.8 || w.Precision != 0.7 || w.DupRate != 0.03 || w.P95 != 3*time.Second || w.CostPerHour != 0.03 || w.CostKnown || w.Calls != 3 {
		t.Errorf("worst = %+v", w)
	}
}

func TestChoose(t *testing.T) {
	s := func(model string, pass bool, recall, cost float64) Scored {
		// The choice reads the worst-of-both numbers: each corpus alone says
		// the opposite.
		inv := Verdict{Worst: Metrics{Recall: 1 - recall, CostPerHour: 1 - cost}}
		return Scored{Model: model, Profile: profileLive, Pass: pass, Combined: Verdict{Worst: Metrics{Recall: recall, CostPerHour: cost}}, STT: inv, Clean: inv}
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
		if got := Choose(c.ss, profileLive); got != c.want {
			t.Errorf("case %d: Choose = %s, want %s", i, got, c.want)
		}
	}
}

func TestTablesNameTheModelTheProviderAndTheChoice(t *testing.T) {
	m := Score(scoringFixture())
	p := Score(passingFixture())
	const ds = "deepseek/deepseek-v4.1-flash-1017"
	deny := map[string]any{"data_collection": "deny"}
	res := Result{Date: "2026-11-24", Profiles: []string{profileLive, profilePostCall},
		Choices: map[string]string{profileLive: "off", profilePostCall: ds},
		Models: []Scored{
			NewScored(ds, items.AdapterChatJSON, profileLive, deny, []Metrics{p}, []Metrics{m}),
			NewScored(ds, items.AdapterChatJSON, profilePostCall, deny, []Metrics{p}, []Metrics{p}),
		},
		STTMeetings: []STTMeeting{{"m01", "nemotron", 0.1, 100, "items-corpus-stt 1"}, {"m02", "nemotron", 0.4, 300, "items-corpus-stt 1"}}}
	out := Tables(res)
	for _, want := range []string{"# Action-item evaluation, 2026-11-24",
		"## Profile `live`: the in-call panel", "## Profile `postcall`: one pass over the whole meeting after End",
		"### `deepseek/deepseek-v4.1-flash-1017` (chat_json), live, 1 runs", "### `deepseek/deepseek-v4.1-flash-1017` (chat_json), postcall, 1 runs",
		`{"data_collection":"deny"}`, "P1 (4), P2 (1)", "| metric | worst of both (the gate) | stt | clean | bar | pass |",
		"| recall | 0.50 | 0.50 | 1.00 | ≥ 0.80 | **no** |",
		// precision: clean 1.00, stt 0.40; the worst is the STT's.
		"| precision | 0.40 | 0.40 | 1.00 | ≥ 0.80 | **no** |",
		// the same 5 s p95 against each profile's bar
		"| latency p95 | 5s | 5s | 100ms | ≤ 10s | yes |", "| latency p95 | 100ms | 100ms | 100ms | ≤ 60s | yes |",
		"| calls, fewest in a run | 1 | 5 | 1 | — | — |",
		// (0.1·100 + 0.4·300) / 400 = 0.325
		"word error rate 0.325 over 400 words (engines: nemotron (2); per meeting: m01 0.100, m02 0.400)",
		"fails, live** (on its own, the STT corpus fails and the clean corpus passes)",
		"passes every gate on both corpora, postcall** (on its own, the STT corpus passes and the clean corpus passes)",
		"**Choice, live:** `off`.", "**Choice, postcall:** `deepseek/deepseek-v4.1-flash-1017`.",
		"**Choices:** live `off`, postcall `deepseek/deepseek-v4.1-flash-1017`."} {
		if !strings.Contains(out, want) {
			t.Errorf("the table has no %q:\n%s", want, out)
		}
	}
}

// passingFixture passes every gate, by hand: one label [1] Ada matched by
// one item with its assignee (recall, precision, assignee 1, duplicates 0),
// one 100 ms call costing $0.0001 over 30 min ($0.0002 per hour), no schema
// or fatal failure.
func passingFixture() []MeetingRun {
	p := &Meeting{ID: "P", Labels: []Label{{ID: "LP1", Assignee: "Ada", Segments: []int64{1}}}}
	return []MeetingRun{{Meeting: p, MeetingID: "P", DurationMS: 30 * 60 * 1000,
		Emitted: []items.Item{{Assignee: "Ada", Segments: []int64{1}}},
		Windows: []WindowRun{{Elapsed: 100 * time.Millisecond, Provider: "P1", CostUSD: 0.0001, CostKnown: true}}}}
}

// ok passes every gate (each metric at or inside its bar) with recall r.
func ok(r float64) Metrics {
	return Metrics{Recall: r, Precision: 0.95, DupRate: 0, Assignee: 0.95, Calls: 100, P95: time.Second, SchemaRate: 0, CostKnown: true, CostPerHour: 0.01}
}

// A model passes only if it passes on BOTH corpora, each metric the worse
// of the two; and the choice reads those worst-of-both numbers.
func TestAModelMustPassBothCorpora(t *testing.T) {
	good, bad := Score(passingFixture()), Score(scoringFixture())
	if !Passes(Gates(good, items.AdapterChatJSON, profileLive)) || Passes(Gates(bad, items.AdapterChatJSON, profileLive)) {
		t.Fatal("the fixtures: passing must pass and scoring must fail")
	}
	cleanOnly := NewScored("a/clean-only", items.AdapterChatJSON, profileLive, nil, []Metrics{good}, []Metrics{bad})
	sttOnly := NewScored("b/stt-only", items.AdapterChatJSON, profileLive, nil, []Metrics{bad}, []Metrics{good})
	both := NewScored("c/both", items.AdapterChatJSON, profileLive, nil, []Metrics{good}, []Metrics{good})
	if cleanOnly.Pass || !cleanOnly.Clean.Pass || cleanOnly.STT.Pass {
		t.Errorf("perfect on clean, 0.50 on stt: pass %v (clean %v, stt %v), want a failure", cleanOnly.Pass, cleanOnly.Clean.Pass, cleanOnly.STT.Pass)
	}
	if sttOnly.Pass || sttOnly.Clean.Pass || !sttOnly.STT.Pass {
		t.Errorf("0.50 on clean, perfect on stt: pass %v (clean %v, stt %v), want a failure", sttOnly.Pass, sttOnly.Clean.Pass, sttOnly.STT.Pass)
	}
	if !both.Pass {
		t.Errorf("passing on both: pass false, gates %+v", both.Combined.Gates)
	}
	// The worst of both, per metric, by hand: recall 0.50 (stt) and
	// precision 0.40 (stt) against 1.00 and 1.00 (clean); cost $0.010 per hour
	// (stt) against $0.0002 (clean).
	if w := cleanOnly.Combined.Worst; w.Recall != 0.50 || w.Precision != 0.40 || !near(w.CostPerHour, 0.010) {
		t.Errorf("worst of both = recall %v precision %v cost %v, want 0.50 0.40 0.010", w.Recall, w.Precision, w.CostPerHour)
	}
	if got := Choose([]Scored{cleanOnly, sttOnly}, profileLive); got != "off" {
		t.Errorf("each passing one corpus only: Choose = %s, want off", got)
	}
	// Two models passing both. A: clean 1.00, stt 0.85; B: 0.90 on both.
	// Worst of both: A 0.85, B 0.90, so B, though A has the better clean
	// recall. And mirrored (A: clean 0.85, stt 1.00), B again, though A has
	// the better STT recall.
	for _, c := range []struct {
		name string
		a    *Scored
	}{
		{"A better on clean", ptr(NewScored("a/x", items.AdapterChatJSON, profileLive, nil, []Metrics{ok(1)}, []Metrics{ok(0.85)}))},
		{"A better on stt", ptr(NewScored("a/x", items.AdapterChatJSON, profileLive, nil, []Metrics{ok(0.85)}, []Metrics{ok(1)}))},
	} {
		b := NewScored("b/y", items.AdapterChatJSON, profileLive, nil, []Metrics{ok(0.9)}, []Metrics{ok(0.9)})
		if !c.a.Pass || !b.Pass {
			t.Fatalf("%s: both must pass: %v %v", c.name, c.a.Pass, b.Pass)
		}
		if got := Choose([]Scored{*c.a, b}, profileLive); got != "b/y" {
			t.Errorf("%s: Choose = %s, want b/y (worst-of-both recall 0.90 against 0.85)", c.name, got)
		}
	}
	if v := Judge(nil, items.AdapterChatJSON, profileLive); v.Pass {
		t.Error("a corpus with no runs passed")
	}
	if v := Combine(Judge([]Metrics{good}, items.AdapterChatJSON, profileLive), Judge(nil, items.AdapterChatJSON, profileLive), items.AdapterChatJSON, profileLive); v.Pass {
		t.Error("a model with no STT runs passed")
	}
}

func ptr(s Scored) *Scored { return &s }

func TestARewrittenSpanInTheNonASCIIMeetingFailsExtract(t *testing.T) {
	runs := scoringFixture()
	runs[1].Meeting.Unicode = true // meeting B; A's 2 rewrites are not in a non-ASCII meeting
	runs[1].Windows[1].Rewritten = 1
	m := Score(runs)
	if m.RewrittenUnicode != 1 || m.Rewritten != 3 {
		t.Fatalf("rewritten %d, in the non-ASCII meeting %d; want 3 and 1", m.Rewritten, m.RewrittenUnicode)
	}
	gate := func(adapter string) Gate {
		for _, g := range Gates(m, adapter, profileLive) {
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
