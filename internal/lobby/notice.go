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
)

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
}

// NoticeFor writes the notice for the two switches on a plugin that
// transcribes.
func NoticeFor(record, ai bool) Notice { return NoticeWith(record, ai, true) }

// NoticeWith writes the notice for the two switches and whether the plugin
// transcribes at all: the "transcribed" clause and the Transcribing badge
// only when it does, and the AI clause only when there is a transcript to
// send.
func NoticeWith(record, ai, transcribed bool) Notice {
	n := Notice{Record: record, AI: ai, Transcribed: transcribed}
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
