package ingest

import (
	"encoding/binary"
	"errors"
	"math"
)

// Egress's PCM, as spike S2 measured it for 31 minutes: binary frames of
// 1920 bytes every 10 ms, interleaved 16-bit little-endian stereo at 48 kHz,
// the two channels identical (the mono track duplicated). No header, no
// timestamps.
const (
	// InRate is Egress's sample rate.
	InRate = 48000
	// InChannels is its channel count.
	InChannels = 2
	// InFrameBytes is one stereo sample: two channels of 16 bits.
	InFrameBytes = 2 * InChannels
	// EgressMessageBytes is one Egress message: 10 ms.
	EgressMessageBytes = InRate / 100 * InFrameBytes
	// OutRate is what every engine takes: 16 kHz mono.
	OutRate = 16000
	// Decimation is InRate / OutRate.
	Decimation = InRate / OutRate
	// ChunkSamples is the frame the decoder hands an engine: 80 ms at 16 kHz
	// (the Nemotron protocol's frame; the other engines take it too).
	ChunkSamples = OutRate * 80 / 1000
)

// ErrMisaligned is a message that is not a whole number of stereo samples:
// not Egress's PCM, or a stream that has lost its alignment. It is refused,
// never carried over, since every sample after it would be read from the
// wrong byte.
var ErrMisaligned = errors.New("ingest: a message that is not a whole number of 16-bit stereo samples")

// Decoder turns Egress's messages into 16 kHz mono chunks of exactly
// ChunkSamples. It takes the FIRST channel only (the two are identical, and
// if they ever differ the first is still one person's microphone), low-pass
// filters it and keeps every third sample.
//
// 🔴 Its time base is the SAMPLE COUNT: Samples() is how many 16 kHz samples
// it has produced since the stream began, whatever the messages' arrival
// times (frames arrived in bursts after stalls of up to 7 s in S2, late but
// not lost).
type Decoder struct {
	fir     []float64
	hist    []float64 // the last len(fir)-1 input samples
	phase   int       // input samples seen, mod Decimation
	pending []int16   // output not yet a whole chunk
	out     int64     // output samples produced
}

// firTaps is the anti-alias filter's length.
const firTaps = 63

// NewDecoder returns a decoder at the start of a stream.
func NewDecoder() *Decoder {
	return &Decoder{fir: lowPass(firTaps, 7000.0/InRate), hist: make([]float64, firTaps-1)}
}

// lowPass is a Hamming-windowed sinc low-pass of n taps, cutoff fc (a
// fraction of the sample rate), with unity gain at DC.
func lowPass(n int, fc float64) []float64 {
	h := make([]float64, n)
	m := float64(n - 1)
	sum := 0.0
	for i := range h {
		x := float64(i) - m/2
		v := 2 * fc
		if x != 0 {
			v = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		v *= 0.54 - 0.46*math.Cos(2*math.Pi*float64(i)/m)
		h[i] = v
		sum += v
	}
	for i := range h {
		h[i] /= sum
	}
	return h
}

// Push takes one message and answers the whole chunks it completes (each
// exactly ChunkSamples long). A message that is not a whole number of stereo
// samples is ErrMisaligned and changes nothing.
func (d *Decoder) Push(msg []byte) ([][]int16, error) {
	if len(msg)%InFrameBytes != 0 {
		return nil, ErrMisaligned
	}
	for i := 0; i+InFrameBytes <= len(msg); i += InFrameBytes {
		x := float64(int16(binary.LittleEndian.Uint16(msg[i:]))) // channel 0 only
		if d.phase == 0 {
			acc := d.fir[0] * x
			for k := 1; k < len(d.fir); k++ {
				acc += d.fir[k] * d.hist[k-1]
			}
			d.pending = append(d.pending, clamp(acc))
			d.out++
		}
		copy(d.hist[1:], d.hist[:len(d.hist)-1])
		d.hist[0] = x
		d.phase = (d.phase + 1) % Decimation
	}
	var chunks [][]int16
	for len(d.pending) >= ChunkSamples {
		c := make([]int16, ChunkSamples)
		copy(c, d.pending[:ChunkSamples])
		d.pending = d.pending[ChunkSamples:]
		chunks = append(chunks, c)
	}
	return chunks, nil
}

// Flush answers what is left at the end of the stream (shorter than a
// chunk; nil when nothing is).
func (d *Decoder) Flush() []int16 {
	if len(d.pending) == 0 {
		return nil
	}
	c := d.pending
	d.pending = nil
	return c
}

// Samples is how many 16 kHz samples the stream has produced.
func (d *Decoder) Samples() int64 { return d.out }

func clamp(v float64) int16 {
	v = math.Round(v)
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}
