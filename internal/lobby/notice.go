package lobby

import "strings"

// The consent notice's wording, from the design ("User flow: A guest", and
// "Consent"). It says what WILL happen while each switch is on: one clause
// per switch, and the transcript, which has no switch. This is the one place
// the words are written; every page shows these strings and never its own.
const (
	NoticeRecorded    = "This meeting is recorded and transcribed."
	NoticeTranscribed = "This meeting is transcribed."
	NoticeAI          = "The transcript text (not the audio) is sent to an AI service (OpenRouter) to find action items."
)

// The badge's parts, shown to everyone in the call while the meeting runs.
const (
	BadgeRecording    = "Recording"
	BadgeTranscribing = "Transcribing"
	BadgeAI           = "AI"
	// BadgeRecordingFailed replaces BadgeRecording while the meeting should
	// be recorded and no egress is (design: "Failure modes": "Egress fails to
	// start or dies: 'Recording failed' badge for everyone").
	BadgeRecordingFailed = "Recording failed"
)

// NoticeAIFrames is the AI clause for a recorded meeting on a plugin with a
// vision model: the frames of shared screens go too.
const NoticeAIFrames = "The transcript text and pictures of shared screens (not the audio) are sent to an AI service (OpenRouter) to find action items."

// WithAIClause is n with its AI clause naming the frames when frames is
// set; otherwise, and for a notice without the clause, n unchanged.
func (n Notice) WithAIClause(frames bool) Notice {
	if !frames {
		return n
	}
	repl := NoticeAIFrames
	out := append([]string(nil), n.Text...)
	for i, t := range out {
		if t == NoticeAI {
			out[i] = repl
		}
	}
	n.Text = out
	return n
}

// Without an engine (MEETINGS_STT_ENGINE off) nothing is transcribed, so
// nothing is sent to the AI service either: the notice then says only what
// does happen.
const (
	NoticeRecordedOnly = "This meeting is recorded."
	NoticeNothing      = "This meeting is not recorded or transcribed."
)

// Notice is the consent notice for a meeting's switches: the sentences a
// guest reads before knocking, and the badge everyone in the call sees.
// Transcribed is the plugin's own state (it has an engine), not a switch.
type Notice struct {
	Record      bool     `json:"record"`
	AI          bool     `json:"ai"`
	Transcribed bool     `json:"transcribed"`
	Text        []string `json:"text"`
	Badge       string   `json:"badge"`
	// RecordingFailed is set while Record is on and the recording is not
	// running (it failed to start, or died and has not restarted). Only a
	// plugin that records says so (omitted otherwise, so the notice of a
	// plugin without a bucket is exactly as before).
	RecordingFailed bool `json:"recording_failed,omitempty"`
	// Recordable says the plugin can record (it has a bucket): only then does
	// the owner's page show the Record switch.
	Recordable bool `json:"recordable"`
}

// WithRecordingFailed is n with the badge saying the recording failed. A
// notice with Record off is unchanged: nothing was meant to be recorded.
func (n Notice) WithRecordingFailed() Notice {
	if !n.Record || !n.Recordable {
		return n
	}
	parts := strings.Split(n.Badge, " · ")
	for i, p := range parts {
		if p == BadgeRecording {
			parts[i] = BadgeRecordingFailed
		}
	}
	n.Badge = strings.Join(parts, " · ")
	n.RecordingFailed = true
	return n
}

// NoticeFor writes the notice for the two switches on a plugin that
// transcribes.
func NoticeFor(record, ai bool) Notice { return NoticeWith(record, ai, true) }

// NoticeWith writes the notice for the two switches and whether the plugin
// transcribes at all, on a plugin that can record: NoticeFull.
func NoticeWith(record, ai, transcribed bool) Notice {
	return NoticeFull(record, ai, transcribed, true)
}

// NoticeFull writes the notice for the two switches, whether the plugin
// transcribes at all (the "transcribed" clause and the Transcribing badge
// only when it does, and the AI clause only when there is a transcript to
// send), and whether it can record at all (it has a bucket). 🔴 On a plugin
// that cannot record, no clause and no badge part mentions recording,
// whatever the Record switch says: nothing is recorded there. Record keeps
// the switch's state (the knock's notice check compares the switches), and
// Recordable says which of the two it is.
func NoticeFull(record, ai, transcribed, recordable bool) Notice {
	n := Notice{Record: record, AI: ai, Transcribed: transcribed, Recordable: recordable}
	record = record && recordable
	var badge []string
	switch {
	case record && transcribed:
		n.Text = append(n.Text, NoticeRecorded)
	case record:
		n.Text = append(n.Text, NoticeRecordedOnly)
	case transcribed:
		n.Text = append(n.Text, NoticeTranscribed)
	default:
		n.Text = append(n.Text, NoticeNothing)
	}
	if record {
		badge = append(badge, BadgeRecording)
	}
	if transcribed {
		badge = append(badge, BadgeTranscribing)
		if ai {
			n.Text = append(n.Text, NoticeAI)
			badge = append(badge, BadgeAI)
		}
	}
	n.Badge = strings.Join(badge, " · ")
	return n
}
