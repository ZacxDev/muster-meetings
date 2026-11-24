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
// Order, every run: the corpus lint, then the four controls (a failure of
// either stops the run before any model is called), then each model -runs
// times. It prints a table per model and writes the machine-readable result
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
	Models   []Scored        `json:"models"`
	Requests []Request       `json:"requests"`
	Choice   string          `json:"choice"`
}

// Request is one call, for the record of which provider answered it.
type Request struct {
	Model    string        `json:"model"`
	Run      int           `json:"run"`
	Meeting  string        `json:"meeting"`
	Window   int           `json:"window"`
	Provider string        `json:"provider"`
	Answered string        `json:"answered_model"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	Class    string        `json:"class,omitempty"`
	CostUSD  float64       `json:"cost_usd"`
}

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
	res := Result{Date: date, Corpus: CorpusStats(ms)}
	if p := Lint(ms); len(p) > 0 {
		fmt.Fprintln(stderr, "items-eval: the corpus fails its lint; no model is scored:")
		for _, x := range p {
			fmt.Fprintln(stderr, "  "+x)
		}
		return 1
	}
	st := res.Corpus
	fmt.Fprintf(stdout, "corpus: %d meetings, %d segments, %d labels, %d windows (%.0f%% with no item) of %d to %d segments (median %d, p90 %d), %.1f meeting-minutes\n\n",
		st.Meetings, st.Segments, st.Labels, st.Windows, st.NoItemShare*100, st.WindowMin, st.WindowMax, st.WindowMedian, st.WindowP90, st.MinutesTotal)

	res.Controls = RunControls(ms)
	fmt.Fprintln(stdout, "controls:")
	for _, c := range res.Controls {
		mark := "pass"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(stdout, "  %-30s %s  want %s, got %s\n", c.Name, mark, c.Want, c.Got)
	}
	fmt.Fprintln(stdout)
	if !ControlsPass(res.Controls) {
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
		s := Scored{Model: model, Adapter: ad, Prefs: p}
		for r := range *runs {
			a, err := items.New(items.Config{Model: model, Adapter: ad, Prefs: p, APIKey: key, BaseURL: *base, Timeout: *timeout}, nil)
			if err != nil {
				fmt.Fprintf(stderr, "items-eval: %s: %v\n", model, err)
				return 1
			}
			o := ReplayOpts{Sleep: time.Sleep, Detail: func(meeting string, seq int, d string) {
				fmt.Fprintf(stderr, "  %s run %d %s window %d: openrouter said: %s\n", model, r+1, meeting, seq, d)
			}}
			var mr []MeetingRun
			for _, m := range ms {
				x := Replay(context.Background(), a, m, o)
				mr = append(mr, x)
				for _, w := range x.Windows {
					res.Requests = append(res.Requests, Request{model, r + 1, m.ID, w.Seq, w.Provider, w.Model, w.Elapsed, w.Class, w.CostUSD})
				}
			}
			met := Score(mr)
			s.Runs = append(s.Runs, met)
			fmt.Fprintf(stdout, "%s run %d: recall %.2f, precision %.2f, duplicates %.2f, p95 %v, %d calls\n",
				model, r+1, met.Recall, met.Precision, met.DupRate, met.P95.Round(time.Millisecond), met.Calls)
		}
		s.Worst = Worst(s.Runs)
		s.Gates = Gates(s.Worst, ad)
		s.Pass = Passes(s.Gates)
		res.Models = append(res.Models, s)
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

// Tables is the result as markdown: one table per model and the choice.
func Tables(res Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Action-item evaluation, %s\n\n", res.Date)
	st := res.Corpus
	fmt.Fprintf(&b, "Corpus: %d meetings, %d labels, %d windows (%.0f%% with no item). Controls: all passed.\n\n", st.Meetings, st.Labels, st.Windows, st.NoItemShare*100)
	for _, s := range res.Models {
		pj, _ := json.Marshal(s.Prefs)
		fmt.Fprintf(&b, "## `%s` (%s), %d runs, worst per metric\n\nProvider object: `%s`. Answered by: %s.\n\n", s.Model, s.Adapter, len(s.Runs), pj, providers(s.Worst.Providers))
		b.WriteString("| metric | value | bar | pass |\n|---|---|---|---|\n")
		for _, g := range s.Gates {
			pass := "yes"
			if !g.Pass {
				pass = "**no**"
			}
			if !g.Gating {
				pass += " (reported, not a gate)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", g.Name, g.Value, g.Bar, pass)
		}
		fmt.Fprintf(&b, "| latency p50 | %v | — | — |\n", s.Worst.P50.Round(time.Millisecond))
		if s.Adapter == items.AdapterExtract {
			fmt.Fprintf(&b, "| spans whose `text` was not the input | %d | — | — |\n", s.Worst.Rewritten)
		}
		verdict := "passes every gate"
		if !s.Pass {
			verdict = "fails"
		}
		fmt.Fprintf(&b, "\n**%s %s.**\n\n", s.Model, verdict)
	}
	fmt.Fprintf(&b, "**Choice:** `%s`.\n", res.Choice)
	return b.String()
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
