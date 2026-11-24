package main

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster-meetings/internal/stt"
)

// The stream's layout: LeadIn of room noise before a speaker's first
// segment, Pause after each one (well past the endpointer's 700 ms, so each
// segment ends in a flush of its own), TailOut after the last.
const (
	LeadInMS  = 500
	PauseMS   = 1500
	TailOutMS = 1000
	// RoomNoise is the room noise's peak, of 32768: a microphone's floor,
	// far under the endpointer's speech threshold, so silence is never
	// bit-exact zeros (which no live microphone sends).
	RoomNoise = 6
)

// Span is one segment's audio in a speaker's stream, in ms.
type Span struct {
	ID             int64
	StartMS, EndMS int64
}

// Line is one stored line, as the plugin stores them (internal/server
// transcribe.go): a final's text, with the late closing marks of the next
// final appended (store.AmendLastLine), or a gap.
type Line struct {
	StartMS, EndMS int64
	Text           string
	Engine         string // the engine that produced it
	Gap            bool
}

// Layout is a speaker's stream: their segments' audio in order, each
// followed by a pause, over room noise of peak noiseAmp; and where each
// segment is.
func Layout(segs []Span, audio [][]int16, noiseAmp int) ([]int16, []Span) {
	var pcm []int16
	rng := uint32(1)
	noise := func(ms int) {
		for range ms * stt.Rate / 1000 {
			rng = rng*1664525 + 1013904223
			pcm = append(pcm, int16(int32(rng>>16)%int32(2*noiseAmp+1)-int32(noiseAmp)))
		}
	}
	noise(LeadInMS)
	out := make([]Span, len(segs))
	for i, s := range segs {
		start := int64(len(pcm))
		pcm = append(pcm, audio[i]...)
		out[i] = Span{s.ID, start * 1000 / stt.Rate, int64(len(pcm)) * 1000 / stt.Rate}
		noise(PauseMS)
	}
	noise(TailOutMS)
	return pcm, out
}

// Transcribe streams pcm through one stream of e, a frame (80 ms) every
// pace (0: as fast as it takes them; the server wants about real time or
// faster, and no more than its backlog), and answers the lines the plugin
// would store from it.
func Transcribe(ctx context.Context, e stt.Engine, ref stt.TrackRef, pcm []int16, pace time.Duration) ([]Line, error) {
	st, err := e.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	done := make(chan []Line, 1)
	go func() {
		var lines []Line
		for seg := range st.Segments() {
			lines = Store(lines, seg, e.Name())
		}
		done <- lines
	}()
	var tick *time.Ticker
	if pace > 0 {
		tick = time.NewTicker(pace)
		defer tick.Stop()
	}
	for off := 0; off < len(pcm); off += stt.FrameSamples {
		if err := st.Write(pcm[off:min(off+stt.FrameSamples, len(pcm))]); err != nil {
			st.Close()
			return nil, err
		}
		if tick != nil {
			select {
			case <-tick.C:
			case <-ctx.Done():
				st.Close()
				return nil, ctx.Err()
			}
		}
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	select {
	case l := <-done:
		return l, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Store is lines after seg, as the plugin stores a stream's segments: a
// final's lead first amends the previous line (unless that is a gap, empty,
// or already ends with a mark), then the final is a line unless it carried
// only a lead; a gap is a gap line; partials, engine switches and flags
// store nothing.
func Store(lines []Line, seg stt.Segment, engine string) []Line {
	if seg.Engine != "" {
		engine = seg.Engine
	}
	switch seg.Kind {
	case stt.Final:
		if lead := stt.CleanLead(seg.Lead); lead != "" && len(lines) > 0 {
			l := &lines[len(lines)-1]
			text := strings.TrimRightFunc(l.Text, unicode.IsSpace)
			if last, _ := utf8.DecodeLastRuneInString(text); !l.Gap && text != "" && !stt.IsMark(last) {
				l.Text = text + lead
			}
		}
		if seg.Text != "" || seg.Lead == "" {
			lines = append(lines, Line{StartMS: seg.StartMS, EndMS: seg.EndMS, Text: seg.Text, Engine: engine})
		}
	case stt.Gap:
		lines = append(lines, Line{StartMS: seg.StartMS, EndMS: seg.EndMS, Engine: engine, Gap: true})
	}
	return lines
}

// Assign is each segment's text: the lines whose time overlaps its audio
// the most, joined in order. A line overlapping none goes to the segment
// nearest it. A segment no line went to is "" (the engine heard nothing).
func Assign(lines []Line, spans []Span) (map[int64]string, error) {
	parts := map[int64][]string{}
	for _, l := range lines {
		if l.Gap {
			return nil, fmt.Errorf("a gap in the transcript at %d-%d ms: audio was not transcribed; run it again", l.StartMS, l.EndMS)
		}
		if strings.TrimSpace(l.Text) == "" || len(spans) == 0 {
			continue
		}
		best, bestOv := -1, int64(0)
		for i, s := range spans {
			if ov := min(l.EndMS, s.EndMS) - max(l.StartMS, s.StartMS); ov > bestOv {
				best, bestOv = i, ov
			}
		}
		if best < 0 {
			var bestDist int64
			for i, s := range spans {
				if dist := max(s.StartMS-l.EndMS, l.StartMS-s.EndMS); best < 0 || dist < bestDist {
					best, bestDist = i, dist
				}
			}
		}
		id := spans[best].ID
		parts[id] = append(parts[id], strings.TrimSpace(l.Text))
	}
	out := map[int64]string{}
	for _, s := range spans {
		out[s.ID] = strings.Join(parts[s.ID], " ")
	}
	return out, nil
}
