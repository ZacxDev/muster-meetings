// Command items-eval is the action-item evaluation (design: "Action-item
// evaluation"). It replays the labelled corpus in testdata/items-eval/
// window by window through the worker's own code (items.Select, the
// adapter, items.Guard) against OpenRouter, scores every model against the
// labels by cited segments, and picks the default model.
//
// 🔴 It costs money and needs a key: it is run by hand, never in CI. CI runs
// its scoring, its corpus lint and its controls (go test ./cmd/items-eval),
// none of which calls OpenRouter.
//
//	OPENROUTER_API_KEY=… go run ./cmd/items-eval \
//	  -prefs 'deepseek/deepseek-v4.1-flash-1017={"order":["<provider>"],"allow_fallbacks":false}'
//
// Each model's provider object comes from -prefs model=JSON, else
// MEETINGS_ITEMS_PROVIDER_PREFS, else {"data_collection": "deny"}; every
// one goes through items.ParsePrefs, as production's does, so deny is
// always present and a chat_json model must be pinned.
//
// The corpus is two: the meetings as written (the clean corpus) and the
// same meetings through the plugin's real STT from synthesised speech (the
// STT corpus, *.stt.yaml beside each source, made by cmd/items-corpus-stt).
// Every model is replayed on both and reported on both; it PASSES only on
// the worst of the two (each metric the worse corpus's: it must pass on
// both), and the choice reads those worst-of-both numbers.
//
// Order, every run: the lint of both corpora (every source must have its
// variant), then the four controls on each (a failure of either stops the
// run before any model is called), then each model -runs times. It prints a table per model and writes the machine-readable result
// to -out (the markdown table, for docs/items-eval-<date>.md, to -md).
// Exit status: 0 when the controls passed and every model was scored
// (whatever the gates said; the choice is in the output), 1 otherwise.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

type prefFlags map[string]string

func (p prefFlags) String() string { return fmt.Sprint(map[string]string(p)) }
func (p prefFlags) Set(v string) error {
	model, js, ok := strings.Cut(v, "=")
	if !ok || model == "" {
		return errors.New("want model=JSON")
	}
	p[model] = js
	return nil
}

// Result is the machine-readable output.
type Result struct {
	Date     string          `json:"date"`
	Corpus   Stats           `json:"corpus"`
	Controls []ControlResult `json:"controls"`
	// STTCorpus and STTControls are the STT corpus's; STTMeetings each
	// variant's record (engine, word error rate).
	STTCorpus   Stats           `json:"stt_corpus"`
	STTControls []ControlResult `json:"stt_controls"`
	STTMeetings []STTMeeting    `json:"stt_meetings"`
	Models      []Scored        `json:"models"`
	Requests    []Request       `json:"requests"`
	Choice      string          `json:"choice"`
}

// Request is one call, for the record of which provider answered it.
type Request struct {
	Model    string        `json:"model"`
	Corpus   string        `json:"corpus"`
	Run      int           `json:"run"`
	Meeting  string        `json:"meeting"`
	Window   int           `json:"window"`
	Provider string        `json:"provider"`
	Answered string        `json:"answered_model"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	Class    string        `json:"class,omitempty"`
	CostUSD  float64       `json:"cost_usd"`
}

// STTMeeting is one variant's record.
type STTMeeting struct {
	Meeting string  `json:"meeting"`
	Engine  string  `json:"engine"`
	WER     float64 `json:"wer"`
	Words   int     `json:"words"`
	Tool    string  `json:"tool"`
}

// The two corpora's names, in the result and the tables.
const (
	corpusClean = "clean"
	corpusSTT   = "stt"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("items-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpus := fs.String("corpus", "testdata/items-eval", "the corpus directory")
	models := fs.String("models", "scaledown/extract,deepseek/deepseek-v4.1-flash-1017", "comma-separated OpenRouter slugs")
	runs := fs.Int("runs", 3, "runs per model (the worst is the score)")
	date := time.Now().UTC().Format("2006-01-02")
	out := fs.String("out", "items-eval-"+date+".json", "the result file")
	md := fs.String("md", "", "also write the result tables as markdown here")
	base := fs.String("base", items.DefaultBaseURL, "OpenRouter's API root")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	controlsOnly := fs.Bool("controls-only", false, "lint the corpus and run the controls, then stop (no key, no cost)")
	prefs := prefFlags{}
	fs.Var(prefs, "prefs", "model=JSON: that model's OpenRouter provider object (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	ms, err := LoadCorpus(*corpus)
	if err != nil {
		fmt.Fprintln(stderr, "items-eval:", err)
		return 1
	}
	sms, err := LoadSTT(*corpus, ms)
	if err != nil {
		fmt.Fprintln(stderr, "items-eval:", err)
		return 1
	}
	res := Result{Date: date, Corpus: CorpusStats(ms), STTCorpus: CorpusStats(sms)}
	for _, v := range sms {
		if v.STT != nil && v.STT.WER != nil {
			res.STTMeetings = append(res.STTMeetings, STTMeeting{v.ID, v.STT.Engine, *v.STT.WER, v.STT.Words, v.STT.Tool})
		}
	}
	if p := append(Lint(ms), LintSTT(ms, sms)...); len(p) > 0 {
		fmt.Fprintln(stderr, "items-eval: the corpus fails its lint; no model is scored:")
		for _, x := range p {
			fmt.Fprintln(stderr, "  "+x)
		}
		return 1
	}
	st := res.Corpus
	fmt.Fprintf(stdout, "corpus: %d meetings (%d non-ASCII), %d segments, %d labels, %d windows (%.0f%% with no item) of %d to %d segments (median %d, p90 %d), %.1f meeting-minutes\n\n",
		st.Meetings, st.UnicodeMeetings, st.Segments, st.Labels, st.Windows, st.NoItemShare*100, st.WindowMin, st.WindowMax, st.WindowMedian, st.WindowP90, st.MinutesTotal)

	fmt.Fprintf(stdout, "stt corpus: %s\n\n", sttSummary(res))

	res.Controls = RunControls(ms)
	res.STTControls = RunControls(sms)
	for _, set := range []struct {
		name string
		cs   []ControlResult
	}{{"controls", res.Controls}, {"controls, stt corpus", res.STTControls}} {
		fmt.Fprintf(stdout, "%s:\n", set.name)
		for _, c := range set.cs {
			mark := "pass"
			if !c.Pass {
				mark = "FAIL"
			}
			fmt.Fprintf(stdout, "  %-30s %s  want %s, got %s\n", c.Name, mark, c.Want, c.Got)
		}
		fmt.Fprintln(stdout)
	}
	if !ControlsPass(res.Controls) || !ControlsPass(res.STTControls) {
		fmt.Fprintln(stderr, "items-eval: a control did not score as stated; no model is scored")
		writeResult(*out, res, stderr)
		return 1
	}
	if *controlsOnly {
		return 0
	}

	key := strings.TrimSpace(getenv("OPENROUTER_API_KEY"))
	if key == "" {
		fmt.Fprintln(stderr, "items-eval: OPENROUTER_API_KEY is not set")
		return 1
	}
	ok := true
	for _, model := range strings.Split(*models, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		raw, set := prefs[model]
		if !set {
			raw = getenv("MEETINGS_ITEMS_PROVIDER_PREFS")
		}
		ad := items.AdapterFor(model)
		p, err := items.ParsePrefs(raw, ad)
		if err != nil {
			fmt.Fprintf(stderr, "items-eval: %s: %v\n", model, err)
			ok = false
			continue
		}
		var clean, sttRuns []Metrics
		for r := range *runs {
			a, err := items.New(items.Config{Model: model, Adapter: ad, Prefs: p, APIKey: key, BaseURL: *base, Timeout: *timeout}, nil)
			if err != nil {
				fmt.Fprintf(stderr, "items-eval: %s: %v\n", model, err)
				return 1
			}
			o := ReplayOpts{Sleep: time.Sleep, Detail: func(meeting string, seq int, d string) {
				fmt.Fprintf(stderr, "  %s run %d %s window %d: openrouter said: %s\n", model, r+1, meeting, seq, d)
			}}
			for _, c := range []struct {
				name string
				ms   []*Meeting
				out  *[]Metrics
			}{{corpusClean, ms, &clean}, {corpusSTT, sms, &sttRuns}} {
				var mr []MeetingRun
				for _, m := range c.ms {
					x := Replay(context.Background(), a, m, o)
					mr = append(mr, x)
					for _, w := range x.Windows {
						res.Requests = append(res.Requests, Request{model, c.name, r + 1, m.ID, w.Seq, w.Provider, w.Model, w.Elapsed, w.Class, w.CostUSD})
					}
				}
				met := Score(mr)
				*c.out = append(*c.out, met)
				fmt.Fprintf(stdout, "%s run %d, %s corpus: recall %.2f, precision %.2f, duplicates %.2f, p95 %v, %d calls\n",
					model, r+1, c.name, met.Recall, met.Precision, met.DupRate, met.P95.Round(time.Millisecond), met.Calls)
			}
		}
		res.Models = append(res.Models, NewScored(model, ad, p, clean, sttRuns))
	}
	res.Choice = Choose(res.Models)
	table := Tables(res)
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, table)
	if !writeResult(*out, res, stderr) {
		ok = false
	}
	if *md != "" {
		if err := os.WriteFile(*md, []byte(table), 0o644); err != nil {
			fmt.Fprintln(stderr, "items-eval:", err)
			ok = false
		}
	}
	if !ok {
		return 1
	}
	return 0
}

func writeResult(path string, res Result, stderr io.Writer) bool {
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(stderr, "items-eval:", err)
		return false
	}
	return true
}

// sttSummary is the STT corpus in one line: its engines, its word error
// rate over the whole corpus, and each meeting's.
func sttSummary(res Result) string {
	var sc struct{ errs, words float64 }
	engines := map[string]int{}
	var per []string
	for _, m := range res.STTMeetings {
		sc.errs += m.WER * float64(m.Words)
		sc.words += float64(m.Words)
		engines[m.Engine]++
		per = append(per, fmt.Sprintf("%s %.3f", m.Meeting, m.WER))
	}
	wer := 0.0
	if sc.words > 0 {
		wer = sc.errs / sc.words
	}
	return fmt.Sprintf("%d meetings, word error rate %.3f over %.0f words (engines: %s; per meeting: %s)",
		len(res.STTMeetings), wer, sc.words, providers(engines), strings.Join(per, ", "))
}

// Tables is the result as markdown: one table per model, its gates on the
// worst of both corpora (the gate) beside each corpus's own values, and the
// choice.
func Tables(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Action-item evaluation, %s\n\n", res.Date)
	st := res.Corpus
	fmt.Fprintf(&b, "Corpus: %d meetings, %d labels, %d windows (%.0f%% with no item), each as written (clean) and through the real STT from synthesised speech (stt: %s). Controls: all passed on both. **The gate is the worse of the two corpora, per metric:** a model passes only if it passes on both, and the choice reads the same worst-of-both numbers.\n\n",
		st.Meetings, st.Labels, st.Windows, st.NoItemShare*100, sttSummary(res))
	val := func(gs []Gate, i int) string {
		if i < len(gs) {
			return gs[i].Value
		}
		return "—"
	}
	pass := func(ok bool) string {
		if ok {
			return "passes"
		}
		return "fails"
	}
	for _, s := range res.Models {
		pj, _ := json.Marshal(s.Prefs)
		fmt.Fprintf(&b, "## `%s` (%s), %d runs, worst per metric\n\nProvider object: `%s`. Answered by: %s.\n\n", s.Model, s.Adapter, len(s.STT.Runs), pj, providers(mergeCounts(s.STT.Worst.Providers, s.Clean.Worst.Providers)))
		b.WriteString("| metric | worst of both (the gate) | stt | clean | bar | pass |\n|---|---|---|---|---|---|\n")
		for i, g := range s.Combined.Gates {
			p := "yes"
			if !g.Pass {
				p = "**no**"
			}
			if !g.Gating {
				p += " (reported, not a gate)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", g.Name, g.Value, val(s.STT.Gates, i), val(s.Clean.Gates, i), g.Bar, p)
		}
		fmt.Fprintf(&b, "| latency p50 | %v | %v | %v | — | — |\n", s.Combined.Worst.P50.Round(time.Millisecond), s.STT.Worst.P50.Round(time.Millisecond), s.Clean.Worst.P50.Round(time.Millisecond))
		if s.Adapter == items.AdapterExtract {
			fmt.Fprintf(&b, "| spans whose `text` was not the input, whole corpus | %d | %d | %d | — | — |\n", s.Combined.Worst.Rewritten, s.STT.Worst.Rewritten, s.Clean.Worst.Rewritten)
		}
		verdict := "passes every gate on both corpora"
		if !s.Pass {
			verdict = "fails"
		}
		fmt.Fprintf(&b, "\n**%s %s** (on its own, the STT corpus %s and the clean corpus %s).\n\n", s.Model, verdict, pass(s.STT.Pass), pass(s.Clean.Pass))
	}
	fmt.Fprintf(&b, "**Choice:** `%s`.\n", res.Choice)
	return b.String()
}

func mergeCounts(a, b map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

func providers(p map[string]int) string {
	if len(p) == 0 {
		return "none recorded"
	}
	var out []string
	for k, v := range p {
		out = append(out, fmt.Sprintf("%s (%d)", k, v))
	}
	return strings.Join(sortedStrings(out), ", ")
}

func sortedStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}
