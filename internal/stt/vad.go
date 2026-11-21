package stt

import "math"

// The endpointer (design: "Endpointing: the plugin's energy VAD, the same
// one the utterance engine uses"): it decides where an utterance starts and
// ends from the audio's energy, after removing DC offset with a high-pass
// filter (clipped and DC-offset microphones were the benchmark's empties).
// One endpointer serves every engine, so a pause finalises the same way
// whichever engine a participant is on.
//
// It works in 20 ms subframes. A subframe is speech when its RMS, after the
// high-pass, is at least Threshold. An utterance starts at the first speech
// subframe (with PreRoll of the audio before it, so the first syllable is not
// cut), and ends when SilenceEnd has passed with no speech subframe, or when
// it reaches MaxUtterance (it is cut there, and a new one begins at once if
// the speech goes on). An utterance with fewer than MinSpeech subframes of
// speech is a click, and is dropped.
//
// A constant signal (digital silence, or a stuck DC level) has no energy
// after the high-pass, so it is never speech: that is the utterance engine's
// constant-signal check, as stt-cohere's own (app.py) is.
const (
	Subframe     = Rate / 50 // 20 ms
	Threshold    = 400.0     // RMS, of 32768 full scale (about -38 dBFS)
	SilenceEnd   = 700       // ms
	MaxUtterance = 15000     // ms
	PreRoll      = 200       // ms
	MinSpeech    = 3         // subframes (60 ms)
	// highPassR is the one-pole high-pass's pole: a corner near 13 Hz at
	// 16 kHz, well under any voice.
	highPassR = 0.995
)

// Utterance is one stretch of speech: its samples, and where it starts and
// ends in the stream (samples from the stream's first).
type Utterance struct {
	Start, End int64
	PCM        []int16
}

// StartMS and EndMS are the utterance's times in the stream, in ms.
func (u Utterance) StartMS() int64 { return ms(u.Start) }
func (u Utterance) EndMS() int64   { return ms(u.End) }

// VADEvent is what Push found: an utterance began (Began, at Start), or one
// ended (Utterance set).
type VADEvent struct {
	Began     bool
	Start     int64
	Utterance *Utterance
}

// Endpointer is one stream's VAD. It is not safe for concurrent use.
type Endpointer struct {
	px, py   float64 // the high-pass's previous input and output
	dcPrimed bool
	pos      int64   // samples consumed
	cur      []int16 // the current subframe's raw samples
	sumsq    float64
	pre      []int16 // raw audio before an utterance (PreRoll at most)
	inSpeech bool
	start    int64
	buf      []int16 // the utterance's raw samples, from start
	lastEnd  int64   // where the last speech subframe ended
	speech   int     // speech subframes in the utterance
	speechN  int64   // samples of speech subframes since the stream began
}

// NewEndpointer returns an endpointer at the start of a stream.
func NewEndpointer() *Endpointer { return &Endpointer{} }

func msSamples(m int) int64 { return int64(m) * Rate / 1000 }

// Push consumes samples and answers what began and ended in them.
func (e *Endpointer) Push(pcm []int16) []VADEvent {
	var out []VADEvent
	for _, s := range pcm {
		x := float64(s)
		if !e.dcPrimed {
			// Start the filter at the signal's own level, so a DC offset is
			// not one big step at the stream's first sample.
			e.px, e.dcPrimed = x, true
		}
		y := x - e.px + highPassR*e.py
		e.px, e.py = x, y
		e.sumsq += y * y
		e.cur = append(e.cur, s)
		e.pos++
		if len(e.cur) == Subframe {
			out = append(out, e.subframe()...)
		}
	}
	return out
}

// subframe judges the subframe just completed.
func (e *Endpointer) subframe() []VADEvent {
	var out []VADEvent
	rms := math.Sqrt(e.sumsq / float64(len(e.cur)))
	isSpeech := rms >= Threshold
	if isSpeech {
		e.speechN += int64(len(e.cur))
	}
	end := e.pos
	begin := end - int64(len(e.cur))
	cur := e.cur
	e.cur, e.sumsq = nil, 0
	if !e.inSpeech {
		if !isSpeech {
			e.pre = append(e.pre, cur...)
			if over := len(e.pre) - int(msSamples(PreRoll)); over > 0 {
				e.pre = e.pre[over:]
			}
			return nil
		}
		e.inSpeech = true
		e.start = begin - int64(len(e.pre))
		e.buf = append(append([]int16(nil), e.pre...), cur...)
		e.pre = nil
		e.lastEnd, e.speech = end, 1
		return []VADEvent{{Began: true, Start: e.start}}
	}
	e.buf = append(e.buf, cur...)
	if isSpeech {
		e.lastEnd = end
		e.speech++
	}
	switch {
	case end-e.lastEnd >= msSamples(SilenceEnd):
		// A pause: the utterance ends a little after its last speech.
		cut := min(e.lastEnd+msSamples(100), end)
		if u := e.cut(cut); u != nil {
			out = append(out, VADEvent{Utterance: u})
		}
		e.inSpeech = false
		rest := e.buf[cut-e.start:]
		e.pre = append([]int16(nil), rest[max(0, len(rest)-int(msSamples(PreRoll))):]...)
		e.buf = nil
	case end-e.start >= msSamples(MaxUtterance):
		// Too long: cut here, and go on with a new utterance if this is
		// still speech.
		if u := e.cut(end); u != nil {
			out = append(out, VADEvent{Utterance: u})
		}
		e.buf = nil
		if isSpeech {
			e.start, e.lastEnd, e.speech = end, end, 0
			out = append(out, VADEvent{Began: true, Start: end})
		} else {
			e.inSpeech = false
		}
	}
	return out
}

// cut ends the utterance at sample at (absolute), or nil when it held too
// little speech to be one.
func (e *Endpointer) cut(at int64) *Utterance {
	if e.speech < MinSpeech {
		return nil
	}
	n := at - e.start
	pcm := make([]int16, n)
	copy(pcm, e.buf[:n])
	return &Utterance{Start: e.start, End: at, PCM: pcm}
}

// Flush ends the stream: an utterance in progress ends at the last sample.
func (e *Endpointer) Flush() []VADEvent {
	if !e.inSpeech {
		return nil
	}
	e.buf = append(e.buf, e.cur...)
	e.cur, e.sumsq = nil, 0
	e.inSpeech = false
	end := min(e.lastEnd+msSamples(100), e.pos)
	if u := e.cut(end); u != nil {
		return []VADEvent{{Utterance: u}}
	}
	return nil
}

// SpeechSamples is how many samples of the stream so far were in speech
// subframes (energy at or over Threshold after the DC filter): the
// silent-start check's measure.
func (e *Endpointer) SpeechSamples() int64 { return e.speechN }

// InSpeech says an utterance is in progress, and where it began (its
// pre-roll included).
func (e *Endpointer) InSpeech() (bool, int64) { return e.inSpeech, e.start }

// Pos is how many samples the endpointer has consumed.
func (e *Endpointer) Pos() int64 { return e.pos }
