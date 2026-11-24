package items_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// recorder answers its n-th call with f(n, w) and keeps every window.
type recorder struct {
	wins []items.Window
	f    func(n int, w items.Window) (items.Result, error)
}

func (r *recorder) Name() string { return "recorder" }
func (r *recorder) Propose(_ context.Context, w items.Window) (items.Result, error) {
	n := len(r.wins)
	r.wins = append(r.wins, w)
	return r.f(n, w)
}

func winIDs(w items.Window) string {
	var out []string
	for _, s := range w.Segments {
		out = append(out, fmt.Sprint(s.ID))
	}
	return strings.Join(out, " ")
}

func span(ss []items.Segment) string { return winIDs(items.Window{Segments: ss}) }

// long is n segments, ids 1..n, each `chars` characters of speech, one
// ending every `every` ms (segment i ends at i·every).
func long(n, chars int, every int64) []items.Segment {
	var out []items.Segment
	for i := 1; i <= n; i++ {
		end := int64(i) * every
		out = append(out, items.Segment{ID: int64(i), Speaker: "Ada", StartMS: end - every + 500, EndMS: end, Text: strings.Repeat("é", chars)})
	}
	return out
}

// 25 segments of 4000 characters, one every 30 s: 12 fill a chunk (48,000),
// and the 90 s overlap is the last three (ends 300, 330 and 360 s are after
// 360 − 90 = 270 s), 12,000 characters, under half the budget: so the
// overlap is bound by TIME. The characters are 2-byte "é": a chunker
// counting bytes fits 6 a chunk.
//
//	chunk 1: 1–12                       (12 × 4000 = 48,000)
//	chunk 2: 10–21  (10–12 again)       (3 + 9 new)
//	chunk 3: 19–25  (19–21 again)
//
// 15 segments of 8000, one every 10 s: the overlap by time would be every
// segment, so it is bound by CHARACTERS: half the budget is 3 segments.
//
//	chunk 1: 1–6, chunk 2: 4–9, chunk 3: 7–12, chunk 4: 10–15
func TestChunksSplitALongMeetingWithA90sOverlap(t *testing.T) {
	cases := []struct {
		name string
		segs []items.Segment
		want []string
	}{
		{"overlap bound by time", long(25, 4000, 30000), []string{"1 2 3 4 5 6 7 8 9 10 11 12", "10 11 12 13 14 15 16 17 18 19 20 21", "19 20 21 22 23 24 25"}},
		{"overlap bound by characters", long(15, 8000, 10000), []string{"1 2 3 4 5 6", "4 5 6 7 8 9", "7 8 9 10 11 12", "10 11 12 13 14 15"}},
		{"a meeting that fits", long(20, 100, 10000), []string{"1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20"}},
		// one segment over the whole budget is still read, alone
		{"a segment over the budget", long(2, items.PostCallChunkChars+1, 10000), []string{"1", "2"}},
	}
	for _, c := range cases {
		var got []string
		for _, ch := range items.Chunks(c.segs) {
			got = append(got, span(ch))
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: chunks %q, want %q", c.name, got, c.want)
		}
	}
	if n := len(items.Chunks(nil)); n != 0 {
		t.Errorf("no segments: %d chunks, want none", n)
	}
}

// PostCallCap: 3 a minute, at least 10, over the meeting's span (the first
// segment's start to the last one's end).
func TestPostCallCapScalesWithTheMeeting(t *testing.T) {
	seg := func(startMS, endMS int64) []items.Segment {
		return []items.Segment{{ID: 1, StartMS: startMS}, {ID: 2, EndMS: endMS}}
	}
	for _, c := range []struct {
		segs []items.Segment
		want int
	}{
		{nil, 10},
		{seg(0, 2*60000), 10},         // 6, under the floor
		{seg(0, 5*60000), 15},         // 15
		{seg(0, 5*60000+1), 16},       // 15.00005 rounds up
		{seg(60000, 61*60000), 180},   // an hour, measured from the first start
		{seg(0, 90*60000+30000), 272}, // 271.5 rounds up
	} {
		if got := items.PostCallCap(c.segs); got != c.want {
			t.Errorf("PostCallCap over %v ms = %d, want %d", c.segs, got, c.want)
		}
	}
}

// A meeting that fits is ONE call reading every segment, and its answer
// goes through the live window's guards.
func TestPostCallReadsAMeetingThatFitsInOneCallThroughTheGuards(t *testing.T) {
	segs := long(20, 100, 10000)
	a := &recorder{f: func(int, items.Window) (items.Result, error) {
		return items.Result{Items: []items.Item{
			{Text: "renew the staging certificate", Assignee: "Ada", Segments: []int64{3, 4}},
			{Text: "Renew the staging certificate!", Segments: []int64{9}},    // a duplicate by text
			{Text: "wire the money", Segments: []int64{7, 99}},                // 99 is not in the meeting
			{Text: "order badges", Assignee: "Mallory", Segments: []int64{5}}, // a stranger
			{Text: "book the room", Segments: []int64{12}},
		}}, nil
	}}
	var steps []items.Step
	got, err := items.PostCall(context.Background(), a, segs, items.PostCallOpts{OnCall: func(_ int, st items.Step) { steps = append(steps, st) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.wins) != 1 || winIDs(a.wins[0]) != span(segs) || len(a.wins[0].Existing) != 0 {
		t.Fatalf("calls %d, first window %q: want one call reading every segment", len(a.wins), winIDs(a.wins[0]))
	}
	if fmt.Sprint(got) != fmt.Sprint([]items.Item{
		{Text: "renew the staging certificate", Assignee: "Ada", Segments: []int64{3, 4}},
		{Text: "book the room", Segments: []int64{12}},
	}) {
		t.Errorf("kept %+v, want the certificate and the room: the guards dropped nothing", got)
	}
	if len(steps) != 1 || reasons(steps[0].Dropped) != "[duplicate-text out-of-window bad-owner]" {
		t.Errorf("the call's drops: %d steps, %v", len(steps), steps)
	}
}

// Across chunks: the model is shown what earlier chunks kept, an item said
// again in the overlap is dropped as a duplicate, and a request at the end
// of one chunk with its acceptance at the start of the next is read whole
// in the second.
func TestPostCallDedupesAcrossChunksAndReadsTheOverlap(t *testing.T) {
	segs := long(25, 4000, 30000) // chunks 1–12, 10–21, 19–25
	a := &recorder{f: func(n int, w items.Window) (items.Result, error) {
		switch n {
		case 0:
			return items.Result{Items: []items.Item{{Text: "renew the staging certificate", Segments: []int64{11}}}}, nil
		case 1:
			var its []items.Item
			if w.Has(12) && w.Has(13) {
				its = append(its, items.Item{Text: "send the signed contract", Segments: []int64{12, 13}})
			}
			return items.Result{Items: append(its,
				items.Item{Text: "Renew the staging certificate.", Segments: []int64{11, 14}},
				items.Item{Text: "book the review room", Segments: []int64{20}})}, nil
		}
		return items.Result{}, nil
	}}
	got, err := items.PostCall(context.Background(), a, segs, items.PostCallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.wins) != 3 {
		t.Fatalf("%d calls, want 3", len(a.wins))
	}
	if e := a.wins[1].Existing; len(e) != 1 || e[0].Text != "renew the staging certificate" {
		t.Errorf("the second chunk was shown current items %+v: want what the first kept", e)
	}
	want := []items.Item{
		{Text: "renew the staging certificate", Segments: []int64{11}},
		{Text: "send the signed contract", Segments: []int64{12, 13}},
		{Text: "book the review room", Segments: []int64{20}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("kept %+v, want %+v: the repeat dropped, the request in one chunk and its acceptance in the next kept", got, want)
	}
}

// The cap is the meeting's: 25 segments one every 4 s is 99.5 s, so 10. Two
// chunks proposing 8 distinct items each keep 8 then 2, and the third chunk
// is never called.
func TestPostCallKeepsAtMostTheMeetingsCap(t *testing.T) {
	segs := long(25, 4000, 4000)
	if c := items.PostCallCap(segs); c != 10 {
		t.Fatalf("the fixture's cap is %d, want 10", c)
	}
	if n := len(items.Chunks(segs)); n < 3 {
		t.Fatalf("the fixture is %d chunks, want at least 3", n)
	}
	a := &recorder{f: func(n int, w items.Window) (items.Result, error) {
		var its []items.Item
		for i, s := range w.Segments[len(w.Segments)-8:] {
			its = append(its, items.Item{Text: fmt.Sprintf("task %c%d", 'a'+n, i), Segments: []int64{s.ID}})
		}
		return items.Result{Items: its}, nil
	}}
	got, err := items.PostCall(context.Background(), a, segs, items.PostCallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 || len(a.wins) != 2 {
		t.Errorf("kept %d in %d calls, want the cap 10 in 2 calls", len(got), len(a.wins))
	}
}

// A chunk is tried 3 times: a retryable failure after the backoff, a
// schema failure at once. A chunk that fails all 3 is lost and the pass
// goes on; the error says so, with the last failure's class.
func TestPostCallRetriesAChunkThenGoesOn(t *testing.T) {
	segs := long(25, 4000, 30000)                                                     // 3 chunks
	script := []error{retry(2 * time.Second), &items.Error{Class: items.Schema}, nil, // chunk 1: the third attempt answers
		&items.Error{Class: items.Schema}, retry(0), &items.Error{Class: items.Schema}, // chunk 2: lost
		nil} // chunk 3
	a := &recorder{f: func(n int, w items.Window) (items.Result, error) {
		if script[n] != nil {
			return items.Result{}, script[n]
		}
		return items.Result{Items: []items.Item{{Text: fmt.Sprintf("item from call %d", n), Segments: []int64{w.Segments[len(w.Segments)-1].ID}}}}, nil
	}}
	var slept []time.Duration
	var chunkOf []int
	got, err := items.PostCall(context.Background(), a, segs, items.PostCallOpts{
		Sleep:  func(d time.Duration) { slept = append(slept, d) },
		OnCall: func(c int, _ items.Step) { chunkOf = append(chunkOf, c) },
	})
	if fmt.Sprint(chunkOf) != "[0 0 0 1 1 1 2]" {
		t.Fatalf("calls by chunk %v, want [0 0 0 1 1 1 2]: each chunk tried up to 3 times", chunkOf)
	}
	// Backoff(1, Retry-After 2 s) = 2 s; Backoff(2, none) = 2 s.
	if fmt.Sprint(slept) != "[2s 2s]" {
		t.Errorf("waits %v, want [2s 2s]: only a retryable failure waits", slept)
	}
	if len(got) != 2 || got[0].Text != "item from call 2" || got[1].Text != "item from call 6" {
		t.Errorf("kept %+v, want the first and the third chunk's items", got)
	}
	if err == nil || !strings.Contains(err.Error(), "lost 1 of 3 chunks after 3 attempts each") || items.ClassOf(err) != items.Schema {
		t.Errorf("err = %v (class %v), want the lost chunk named, class schema", err, items.ClassOf(err))
	}
}

func TestPostCallStopsOnAFatalFailure(t *testing.T) {
	segs := long(25, 4000, 30000) // 3 chunks
	a := &recorder{f: func(n int, w items.Window) (items.Result, error) {
		if n == 1 {
			return items.Result{}, &items.Error{Class: items.Fatal, Status: 404}
		}
		return items.Result{Items: []items.Item{{Text: "the first chunk's item", Segments: []int64{1}}}}, nil
	}}
	got, err := items.PostCall(context.Background(), a, segs, items.PostCallOpts{Sleep: func(time.Duration) {}})
	if len(a.wins) != 2 || items.ClassOf(err) != items.Fatal || len(got) != 1 {
		t.Errorf("%d calls, err %v, kept %d: want the pass stopped at the fatal second call with the first chunk's item", len(a.wins), err, len(got))
	}
}

func TestThePostCallConfigWritesMoreAndWaitsLonger(t *testing.T) {
	base := items.Config{Model: "deepseek/deepseek-v4.1-flash-1017", Adapter: items.AdapterChatJSON, Timeout: 5 * time.Second}
	if c := items.PostCallConfig(base); c.MaxTokens != 8192 || c.Timeout != 2*time.Minute || c.Model != base.Model {
		t.Errorf("PostCallConfig(chat_json) = max_tokens %d, timeout %v; want 8192 and 2m", c.MaxTokens, c.Timeout)
	}
	if c := items.PostCallConfig(items.Config{Adapter: items.AdapterExtract}); c.MaxTokens != 0 || c.Timeout != 2*time.Minute {
		t.Errorf("PostCallConfig(extract) = max_tokens %d, timeout %v; want extract's own default and 2m", c.MaxTokens, c.Timeout)
	}
	f := newFake(t, func(int, []byte) (int, map[string]string, string) { return content(`{"items":[]}`) })
	p, _ := items.ParsePrefs(pin, items.AdapterChatJSON)
	a, err := items.New(items.PostCallConfig(items.Config{Model: base.Model, Adapter: items.AdapterChatJSON, Prefs: p, APIKey: "sk-test-key", BaseURL: f.srv.URL + "/api/v1"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Propose(context.Background(), hostile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.bodies[0]), `"max_tokens":8192,`) {
		t.Errorf("a post-call request sent %s: want max_tokens 8192", f.bodies[0])
	}
}
