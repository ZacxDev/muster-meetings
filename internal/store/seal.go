package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

// LinkKeyBytes is the link key's length: AES-256.
const LinkKeyBytes = 32

// ErrLinkKey is a MEETINGS_LINK_KEY that is missing or not 32 bytes of base64. Its text
// never carries the value.
var ErrLinkKey = errors.New("MEETINGS_LINK_KEY is missing or malformed: it must be 32 random bytes, base64 (e.g. `openssl rand -base64 32`); its value is not shown")

// ErrSealed is a sealed code that does not open: another key, a row it was
// not sealed for, or a damaged value.
var ErrSealed = errors.New("the sealed link does not open")

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

// sealer seals a meeting's join code so its owner can copy the link again
// while the meeting is open (the hash alone cannot give it back). AES-256-GCM,
// a random 96-bit nonce per seal, and the meeting id as associated data, so a
// sealed code moved onto another meeting's row does not open.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(key []byte) (*sealer, error) {
	if len(key) != LinkKeyBytes {
		return nil, ErrLinkKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func linkAD(meetingID string) []byte { return []byte("muster-meetings link v1\x00" + meetingID) }

// seal is nonce || ciphertext+tag.
func (s *sealer) seal(meetingID, code string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return s.aead.Seal(nonce, nonce, []byte(code), linkAD(meetingID))
}

func (s *sealer) open(meetingID string, sealed []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return "", ErrSealed
	}
	pt, err := s.aead.Open(nil, sealed[:n], sealed[n:], linkAD(meetingID))
	if err != nil || !ValidJoinCode(string(pt)) {
		return "", ErrSealed
	}
	return string(pt), nil
}
