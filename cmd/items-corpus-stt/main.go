// Command items-corpus-stt makes the speech-to-text variant of each meeting
// in the action-item evaluation corpus (design: "Action-item evaluation":
// "a variant of each meeting passed through the real STT from synthesised
// speech, so the errors are real STT errors"). For each source meeting
// testdata/items-eval/<name>.yaml it writes <name>.stt.yaml beside it: the
// same segment ids, speakers, labels and traps, each segment's text replaced
// by what the plugin's STT heard, and an `stt` block recording the engine,
// the voices, this tool's version and the word error rate against the
// source text. cmd/items-eval lints the pair and gates on the variants.
//
// 🔴 Run by hand, never in CI: it needs the network (the Piper voices, the
// STT servers) and the GPU the STT servers share with live dictation. It
// opens ONE stream at a time.
//
// The pipeline, per meeting and per speaker (one stream each, as each
// participant's microphone is one track in a call):
//
//  1. Piper (`piper` on PATH: nixpkgs piper-tts) speaks each of the
//     speaker's segments in their voice (internal/sttcorpus/voices.yaml: models pinned by
//     revision and sha256, downloaded into -cache; never committed).
//  2. The audio is laid out as one stream: room noise, then each segment
//     followed by a 1.5 s pause, resampled to 16 kHz mono.
//  3. The stream goes, in real time (-speed), through the plugin's own
//     engine (internal/stt): `nemotron` (the stt-nemotron server: 80 ms
//     frames, silence included, a flush per utterance by the shared
//     endpointer, with its per-participant fallback to `utterance`), or
//     `utterance` (the POST /v1/audio/transcriptions service).
//  4. Its finals are stored as the plugin stores them (a final's `lead`
//     amends the previous line), and each line goes to the segment whose
//     audio it overlaps most.
//
// Endpoints and tokens come from the environment, with the plugin's names;
// a token is never printed:
//
//	MEETINGS_STREAM_STT_URL, MEETINGS_STREAM_STT_TOKEN   nemotron
//	MEETINGS_STT_URL, MEETINGS_STT_TOKEN [, MEETINGS_STT_MODEL]
//	                                                     utterance, and nemotron's fallback
//
//	nix-shell -p piper-tts --run 'go run ./cmd/items-corpus-stt'
//	go run ./cmd/items-corpus-stt -engine utterance -meetings m01,m02
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ZacxDev/muster-meetings/internal/stt"
	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
)

// source is the part of a corpus meeting the tool reads.
type source struct {
	ID       string `yaml:"id"`
	Speakers []struct {
		Name string `yaml:"name"`
	} `yaml:"speakers"`
	Segments []struct {
		ID      int64  `yaml:"id"`
		Speaker string `yaml:"speaker"`
		Text    string `yaml:"text"`
	} `yaml:"segments"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("items-corpus-stt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpus := fs.String("corpus", "testdata/items-eval", "the corpus directory")
	voices := fs.String("voices", "", "the voice config (default: the embedded voices.yaml)")
	cacheDefault, _ := os.UserCacheDir()
	cache := fs.String("cache", filepath.Join(cacheDefault, "items-corpus-stt"), "where voice models and synthesised audio are cached")
	engineName := fs.String("engine", sttcorpus.EngineNemotron, "nemotron or utterance")
	only := fs.String("meetings", "", "comma-separated meeting ids (default: every one)")
	speed := fs.Float64("speed", 1, "audio seconds sent per wall second (1 is real time, as a live call)")
	piper := fs.String("piper", "piper", "the piper binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(f string, a ...any) int { fmt.Fprintf(stderr, "items-corpus-stt: "+f+"\n", a...); return 1 }
	raw := sttcorpus.VoicesYAML
	if *voices != "" {
		b, err := os.ReadFile(*voices)
		if err != nil {
			return fail("%v", err)
		}
		raw = b
	}
	cfg, err := sttcorpus.ParseConfig(raw)
	if err != nil {
		return fail("voices: %v", err)
	}
	if *speed <= 0 {
		return fail("-speed must be positive")
	}
	engine, err := newEngine(*engineName, getenv, log.New(stderr, "stt: ", 0))
	if err != nil {
		return fail("%v", err)
	}
	files, err := filepath.Glob(filepath.Join(*corpus, "*.yaml"))
	if err != nil {
		return fail("%v", err)
	}
	sort.Strings(files)
	want := map[string]bool{}
	for _, id := range strings.Split(*only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	tts := &TTS{Config: cfg, Cache: *cache, Piper: *piper, HTTP: &http.Client{Timeout: 10 * time.Minute}}
	pace := time.Duration(float64(stt.FrameSamples*int(time.Second/stt.Rate)) / *speed)
	var total float64
	n := 0
	for _, f := range files {
		if sttcorpus.IsVariant(f) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return fail("%v", err)
		}
		var src source
		if err := yaml.Unmarshal(b, &src); err != nil {
			return fail("%s: %v", f, err)
		}
		if len(want) > 0 && !want[src.ID] {
			continue
		}
		out, info, err := variant(ctx, b, src, engine, tts, pace)
		if err != nil {
			return fail("%s: %v", filepath.Base(f), err)
		}
		dst := filepath.Join(*corpus, sttcorpus.VariantName(filepath.Base(f)))
		if err := os.WriteFile(dst, out, 0o644); err != nil {
			return fail("%v", err)
		}
		total += info.AudioSeconds
		n++
		fmt.Fprintf(stdout, "%s: engine %s %v, wer %.4f (%d errors over %d words), %.1f s of audio -> %s\n",
			src.ID, info.Engine, info.Lines, *info.WER, info.Errors, info.Words, info.AudioSeconds, filepath.Base(dst))
	}
	if n == 0 {
		return fail("no meeting matched")
	}
	fmt.Fprintf(stdout, "%d meetings, %.1f minutes of audio through the %s engine\n", n, total/60, engine.Name())
	return 0
}

func newEngine(name string, getenv func(string) string, logger *log.Logger) (stt.Engine, error) {
	utterance := func() (stt.Engine, error) {
		tok, err := stt.CheckToken("MEETINGS_STT_TOKEN", getenv("MEETINGS_STT_TOKEN"))
		if err != nil {
			return nil, err
		}
		cl, err := stt.NewClient(getenv("MEETINGS_STT_URL"), tok, getenv("MEETINGS_STT_MODEL"), stt.CallTimeout)
		if err != nil {
			return nil, err
		}
		return stt.NewUtterance(cl), nil
	}
	switch name {
	case sttcorpus.EngineUtterance:
		return utterance()
	case sttcorpus.EngineNemotron:
		u, err := stt.CheckStreamURL(getenv("MEETINGS_STREAM_STT_URL"))
		if err != nil {
			return nil, err
		}
		tok, err := stt.CheckToken("MEETINGS_STREAM_STT_TOKEN", getenv("MEETINGS_STREAM_STT_TOKEN"))
		if err != nil {
			return nil, err
		}
		fb, err := utterance()
		if err != nil {
			return nil, fmt.Errorf("nemotron's fallback: %w", err)
		}
		return stt.NewNemotron(u, tok, fb, logger), nil
	}
	return nil, fmt.Errorf("-engine %q: want nemotron or utterance", name)
}

// variant is one meeting's variant file and its record.
func variant(ctx context.Context, raw []byte, src source, e stt.Engine, tts *TTS, pace time.Duration) ([]byte, sttcorpus.Info, error) {
	var names []string
	for _, s := range src.Speakers {
		names = append(names, s.Name)
	}
	voices, err := tts.Config.VoicesFor(names)
	if err != nil {
		return nil, sttcorpus.Info{}, err
	}
	texts, lines, samples, err := transcribeMeeting(ctx, src, voices, e, func(v Voice, text string) ([]int16, error) {
		return tts.Speak(ctx, v, text)
	}, pace, RoomNoise)
	if err != nil {
		return nil, sttcorpus.Info{}, err
	}
	info := Record(src, texts, lines, voices, e.Name())
	info.AudioSeconds = float64(samples*10/stt.Rate) / 10
	out, err := sttcorpus.Write(raw, texts, info)
	return out, info, err
}

// transcribeMeeting streams each speaker's segments, one stream at a time,
// and answers each segment's text, the lines per engine, and the samples
// streamed.
func transcribeMeeting(ctx context.Context, src source, voices map[string]Voice, e stt.Engine,
	speak func(Voice, string) ([]int16, error), pace time.Duration, noise int) (map[int64]string, map[string]int, int64, error) {
	texts := map[int64]string{}
	lines := map[string]int{}
	var samples int64
	for si, sp := range src.Speakers {
		var spans []Span
		var audio [][]int16
		for _, s := range src.Segments {
			if s.Speaker != sp.Name {
				continue
			}
			pcm, err := speak(voices[sp.Name], s.Text)
			if err != nil {
				return nil, nil, 0, fmt.Errorf("segment %d: %w", s.ID, err)
			}
			spans = append(spans, Span{ID: s.ID})
			audio = append(audio, pcm)
		}
		if len(spans) == 0 {
			continue
		}
		pcm, spans := Layout(spans, audio, noise)
		samples += int64(len(pcm))
		ls, err := Transcribe(ctx, e, stt.TrackRef{MeetingID: src.ID, Track: fmt.Sprintf("%s-%d", src.ID, si), Identity: sp.Name}, pcm, pace)
		if err != nil {
			return nil, nil, 0, err
		}
		got, err := Assign(ls, spans)
		if err != nil {
			return nil, nil, 0, err
		}
		for id, t := range got {
			texts[id] = t
		}
		for _, l := range ls {
			if strings.TrimSpace(l.Text) != "" {
				lines[l.Engine]++
			}
		}
	}
	return texts, lines, samples, nil
}

// Record is a variant's stt block: the engine (mixed when a nemotron stream
// fell back for some lines), the lines per engine, the voices, and the word
// error rate of texts against the source.
func Record(src source, texts map[int64]string, lines map[string]int, voices map[string]Voice, configured string) sttcorpus.Info {
	in := sttcorpus.Info{Engine: configured, Lines: lines, Tool: sttcorpus.Tool, Voices: map[string]string{}}
	if len(lines) > 1 {
		in.Engine = sttcorpus.EngineMixed
	} else {
		for e := range lines {
			in.Engine = e
		}
	}
	for n, v := range voices {
		in.Voices[n] = v.String()
	}
	var sc sttcorpus.Score
	for _, s := range src.Segments {
		sc = sc.Add(s.Text, texts[s.ID])
	}
	wer := sc.Rate()
	in.WER, in.Words, in.Errors = &wer, sc.Words, sc.Errors
	return in
}
