package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ZacxDev/muster-meetings/internal/stt"
	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
	"github.com/ZacxDev/muster-meetings/internal/stttest"
)

const fixtureSource = `id: m97
kind: one_to_one
title: A fixture
speakers:
  - {name: Ada}
  - {name: Bo}
segments:
  - {id: 9701, speaker: Ada, text: "Bo, can you renew the cert today?"}
  - {id: 9702, speaker: Bo, text: "Sure, I'll renew it."}
  - {id: 9703, speaker: Ada, text: "Thanks."}
labels:
  - {id: m97-1, text: "Renew the cert", assignee: Bo, segments: [9701, 9702], cases: [cross_speaker]}
`

// tone is d seconds of speech-loud audio (far over the endpointer's
// threshold), so each segment is one utterance.
func tone(d float64) []int16 {
	out := make([]int16, int(d*stt.Rate))
	for i := range out {
		out[i] = int16(8000 * math.Sin(2*math.Pi*220*float64(i)/stt.Rate))
	}
	return out
}

type noTranscriber struct{}

func (noTranscriber) Transcribe(context.Context, []int16) (string, error) {
	return "", stt.ErrUnavailable
}

// The whole STT leg through the plugin's real nemotron engine against the
// protocol-exact fake server: one stream per speaker, each line to its
// segment, and the late closing marks (the fake holds them back to the
// next final's lead, as the model does) amended onto the line they end.
func TestAMeetingThroughTheNemotronEngine(t *testing.T) {
	srv := stttest.New("tok")
	t.Cleanup(srv.Close)
	srv.Set(func(s *stttest.Server) { s.Lines = []string{"bo can you renew the search today?", "thanks."} })
	e := stt.NewNemotron(srv.URL(), "tok", stt.NewUtterance(noTranscriber{}), nil)
	var src source
	if err := yaml.Unmarshal([]byte(fixtureSource), &src); err != nil {
		t.Fatal(err)
	}
	voices := map[string]Voice{"Ada": {Model: "en_GB-vctk-medium", Speaker: 0}, "Bo": {Model: "en_US-ryan-medium", Speaker: 0}}
	speak := func(v Voice, text string) ([]int16, error) {
		return tone(1.0 + 0.1*float64(len(strings.Fields(text)))), nil
	}
	// No room noise: the fake reads any varying segment as speech.
	texts, lines, samples, err := transcribeMeeting(t.Context(), src, voices, e, speak, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Ada's stream: 9701 then 9703, whose final carries 9701's "?"; the end's
	// final carries 9703's ".". Bo's stream: the script's first line, its
	// "?" in the end's final.
	want := map[int64]string{9701: "bo can you renew the search today?", 9702: "bo can you renew the search today?", 9703: "thanks."}
	for id, w := range want {
		if texts[id] != w {
			t.Errorf("segment %d: %q, want %q", id, texts[id], w)
		}
	}
	if lines["nemotron"] != 3 || len(lines) != 1 {
		t.Errorf("lines per engine %v, want 3 from nemotron", lines)
	}
	if got := len(srv.Records()); got != 2 {
		t.Errorf("%d streams, want one per speaker (2)", got)
	}
	// Ada: 500 + 1.7 s + 1500 + 1.1 s + 1500 + 1000 ms; Bo: 500 + 1.4 s + 1500 + 1000 ms.
	if want := int64(((0.5 + 1.7 + 1.5 + 1.1 + 1.5 + 1.0) + (0.5 + 1.4 + 1.5 + 1.0)) * stt.Rate); samples < want-2*stt.Rate/100 || samples > want+2*stt.Rate/100 {
		t.Errorf("%d samples streamed, want about %d", samples, want)
	}
	in := Record(src, texts, lines, voices, "nemotron")
	// By hand, per segment: 9701 "bo can you renew the cert today" vs "... search ...": 1 of 7;
	// 9702 [sure i'll renew it] vs 7 words sharing only "renew": 6 of 4; 9703: 0 of 1.
	if in.Errors != 7 || in.Words != 12 || *in.WER != 0.5833 || in.Engine != "nemotron" || in.Voices["Bo"] != "en_US-ryan-medium#0" || in.Tool != "items-corpus-stt 1" {
		t.Errorf("record = %+v (wer %v), want 7 errors over 12 words, 0.5833, nemotron", in, *in.WER)
	}
}

func TestStoreAmendsAsThePluginDoes(t *testing.T) {
	var ls []Line
	ls = Store(ls, stt.Segment{Kind: stt.Partial, Text: "so the"}, "nemotron")
	ls = Store(ls, stt.Segment{Kind: stt.Final, Text: "so the plan", StartMS: 0, EndMS: 900}, "nemotron")
	ls = Store(ls, stt.Segment{Kind: stt.Final, Text: "next", Lead: " ?x", StartMS: 900, EndMS: 2000}, "nemotron")
	ls = Store(ls, stt.Segment{Kind: stt.Final, Lead: ".", StartMS: 2000, EndMS: 2100}, "nemotron") // only a lead: no line
	ls = Store(ls, stt.Segment{Kind: stt.Final, Lead: "!", StartMS: 2100, EndMS: 2200}, "nemotron") // already ends with a mark
	ls = Store(ls, stt.Segment{Kind: stt.Final, Text: "fallback words", Engine: "utterance", StartMS: 3000, EndMS: 4000}, "nemotron")
	if len(ls) != 3 || ls[0].Text != "so the plan?" || ls[1].Text != "next." || ls[2].Engine != "utterance" {
		t.Errorf("lines = %+v", ls)
	}
}

func TestAssignByOverlapAndRefuseAGap(t *testing.T) {
	spans := []Span{{1, 500, 2000}, {2, 3500, 6000}, {3, 7500, 8000}}
	got, err := Assign([]Line{
		{StartMS: 0, EndMS: 2700, Text: "one"},     // 1500 ms on 1
		{StartMS: 2700, EndMS: 4500, Text: "two"},  // 1000 on 2
		{StartMS: 4500, EndMS: 6800, Text: "more"}, // 1500 on 2
		{StartMS: 9000, EndMS: 9500, Text: "late"}, // nothing: nearest is 3
	}, spans)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != "one" || got[2] != "two more" || got[3] != "late" {
		t.Errorf("assigned %v", got)
	}
	if _, err := Assign([]Line{{StartMS: 10, EndMS: 20, Gap: true}}, spans); err == nil || !strings.Contains(err.Error(), "a gap in the transcript") {
		t.Errorf("a gap: %v", err)
	}
}

// The shipped voice map covers every speaker of the corpus, never gives two
// speakers of one meeting the same voice, and has two accents.
func TestTheVoiceMapCoversTheCorpus(t *testing.T) {
	cfg, err := sttcorpus.Voices()
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Accents(); len(a) < 2 {
		t.Errorf("accents %v, want at least two", a)
	}
	files, _ := filepath.Glob("../../testdata/items-eval/*.yaml")
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".stt.yaml") {
			continue
		}
		b, _ := os.ReadFile(f)
		var src source
		if err := yaml.Unmarshal(b, &src); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range src.Speakers {
			names = append(names, s.Name)
		}
		if _, err := cfg.VoicesFor(names); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
		n++
	}
	if n != 12 {
		t.Errorf("%d source meetings, want 12", n)
	}
	if _, err := cfg.VoicesFor([]string{"Bo", "Mo"}); err == nil || !strings.Contains(err.Error(), "share voice") {
		t.Errorf("two speakers on one voice: %v", err)
	}
}

func TestResampleKeepsLengthAndPitch(t *testing.T) {
	in := make([]int16, 22050)
	for i := range in {
		in[i] = int16(10000 * math.Sin(2*math.Pi*1000*float64(i)/22050))
	}
	out := Resample(in, 22050, 16000)
	if len(out) != 16000 {
		t.Fatalf("%d samples, want 16000", len(out))
	}
	cross := 0
	for i := 1; i < len(out); i++ {
		if (out[i-1] < 0) != (out[i] < 0) {
			cross++
		}
	}
	if cross < 1990 || cross > 2010 { // a 1 kHz tone crosses zero 2000 times a second
		t.Errorf("%d zero crossings, want about 2000", cross)
	}
}

func TestSpeakText(t *testing.T) {
	if got := SpeakText("the queue’s calmer 🎉 so that’s nice"); got != "the queue's calmer so that's nice" {
		t.Errorf("SpeakText = %q", got)
	}
}
