package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
)

func TestTheSTTCorpusPassesItsLint(t *testing.T) {
	clean, stt := load(t), loadSTT(t)
	if p := LintSTT(clean, stt); len(p) > 0 {
		t.Fatalf("the STT corpus fails its lint:\n%s", strings.Join(p, "\n"))
	}
	if len(stt) != 12 {
		t.Errorf("%d variants, want 12", len(stt))
	}
	for i, v := range stt {
		if v.source == nil || v.source.file != clean[i].file || v.STT.Engine == "" {
			t.Errorf("%s: source %v, engine %q", v.file, v.source != nil, v.STT.Engine)
		}
		// Its windows are its source's: the same times, so the same ticks.
		if a, b := len(v.Windows()), len(clean[i].Windows()); a != b {
			t.Errorf("%s: %d windows, its source %d", v.file, a, b)
		}
	}
}

// brokenSTT is the real corpora with one change, for the STT lint's
// negative cases.
func brokenSTT(t *testing.T, f func(clean, stt []*Meeting) ([]*Meeting, []*Meeting)) []string {
	t.Helper()
	clean, stt := f(load(t), loadSTT(t))
	for _, m := range stt {
		m.build()
	}
	return append(Lint(clean), LintSTT(clean, stt)...)
}

func TestTheSTTLintCatchesEachProblem(t *testing.T) {
	cases := []struct {
		name, want string
		f          func(clean, stt []*Meeting) ([]*Meeting, []*Meeting)
	}{
		{"a source without its variant", "m01-standup.yaml: no speech-to-text variant (m01-standup.stt.yaml)",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { return c, s[1:] }},
		{"a variant without its source", "m01-standup.stt.yaml: no source meeting (m01-standup.yaml)",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].source = nil; return c, s }},
		{"a changed label", "m01-standup.stt.yaml: label m01-1 differs from the source's",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Labels[0].Assignee = "Ada"; return c, s }},
		{"a label's citation changed", "m01-standup.stt.yaml: label m01-2 differs from the source's",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Labels[1].Segments = []int64{105}; return c, s }},
		{"a label dropped", "m01-standup.stt.yaml: 2 labels, the source has 3",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Labels = s[0].Labels[:2]; return c, s }},
		{"a changed trap", "m01-standup.stt.yaml: its traps differ from the source's",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Traps[0].Kind = "question"; return c, s }},
		{"a changed segment id", "m01-standup.stt.yaml: segment 1 is 9999 by \"Bo\", the source's is 100 by \"Bo\"",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Segments[1].ID = 9999; return c, s }},
		{"a variant with its own times", "m01-standup.stt.yaml: segment 99's dur differs from the source's",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].Segments[0].Dur = 9; return c, s }},
		{"no stt block", "m01-standup.stt.yaml: no stt block",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].STT = nil; return c, s }},
		{"no word error rate", "m01-standup.stt.yaml: records no word error rate",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].STT.WER = nil; return c, s }},
		{"a word error rate its text does not give", "m01-standup.stt.yaml: records wer",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) {
				s[0].Segments[0].Text = "nothing like what was said"
				return c, s
			}},
		{"a speaker without a voice", "m01-standup.stt.yaml: voices are for [Bo Cleo Dev], the speakers are [Ada Bo Cleo Dev]",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { delete(s[0].STT.Voices, "Ada"); return c, s }},
		{"two speakers on one voice", "m01-standup.stt.yaml: voice \"en_GB-vctk-medium#0\" is used for two speakers",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) {
				s[0].STT.Voices["Bo"] = s[0].STT.Voices["Ada"]
				return c, s
			}},
		{"an unknown engine", "m01-standup.stt.yaml: engine \"whisper\" is not nemotron, utterance or mixed",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].STT.Engine = "whisper"; return c, s }},
		{"no tool version", "m01-standup.stt.yaml: records no tool version",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { s[0].STT.Tool = ""; return c, s }},
		{"a source with an stt block", "m01-standup.yaml: has an stt block",
			func(c, s []*Meeting) ([]*Meeting, []*Meeting) { c[0].STT = &sttcorpus.Info{}; return c, s }},
	}
	for _, c := range cases {
		p := strings.Join(brokenSTT(t, c.f), "\n")
		if !strings.Contains(p, c.want) {
			t.Errorf("%s: lint said\n%s\nwant %q", c.name, p, c.want)
		}
	}
}

// A run needs the STT corpus: a source without its variant stops it at the
// lint, before any model is called.
func TestARunWithoutTheSTTCorpusScoresNoModel(t *testing.T) {
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	for _, src := range files {
		if filepath.Base(src) == "m05-planning.stt.yaml" {
			continue
		}
		b, _ := os.ReadFile(src)
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
	if code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), "m05-planning.yaml: no speech-to-text variant") {
		t.Errorf("a missing variant: exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
}

// The STT half of the failed-control stop: a corpus whose STT controls fail
// while its clean controls pass still stops the run before any model. The
// variant of m01 is remade with segment 103 heard as nothing (its record's
// WER remade to match, so the lint passes): label m01-1 cites only 103, so
// the STT oracle cannot reach it and misses recall 1.0.
func TestAFailedSTTControlStopsTheRunBeforeAnyModel(t *testing.T) {
	dir := t.TempDir()
	files, _ := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	for _, src := range files {
		b, _ := os.ReadFile(src)
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(src)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v := loadSTT(t)[0]
	texts := map[int64]string{}
	var sc sttcorpus.Score
	for i, s := range v.Segments {
		texts[s.ID] = s.Text
		if s.ID == 103 {
			texts[s.ID] = ""
		}
		sc = sc.Add(v.source.Segments[i].Text, texts[s.ID])
	}
	info := *v.STT
	wer := sc.Rate()
	info.WER, info.Words, info.Errors = &wer, sc.Words, sc.Errors
	srcBytes, _ := os.ReadFile(filepath.Join(corpusDir, "m01-standup.yaml"))
	out, err := sttcorpus.Write(srcBytes, texts, info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m01-standup.stt.yaml"), out, 0o644); err != nil {
		t.Fatal(err)
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
	clean, sttc, _ := strings.Cut(so.String(), "controls, stt corpus:")
	if !strings.Contains(clean, "oracle                         pass") || !strings.Contains(sttc, "oracle                         FAIL") {
		t.Fatalf("the fixture: want the clean oracle passing and the STT oracle failing:\n%s\n%s", so.String(), se.String())
	}
	if code != 1 || len(f.bodies) != 0 || !strings.Contains(se.String(), "a control did not score as stated") {
		t.Errorf("a failing STT oracle: exit %d, %d requests, stderr %s", code, len(f.bodies), se.String())
	}
}

// A segment the engine heard nothing in is no line (production stores none)
// and the others keep their source times.
func TestAnEmptyTranscriptIsNoLine(t *testing.T) {
	v := loadSTT(t)[0]
	src := v.source.Timeline()
	v.Segments[1].Text = " "
	v.build()
	tl := v.Timeline()
	if len(tl) != len(src)-1 || tl[1].ID != src[2].ID || tl[1].StartMS != src[2].StartMS || v.DurationMS() != v.source.DurationMS() {
		t.Errorf("timeline %d segments (source %d), second %d at %d", len(tl), len(src), tl[1].ID, tl[1].StartMS)
	}
}
