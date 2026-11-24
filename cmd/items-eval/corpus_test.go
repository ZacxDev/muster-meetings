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

	"github.com/ZacxDev/muster-meetings/internal/items"
)

const corpusDir = "../../testdata/items-eval"

func load(t *testing.T) []*Meeting {
	t.Helper()
	ms, err := LoadCorpus(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestTheCorpusPassesItsLint(t *testing.T) {
	ms := load(t)
	if p := Lint(ms); len(p) > 0 {
		t.Fatalf("the corpus fails its lint:\n%s", strings.Join(p, "\n"))
	}
	st := CorpusStats(ms)
	// The design's corpus: 12 meetings, about 150 windows, ≥ 30% with no
	// item, every hard case several times, all four kinds.
	if st.UnicodeMeetings < 1 {
		t.Errorf("no non-ASCII meeting: %+v", st)
	}
	if st.Meetings != 12 || st.Windows < 130 || st.Windows > 170 || st.NoItemShare < 0.30 {
		t.Errorf("stats = %+v", st)
	}
	for _, k := range []string{"hypothetical", "past_tense", "question", "retracted", "injection", "cross_speaker", "restated", "asr_noise"} {
		if st.HardCases[k] < 3 {
			t.Errorf("hard case %s: %d", k, st.HardCases[k])
		}
	}
	for _, k := range []string{"standup", "planning", "customer", "one_to_one"} {
		if st.Kinds[k] != 3 {
			t.Errorf("kind %s: %d meetings, want 3", k, st.Kinds[k])
		}
	}
}

func loadSTT(t *testing.T) []*Meeting {
	t.Helper()
	ms, err := LoadSTT(corpusDir, load(t))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// broken is the real corpus with one change, for the lint's negative cases.
func broken(t *testing.T, f func([]*Meeting)) []string {
	ms := load(t)
	f(ms)
	for _, m := range ms {
		m.build()
	}
	return Lint(ms)
}

func TestTheLintCatchesEachProblem(t *testing.T) {
	cases := []struct {
		name, want string
		f          func([]*Meeting)
	}{
		{"a label citing a segment not in its meeting", "label m01-1 cites segment 201, which is not in this meeting",
			func(ms []*Meeting) { ms[0].Labels[0].Segments = []int64{201} }},
		{"a label citing nothing", "label m01-1 cites no segment",
			func(ms []*Meeting) { ms[0].Labels[0].Segments = nil }},
		{"an assignee who is not a speaker", `label m01-1's assignee "Zoe" is not a speaker`,
			func(ms []*Meeting) { ms[0].Labels[0].Assignee = "Zoe" }},
		{"a segment id used twice", "segment 201 is also in m01",
			func(ms []*Meeting) { ms[0].Segments[len(ms[0].Segments)-1].ID = 201 }},
		{"a speaker not listed", `segment 99's speaker "Zoe" is not in speakers`,
			func(ms []*Meeting) { ms[0].Segments[0].Speaker = "Zoe" }},
		{"no retracted case", "hard case retracted occurs 0 times",
			func(ms []*Meeting) {
				for _, m := range ms {
					var keep []Trap
					for _, x := range m.Traps {
						if x.Kind != "retracted" {
							keep = append(keep, x)
						}
					}
					m.Traps = keep
				}
			}},
		{"no ASR meeting", "hard case asr_noise occurs 0 times",
			func(ms []*Meeting) {
				for _, m := range ms {
					m.ASR = false
				}
			}},
		{"too few no-item windows", "of windows have no item, want at least 0.30",
			func(ms []*Meeting) {
				for _, m := range ms {
					var all []int64
					for _, s := range m.Segments {
						all = append(all, s.ID)
					}
					m.Labels = append(m.Labels, Label{ID: m.ID + "-all", Text: "x", Assignee: m.Speakers[0].Name, Segments: all})
					m.Traps = nil
				}
			}},
		{"no non-ASCII meeting", "no meeting marked unicode",
			func(ms []*Meeting) {
				for _, m := range ms {
					m.Unicode = false
				}
			}},
		{"a non-ASCII meeting with no character outside the BMP", "no meeting marked unicode",
			func(ms []*Meeting) {
				for _, m := range ms {
					for i := range m.Segments {
						m.Segments[i].Text = strings.ReplaceAll(m.Segments[i].Text, "🎉", "!")
					}
				}
			}},
		{"an injection said by the owner", `trap injection is said by "Tess", who is not a guest`,
			func(ms []*Meeting) {
				for _, m := range ms {
					for i, x := range m.Traps {
						if x.Kind == "injection" && m.ID == "m07" && x.Segments[0] == 712 {
							m.Traps[i].Segments = []int64{701}
						}
					}
				}
			}},
		{"a retraction in a later window", "trap retracted's segments arrive in different windows",
			func(ms []*Meeting) { ms[0].Traps[4].Segments = []int64{113, 116} }},
	}
	for _, c := range cases {
		p := broken(t, c.f)
		if !strings.Contains(strings.Join(p, "\n"), c.want) {
			t.Errorf("%s: lint said\n%s\nwant %q", c.name, strings.Join(p, "\n"), c.want)
		}
	}
}

func TestTheLintWantsTwelveMeetings(t *testing.T) {
	p := Lint(load(t)[:11])
	if !strings.Contains(strings.Join(p, "\n"), "corpus: 11 meetings, want 12") {
		t.Errorf("eleven meetings: lint said\n%s", strings.Join(p, "\n"))
	}
}

func TestTheControlsScoreAsStatedOnTheCorpus(t *testing.T) {
	testControls(t, load(t))
}

func TestTheControlsScoreAsStatedOnTheSTTCorpus(t *testing.T) {
	testControls(t, loadSTT(t))
}

// The controls through each profile's replay: five live (the duplicator
// raw and guarded), four post-call (guarded only).
func testControls(t *testing.T, ms []*Meeting) {
	t.Helper()
	for _, c := range []struct {
		profile string
		want    string
	}{
		{profileLive, "[null oracle duplicator duplicator through the guards liar]"},
		{profilePostCall, "[null oracle duplicator through the guards liar]"},
	} {
		cs := RunControls(ms, c.profile)
		var names []string
		for _, x := range cs {
			names = append(names, x.Name)
			if !x.Pass {
				t.Errorf("%s control %s: want %s, got %s", c.profile, x.Name, x.Want, x.Got)
			}
		}
		if fmt.Sprint(names) != c.want {
			t.Errorf("%s controls %v, want %s", c.profile, names, c.want)
		}
		if !ControlsPass(cs) {
			t.Errorf("%s: ControlsPass = false", c.profile)
		}
	}
}

// fakeRouter answers every chat completion with nothing found, and records
// the requests.
type fakeRouter struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (f *fakeRouter) serve(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.mu.Lock()
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		content := `{"items":[]}`
		if m["model"] == "scaledown/extract" {
			content = `{"entities":[]}`
		}
		out, _ := json.Marshal(map[string]any{"model": m["model"], "provider": "FakeProvider",
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 5, "cost": 0.00001}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestARunGoesThroughProductionsClientWithItsProviderObject(t *testing.T) {
	f := &fakeRouter{}
	srv := f.serve(t)
	out := filepath.Join(t.TempDir(), "r.json")
	var so, se bytes.Buffer
	env := map[string]string{"OPENROUTER_API_KEY": "sk-test"}
	code := run([]string{"-corpus", corpusDir, "-runs", "1", "-out", out, "-base", srv.URL + "/api/v1",
		"-models", "scaledown/extract,deepseek/deepseek-v4.1-flash-1017",
		"-prefs", `deepseek/deepseek-v4.1-flash-1017={"order":["deepinfra/fp8"],"allow_fallbacks":false}`},
		func(k string) string { return env[k] }, &so, &se)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, so.String(), se.String())
	}
	// Every model is replayed on both corpora (the clean one and the STT
	// one) under both profiles: live, one request per window; postcall, one
	// per meeting (every corpus meeting fits one chunk).
	windows := CorpusStats(load(t)).Windows + CorpusStats(loadSTT(t)).Windows
	meetings := len(load(t)) + len(loadSTT(t))
	if len(f.bodies) != 2*(windows+meetings) {
		t.Errorf("%d requests, want per model one per window and one per meeting per corpus (2 × (%d + %d))", len(f.bodies), windows, meetings)
	}
	maxTokens := map[float64]int{}
	for _, b := range f.bodies {
		if b["model"] == "deepseek/deepseek-v4.1-flash-1017" {
			mt, _ := b["max_tokens"].(float64)
			maxTokens[mt]++
		}
	}
	if maxTokens[4096] != windows || maxTokens[8192] != meetings {
		t.Errorf("DeepSeek max_tokens %v, want 4096 on each of %d live windows and 8192 on each of %d post-call meetings", maxTokens, windows, meetings)
	}
	for _, b := range f.bodies {
		p, _ := b["provider"].(map[string]any)
		if p["data_collection"] != "deny" {
			t.Fatalf("a request without data_collection deny: %v", b["provider"])
		}
		if b["model"] == "deepseek/deepseek-v4.1-flash-1017" && (p["allow_fallbacks"] != false || p["order"] == nil) {
			t.Fatalf("a DeepSeek request without its pin: %v", p)
		}
	}
	var res Result
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 2*(windows+meetings) || res.Requests[0].Provider != "FakeProvider" || !res.Requests[0].CostKnown ||
		fmt.Sprint(res.Choices) != "map[live:off postcall:off]" || len(res.Models) != 4 {
		t.Errorf("result: %d requests, first %+v, choices %v, %d models", len(res.Requests), res.Requests[0], res.Choices, len(res.Models))
	}
	byProfile := map[string]int{}
	for _, r := range res.Requests {
		byProfile[r.Profile]++
	}
	if byProfile[profileLive] != 2*windows || byProfile[profilePostCall] != 2*meetings {
		t.Errorf("requests by profile %v, want live %d and postcall %d", byProfile, 2*windows, 2*meetings)
	}
	if res.Models[0].Pass || res.Models[0].STT.Worst.Recall != 0 || res.Models[0].Clean.Worst.Recall != 0 {
		t.Errorf("a model that found nothing passed: %+v", res.Models[0].STT.Gates)
	}
	corpora := map[string]int{}
	for _, r := range res.Requests {
		corpora[r.Corpus]++
	}
	if corpora["clean"] == 0 || corpora["stt"] == 0 || len(res.STTMeetings) != 12 || len(res.STTControls) != 5 || len(res.STTPostCallControls) != 4 {
		t.Errorf("requests per corpus %v, %d stt meetings, %d stt controls, %d stt post-call controls", corpora, len(res.STTMeetings), len(res.STTControls), len(res.STTPostCallControls))
	}
	if !strings.Contains(so.String(), "**Choices:** live `off`, postcall `off`.") {
		t.Errorf("stdout has no choice:\n%s", so.String())
	}
}

func TestAnUnpinnedDeepSeekIsRefused(t *testing.T) {
	f := &fakeRouter{}
	srv := f.serve(t)
	var so, se bytes.Buffer
	code := run([]string{"-corpus", corpusDir, "-runs", "1", "-out", filepath.Join(t.TempDir(), "r.json"), "-base", srv.URL + "/api/v1",
		"-models", "deepseek/deepseek-v4.1-flash-1017"}, func(k string) string {
		if k == "OPENROUTER_API_KEY" {
			return "sk-test"
		}
		return ""
	}, &so, &se)
	if code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), "must pin its provider") {
		t.Errorf("exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
}

func TestAFailedControlStopsTheRunBeforeAnyModel(t *testing.T) {
	// The corpus with one label's text made the same as an earlier label's
	// in its meeting: the guards drop the oracle's second item as a
	// duplicate, the oracle misses recall 1.0, and no model may be called.
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	for _, src := range files {
		b, _ := os.ReadFile(src)
		// The variant carries the same labels, so the change is made to both
		// (else the STT lint, not the control, would stop the run).
		if n := filepath.Base(src); n == "m01-standup.yaml" || n == "m01-standup.stt.yaml" {
			b = bytes.Replace(b, []byte(`text: "Send the release notes draft to the team"`), []byte(`text: "Renew the staging certificate"`), 1)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), b, 0o644); err != nil {
			t.Fatal(err)
		}
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
	if code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), "a control did not score as stated") {
		t.Errorf("a failing oracle: exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
	if !strings.Contains(so.String(), "oracle                         FAIL") {
		t.Errorf("stdout does not show the oracle failing:\n%s", so.String())
	}
}

// After retryable failures the anchor stays, so the first window that
// succeeds covers every segment since the last success, even ones older than
// the 90 s lookback; a schema failure spends its window.
func TestReplayCoversTheGapAfterRetriesAndSpendsASchemaFailure(t *testing.T) {
	m := load(t)[0]
	var calls [][]int64
	var at []int64
	a := &scripted{f: func(n int, w items.Window) (items.Result, error) {
		var ids []int64
		for _, s := range w.Segments {
			ids = append(ids, s.ID)
		}
		calls = append(calls, ids)
		switch {
		case n < 10: // 100 s of meeting time: longer than the lookback
			return items.Result{}, &items.Error{Class: items.Retry, Status: 429}
		case n == 10:
			return items.Result{}, nil
		case n == 11:
			return items.Result{}, &items.Error{Class: items.Schema}
		}
		return items.Result{}, nil
	}}
	run := Replay(t.Context(), a, m, ReplayOpts{})
	for _, w := range run.Windows {
		at = append(at, w.AtMS)
	}
	if len(calls) < 13 {
		t.Fatalf("%d calls", len(calls))
	}
	if at[10]-at[0] <= items.Lookback.Milliseconds() {
		t.Fatalf("the failures span %d ms, not past the lookback: the fixture cannot see the anchor", at[10]-at[0])
	}
	if !hasAll(calls[10], calls[0]) {
		t.Errorf("after 10 failed windows the first success %v does not cover the first failed window %v", calls[10], calls[0])
	}
	if run.Windows[0].Class != "retry" || run.Windows[10].Class != "" || run.Windows[11].Class != "schema" {
		t.Errorf("classes %q %q %q", run.Windows[0].Class, run.Windows[10].Class, run.Windows[11].Class)
	}
	// The schema failure spent its window: the next call is made only once a
	// new segment ends, and none of 11's segments is new in it.
	var top int64
	for _, id := range calls[11] {
		top = max(top, id)
	}
	if run.Windows[12].Segments[len(run.Windows[12].Segments)-1] <= top {
		t.Errorf("after a schema failure the next window %v has no segment newer than %d", calls[12], top)
	}
}

type scripted struct {
	n int
	f func(int, items.Window) (items.Result, error)
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Propose(_ context.Context, w items.Window) (items.Result, error) {
	n := s.n
	s.n++
	return s.f(n, w)
}

func hasAll(a, b []int64) bool {
	for _, x := range b {
		found := false
		for _, y := range a {
			if x == y {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
