package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
)

// LintSTT is every way the speech-to-text corpus falls short (design:
// "Action-item evaluation": "a variant of each meeting passed through the
// real STT from synthesised speech"): each source in clean has its variant,
// and each variant is its source with only the text changed (the same
// meeting, speakers, segment ids and speakers, labels and traps: the
// commitment was said, and the gate measures whether a model still finds it
// in the transcribed text), and records how it was made and its word error
// rate, which must be what its text gives against the source's.
//
// The record is checked against the current pipeline too: made by this
// tool version (sttcorpus.Tool), its lines per engine agreeing with its
// engine, and each speaker's voice the one voices.yaml gives them, so a
// variant made by an older pipeline or with other voices is remade, never
// silently scored.
func LintSTT(clean, stt []*Meeting) []string {
	var p []string
	bad := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	cfg, err := sttcorpus.Voices()
	if err != nil {
		bad("voices.yaml: %v", err)
		cfg = &sttcorpus.Config{}
	}
	variant := map[string]*Meeting{}
	for _, v := range stt {
		variant[v.file] = v
	}
	for _, m := range clean {
		if variant[sttcorpus.VariantName(m.file)] == nil {
			bad("%s: no speech-to-text variant (%s)", m.file, sttcorpus.VariantName(m.file))
		}
	}
	for _, v := range stt {
		where, src := v.file, v.source
		if src == nil {
			bad("%s: no source meeting (%s.yaml)", where, strings.TrimSuffix(where, sttcorpus.Suffix))
			continue
		}
		if v.ID != src.ID || v.Kind != src.Kind || v.Title != src.Title || v.ASR != src.ASR || v.Unicode != src.Unicode {
			bad("%s: its id, kind, title, asr or unicode differs from the source's", where)
		}
		if !reflect.DeepEqual(v.Speakers, src.Speakers) {
			bad("%s: its speakers differ from the source's", where)
		}
		if len(v.Segments) != len(src.Segments) {
			bad("%s: %d segments, the source has %d", where, len(v.Segments), len(src.Segments))
		}
		for i := range min(len(v.Segments), len(src.Segments)) {
			s, o := v.Segments[i], src.Segments[i]
			if s.ID != o.ID || s.Speaker != o.Speaker {
				bad("%s: segment %d is %d by %q, the source's is %d by %q", where, i, s.ID, s.Speaker, o.ID, o.Speaker)
			}
			if s.Dur != o.Dur {
				bad("%s: segment %d's dur differs from the source's (a variant's times are its source's)", where, s.ID)
			}
			if strings.ContainsAny(s.Text, "\n\r") {
				bad("%s: segment %d's text has a line break", where, s.ID)
			}
		}
		if len(v.Labels) != len(src.Labels) {
			bad("%s: %d labels, the source has %d", where, len(v.Labels), len(src.Labels))
		}
		for i := range min(len(v.Labels), len(src.Labels)) {
			if !reflect.DeepEqual(v.Labels[i], src.Labels[i]) {
				bad("%s: label %s differs from the source's", where, v.Labels[i].ID)
			}
		}
		if !reflect.DeepEqual(v.Traps, src.Traps) {
			bad("%s: its traps differ from the source's", where)
		}
		lintInfo(v, src, cfg, bad)
	}
	return p
}

func lintInfo(v, src *Meeting, cfg *sttcorpus.Config, bad func(string, ...any)) {
	where, in := v.file, v.STT
	if in == nil {
		bad("%s: no stt block (engine, voices, tool, word error rate)", where)
		return
	}
	if !contains([]string{sttcorpus.EngineNemotron, sttcorpus.EngineUtterance, sttcorpus.EngineMixed}, in.Engine) {
		bad("%s: engine %q is not nemotron, utterance or mixed", where, in.Engine)
	}
	if strings.TrimSpace(in.Tool) == "" {
		bad("%s: records no tool version", where)
	} else if in.Tool != sttcorpus.Tool {
		bad("%s: made by %q, the current tool is %q: run cmd/items-corpus-stt again", where, in.Tool, sttcorpus.Tool)
	}
	engines := make([]string, 0, len(in.Lines))
	for e, n := range in.Lines {
		if n > 0 {
			engines = append(engines, e)
		}
	}
	sort.Strings(engines)
	switch {
	case len(engines) == 0:
		bad("%s: records no lines per engine", where)
	case in.Engine == sttcorpus.EngineMixed && len(engines) < 2:
		bad("%s: engine mixed, but its lines are all from %v", where, engines)
	case in.Engine != sttcorpus.EngineMixed && (len(engines) != 1 || engines[0] != in.Engine):
		bad("%s: engine %s, but its lines are from %v", where, in.Engine, engines)
	}
	var names []string
	for _, s := range src.Speakers {
		names = append(names, s.Name)
	}
	voiced := keysOf(in.Voices)
	sort.Strings(names)
	if !reflect.DeepEqual(voiced, names) {
		bad("%s: voices are for %v, the speakers are %v", where, voiced, names)
	}
	for _, n := range voiced {
		if want, ok := cfg.Speakers[n]; !ok || in.Voices[n] != want {
			bad("%s: speaker %s's voice is %q, voices.yaml gives %q", where, n, in.Voices[n], want)
		}
	}
	used := map[string]bool{}
	for _, n := range voiced {
		if used[in.Voices[n]] {
			bad("%s: voice %q is used for two speakers", where, in.Voices[n])
		}
		used[in.Voices[n]] = true
	}
	if in.WER == nil {
		bad("%s: records no word error rate", where)
		return
	}
	var sc sttcorpus.Score
	for i := range min(len(v.Segments), len(src.Segments)) {
		sc = sc.Add(src.Segments[i].Text, v.Segments[i].Text)
	}
	if *in.WER != sc.Rate() || in.Words != sc.Words || in.Errors != sc.Errors {
		bad("%s: records wer %v (%d errors over %d words); its text against the source's is %v (%d over %d)",
			where, *in.WER, in.Errors, in.Words, sc.Rate(), sc.Errors, sc.Words)
	}
}

func keysOf(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
