package timeline_test

import (
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/timeline"
)

// The meeting started at Unix ms 1_790_000_000_000. The first egress
// reported its file began 4.2 s later; it ran 600 s and failed; a new egress
// reported its file began at meeting time 611_500 (11.5 s of gap) and ran
// 1_200 s. Every expected value below is written out, not computed.
const meetingStartMS = 1_790_000_000_000

var parts = []timeline.Part{
	{N: 1, StartMS: 4_200, DurationMS: 600_000},
	{N: 2, StartMS: 611_500, DurationMS: 1_200_000},
}

func TestMeetingMSFromTheEgressReportedStart(t *testing.T) {
	// FileInfo.started_at is Unix NANOseconds.
	if got := timeline.MeetingMS(1_790_000_004_200_000_000, meetingStartMS); got != 4_200 {
		t.Fatalf("part 1 start = %d ms, want 4200", got)
	}
	if got := timeline.MeetingMS(1_790_000_611_500_999_999, meetingStartMS); got != 611_500 {
		t.Fatalf("part 2 start = %d ms, want 611500", got)
	}
}

func TestLocateAtTheMeetingStartAndMidMeetingAfterARestart(t *testing.T) {
	for _, c := range []struct {
		name string
		t    int64
		want *timeline.Offset
	}{
		{"the meeting's first instant: before the first file began", 0, nil},
		{"one ms before part 1", 4_199, nil},
		{"part 1's first frame", 4_200, &timeline.Offset{Part: 1, OffsetMS: 0}},
		{"a line 1 min 30 s in", 90_000, &timeline.Offset{Part: 1, OffsetMS: 85_800}},
		{"part 1's last ms", 604_199, &timeline.Offset{Part: 1, OffsetMS: 599_999}},
		{"in the gap after part 1 failed", 604_200, nil},
		{"still in the gap", 611_499, nil},
		{"part 2's first frame", 611_500, &timeline.Offset{Part: 2, OffsetMS: 0}},
		{"mid-meeting after the restart, 15 min in", 900_000, &timeline.Offset{Part: 2, OffsetMS: 288_500}},
		{"after part 2 ended", 1_811_500, nil},
	} {
		got := timeline.Locate(parts, c.t)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s (t=%d): %+v, want no place in the recording", c.name, c.t, *got)
		case c.want != nil && (got == nil || *got != *c.want):
			t.Errorf("%s (t=%d): %+v, want %+v", c.name, c.t, got, *c.want)
		}
	}
}

func TestAPartOfUnknownDurationCoversNothing(t *testing.T) {
	running := []timeline.Part{{N: 1, StartMS: 4_200, DurationMS: -1}}
	if got := timeline.Locate(running, 10_000); got != nil {
		t.Fatalf("a running part placed t=10000 at %+v", *got)
	}
}

func TestOverlap(t *testing.T) {
	share := timeline.Interval{FromMS: 100_000, ToMS: -1}
	seg := timeline.Interval{FromMS: 95_000, ToMS: 103_000}
	got, ok := timeline.Overlap(seg, share, 500_000)
	if !ok || got != (timeline.Interval{FromMS: 100_000, ToMS: 103_000}) {
		t.Fatalf("overlap = %+v %v", got, ok)
	}
	if _, ok := timeline.Overlap(timeline.Interval{FromMS: 1, ToMS: 2}, share, 500_000); ok {
		t.Fatal("disjoint intervals overlap")
	}
}
