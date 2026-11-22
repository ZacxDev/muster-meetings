package store_test

import (
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The marks the server's leads are made of (stt.Marks; the store takes them
// as an argument, so it does not depend on the engines).
const marks = ".,;:!?…"

// AmendLastLine is the store's half of a late closing mark: the
// participant's LATEST segment, if it is a line with text that does not
// already end with a mark, gets the lead appended. Each case starts from a
// fresh meeting with two participants on two tracks.
func TestAmendLastLine(t *testing.T) {
	type seg struct{ track, kind, text string }
	for _, c := range []struct {
		name     string
		segs     []seg
		identity string
		lead     string
		want     []string // every segment's text afterwards, in order
		changed  bool
	}{
		{"appends to the participant's latest line", []seg{{"TR_own", "text", "Can everyone hear me clearly"}}, store.OwnerIdentity, "?",
			[]string{"Can everyone hear me clearly?"}, true},
		{"trailing whitespace goes before the mark", []seg{{"TR_own", "text", "so  "}}, store.OwnerIdentity, ",",
			[]string{"so,"}, true},
		{"already ends with the same mark", []seg{{"TR_own", "text", "Done."}}, store.OwnerIdentity, ".", []string{"Done."}, false},
		{"already ends with another mark", []seg{{"TR_own", "text", "Done."}}, store.OwnerIdentity, "?", []string{"Done."}, false},
		{"already ends with an ellipsis", []seg{{"TR_own", "text", "well…"}}, store.OwnerIdentity, ".", []string{"well…"}, false},
		{"no previous segment", nil, store.OwnerIdentity, "?", nil, false},
		{"an empty lead", []seg{{"TR_own", "text", "Hello"}}, store.OwnerIdentity, "", []string{"Hello"}, false},
		{"the latest is a gap marker", []seg{{"TR_own", "text", "Hello"}, {"TR_own", "gap", ""}}, store.OwnerIdentity, "?",
			[]string{"Hello", ""}, false},
		{"the latest is an empty line", []seg{{"TR_own", "text", "Hello"}, {"TR_own", "text", ""}}, store.OwnerIdentity, "?",
			[]string{"Hello", ""}, false},
		{"another participant's newer line is not theirs", []seg{{"TR_own", "text", "Hello"}, {"TR_gst", "text", "Hi there"}}, store.OwnerIdentity, "?",
			[]string{"Hello?", "Hi there"}, true},
		{"a participant with no line of their own", []seg{{"TR_gst", "text", "Hi there"}}, store.OwnerIdentity, "?",
			[]string{"Hi there"}, false},
		{"only the latest of the participant's lines", []seg{{"TR_own", "text", "One"}, {"TR_own", "text", "Two"}}, store.OwnerIdentity, "!",
			[]string{"One", "Two!"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := open(t)
			m := mk(t, s, subA, t0)
			room := store.RoomName(m.Meeting.ID)
			if _, err := s.UpsertTrack(ctx, room, "TR_own", store.OwnerIdentity, "Ada", t0); err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpsertTrack(ctx, room, "TR_gst", "guest-7f3a", "Grace (guest)", t0); err != nil {
				t.Fatal(err)
			}
			for i, g := range c.segs {
				if _, err := s.AddSegment(ctx, g.track, g.kind, int64(1000*i), int64(1000*i+500), g.text, "nemotron"); err != nil {
					t.Fatal(err)
				}
			}
			id, changed, err := s.AmendLastLine(ctx, m.Meeting.ID, c.identity, c.lead, marks)
			if err != nil || changed != c.changed {
				t.Fatalf("changed %v, %v; want %v", changed, err, c.changed)
			}
			var got []string
			var ids []int64
			rows, err := s.SegmentsAfter(ctx, m.Meeting.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range rows {
				got = append(got, g.Text)
				ids = append(ids, g.ID)
			}
			if len(got) != len(c.want) {
				t.Fatalf("segments %q, want %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("segments %q, want %q", got, c.want)
				}
				if c.changed && got[i] != c.segs[i].text && ids[i] != id {
					t.Fatalf("segment %d changed, but %d was answered", ids[i], id)
				}
			}
		})
	}
}
