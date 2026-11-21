package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
)

// The plugin's codes and tokens, all base64url without padding.
const (
	// JoinCodeBytes is a guest link's code: 128 bits, 22 characters
	// (design: "Guests: links, the lobby, admission"). It is derived, not
	// drawn: Store.JoinCode.
	JoinCodeBytes = 16
	// SessionTokenBytes is an owner session's token: 32 bytes, 43
	// characters after the "mmo_" prefix (design: "The owner: the hand-off").
	SessionTokenBytes = 32
	// MeetingIDBytes is a meeting's id: 96 bits, 16 characters. It is not a
	// credential (an owner route also needs the owner's session), but it is
	// random so ids reveal no count, and it fits muster's ref pattern
	// ^[A-Za-z0-9_-]{1,64}$ so a knock notification can carry it.
	MeetingIDBytes = 12
)

// SessionPrefix starts every owner session token.
const SessionPrefix = "mmo_"

var (
	joinCodeRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	sessionTokenRE = regexp.MustCompile(`^mmo_[A-Za-z0-9_-]{43}$`)
	meetingIDRE    = regexp.MustCompile(`^[A-Za-z0-9_-]{16}$`)
	subjectRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform Go supports; a plugin
		// that cannot draw a secret must not make up a weak one.
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewSessionToken is a fresh owner session token, with its prefix.
func NewSessionToken() string { return SessionPrefix + random(SessionTokenBytes) }

// NewMeetingID is a fresh meeting id.
func NewMeetingID() string { return random(MeetingIDBytes) }

// ValidJoinCode, ValidSessionToken and ValidMeetingID check a value's shape
// before it is hashed or looked up.
func ValidJoinCode(s string) bool     { return joinCodeRE.MatchString(s) }
func ValidSessionToken(s string) bool { return sessionTokenRE.MatchString(s) }
func ValidMeetingID(s string) bool    { return meetingIDRE.MatchString(s) }

// ValidTicket is a knock's ticket shape (TicketBytes, 22 base64url
// characters), and ValidParticipantID a lobby row id's.
func ValidTicket(s string) bool        { return joinCodeRE.MatchString(s) }
func ValidParticipantID(s string) bool { return meetingIDRE.MatchString(s) }

// ValidSubject is the shape of the subject muster's server-app redeem gives:
// 128 random bits, 22 base64url characters (design: "Data model and
// migration 0171"). A decimal user id, an email or "" is not one.
func ValidSubject(s string) bool { return subjectRE.MatchString(s) }

// Hash is the stored form of a code or token: hex SHA-256.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
