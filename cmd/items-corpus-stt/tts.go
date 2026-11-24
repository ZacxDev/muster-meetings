package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/ZacxDev/muster-meetings/internal/stt"
	"github.com/ZacxDev/muster-meetings/internal/sttcorpus"
)

// The voice config's types are sttcorpus's (the corpus lint checks a
// variant's voices against the same config).
type (
	Config = sttcorpus.Config
	Model  = sttcorpus.Model
	Voice  = sttcorpus.Voice
)

// TTS synthesises speech with Piper, caching both the models and each
// segment's audio under Cache.
type TTS struct {
	Config *Config
	Cache  string
	Piper  string // the piper binary
	HTTP   *http.Client
}

const voicesURL = "https://huggingface.co/rhasspy/piper-voices/resolve/"

// fetch puts the model's file (suffix ".onnx" or ".onnx.json") in the cache,
// downloading it if it is missing or its sha256 is not the pinned one.
func (t *TTS) fetch(ctx context.Context, name, suffix, want string) (string, error) {
	dst := filepath.Join(t.Cache, "voices", name+suffix)
	if sum, err := fileSHA(dst); err == nil && sum == want {
		return dst, nil
	}
	m := t.Config.Models[name]
	url := voicesURL + t.Config.Revision + "/" + m.Path + "/" + name + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("%s: sha256 %s, the config pins %s", url, got, want)
	}
	return dst, os.Rename(tmp, dst)
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SpeakText is a segment's text as it is given to Piper: curly quotes made
// straight, and symbols (an emoji) dropped, since nobody says them.
func SpeakText(s string) string {
	s = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`).Replace(s)
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) || r == '️' {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Speak is text in voice v as 16 kHz mono.
func (t *TTS) Speak(ctx context.Context, v Voice, text string) ([]int16, error) {
	m := t.Config.Models[v.Model]
	text = SpeakText(text)
	key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%d", m.SHA256, v.Speaker, text, stt.Rate)))
	cached := filepath.Join(t.Cache, "tts", hex.EncodeToString(key[:])+".pcm")
	if b, err := os.ReadFile(cached); err == nil {
		return fromBytes(b), nil
	}
	onnx, err := t.fetch(ctx, v.Model, ".onnx", m.SHA256)
	if err != nil {
		return nil, err
	}
	cfg, err := t.fetch(ctx, v.Model, ".onnx.json", m.ConfigSHA256)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		return nil, err
	}
	wav := cached + ".wav"
	defer os.Remove(wav)
	cmd := exec.CommandContext(ctx, t.Piper, "-m", onnx, "-c", cfg, "-s", strconv.Itoa(v.Speaker), "-f", wav)
	cmd.Stdin = strings.NewReader(text + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper: %v: %s", err, tail(stderr.String()))
	}
	b, err := os.ReadFile(wav)
	if err != nil {
		return nil, err
	}
	pcm, rate, err := ParseWAV(b)
	if err != nil {
		return nil, err
	}
	out := Resample(pcm, rate, stt.Rate)
	if len(out) == 0 {
		return nil, errors.New("piper produced no audio")
	}
	// Atomically, as the model download: a run killed mid-write leaves a
	// .part, never a short cache entry the next run would read as audio.
	part := cached + ".part"
	if err := os.WriteFile(part, toBytes(out), 0o644); err != nil {
		os.Remove(part)
		return nil, err
	}
	return out, os.Rename(part, cached)
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// ParseWAV reads a 16-bit mono PCM WAVE file: its samples and rate.
func ParseWAV(b []byte) ([]int16, int, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAVE file")
	}
	var rate int
	fmtOK := false
	for p := 12; p+8 <= len(b); {
		id, n := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := b[p+8 : min(p+8+n, len(b))]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, 0, errors.New("a short fmt chunk")
			}
			format, ch, bits := binary.LittleEndian.Uint16(body[0:]), binary.LittleEndian.Uint16(body[2:]), binary.LittleEndian.Uint16(body[14:])
			if format != 1 || ch != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("format %d, %d channels, %d bits: want 16-bit mono PCM", format, ch, bits)
			}
			rate = int(binary.LittleEndian.Uint32(body[4:]))
			fmtOK = true
		case "data":
			if !fmtOK {
				return nil, 0, errors.New("data before fmt")
			}
			return fromBytes(body), rate, nil
		}
		p += 8 + n + n%2
	}
	return nil, 0, errors.New("no data chunk")
}

func fromBytes(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}

func toBytes(p []int16) []byte {
	b := make([]byte, 2*len(p))
	for i, s := range p {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	return b
}

// resampleHalf is the windowed-sinc kernel's half width, in input samples.
const resampleHalf = 32

// Resample converts pcm from rate in to rate out with a Hann-windowed sinc
// low-pass at 0.45 of the lower rate (band-limited interpolation).
func Resample(pcm []int16, in, out int) []int16 {
	if in == out {
		return append([]int16{}, pcm...)
	}
	n := int(int64(len(pcm)) * int64(out) / int64(in))
	res := make([]int16, n)
	fc := 0.45 * float64(min(in, out)) / float64(in) // cycles per input sample
	ratio := float64(in) / float64(out)
	for i := range res {
		t := float64(i) * ratio
		c := int(math.Floor(t))
		var acc float64
		for k := c - resampleHalf + 1; k <= c+resampleHalf; k++ {
			if k < 0 || k >= len(pcm) {
				continue
			}
			d := t - float64(k)
			w := 0.5 + 0.5*math.Cos(math.Pi*d/resampleHalf)
			acc += float64(pcm[k]) * 2 * fc * sinc(2*fc*d) * w
		}
		res[i] = int16(max(-32768, min(32767, math.Round(acc))))
	}
	return res
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	return math.Sin(math.Pi*x) / (math.Pi * x)
}
