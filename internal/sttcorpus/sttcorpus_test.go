package sttcorpus

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWordsNormalises(t *testing.T) {
	got := Words("Okay, Søren’s cert — 'renewed'?  I’ll do it 10 times 🎉")
	want := []string{"okay", "søren's", "cert", "renewed", "i'll", "do", "it", "10", "times"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Words = %q, want %q", got, want)
	}
}

// Each expected count is by hand: the fewest substitutions, deletions and
// insertions.
func TestDistanceAndRate(t *testing.T) {
	cases := []struct {
		ref, hyp string
		want     int
	}{
		{"the cat sat", "the cat sat", 0},
		{"the cat sat", "the bat sat", 1},                             // one substitution
		{"the cat sat", "the sat", 1},                                 // one deletion
		{"the cat sat", "the fat cat sat down", 2},                    // two insertions
		{"renew the staging cert", "renew a staging search today", 3}, // 2 subs + 1 ins
		{"a b c d", "", 4},
		{"", "a b", 2},
		{"I'll renew it.", "ill renew it", 1}, // i'll vs ill: an apostrophe is a letter here
	}
	for _, c := range cases {
		if got := Distance(Words(c.ref), Words(c.hyp)); got != c.want {
			t.Errorf("Distance(%q, %q) = %d, want %d", c.ref, c.hyp, got, c.want)
		}
	}
	// 1 + 3 errors over 3 + 4 reference words.
	s := Score{}.Add("the cat sat", "the bat sat").Add("renew the staging cert", "renew a staging search today")
	if s.Errors != 4 || s.Words != 7 || s.Rate() != 0.5714 {
		t.Errorf("score = %+v rate %v, want 4 errors over 7 words, rate 0.5714", s, s.Rate())
	}
	if (Score{}).Rate() != 0 || (Score{Errors: 2, Words: 3}).Rate() != 0.6667 {
		t.Errorf("rates: %v %v, want 0 and 0.6667", (Score{}).Rate(), (Score{Errors: 2, Words: 3}).Rate())
	}
}

const source = `id: m98
kind: standup
title: A fixture
# A comment the variant keeps.
speakers:
  - {name: Ada}
  - {name: Bo, guest: true}
segments:
  - {id: 9801, speaker: Ada, text: "Bo, can you renew the cert today?"}
  - {id: 9802, speaker: Bo, text: "Sure, I’ll renew it this morning.", dur: 3.5}
  - {id: 9803, speaker: Ada, text: "Thanks."}
labels:
  - {id: m98-1, text: "Renew the cert", assignee: Bo, due: "this morning", segments: [9801, 9802], cases: [cross_speaker]}
traps:
  - {kind: question, segments: [9803]}
`

type fixture struct {
	ID       string           `yaml:"id"`
	Kind     string           `yaml:"kind"`
	Title    string           `yaml:"title"`
	Speakers []map[string]any `yaml:"speakers"`
	Segments []map[string]any `yaml:"segments"`
	Labels   []map[string]any `yaml:"labels"`
	Traps    []map[string]any `yaml:"traps"`
	STT      *Info            `yaml:"stt"`
	Rest     map[string]any   `yaml:",inline"`
}

func TestWriteKeepsEverythingButTheText(t *testing.T) {
	wer := 0.25
	info := Info{Engine: EngineNemotron, Lines: map[string]int{"nemotron": 3}, Tool: "items-corpus-stt 1",
		Voices: map[string]string{"Ada": "en_GB-vctk-medium#3", "Bo": "en_US-ryan-medium#0"}, WER: &wer, Words: 16, Errors: 4, AudioSeconds: 12.5}
	texts := map[int64]string{9801: "bo can you renew the search today", 9802: "sure i'll renew it this morning", 9803: ""}
	out, err := Write([]byte(source), texts, info)
	if err != nil {
		t.Fatal(err)
	}
	var src, got fixture
	if err := yaml.Unmarshal([]byte(source), &src); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.ID != "m98" || got.Kind != src.Kind || got.Title != src.Title {
		t.Errorf("header: %s %s %s", got.ID, got.Kind, got.Title)
	}
	if !reflect.DeepEqual(got.Speakers, src.Speakers) || !reflect.DeepEqual(got.Labels, src.Labels) || !reflect.DeepEqual(got.Traps, src.Traps) {
		t.Errorf("speakers, labels or traps changed:\n%s", out)
	}
	if len(got.Segments) != 3 {
		t.Fatalf("%d segments", len(got.Segments))
	}
	for i, s := range got.Segments {
		ss := src.Segments[i]
		if s["id"] != ss["id"] || s["speaker"] != ss["speaker"] || s["dur"] != ss["dur"] {
			t.Errorf("segment %d: %v, source %v", i, s, ss)
		}
		if want := texts[int64(ss["id"].(int))]; s["text"] != want {
			t.Errorf("segment %v text %q, want %q", ss["id"], s["text"], want)
		}
	}
	if got.STT == nil || got.STT.WER == nil || *got.STT.WER != 0.25 || got.STT.Words != 16 || got.STT.Errors != 4 ||
		got.STT.Engine != "nemotron" || got.STT.Voices["Bo"] != "en_US-ryan-medium#0" || got.STT.Tool != "items-corpus-stt 1" {
		t.Errorf("stt block = %+v", got.STT)
	}
	if !strings.Contains(string(out), "# A comment the variant keeps.") || !strings.Contains(string(out), "  - {id: 9801, speaker: Ada, text: \"bo can you renew the search today\"}") {
		t.Errorf("the source's layout or comments were not kept:\n%s", out)
	}
	// A variant is not a source.
	if _, err := Write(out, texts, info); err == nil || !strings.Contains(err.Error(), "already has an stt block") {
		t.Errorf("writing a variant of a variant: %v", err)
	}
}

func TestWriteRefusesAMissingOrAnExtraTranscript(t *testing.T) {
	_, err := Write([]byte(source), map[int64]string{9801: "a", 9802: "b"}, Info{})
	if err == nil || !strings.Contains(err.Error(), "segment 9803 has no transcript") {
		t.Errorf("a missing transcript: %v", err)
	}
	_, err = Write([]byte(source), map[int64]string{9801: "a", 9802: "b", 9803: "c", 42: "d"}, Info{})
	if err == nil || !strings.Contains(err.Error(), "segments not in the source: 42") {
		t.Errorf("an extra transcript: %v", err)
	}
	_, err = Write([]byte(source), map[int64]string{9801: "a\nb", 9802: "b", 9803: "c"}, Info{})
	if err == nil || !strings.Contains(err.Error(), "has a line break") {
		t.Errorf("a line break: %v", err)
	}
}

func TestVariantName(t *testing.T) {
	if VariantName("m01-standup.yaml") != "m01-standup.stt.yaml" || !IsVariant("m01-standup.stt.yaml") || IsVariant("m01-standup.yaml") {
		t.Error("variant names")
	}
}
