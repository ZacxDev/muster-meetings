package items_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

var vwin = items.VisionWindow{
	Frames: []items.FrameIn{{MS: 6000, JPEG: []byte{0xFF, 0xD8}}, {MS: 9000, JPEG: []byte{0xFF, 0xD8}}},
	Segments: []items.Segment{
		{ID: 11, Speaker: "Ada L.", StartMS: 5000, EndMS: 7000, Text: "On this slide, I'll close the ticket."},
		{ID: 12, Speaker: "Grace H.", Guest: true, StartMS: 8000, EndMS: 9500, Text: "And I'll update the chart."},
	},
	Existing: []items.Item{{Text: "Close the ticket", Assignee: "Ada L.", Segments: []int64{11}}},
}

// 🔴 The vision guards: a note must cite a frame that was sent and only
// excerpt segments; an item must cite a sent frame; one restating an item
// the meeting has is that item's refinement, never a new item.
func TestGuardVision(t *testing.T) {
	notes := []items.Note{
		{FrameMS: 6000, Text: "Ticket MEET-42 is open.", Segments: []int64{11}},
		{FrameMS: 6000, Text: "a second note for the same frame", Segments: []int64{11}},
		{FrameMS: 7777, Text: "a frame never sent", Segments: []int64{11}},
		{FrameMS: 9000, Text: "cites a line from elsewhere", Segments: []int64{99}},
		{FrameMS: 9000, Text: "  ", Segments: []int64{}},
	}
	proposed := []items.Item{
		{Text: "Close ticket MEET-42", Assignee: "Ada L.", Segments: []int64{11}, Frames: []int64{6000}},
		{Text: "Update the Q3 chart", Assignee: "Grace H.", Segments: []int64{12}, Frames: []int64{9000}},
		{Text: "Cites no frame", Assignee: "Grace H.", Segments: []int64{12}},
		{Text: "Cites an unsent frame", Assignee: "Grace H.", Segments: []int64{12}, Frames: []int64{1}},
		{Text: "Cites another meeting's line", Assignee: "Grace H.", Segments: []int64{99}, Frames: []int64{9000}},
	}
	kept, its, refs, dropped := items.GuardVision(vwin, notes, proposed, 10)
	if len(kept) != 1 || kept[0].Text != "Ticket MEET-42 is open." {
		t.Errorf("notes kept = %+v", kept)
	}
	if len(its) != 1 || its[0].Text != "Update the Q3 chart" || its[0].Frames[0] != 9000 {
		t.Errorf("items kept = %+v", its)
	}
	if len(refs) != 1 || refs[0].Index != 0 || refs[0].Text != "Close ticket MEET-42" || refs[0].Frames[0] != 6000 {
		t.Errorf("refinements = %+v", refs)
	}
	var why []string
	for _, d := range dropped {
		why = append(why, d.Reason)
	}
	want := "duplicate-note frame-not-sent out-of-window empty-note no-frame frame-not-sent out-of-window"
	if strings.Join(why, " ") != want {
		t.Errorf("dropped = %s, want %s", strings.Join(why, " "), want)
	}
	if _, its, _, _ := items.GuardVision(vwin, nil, proposed[1:2], 0); len(its) != 0 {
		t.Errorf("past the limit an item was kept")
	}
}

// 🔴 A frame item restating a transcript item from the same sentence (in
// the screen's words, sharing few of the transcript's) is that item's
// refinement, never a second item.
func TestGuardVisionRefinesAnItemFromTheSameSentence(t *testing.T) {
	w := items.VisionWindow{
		Frames:   []items.FrameIn{{MS: 6000, JPEG: []byte{0xFF, 0xD8}}},
		Segments: []items.Segment{{ID: 5, Speaker: "Ada L.", StartMS: 5000, EndMS: 7000, Text: "This slide has the pricing. I'll send the deck by Friday."}},
		Existing: []items.Item{{Text: "Send the deck by Friday", Assignee: "Ada L.", Segments: []int64{5}}},
	}
	_, its, refs, dropped := items.GuardVision(w, nil, []items.Item{
		{Text: "Email the Q3 pricing slides (v2) to the team by Friday", Assignee: "Ada L.", Segments: []int64{5}, Frames: []int64{6000}},
	}, 10)
	if len(its) != 0 || len(refs) != 1 || refs[0].Index != 0 || refs[0].Text != "Email the Q3 pricing slides (v2) to the team by Friday" || len(dropped) != 0 {
		t.Errorf("items %+v, refinements %+v, dropped %+v; want one refinement of item 0 and no new item", its, refs, dropped)
	}
}

func TestExcerptAndSpread(t *testing.T) {
	segs := []items.Segment{{ID: 1, StartMS: 0, EndMS: 1000}, {ID: 2, StartMS: 70_000, EndMS: 71_000}, {ID: 3, StartMS: 140_000, EndMS: 141_000}, {ID: 4, StartMS: 175_000, EndMS: 176_000}}
	var got []int64
	for _, s := range items.Excerpt(segs, []int64{130_000, 140_000}) {
		got = append(got, s.ID)
	}
	if b, _ := json.Marshal(got); string(b) != "[2,3]" {
		t.Errorf("excerpt = %s, want [2,3] (from 60 s before the first frame to 30 s after the last)", b)
	}
	if b, _ := json.Marshal(items.Spread([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 4)); string(b) != "[1,4,7,10]" {
		t.Errorf("spread = %s", b)
	}
	if b, _ := json.Marshal(items.Spread([]int64{1, 2}, 4)); string(b) != "[1,2]" {
		t.Errorf("spread of fewer = %s", b)
	}
}

// The vision request: the provider object as configured (deny), the
// document then each frame as a data: URL; the answer parsed.
func TestVisionRequestAndAnswer(t *testing.T) {
	f := newFake(t, func(n int, body []byte) (int, map[string]string, string) {
		return content(`{"frames":[{"frame_ms":6000,"note":"MEET-42","segment_ids":[11]}],"items":[]}`)
	})
	p, _ := items.ParsePrefs(`{"order":["anthropic"],"allow_fallbacks":false}`, items.AdapterChatJSON)
	v, err := items.NewVision(items.Config{Model: "anthropic/claude-haiku-5.5", Prefs: p, APIKey: "sk-test-key", BaseURL: f.srv.URL + "/api/v1", Timeout: 2 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	notes, its, _, err := v.Describe(context.Background(), vwin)
	if err != nil || len(notes) != 1 || notes[0].Text != "MEET-42" || len(its) != 0 {
		t.Fatalf("describe = %+v %+v %v", notes, its, err)
	}
	var req struct {
		Provider map[string]any `json:"provider"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(f.bodies[0], &req)
	if req.Provider["data_collection"] != "deny" {
		t.Errorf("provider = %v", req.Provider)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(req.Messages[1].Content, &parts); err != nil || len(parts) != 3 || parts[0].Type != "text" ||
		!strings.HasPrefix(parts[1].ImageURL.URL, "data:image/jpeg;base64,") || !strings.Contains(parts[0].Text, `"frames":[6000,9000]`) {
		t.Errorf("user message = %s", req.Messages[1].Content)
	}
	var sys string
	json.Unmarshal(req.Messages[0].Content, &sys)
	if sys != items.VisionSystem || strings.Contains(sys, "close the ticket") {
		t.Errorf("the system message carries the transcript")
	}
	if _, err := items.NewVision(items.Config{Model: "anthropic/claude-haiku-5.5", Prefs: map[string]any{"data_collection": "deny"}}, nil); err == nil {
		t.Error("a vision model with no provider pin was accepted")
	}
}
