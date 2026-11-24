// Package sttcorpus is the speech-to-text variant of the action-item
// evaluation corpus (design: "Action-item evaluation": "a variant of each
// meeting passed through the real STT from synthesised speech"): the word
// error rate both the tool that makes a variant (cmd/items-corpus-stt) and
// the corpus lint (cmd/items-eval) compute, the record each variant carries,
// and the writer that turns a source meeting into its variant.
package sttcorpus

import (
	"strings"
	"unicode"
)

// Words is s as the word error rate compares it: lower case, curly
// apostrophes made straight, every run of characters that is not a letter,
// a digit or an apostrophe a word break, and apostrophes at either end of a
// word dropped. So "Thursday’s retro." and "thursday's retro" are the same
// two words. Numbers are not spelled out: "ten" and "10" differ.
func Words(s string) []string {
	s = strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(s))
	f := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	out := f[:0]
	for _, w := range f {
		if w = strings.Trim(w, "'"); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// Distance is the word-level edit distance from ref to hyp: the fewest
// substitutions, deletions and insertions that turn one into the other.
func Distance(ref, hyp []string) int {
	prev := make([]int, len(hyp)+1)
	cur := make([]int, len(hyp)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ref); i++ {
		cur[0] = i
		for j := 1; j <= len(hyp); j++ {
			sub := prev[j-1]
			if ref[i-1] != hyp[j-1] {
				sub++
			}
			cur[j] = min(sub, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return prev[len(hyp)]
}

// Score is a word error count over some reference words.
type Score struct {
	Errors int
	Words  int
}

// Add is s with the errors of hyp against ref added.
func (s Score) Add(ref, hyp string) Score {
	r := Words(ref)
	return Score{s.Errors + Distance(r, Words(hyp)), s.Words + len(r)}
}

// Rate is the word error rate, Errors / Words, rounded to 4 places as it is
// recorded (0 with no reference words).
func (s Score) Rate() float64 {
	if s.Words == 0 {
		return 0
	}
	return Round4(float64(s.Errors) / float64(s.Words))
}

// Round4 is x rounded to 4 decimal places.
func Round4(x float64) float64 {
	const k = 10000
	if x < 0 {
		return -Round4(-x)
	}
	return float64(int64(x*k+0.5)) / k
}
