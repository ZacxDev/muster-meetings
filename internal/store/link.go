package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// LinkKeyBytes is MEETINGS_LINK_KEY's length.
const LinkKeyBytes = 32

// ErrLinkKey is a MEETINGS_LINK_KEY that is missing or not 32 bytes of
// base64. Its text never carries the value.
var ErrLinkKey = errors.New("MEETINGS_LINK_KEY is missing or malformed: it must be 32 random bytes, base64 (e.g. `openssl rand -base64 32`); its value is not shown")

// ParseLinkKey reads MEETINGS_LINK_KEY: 32 bytes in standard or URL-safe
// base64, padded or not.
func ParseLinkKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) == LinkKeyBytes {
			return k, nil
		}
	}
	return nil, ErrLinkKey
}

// linkDomain separates this HMAC's inputs from any other use of the key.
const linkDomain = "muster-meetings link v1\x00"

// JoinCode is a meeting's guest link code: HMAC-SHA256 under the link key
// over linkDomain and the meeting id, cut to JoinCodeBytes (128 bits) and
// written as 22 base64url characters. The code is never stored: the owner's
// link is derived again when they ask for it, and a guest's /j/<code> is
// looked up by its SHA-256. A different key, or a different meeting id, gives
// an unrelated code, so rotating MEETINGS_LINK_KEY changes every link.
func (s *Store) JoinCode(meetingID string) string {
	m := hmac.New(sha256.New, s.linkKey)
	m.Write([]byte(linkDomain + meetingID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:JoinCodeBytes])
}
