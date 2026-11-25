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
	w.Existing = []items.Item{{Text: "ship the beta build", Segments: []int64{3, 4}}}
	kept, dropped := items.Guard(w, []items.Item{
		{Text: "ship the beta", Segments: []int64{4}},           // all cited by the existing item, J=2/3: the same task
		{Text: "write release notes", Segments: []int64{4}},     // all cited by it, no word shared: another task
		{Text: "update the changelog", Segments: []int64{4, 5}}, // 5 is new: kept
	})
	if len(kept) != 2 || kept[0].Text != "write release notes" || kept[1].Text != "update the changelog" || reasons(dropped) != "[duplicate-citation]" || dropped[0].Item.Text != "ship the beta" {
		t.Errorf("by citation: kept %+v, dropped %s; want only the reworded re-proposal dropped", kept, reasons(dropped))
	}
}

// 🔴 Regression: real streaming STT returned one speaker's whole turn as
// ONE segment, and the two commitments in it were proposed citing it. The
// second was dropped as duplicate-citation (its cited segments are a subset
// of the first's) in every window, live and post-call. Two commitments in
// one segment are two items; proposing them again is still a duplicate.
func TestGuardKeepsTwoCommitmentsSaidInOneSegment(t *testing.T) {
	turn := items.Segment{ID: 7, Speaker: "Ada", StartMS: 0, EndMS: 16000,
		Text: "Hi everyone, thanks for making time. I will send the revised budget to the whole team by Friday. Also, I can draft the customer survey questions before our next call"}
	budget := items.Item{Text: "Send the revised budget to the whole team by Friday", Assignee: "Ada", Due: "Friday", Segments: []int64{7}}
	survey := items.Item{Text: "Draft the customer survey questions before the next call", Assignee: "Ada", Segments: []int64{7}}
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

// The citation rule's text bar at 0.40 exactly (2/5), below it (3/8 =
// 0.375) and above it (2/4 = 0.5), each citing only what the existing item
// cites; and the same 0.5 pair citing a segment the existing item does not,
// which no rule makes a duplicate. Texts are pairwise distinct across the
// cases.
func TestGuardDuplicateByCitationNeedsJaccard0Point4(t *testing.T) {
	cases := []struct {
		name, existing, proposed string
		cites                    []int64
		dup                      bool
	}{
		{"J=2/5", "renew office lease", "Renew the office parking permits", []int64{3}, true},
		{"J=3/8", "audit vendor invoices march ledger", "audit vendor invoices, April payroll report", []int64{3}, false},
		{"J=2/4", "email the auditor", "Email the auditor tomorrow morning", []int64{4, 3}, true},
		{"J=2/4, a new segment", "phone the landlord", "Phone the landlord at noon today", []int64{3, 5}, false},
	}
	for _, c := range cases {
		w := win(3, 4, 5)
		w.Existing = []items.Item{{Text: c.existing, Segments: []int64{3, 4}}}
		kept, dropped := items.Guard(w, []items.Item{{Text: c.proposed, Segments: c.cites}})
		if c.dup && (len(kept) != 0 || reasons(dropped) != "[duplicate-citation]") {
			t.Errorf("%s: kept %+v dropped %s, want it dropped as duplicate-citation", c.name, kept, reasons(dropped))
		}
		if !c.dup && (len(kept) != 1 || len(dropped) != 0) {
			t.Errorf("%s: dropped %s, want it kept", c.name, reasons(dropped))
		}
	}
}

func TestGuardDedupesWithinOneWindow(t *testing.T) {
	kept, dropped := items.Guard(win(1, 2, 3), []items.Item{
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
