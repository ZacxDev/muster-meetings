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

// Notice is the consent notice for a meeting's switches: the sentences a
// guest reads before knocking, and the badge everyone in the call sees.
type Notice struct {
	Record bool     `json:"record"`
	AI     bool     `json:"ai"`
	Text   []string `json:"text"`
	Badge  string   `json:"badge"`
}

// NoticeFor writes the notice for the two switches.
func NoticeFor(record, ai bool) Notice {
	n := Notice{Record: record, AI: ai}
	var badge []string
	if record {
		n.Text = append(n.Text, NoticeRecorded)
		badge = append(badge, BadgeRecording)
	} else {
		n.Text = append(n.Text, NoticeTranscribed)
	}
	badge = append(badge, BadgeTranscribing)
	if ai {
		n.Text = append(n.Text, NoticeAI)
		badge = append(badge, BadgeAI)
	}
	n.Badge = strings.Join(badge, " · ")
	return n
}
