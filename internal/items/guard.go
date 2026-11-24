package items

import (
	"strings"
	"unicode"
)

// MaxPerWindow is the most new items one window may add.
const MaxPerWindow = 5

// DupJaccard is the token Jaccard at or above which two items are one.
const DupJaccard = 0.7

// Why an item was dropped.
const (
	DropNoCitation  = "no-citation"
	DropOutOfWindow = "out-of-window"
	DropBadOwner    = "bad-owner"
	DropDupText     = "duplicate-text"
	DropDupCitation = "duplicate-citation"
	DropOverCap     = "over-cap"
)

// Dropped is a proposed item the guards refused, and why.
type Dropped struct {
	Item   Item
	Reason string
}

// Guard is the deterministic check after the model (design: "The worker"),
// in order:
//
//   - an item citing no segment, or ANY segment outside the window, is
//     dropped;
//   - an item whose owner is neither a speaker in the window nor a name said
//     in one of its cited segments is dropped (a model, or speech that
//     reached it, cannot name a stranger as the owner); no owner is allowed;
//   - an item whose normalised text has token Jaccard ≥ 0.7 with an
//     existing item (or one kept earlier in this window), or whose cited
//     segments are all cited by one such item, is a duplicate and dropped;
//   - at most 5 are kept.
//
// The kept items' citations are deduplicated, in the model's order.
func Guard(w Window, proposed []Item) (kept []Item, dropped []Dropped) {
	return GuardCap(w, proposed, MaxPerWindow)
}

// GuardCap is Guard keeping at most limit items: the post-call pass's cap is
// what is left of the meeting's (PostCallMaxItems), not 5 per window.
func GuardCap(w Window, proposed []Item, limit int) (kept []Item, dropped []Dropped) {
	seen := append([]Item{}, w.Existing...)
	for _, it := range proposed {
		it.Segments = uniq(it.Segments)
		if len(it.Segments) == 0 {
			dropped = append(dropped, Dropped{it, DropNoCitation})
			continue
		}
		in := true
		for _, id := range it.Segments {
			if !w.Has(id) {
				in = false
				break
			}
		}
		if !in {
			dropped = append(dropped, Dropped{it, DropOutOfWindow})
			continue
		}
		if !ownerKnown(it, w) {
			dropped = append(dropped, Dropped{it, DropBadOwner})
			continue
		}
		if why := duplicate(it, seen); why != "" {
			dropped = append(dropped, Dropped{it, why})
			continue
		}
		if len(kept) >= limit {
			dropped = append(dropped, Dropped{it, DropOverCap})
			continue
		}
		kept = append(kept, it)
		seen = append(seen, it)
	}
	return kept, dropped
}

// duplicate is why it duplicates one of seen, "" for not.
func duplicate(it Item, seen []Item) string {
	toks := Tokens(it.Text)
	for _, s := range seen {
		if Jaccard(toks, Tokens(s.Text)) >= DupJaccard {
			return DropDupText
		}
	}
	for _, s := range seen {
		if subset(it.Segments, s.Segments) {
			return DropDupCitation
		}
	}
	return ""
}

// ownerKnown reports it's owner is "", a window speaker (or a speaker's
// first name), or a name said, as whole words, in a segment it cites. Case
// is ignored.
func ownerKnown(it Item, w Window) bool {
	o := words(it.Assignee)
	if o == "" {
		return true
	}
	for _, s := range w.Segments {
		if o == words(s.Speaker) || o == words(firstWord(s.Speaker)) {
			return true
		}
	}
	for _, s := range w.Segments {
		for _, id := range it.Segments {
			if s.ID == id && strings.Contains(" "+words(s.Text)+" ", " "+o+" ") {
				return true
			}
		}
	}
	return false
}

// words is s lower-cased with every run of non-letters one space.
func words(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }), " ")
}

func subset(a, b []int64) bool {
	for _, x := range a {
		found := false
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(a) > 0
}

func uniq(ids []int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// stopWords are removed before comparing two items' text.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but if of to in on at by for with from as is are was were be been being
		it its this that these those i i'll i'm i've you you'll your we we'll our they their he she him her his them me my
		will would shall should can could may might must do does did done have has had not no so then than there here
		up out about into over just also too very all any some each which who whom what when where why how okay ok yes yeah sure`) {
		stopWords[w] = true
	}
}

// Tokens is text normalised: lower case, punctuation removed (an apostrophe
// inside a word is kept), stop words removed, as a set.
func Tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’'
	}) {
		w = strings.Trim(strings.ReplaceAll(w, "’", "'"), "'")
		if w != "" && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// Jaccard is |a ∩ b| / |a ∪ b|; two empty sets are 0 (nothing to compare,
// so not a duplicate by text).
func Jaccard(a, b map[string]bool) float64 {
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
