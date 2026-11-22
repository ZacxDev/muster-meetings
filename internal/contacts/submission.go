package contacts

import (
	"encoding/json"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Meeting is the meeting a submission names (design: POST /v1/submissions'
// `meeting`). Title is the only free text: the owner's title, or the default
// label meetings shows, at most MaxTitle characters.
type Meeting struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	StartedAt *time.Time `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
}

// Participant is the guest a submission is about, as meetings knows them:
// its participants row id, the LiveKit identity (for the transcript's
// lines), what they typed at the knock, and when they were admitted and
// left.
type Participant struct {
	ID        string     `json:"id"`
	Identity  string     `json:"identity"`
	TypedName string     `json:"typed_name"`
	JoinedAt  *time.Time `json:"joined_at"`
	LeftAt    *time.Time `json:"left_at"`
}

// Submission is the body of POST /v1/submissions: a guest's card. Owner is
// the subject meetings holds for the meeting's owner (a linked pair's shared
// subject). CardID is the participant's card's public id (never the secret
// the guest's page holds): one card per participant, so it is the
// participant id, and the Idempotency-Key is "card:" + CardID.
type Submission struct {
	Owner       string      `json:"owner"`
	CardID      string      `json:"card_id"`
	Meeting     Meeting     `json:"meeting"`
	Participant Participant `json:"participant"`
	Name        string      `json:"name"`
	Email       string      `json:"email"`
}

// MaxTitle is the longest title sent (design: "at most 80 characters").
const MaxTitle = 80

// Key is the submission's Idempotency-Key (design: "Key card:<card_id>").
func (s Submission) Key() string { return "card:" + s.CardID }

// Body is the submission as sent: JSON, the title cut to MaxTitle runes.
func (s Submission) Body() ([]byte, error) {
	if r := []rune(s.Meeting.Title); len(r) > MaxTitle {
		s.Meeting.Title = string(r[:MaxTitle])
	}
	for _, t := range []**time.Time{&s.Meeting.StartedAt, &s.Meeting.EndedAt, &s.Participant.JoinedAt, &s.Participant.LeftAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return json.Marshal(s)
}

// DefaultTitle is a meeting's label when its owner gave it no title (the
// list shows its date and time).
func DefaultTitle(created time.Time) string {
	return "Meeting on " + created.UTC().Format("2 Jan 2006, 15:04 UTC")
}

// SubmissionItem is the outbox row for a card sent with name and email.
func SubmissionItem(c store.Card, name, email string) (store.OutboxItem, error) {
	title := c.Title
	if title == "" {
		title = DefaultTitle(c.CreatedAt)
	}
	sub := Submission{
		Owner:       c.Subject,
		CardID:      c.ParticipantID,
		Meeting:     Meeting{ID: c.MeetingID, Title: title, StartedAt: c.StartedAt, EndedAt: c.EndedAt},
		Participant: Participant{ID: c.ParticipantID, Identity: c.Identity, TypedName: c.TypedName, JoinedAt: c.AdmittedAt, LeftAt: c.LeftAt},
		Name:        name,
		Email:       email,
	}
	b, err := sub.Body()
	if err != nil {
		return store.OutboxItem{}, err
	}
	return store.OutboxItem{Key: sub.Key(), Kind: "submission", Path: PathSubmissions, Body: b}, nil
}
