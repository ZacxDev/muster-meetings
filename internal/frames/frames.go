// Package frames samples still frames from a finished recording at given
// meeting times (the operator's decision 4: "sample screen-share frames at
// transcript moments", for the post-call analysis). It calls ffmpeg, which
// the image carries (a minimal static build: the MP4 demuxer, the H.264
// decoder, the scaler and the JPEG encoder, nothing else; see the
// Dockerfile).
//
// The recording is read from the store, never copied to disk: a part can be
// ~1.4 GB per hour, and the container's /tmp is small. Each Sample serves
// the parts it needs on a loopback listener of its own (a random path, for
// this call only) by passing each byte range ffmpeg asks for straight to
// the store, and ffmpeg seeks in the file over HTTP. Egress writes its MP4
// with the index (moov) at the END, so ffmpeg's first ranges are the file's
// tail; the frame itself is one more range.
//
// 🔴 No model is called here: this returns JPEG bytes and their meeting
// times, and nothing else.
package frames

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/timeline"
)

// Object reads one recording file from the store: the byte range rng ("" for
// all of it), answered as the store answered it (200, 206 or 416). The
// caller closes the body.
type Object func(ctx context.Context, rng string) (*http.Response, error)

// Limits.
const (
	// MaxFrames is the most frames one Sample extracts.
	MaxFrames = 120
	// MaxWidth is the widest frame returned (a wider recording is scaled
	// down, keeping its aspect).
	MaxWidth = 1280
	// MaxJPEG is the largest frame accepted from ffmpeg.
	MaxJPEG = 8 << 20
	// DefaultTimeout bounds one frame's extraction.
	DefaultTimeout = 30 * time.Second
)

// Sampler runs ffmpeg.
type Sampler struct {
	// FFmpeg is the ffmpeg binary's path; "" is frame sampling off.
	FFmpeg string
	// Timeout bounds one frame (default DefaultTimeout).
	Timeout time.Duration
}

// Find is a sampler for the ffmpeg at path, or found on PATH when path is ""
// (a sampler whose FFmpeg is "" when there is none).
func Find(path string) Sampler {
	if path == "" {
		path = "ffmpeg"
	}
	p, err := exec.LookPath(path)
	if err != nil {
		return Sampler{}
	}
	return Sampler{FFmpeg: p}
}

// Available says the sampler has an ffmpeg.
func (s Sampler) Available() bool { return s.FFmpeg != "" }

// ErrUnavailable is Sample on a sampler with no ffmpeg.
var ErrUnavailable = errors.New("frames: no ffmpeg: frame sampling is off")

// Frame is one extracted frame.
type Frame struct {
	// MeetingMS is the meeting time asked for; Part and OffsetMS where that
	// is in the recording (timeline.Locate).
	MeetingMS int64 `json:"meeting_ms"`
	Part      int   `json:"part"`
	OffsetMS  int64 `json:"offset_ms"`
	// JPEG is the frame. Never logged.
	JPEG []byte `json:"-"`
}

// Skip is a time with no frame, and why: a fixed phrase.
type Skip struct {
	MeetingMS int64  `json:"meeting_ms"`
	Why       string `json:"why"`
}

// The reasons a time is skipped.
const (
	SkipNotRecorded = "not in the recording"
	SkipFailed      = "ffmpeg found no frame there"
	SkipOverLimit   = "over the frame limit"
)

// Sample extracts one frame at each of times (meeting ms) from the finished
// recording parts, reading part n through open(n). A time no part covers
// (before the recording, in a gap, after it) is skipped, not guessed; so is
// one ffmpeg cannot decode, and every time past MaxFrames. The frames come
// back in the order asked.
func (s Sampler) Sample(ctx context.Context, parts []timeline.Part, open func(part int) Object, times []int64) ([]Frame, []Skip, error) {
	if !s.Available() {
		return nil, nil, ErrUnavailable
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	src, err := serve(ctx, open)
	if err != nil {
		return nil, nil, err
	}
	defer src.close()
	var out []Frame
	var skips []Skip
	for i, t := range times {
		if i >= MaxFrames {
			skips = append(skips, Skip{MeetingMS: t, Why: SkipOverLimit})
			continue
		}
		at := timeline.Locate(parts, t)
		if at == nil {
			skips = append(skips, Skip{MeetingMS: t, Why: SkipNotRecorded})
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, timeout)
		jpg, err := s.extract(fctx, src.url(at.Part), at.OffsetMS)
		cancel()
		if ctx.Err() != nil {
			return out, skips, ctx.Err()
		}
		if err != nil {
			skips = append(skips, Skip{MeetingMS: t, Why: SkipFailed})
			continue
		}
		out = append(out, Frame{MeetingMS: t, Part: at.Part, OffsetMS: at.OffsetMS, JPEG: jpg})
	}
	return out, skips, nil
}

// extract runs ffmpeg once: an input seek to offset (fast: it reads the
// index, then decodes from the keyframe before offset to exactly offset),
// one frame, scaled to at most MaxWidth, as JPEG on stdout.
func (s Sampler) extract(ctx context.Context, input string, offsetMS int64) ([]byte, error) {
	sec := strconv.FormatFloat(float64(offsetMS)/1000, 'f', 3, 64)
	cmd := exec.CommandContext(ctx, s.FFmpeg, Args(input, sec)...)
	var stdout bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, n: MaxJPEG}
	cmd.Stderr = io.Discard // its text names the loopback URL; nothing in it is needed
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("frames: ffmpeg: %w", err)
	}
	b := stdout.Bytes()
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, errors.New("frames: ffmpeg wrote no JPEG")
	}
	return b, nil
}

// Args is one extraction's ffmpeg arguments. 🔴 One thread everywhere
// (decoder, filters, encoder): ffmpeg's defaults start a thread per core with
// a frame's buffers each, ~198 MiB for a 1080p H.264 run (measured), in a
// plugin limited to 256 MiB; the plugin also runs one ffmpeg at a time.
func Args(input, offsetSeconds string) []string {
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1", "-filter_threads", "1",
		"-ss", offsetSeconds, "-i", input, "-frames:v", "1", "-an", "-sn",
		"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", MaxWidth),
		"-threads", "1", "-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "3", "pipe:1"}
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("frames: the frame is over MaxJPEG")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// source is the loopback listener one Sample reads its parts through.
type source struct {
	srv  *http.Server
	base string
}

func (s *source) url(part int) string { return s.base + "/" + strconv.Itoa(part) + ".mp4" }

func (s *source) close() { _ = s.srv.Close() }

func serve(ctx context.Context, open func(part int) Object) (*source, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	secret := "/" + base64.RawURLEncoding.EncodeToString(b)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, secret+"/")
		n, err := strconv.Atoi(strings.TrimSuffix(rest, ".mp4"))
		if !ok || err != nil || n < 1 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		resp, err := open(n)(r.Context(), r.Header.Get("Range"))
		if err != nil {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, resp.Body)
		}
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go srv.Serve(ln)
	return &source{srv: srv, base: "http://" + ln.Addr().String() + secret}, nil
}
