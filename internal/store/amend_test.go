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
		out      store.AmendOutcome
	}{
		{"appends to the participant's latest line", []seg{{"TR_own", "text", "Can everyone hear me clearly"}}, store.OwnerIdentity, "?",
			[]string{"Can everyone hear me clearly?"}, true, store.Amended},
		{"trailing whitespace goes before the mark", []seg{{"TR_own", "text", "so  "}}, store.OwnerIdentity, ",",
			[]string{"so,"}, true, store.Amended},
		{"already ends with the same mark", []seg{{"TR_own", "text", "Done."}}, store.OwnerIdentity, ".", []string{"Done."}, false, store.AmendHasMark},
		{"already ends with another mark", []seg{{"TR_own", "text", "Done."}}, store.OwnerIdentity, "?", []string{"Done."}, false, store.AmendHasMark},
		{"already ends with an ellipsis", []seg{{"TR_own", "text", "well…"}}, store.OwnerIdentity, ".", []string{"well…"}, false, store.AmendHasMark},
		{"no previous segment", nil, store.OwnerIdentity, "?", nil, false, store.AmendNoLine},
		{"an empty lead", []seg{{"TR_own", "text", "Hello"}}, store.OwnerIdentity, "", []string{"Hello"}, false, store.AmendNoLead},
		{"the latest is a gap marker", []seg{{"TR_own", "text", "Hello"}, {"TR_own", "gap", ""}}, store.OwnerIdentity, "?",
			[]string{"Hello", ""}, false, store.AmendGap},
		{"the latest is an empty line", []seg{{"TR_own", "text", "Hello"}, {"TR_own", "text", ""}}, store.OwnerIdentity, "?",
			[]string{"Hello", ""}, false, store.AmendEmpty},
		{"another participant's newer line is not theirs", []seg{{"TR_own", "text", "Hello"}, {"TR_gst", "text", "Hi there"}}, store.OwnerIdentity, "?",
			[]string{"Hello?", "Hi there"}, true, store.Amended},
		{"a participant with no line of their own", []seg{{"TR_gst", "text", "Hi there"}}, store.OwnerIdentity, "?",
			[]string{"Hi there"}, false, store.AmendNoLine},
		{"only the latest of the participant's lines", []seg{{"TR_own", "text", "One"}, {"TR_own", "text", "Two"}}, store.OwnerIdentity, "!",
			[]string{"One", "Two!"}, true, store.Amended},
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
			id, out, err := s.AmendLastLine(ctx, m.Meeting.ID, c.identity, c.lead, marks)
			changed := out == store.Amended
			if err != nil || changed != c.changed || out != c.out {
				t.Fatalf("%q, %v; want %q", out, err, c.out)
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

// 🔴 A lead stays in its meeting. The owner's identity is the same constant
// in every meeting, so only the meeting filter keeps a late mark in meeting
// A off the owner's newer line in meeting B.
func TestAmendLastLineStaysInItsMeeting(t *testing.T) {
	s, _ := open(t)
	a, b := mk(t, s, subA, t0), mk(t, s, subA, t0)
	for _, m := range []struct{ id, track, text string }{{a.Meeting.ID, "TR_a", "In meeting A"}, {b.Meeting.ID, "TR_b", "In meeting B"}} {
		if _, err := s.UpsertTrack(ctx, store.RoomName(m.id), m.track, store.OwnerIdentity, "Ada", t0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddSegment(ctx, m.track, "text", 1000, 1500, m.text, "nemotron"); err != nil {
			t.Fatal(err)
		}
	}
	if _, out, err := s.AmendLastLine(ctx, a.Meeting.ID, store.OwnerIdentity, "?", marks); err != nil || out != store.Amended {
		t.Fatalf("%q, %v", out, err)
	}
	for _, m := range []struct{ id, want string }{{a.Meeting.ID, "In meeting A?"}, {b.Meeting.ID, "In meeting B"}} {
		segs, err := s.SegmentsAfter(ctx, m.id, 0, 10)
		if err != nil || len(segs) != 1 || segs[0].Text != m.want {
			t.Fatalf("meeting %s holds %+v (%v), want %q", m.id, segs, err, m.want)
		}
	}
}
