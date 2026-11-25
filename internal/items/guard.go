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
//     existing item (or one kept earlier in this window) is a duplicate by
//     text; one whose cited segments are all cited by one such item, and
//     that anchors to the same sentence of those segments as that item
//     (sameSentence), is a duplicate by citation; either is dropped. Each
//     item anchors to the sentence(s) of the segments it cites sharing the
//     most Tokens with its text. Streaming STT can return one speaker's
//     whole turn as one segment, so two commitments said in it ("I'll send
//     the budget … Also, I can draft the survey …") cite the same segment
//     but anchor to different sentences: both are kept. A rewording of one
//     anchors to its sentence however few words it keeps. An item sharing
//     no token with any sentence of those segments falls back to the
//     citation alone (a duplicate);
//   - at most 5 are kept.
//
// The kept items' citations are deduplicated, in the model's order.
func Guard(w Window, proposed []Item) (kept []Item, dropped []Dropped) {
	return GuardCap(w, proposed, MaxPerWindow)
}

// GuardCap is Guard keeping at most limit items: the post-call pass's cap is
// what is left of the meeting's (PostCallMaxItems), not 5 per window.
func GuardCap(w Window, proposed []Item, limit int) (kept []Item, dropped []Dropped) {
	return guard(w, proposed, limit, true)
}

// GuardSupplied is the same guards over items the meeting's OWNER supplied
// (their own Claude read the transcript: the MCP tool meeting_items_set),
// with one difference: an item may cite no segment (an owner's own note
// needs no citation). Every segment it does cite must be in the window
// (the meeting's lines), its owner must be "" or known as Guard's are, a
// duplicate is dropped (by text, or by citation when it cites), and at
// most limit are kept. The caller refuses the whole set when anything is
// dropped.
func GuardSupplied(w Window, proposed []Item, limit int) (kept []Item, dropped []Dropped) {
	return guard(w, proposed, limit, false)
}

// guard is the one copy of the guards; cite says an item must cite a
// segment.
func guard(w Window, proposed []Item, limit int, cite bool) (kept []Item, dropped []Dropped) {
	seen := append([]Item{}, w.Existing...)
	for _, it := range proposed {
		it.Segments = uniq(it.Segments)
		if cite && len(it.Segments) == 0 {
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
		if _, why := duplicate(w, it, seen); why != "" {
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

// duplicate is the index in seen of the item it duplicates and why (Guard's
// two rules, by text first), -1 and "" for none. The one copy of the rule:
// the item guards drop on it, and GuardVision turns it into a Refinement.
// w holds the text of every segment it cites.
func duplicate(w Window, it Item, seen []Item) (int, string) {
	toks := Tokens(it.Text)
	for j, s := range seen {
		if Jaccard(toks, Tokens(s.Text)) >= DupJaccard {
			return j, DropDupText
		}
	}
	for j, s := range seen {
		if subset(it.Segments, s.Segments) && sameSentence(w, it, s) {
			return j, DropDupCitation
		}
	}
	return -1, ""
}

// sameSentence reports it and s anchor to one sentence of the segments it
// cites (all of which s cites too): each item's anchor is the set of those
// sentences sharing the most Tokens with its text (ties all count), and
// they are the same when the two sets meet. When either item shares no
// token with any of the sentences, there is nothing to tell them apart by,
// and it is true: the citation alone decides, as a duplicate.
func sameSentence(w Window, it, s Item) bool {
	var sents []map[string]bool
	for _, id := range it.Segments {
		for _, seg := range w.Segments {
			if seg.ID == id {
				for _, x := range Sentences(seg.Text) {
					sents = append(sents, Tokens(x))
				}
			}
		}
	}
	a, b := anchor(Tokens(it.Text), sents), anchor(Tokens(s.Text), sents)
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for i := range a {
		if b[i] {
			return true
		}
	}
	return false
}

// anchor is the indexes of the sentences sharing the most tokens with toks;
// none when no sentence shares one.
func anchor(toks map[string]bool, sents []map[string]bool) map[int]bool {
	out, best := map[int]bool{}, 0
	for i, st := range sents {
		n := 0
		for t := range toks {
			if st[t] {
				n++
			}
		}
		switch {
		case n > best:
			out, best = map[int]bool{i: true}, n
		case n == best && n > 0:
			out[i] = true
		}
	}
	return out
}

// abbrevs end in a full stop that does not end a sentence.
var abbrevs = map[string]bool{"e.g": true, "i.e": true, "etc": true, "vs": true, "mr": true, "mrs": true, "ms": true, "dr": true}

// Sentences is text split after each '.', '!' or '?' that a space (or the
// end) follows, unless the word before a '.' is one of abbrevs ("e.g.").
// Blank sentences are left out.
func Sentences(text string) []string {
	var out []string
	rs := []rune(text)
	start := 0
	for i, r := range rs {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) {
			continue
		}
		if r == '.' {
			w := string(rs[start:i])
			if k := strings.LastIndexFunc(w, unicode.IsSpace); k >= 0 {
				w = w[k+1:]
			}
			if abbrevs[strings.ToLower(strings.TrimLeft(w, "(\"'"))] {
				continue
			}
		}
		if x := strings.TrimSpace(string(rs[start : i+1])); x != "" {
			out = append(out, x)
		}
		start = i + 1
	}
	if x := strings.TrimSpace(string(rs[start:])); x != "" {
		out = append(out, x)
	}
	return out
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
