// Package stt is the plugin's speech-to-text (design: "Transcription",
// "Three engines, one interface", "Segments"). An Engine opens one Stream
// per participant's microphone track; the ingest writes 16 kHz mono into it
// and reads Segments out.
//
// This build has three engines:
//
//	utterance  the plugin's endpointer cuts the audio at pauses (≤ 15 s), and
//	           each utterance is sent to muster's existing STT service
//	           (stt-cohere, the OpenAI-style POST /v1/audio/transcriptions
//	           that muster's dictation uses): one final per utterance, about
//	           1–2 s after the speaker pauses. At most 4 in flight per
//	           plugin (it shares the GPU with dictation); each participant's
//	           queue holds 30 s of audio, and past that the oldest is dropped
//	           with a gap marker.
//	fake       the same endpointer, and a scripted line per utterance: for
//	           tests only (MEETINGS_TEST_MODE=1).
//
//	nemotron   the Nemotron server's streaming protocol (nemotron.go):
//	           partials as live captions, a final at each of the shared
//	           endpointer's pauses (its `flush`), and a per-participant
//	           fallback to utterance when the server is full or down.
//
// 🔴 A segment's text is what a participant SAID, and a guest is a stranger:
// it is untrusted text. Every page renders it with textContent only, every
// export escapes it for its format, it never reaches a log, and anything that
// later hands it to a model (phase 4's action items) must fence it as data.
// Times are in the stream's own audio time, by SAMPLE COUNT, never by
// arrival.
package stt

import (
	"context"
	"errors"
)

// Rate is the sample rate every engine takes: 16 kHz mono.
const Rate = 16000

// TrackRef names the track a stream is for. Speaker is a label for logs and
// fakes; it is guest-typed text and is never logged by this package.
type TrackRef struct {
	MeetingID string
	Track     string
	Identity  string
}

// Kind is what a Segment is.
type Kind string

const (
	// Partial is text so far for an utterance in progress: sent to the
	// panels, never stored.
	Partial Kind = "partial"
	// Final is an utterance's text: stored and sent.
	Final Kind = "final"
	// Gap is audio that was not transcribed (the engine dropped it, or the
	// STT service failed): stored as a marker so the archive shows where
	// text is missing, and the panel says "Transcript paused".
	Gap Kind = "gap"
	// EngineSwitch says the track's audio now goes to the engine Engine
	// names (the nemotron engine's per-participant fallback and its return):
	// the panel says "Captions delayed" while it is not the configured one.
	// Nothing is stored.
	EngineSwitch Kind = "engine"
	// Flag is the silent-start check's telemetry (Text: FlagSilentStart or
	// FlagDeadInput): logged and counted, never stored or shown.
	Flag Kind = "flag"
)

// Segment is one piece of a stream's transcript. StartMS and EndMS are in
// the stream's audio time (milliseconds from its first sample, by sample
// count).
type Segment struct {
	Kind    Kind
	StartMS int64
	EndMS   int64
	Text    string
	// Engine is the engine that produced it ("" is the configured one): a
	// nemotron stream's fallback lines are stored as the utterance
	// engine's.
	Engine string
}

// Engine opens streams.
type Engine interface {
	// Open starts one stream for one participant's track.
	Open(ctx context.Context, track TrackRef) (Stream, error)
	// Name is the engine's MEETINGS_STT_ENGINE value, stored on each
	// segment.
	Name() string
}

// Stream is one track's transcription.
type Stream interface {
	// Write takes the next 16 kHz mono samples (one channel of Egress' S16LE
	// stereo 48 kHz, resampled). It must not block on the STT service.
	Write(pcm []int16) error
	// Segments delivers partials, finals and gaps. It is closed after Close
	// has flushed the last of them.
	Segments() <-chan Segment
	// Close ends the stream: what is buffered is finalised, then Segments
	// closes.
	Close() error
}

// ErrClosed is a Write after Close.
var ErrClosed = errors.New("stt: the stream is closed")

// ms is a sample count at Rate in milliseconds.
func ms(samples int64) int64 { return samples * 1000 / Rate }
