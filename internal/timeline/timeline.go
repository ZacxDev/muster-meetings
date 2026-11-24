// Package timeline maps meeting time to recording time (design: "Media:
// LiveKit, TURN, Egress", the operator's decision 4: the recording is
// time-aligned to the transcript).
//
// Meeting time is milliseconds from the meeting's start (when the owner first
// joined, else its creation): every transcript segment is stored in it. A
// meeting's recording is one or more PARTS, one per room composite egress (a
// part ends when its egress fails and a new one is started). Each part's
// start is stored in meeting time, taken from the egress's OWN report of when
// its file began (Egress's FileInfo.started_at, the moment its pipeline
// reached PLAYING), never from when the plugin heard of it. So a segment at
// meeting time T lies in the part p with p.Start <= T < p.Start+p.Duration,
// at offset T - p.Start into that part's file. Nothing per segment is stored:
// the offset is computed here, the one place.
package timeline

// Part is one recording file's place in meeting time.
type Part struct {
	N int `json:"part"` // 1-based, in the order the egresses were started
	// StartMS is the meeting time of the file's first frame.
	StartMS int64 `json:"start_ms"`
	// DurationMS is the file's length; -1 while it is unknown (the egress is
	// still running, or it failed before it reported one).
	DurationMS int64 `json:"duration_ms"`
}

// EndMS is the meeting time the part ends at, and whether it is known.
func (p Part) EndMS() (int64, bool) {
	if p.DurationMS < 0 {
		return 0, false
	}
	return p.StartMS + p.DurationMS, true
}

// Covers says meeting time t lies in p. 🔴 A part of unknown duration covers
// nothing: until its egress has reported how long the file is, an offset into
// it could point past its end.
func (p Part) Covers(t int64) bool {
	end, ok := p.EndMS()
	return ok && t >= p.StartMS && t < end
}

// Offset is where meeting time t is in the recording: the part, and the
// milliseconds into that part's file.
type Offset struct {
	Part     int   `json:"part"`
	OffsetMS int64 `json:"offset_ms"`
}

// Locate is meeting time t's place in the recording, or nil when no part
// covers it (before the recording began, in a gap, after it ended).
func Locate(parts []Part, t int64) *Offset {
	for _, p := range parts {
		if p.Covers(t) {
			return &Offset{Part: p.N, OffsetMS: t - p.StartMS}
		}
	}
	return nil
}

// MeetingMS is a part's start in meeting time, from the egress's reported
// start (Unix nanoseconds) and the meeting's start (Unix milliseconds).
func MeetingMS(egressStartNS, meetingStartMS int64) int64 {
	return egressStartNS/1_000_000 - meetingStartMS
}

// Interval is a stretch of meeting time; ToMS is -1 while it is open.
type Interval struct {
	FromMS int64 `json:"from_ms"`
	ToMS   int64 `json:"to_ms"`
}

// Overlap is the part of a that lies in b, and whether there is any. An open
// interval (ToMS -1) runs to end.
func Overlap(a, b Interval, end int64) (Interval, bool) {
	at, bt := a.ToMS, b.ToMS
	if at < 0 {
		at = end
	}
	if bt < 0 {
		bt = end
	}
	from, to := max(a.FromMS, b.FromMS), min(at, bt)
	if from >= to {
		return Interval{}, false
	}
	return Interval{FromMS: from, ToMS: to}, true
}
