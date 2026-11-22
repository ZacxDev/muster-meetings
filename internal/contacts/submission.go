package contacts

import (
	"encoding/json"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The wire types are the contacts plugin's own (contactsapi, pinned in
// go.mod): meetings never defines a body of its own.

// DefaultTitle is a meeting's label when its owner gave it no title (the
// list shows its date and time).
func DefaultTitle(created time.Time) string {
	return "Meeting on " + created.UTC().Format("2 Jan 2006, 15:04 UTC")
}

// utc is t in UTC (nil stays nil): the wire's times are RFC 3339 UTC.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// SubmissionItem is the outbox row for a card sent with name and email
// (already checked by contactsapi.CleanName and contactsapi.CheckEmail). The
// body is checked with contactsapi's own Submission.Check before it is
// written, so a row contacts would refuse never enters the outbox.
func SubmissionItem(c store.Card, name, email string) (store.OutboxItem, error) {
	title := c.Title
	if r := []rune(title); len(r) > contactsapi.MaxTitleRunes {
		title = string(r[:contactsapi.MaxTitleRunes])
	}
	if t, err := contactsapi.CleanTitle(title); err != nil || t == "" {
		title = DefaultTitle(c.CreatedAt)
	} else {
		title = t
	}
	sub := contactsapi.Submission{
		Owner:       c.Subject,
		CardID:      c.CardID,
		Meeting:     contactsapi.Meeting{ID: c.MeetingID, Title: title, StartedAt: utc(c.StartedAt), EndedAt: utc(c.EndedAt)},
		Participant: contactsapi.Participant{ID: c.ParticipantID, Identity: c.Identity, TypedName: c.TypedName, JoinedAt: utc(c.AdmittedAt), LeftAt: utc(c.LeftAt)},
		Name:        name,
		Email:       email,
	}
	if _, _, _, _, err := sub.Check(); err != nil {
		return store.OutboxItem{}, err
	}
	b, err := json.Marshal(sub)
	if err != nil {
		return store.OutboxItem{}, err
	}
	return store.OutboxItem{Key: contactsapi.KeyForCard(sub.CardID), Kind: "submission", Path: contactsapi.RouteSubmissions, Body: b}, nil
}
