package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// The pass bars (design: "Action-item evaluation"). Every bar but latency
// is the same for both profiles.
const (
	barRecall      = 0.80
	barPrecision   = 0.80
	barDupRate     = 0.05
	barAssignee    = 0.85
	barSchemaRate  = 0.01
	barCostPerHour = 0.05
)

// The two uses a model is judged for, each with its own latency bar
// (operator's decision: separate bars, quality over speed).
const (
	// profileLive is the in-call panel: items.Loop, a window every 10 s. Its
	// p95 bar is the window period (it was 4 s): a call slower than that
	// still lands before the next window is due.
	profileLive = "live"
	// profilePostCall is the pass over the whole meeting after End
	// (items.PostCall), feeding the owner-approved emails: 60 s a call.
	profilePostCall = "postcall"
)

// profiles is every profile, in the order they are run and reported.
var profiles = []string{profileLive, profilePostCall}

// barP95 is a profile's latency bar; an unknown profile has none and fails.
func barP95(profile string) (time.Duration, bool) {
	switch profile {
	case profileLive:
		return 10 * time.Second, true
	case profilePostCall:
		return 60 * time.Second, true
	}
	return 0, false
}

// Metrics is one run's score (or the worst of several).
type Metrics struct {
	Labels          int `json:"labels"`
	Emitted         int `json:"emitted"`
	MatchedLabels   int `json:"matched_labels"`
	MatchingEmitted int `json:"matching_emitted"`
	// DupLabels is the labels at least one duplicate is charged to;
	// DupItems the duplicates (match.go).
	DupLabels     int `json:"dup_labels"`
	DupItems      int `json:"dup_items"`
	AssigneeRight int `json:"assignee_right"`

	Recall    float64 `json:"recall"`
	Precision float64 `json:"precision"`
	DupRate   float64 `json:"dup_rate"`
	Assignee  float64 `json:"assignee"`

	Calls          int           `json:"calls"`
	SchemaFailures int           `json:"schema_failures"`
	RetryFailures  int           `json:"retry_failures"`
	FatalFailures  int           `json:"fatal_failures"`
	SchemaRate     float64       `json:"schema_rate"`
	P50            time.Duration `json:"p50_ns"`
	P95            time.Duration `json:"p95_ns"`

	CostUSD     float64        `json:"cost_usd"`
	CostKnown   bool           `json:"cost_known"`
	Hours       float64        `json:"hours"`
	CostPerHour float64        `json:"cost_per_meeting_hour"`
	Providers   map[string]int `json:"providers"`
	Rewritten   int            `json:"rewritten"`
	// RewrittenUnicode is Rewritten in the meetings marked unicode: a span
	// whose text is not the input there says the offsets are not counted
	// as the adapter counts them (or the model rewrote it), and fails the
	// run for extract.
	RewrittenUnicode int `json:"rewritten_unicode"`
}

// normName is an assignee for comparison: case, space and a " (guest)"
// suffix ignored.
func normName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSpace(strings.TrimSuffix(s, "(guest)"))
}

// Score measures runs (one per meeting).
func Score(runs []MeetingRun) Metrics {
	m := Metrics{Providers: map[string]int{}, CostKnown: true}
	var lat []time.Duration
	var ms int64
	for _, r := range runs {
		ms += r.DurationMS
		labels := r.Meeting.Labels
		m.Labels += len(labels)
		m.Emitted += len(r.Emitted)
		mt := match(r.Emitted, labels)
		dup := make([]bool, len(labels))
		for i, e := range r.Emitted {
			if j := mt.label[i]; j >= 0 {
				m.MatchedLabels++
				m.MatchingEmitted++
				if normName(e.Assignee) == normName(labels[j].Assignee) {
					m.AssigneeRight++
				}
			}
			if j := mt.dupOf[i]; j >= 0 {
				m.DupItems++
				dup[j] = true
			}
		}
		for _, d := range dup {
			if d {
				m.DupLabels++
			}
		}
		for _, w := range r.Windows {
			m.Calls++
			lat = append(lat, w.Elapsed)
			switch w.Class {
			case "schema":
				m.SchemaFailures++
			case "retry":
				m.RetryFailures++
			case "fatal":
				m.FatalFailures++
			}
			if w.Class == "" || w.Class == "schema" {
				if w.CostKnown {
					m.CostUSD += w.CostUSD
				} else {
					m.CostKnown = false
				}
			}
			if w.Provider != "" {
				m.Providers[w.Provider]++
			}
			m.Rewritten += w.Rewritten
			if r.Meeting.Unicode {
				m.RewrittenUnicode += w.Rewritten
			}
		}
	}
	m.Recall = ratio(m.MatchedLabels, m.Labels)
	m.Precision = ratio(m.MatchingEmitted, m.Emitted)
	m.DupRate = ratio(m.DupLabels, m.Labels)
	m.Assignee = ratio(m.AssigneeRight, m.MatchingEmitted)
	m.SchemaRate = ratio(m.SchemaFailures, m.Calls)
	m.P50, m.P95 = percentile(lat, 0.50), percentile(lat, 0.95)
	m.Hours = float64(ms) / float64(time.Hour.Milliseconds())
	if m.Hours > 0 {
		m.CostPerHour = m.CostUSD / m.Hours
	}
	if m.Calls == 0 {
		m.CostKnown = false
	}
	return m
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// percentile is the nearest-rank percentile: the smallest value with at
// least p of the values at or below it.
func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration{}, d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// Worst is each metric's worst value over runs (design: "Each model runs 3
// times; the worst run is the score", read per metric, the stricter way).
func Worst(runs []Metrics) Metrics {
	if len(runs) == 0 {
		return Metrics{}
	}
	w := runs[0]
	w.Providers = map[string]int{}
	for _, r := range runs {
		w.Recall = math.Min(w.Recall, r.Recall)
		w.Precision = math.Min(w.Precision, r.Precision)
		w.DupRate = math.Max(w.DupRate, r.DupRate)
		w.Assignee = math.Min(w.Assignee, r.Assignee)
		w.SchemaRate = math.Max(w.SchemaRate, r.SchemaRate)
		w.P50 = max(w.P50, r.P50)
		w.P95 = max(w.P95, r.P95)
		w.CostPerHour = math.Max(w.CostPerHour, r.CostPerHour)
		w.CostKnown = w.CostKnown && r.CostKnown
		// The fewest calls: the latency and schema gates fail a run (or a
		// corpus) that made none.
		w.Calls = min(w.Calls, r.Calls)
		w.FatalFailures = max(w.FatalFailures, r.FatalFailures)
		w.RetryFailures = max(w.RetryFailures, r.RetryFailures)
		w.SchemaFailures = max(w.SchemaFailures, r.SchemaFailures)
		w.Rewritten = max(w.Rewritten, r.Rewritten)
		w.RewrittenUnicode = max(w.RewrittenUnicode, r.RewrittenUnicode)
		for p, n := range r.Providers {
			w.Providers[p] += n
		}
	}
	return w
}

// Gate is one pass bar's verdict.
type Gate struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Bar    string `json:"bar"`
	Pass   bool   `json:"pass"`
	Gating bool   `json:"gating"`
}

// Gates are m's verdicts for an adapter under a profile: assignee accuracy
// is reported, not gated, for extract; the latency bar is the profile's.
func Gates(m Metrics, adapter, profile string) []Gate {
	f2 := func(x float64) string { return fmt.Sprintf("%.2f", x) }
	cost := "unknown"
	if m.CostKnown {
		cost = fmt.Sprintf("$%.4f", m.CostPerHour)
	}
	p95, known := barP95(profile)
	return []Gate{
		{"recall", f2(m.Recall), "≥ 0.80", m.Recall >= barRecall, true},
		{"precision", f2(m.Precision), "≥ 0.80", m.Precision >= barPrecision, true},
		{"duplicate rate", f2(m.DupRate), "≤ 0.05", m.DupRate <= barDupRate, true},
		{"assignee accuracy", f2(m.Assignee), "≥ 0.85", m.Assignee >= barAssignee, adapter != items.AdapterExtract},
		{"latency p95", m.P95.Round(time.Millisecond).String(), fmt.Sprintf("≤ %ds", int(p95.Seconds())), known && m.Calls > 0 && m.P95 <= p95, true},
		{"schema failures", fmt.Sprintf("%.1f%%", m.SchemaRate*100), "≤ 1%", m.Calls > 0 && m.SchemaRate <= barSchemaRate, true},
		// A cost OpenRouter did not report fails: it is not read as free.
		{"cost per meeting-hour", cost, "≤ $0.05", m.CostKnown && m.CostPerHour <= barCostPerHour, true},
		// A request refused for good (no provider under the policy, a bad
		// key) is a failed run, never a skipped one.
		{"fatal failures", fmt.Sprint(m.FatalFailures), "0", m.FatalFailures == 0, true},
		// The non-ASCII meeting: every extract span there must be the input
		// at its offsets, or the offset unit is in doubt.
		{"rewritten spans, non-ASCII meeting", fmt.Sprint(m.RewrittenUnicode), "0", m.RewrittenUnicode == 0, adapter == items.AdapterExtract},
	}
}

// Passes reports every gating gate passing.
func Passes(gs []Gate) bool {
	for _, g := range gs {
		if g.Gating && !g.Pass {
			return false
		}
	}
	return true
}

// Verdict is one model on one corpus: its runs, the worst of them, and the
// gates on that worst.
type Verdict struct {
	Runs  []Metrics `json:"runs"`
	Worst Metrics   `json:"worst"`
	Gates []Gate    `json:"gates"`
	Pass  bool      `json:"pass"`
}

// Judge is the verdict on runs for an adapter under a profile.
func Judge(runs []Metrics, adapter, profile string) Verdict {
	v := Verdict{Runs: runs, Worst: Worst(runs)}
	v.Gates = Gates(v.Worst, adapter, profile)
	v.Pass = len(runs) > 0 && Passes(v.Gates)
	return v
}

// Scored is one model's result on the clean corpus (as written), on the
// speech-to-text corpus (the same meetings through the real STT, design:
// "ASR-style noise (a variant of each meeting passed through the real STT
// from synthesised speech)"), and Combined: each metric the worse of the
// two. Combined is the gate: a model passes only if it passes on both.
type Scored struct {
	Model   string `json:"model"`
	Adapter string `json:"adapter"`
	// Profile is the use it is judged for: profileLive or profilePostCall.
	Profile  string  `json:"profile"`
	Prefs    any     `json:"provider_prefs"`
	Clean    Verdict `json:"clean"`
	STT      Verdict `json:"stt"`
	Combined Verdict `json:"combined"`
	Pass     bool    `json:"pass"`
}

// NewScored judges a model's clean and STT runs under a profile, and their
// worst-of-both; it passes when that does.
func NewScored(model, adapter, profile string, prefs any, clean, stt []Metrics) Scored {
	s := Scored{Model: model, Adapter: adapter, Profile: profile, Prefs: prefs, Clean: Judge(clean, adapter, profile), STT: Judge(stt, adapter, profile)}
	s.Combined = Combine(s.Clean, s.STT, adapter, profile)
	s.Pass = s.Combined.Pass
	return s
}

// Combine is the worst-of-both verdict: each metric the worse of the two
// corpora's worst runs (Worst), gated as one. A corpus with no runs fails
// it.
func Combine(clean, stt Verdict, adapter, profile string) Verdict {
	if len(clean.Runs) == 0 || len(stt.Runs) == 0 {
		return Verdict{}
	}
	w := Worst([]Metrics{clean.Worst, stt.Worst})
	g := Gates(w, adapter, profile)
	return Verdict{Worst: w, Gates: g, Pass: Passes(g)}
}

// Choose is a profile's default model (design: "Choice"): of the models
// judged for that profile and passing every gate on both corpora, the
// better worst-of-both recall, then the cheaper worst-of-both cost; "off" if
// none passes. A model's verdict for another profile is never read.
func Choose(ss []Scored, profile string) string {
	best := -1
	for i, s := range ss {
		if s.Profile != profile || !s.Pass {
			continue
		}
		w, b := s.Combined.Worst, Metrics{}
		if best >= 0 {
			b = ss[best].Combined.Worst
		}
		if best < 0 || w.Recall > b.Recall || (w.Recall == b.Recall && w.CostPerHour < b.CostPerHour) {
			best = i
		}
	}
	if best < 0 {
		return items.Off
	}
	return ss[best].Model
}
