package items_test

import (
	"fmt"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

func win(ids ...int64) items.Window {
	var w items.Window
	for _, id := range ids {
		w.Segments = append(w.Segments, items.Segment{ID: id, Speaker: "Ada", EndMS: id * 1000, Text: "line"})
	}
	return w
}

func reasons(d []items.Dropped) string {
	var out []string
	for _, x := range d {
		out = append(out, x.Reason)
	}
	return fmt.Sprint(out)
}

func TestGuardDropsAnItemCitingNoSegment(t *testing.T) {
	kept, dropped := items.Guard(win(1, 2), []items.Item{{Text: "send the invoice"}, {Text: "pay the invoice", Segments: []int64{}}})
	if len(kept) != 0 || reasons(dropped) != "[no-citation no-citation]" {
		t.Errorf("an item citing no segment: kept %+v, dropped %s; want both dropped as no-citation", kept, reasons(dropped))
	}
}

func TestGuardDropsAnItemCitingASegmentOutsideTheWindow(t *testing.T) {
	kept, dropped := items.Guard(win(4, 5, 6), []items.Item{
		{Text: "renew the domain", Segments: []int64{9}},
		{Text: "order new badges", Segments: []int64{5, 3}}, // one in, one out: still dropped
		{Text: "email the auditor", Segments: []int64{4, 6}},
	})
	if len(kept) != 1 || kept[0].Text != "email the auditor" || reasons(dropped) != "[out-of-window out-of-window]" {
		t.Errorf("out-of-window: kept %+v, dropped %s; want only the item citing 4 and 6", kept, reasons(dropped))
	}
}

// The Jaccard threshold at 0.70 exactly, below it (9/13 ≈ 0.692) and above
// it (5/7 ≈ 0.714), on texts that are pairwise distinct across the cases,
// so no hardcoded answer passes all three.
func TestGuardDropsADuplicateByTextAtJaccard0Point7(t *testing.T) {
	cases := []struct {
		name, existing, proposed string
		dup                      bool
	}{
		{"J=7/10", "rotate staging database credentials vault quarterly audit report", "Rotate the staging database credentials, vault, quarterly audit summary memo.", true},
		{"J=9/13", "draft pricing page copy hero banner footer legal links signup button", "draft pricing page copy hero banner footer legal links checkout modal", false},
		{"J=5/7", "schedule vendor onboarding call next tuesday", "Schedule the vendor onboarding call next Thursday!", true},
	}
	for _, c := range cases {
		w := win(1, 2)
		w.Existing = []items.Item{{Text: c.existing, Segments: []int64{1}}}
		kept, dropped := items.Guard(w, []items.Item{{Text: c.proposed, Segments: []int64{2}}})
		if c.dup && (len(kept) != 0 || reasons(dropped) != "[duplicate-text]") {
			t.Errorf("%s: kept %+v, want it dropped as duplicate-text", c.name, kept)
		}
		if !c.dup && len(kept) != 1 {
			t.Errorf("%s: dropped %s, want it kept (under 0.7)", c.name, reasons(dropped))
		}
	}
}

func TestGuardDropsADuplicateByCitation(t *testing.T) {
	w := win(3, 4, 5)
	w.Segments[1].Text = "We ship the beta build on Monday. I'll write release notes tonight."
	w.Existing = []items.Item{{Text: "ship the beta build", Segments: []int64{3, 4}}}
	kept, dropped := items.Guard(w, []items.Item{
		{Text: "ship the beta", Segments: []int64{4}},           // all cited by the existing item, the same sentence
		{Text: "write release notes", Segments: []int64{4}},     // all cited by it, another sentence: another task
		{Text: "update the changelog", Segments: []int64{4, 5}}, // 5 is new: kept
	})
	if len(kept) != 2 || kept[0].Text != "write release notes" || kept[1].Text != "update the changelog" || reasons(dropped) != "[duplicate-citation]" || dropped[0].Item.Text != "ship the beta" {
		t.Errorf("by citation: kept %+v, dropped %s; want only the re-proposal from the same sentence dropped", kept, reasons(dropped))
	}
}

// turn is one speaker's whole 16 s turn that real streaming STT returned as
// ONE segment, with two commitments in it.
var turn = items.Segment{ID: 7, Speaker: "Ada", StartMS: 0, EndMS: 16000,
	Text: "Hi everyone, thanks for making time. I will send the revised budget to the whole team by Friday. Also, I can draft the customer survey questions before our next call"}

const (
	budgetText = "Send the revised budget to the whole team by Friday"
	surveyText = "Draft the customer survey questions before the next call"
)

// 🔴 Regression: the two commitments in turn were proposed citing it, and
// the second was dropped as duplicate-citation (its cited segments are a
// subset of the first's) in every window, live and post-call. Two
// commitments in one segment are two items; proposing them again is still a
// duplicate.
func TestGuardKeepsTwoCommitmentsSaidInOneSegment(t *testing.T) {
	budget := items.Item{Text: budgetText, Assignee: "Ada", Due: "Friday", Segments: []int64{7}}
	survey := items.Item{Text: surveyText, Assignee: "Ada", Segments: []int64{7}}
	w := items.Window{Segments: []items.Segment{turn}}
	kept, dropped := items.Guard(w, []items.Item{budget, survey})
	if len(kept) != 2 || kept[0].Text != budget.Text || kept[1].Text != survey.Text || len(dropped) != 0 {
		t.Fatalf("one segment, two commitments: kept %+v, dropped %s %+v; want both kept", kept, reasons(dropped), dropped)
	}
	// The next window holds the same segment and both items: proposed again,
	// both are duplicates.
	w.Existing = kept
	kept, dropped = items.Guard(w, []items.Item{survey, budget})
	if len(kept) != 0 || reasons(dropped) != "[duplicate-text duplicate-text]" {
		t.Errorf("both proposed again: kept %+v, dropped %s; want both dropped as duplicate-text", kept, reasons(dropped))
	}
}

// The citation rule by sentence: an item citing only what an existing item
// cites is a duplicate when both anchor to the same sentence of it (the
// sentence sharing the most words with each), however it is reworded (the
// live loop re-proposes a segment in each of the ~9 windows that hold it);
// a commitment from another sentence of the segment is kept; an item
// sharing no word with any sentence falls back to the citation alone.
func TestGuardDuplicateByCitationIsBySentence(t *testing.T) {
	segs := []items.Segment{turn,
		{ID: 8, Speaker: "Ada", StartMS: 20000, EndMS: 23000, Text: "Thanks all. I'll send the deck by Friday."},
		{ID: 9, Speaker: "Ada", StartMS: 24000, EndMS: 28000, Text: "Great, that settles the date. I'll book the caterer."},
	}
	cases := []struct {
		existing string
		cites    int64
		proposed string
		want     string // the drop reason, "" for kept
	}{
		{budgetText, 7, "Ada to share the updated budget with the team before Friday", "duplicate-citation"},
		{"Send the deck by Friday", 8, "Email the slide deck to everyone by Friday", "duplicate-citation"},
		{"Book the caterer", 9, "Ada will contact the catering company to book", "duplicate-citation"},
		{budgetText, 7, "Send revised budget to team (Friday)", "duplicate-text"},
		{surveyText, 7, "Prepare customer survey questions for next call", "duplicate-citation"},
		{budgetText, 7, surveyText, ""},
		// "whole team" is in the budget's sentence, but the survey's shares more.
		{budgetText, 7, "Draft the survey questions for the whole team", ""},
		// no word of it in any sentence of the segment: the citation decides
		{budgetText, 7, "Ada will follow up on it", "duplicate-citation"},
	}
	for _, c := range cases {
		w := items.Window{Segments: segs, Existing: []items.Item{{Text: c.existing, Segments: []int64{c.cites}}}}
		kept, dropped := items.Guard(w, []items.Item{{Text: c.proposed, Segments: []int64{c.cites}}})
		got := ""
		if len(dropped) == 1 {
			got = dropped[0].Reason
		}
		if got != c.want || len(kept)+len(dropped) != 1 {
			t.Errorf("%q after %q: kept %d, dropped %s; want %q", c.proposed, c.existing, len(kept), reasons(dropped), c.want)
		}
	}
}

func TestSentences(t *testing.T) {
	got := items.Sentences("Use a tool, e.g. the tracker. Then ship v2.1 to Dr. Bo! Done? yes")
	if want := `["Use a tool, e.g. the tracker." "Then ship v2.1 to Dr. Bo!" "Done?" "yes"]`; fmt.Sprintf("%q", got) != want {
		t.Errorf("Sentences = %q, want %s", got, want)
	}
}

func TestGuardDedupesWithinOneWindow(t *testing.T) {
	w := win(1, 2, 3)
	w.Segments[0].Text = "Book the venue for Friday. Then hire a caterer."
	kept, dropped := items.Guard(w, []items.Item{
		{Text: "book the venue", Segments: []int64{1}},
		{Text: "Book the venue.", Segments: []int64{2}},
		{Text: "book the venue for Friday", Segments: []int64{1}},
		{Text: "hire a caterer", Segments: []int64{1}},
	})
	if len(kept) != 2 || kept[1].Text != "hire a caterer" || reasons(dropped) != "[duplicate-text duplicate-citation]" {
		t.Errorf("within a window: kept %+v, dropped %s; want the two restatements dropped and the caterer kept", kept, reasons(dropped))
	}
}

func TestGuardKeepsAtMostFivePerWindow(t *testing.T) {
	texts := []string{"call the plumber", "renew passports", "buy printer toner", "archive old tickets", "invite new hires", "paint the fence", "sell the van"}
	var p []items.Item
	for i, s := range texts {
		p = append(p, items.Item{Text: s, Segments: []int64{int64(i + 1)}})
	}
	kept, dropped := items.Guard(win(1, 2, 3, 4, 5, 6, 7), p)
	if len(kept) != 5 || kept[4].Text != "invite new hires" || reasons(dropped) != "[over-cap over-cap]" {
		t.Errorf("cap: kept %d, dropped %s; want the first 5 kept and 2 over-cap", len(kept), reasons(dropped))
	}
}

func TestTokensAndJaccard(t *testing.T) {
	got := items.Tokens("I'll send Bo the deck by Friday — OK?")
	if fmt.Sprint(got) != "map[bo:true deck:true friday:true send:true]" {
		t.Errorf("Tokens = %v", got)
	}
	if j := items.Jaccard(items.Tokens("the"), items.Tokens("a")); j != 0 {
		t.Errorf("two empty sets: %v, want 0", j)
	}
}

func seg(id, endMS int64) items.Segment {
	return items.Segment{ID: id, Speaker: "Ada", EndMS: endMS, Text: "x"}
}

func ids(s []items.Segment) string {
	var out []int64
	for _, x := range s {
		out = append(out, x.ID)
	}
	return fmt.Sprint(out)
}

func TestSelectMakesNoCallWithoutANewSegment(t *testing.T) {
	finals := []items.Segment{seg(1, 5000), seg(2, 12000), seg(3, 31000)}
	if w, ok := items.Select(finals, 40000, 3); ok || w != nil {
		t.Errorf("no segment after 3: window %s ok %v, want no call", ids(w), ok)
	}
	// segment 3 has not ended at 20 s: it is not final yet
	if w, ok := items.Select(finals, 20000, 2); ok {
		t.Errorf("at 20 s after 2: window %s, want no call", ids(w))
	}
	if w, ok := items.Select(finals, 40000, 2); !ok || ids(w) != "[1 2 3]" {
		t.Errorf("after 2: window %s ok %v, want [1 2 3]", ids(w), ok)
	}
}

func TestSelectLooksBack90sAndCoversAGapAfterFailures(t *testing.T) {
	finals := []items.Segment{seg(1, 5000), seg(2, 50000), seg(3, 100000), seg(4, 150000)}
	// at 150 s after 3: 2 ended 100 s ago (out), 3 and 4 in the last 90 s
	if w, ok := items.Select(finals, 150000, 3); !ok || ids(w) != "[3 4]" {
		t.Errorf("lookback: window %s, want [3 4]", ids(w))
	}
	// the calls since segment 1 failed: 2 is older than 90 s but not yet covered
	if w, ok := items.Select(finals, 150000, 1); !ok || ids(w) != "[2 3 4]" {
		t.Errorf("gap: window %s, want [2 3 4] (the window is anchored on the last segment processed)", ids(w))
	}
}

func TestGuardDropsAnOwnerWhoIsNeitherASpeakerNorNamed(t *testing.T) {
	w := items.Window{Segments: []items.Segment{
		{ID: 1, Speaker: "Ada L.", Text: "I'll send the deck."},
		{ID: 2, Speaker: "Eve", Guest: true, Text: "Can you ask Fay to sign it?"},
		{ID: 3, Speaker: "Eve", Guest: true, Text: "Sure, I will chase it."},
	}}
	kept, dropped := items.Guard(w, []items.Item{
		{Text: "send the deck", Assignee: "Ada", Segments: []int64{1}},         // a speaker's first name
		{Text: "chase the signature", Assignee: "eve", Segments: []int64{3}},   // a speaker, case ignored
		{Text: "sign the contract", Assignee: "Fay", Segments: []int64{2}},     // named in a cited segment
		{Text: "countersign the order", Assignee: "Fay", Segments: []int64{1}}, // named, but not where it cites
		{Text: "wire the money", Assignee: "Mallory", Segments: []int64{3}},    // a stranger
		{Text: "update the tracker", Assignee: "", Segments: []int64{2, 3}},    // nobody took it
	})
	if len(kept) != 4 || reasons(dropped) != "[bad-owner bad-owner]" || dropped[0].Item.Text != "countersign the order" || dropped[1].Item.Text != "wire the money" {
		t.Errorf("owners: kept %d, dropped %s %+v; want Fay-elsewhere and Mallory dropped as bad-owner", len(kept), reasons(dropped), dropped)
	}
}

// 🔴 The owner's own items (GuardSupplied, the MCP tool meeting_items_set):
// the model path's guards, except that an item may cite nothing. A cited
// segment outside the meeting, an owner no one by that name, a duplicate
// and an item past the cap are dropped as Guard drops them.
func TestGuardSuppliedIsGuardButAnItemMayCiteNothing(t *testing.T) {
	w := win(4, 5)
	kept, dropped := items.GuardSupplied(w, []items.Item{
		{Text: "renew the domain"},                                        // no citation: kept
		{Text: "order new badges", Assignee: "Ada", Segments: []int64{5}}, // a speaker: kept
		{Text: "book the venue hall", Segments: []int64{9}},               // not a line of the meeting
		{Text: "email the auditor", Assignee: "Mallory"},                  // no one by that name
		{Text: "Renew the domain."},                                       // the first again, by text
		{Text: "print the seating chart", Segments: []int64{4}},           // the third kept
		{Text: "water the office plants"},                                 // over the cap of 3
	}, 3)
	var got []string
	for _, k := range kept {
		got = append(got, k.Text)
	}
	if fmt.Sprint(got) != "[renew the domain order new badges print the seating chart]" ||
		reasons(dropped) != "[out-of-window bad-owner duplicate-text over-cap]" {
		t.Errorf("kept %v, dropped %s", got, reasons(dropped))
	}
	// The model path still drops the uncited.
	if k, d := items.GuardCap(w, []items.Item{{Text: "renew the domain"}}, 3); len(k) != 0 || reasons(d) != "[no-citation]" {
		t.Errorf("GuardCap of an uncited item: kept %+v, dropped %s", k, reasons(d))
	}
}
