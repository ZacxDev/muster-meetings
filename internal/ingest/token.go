package ingest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The ingest URL's token (design: "The plugin's HTTP API", the ingest row).
// Egress sends NO Authorization header or any other credential on its
// websocket upgrade (spike S2), so the URL is the only place a secret can
// go: /stt/ingest/<track>/<token>.
//
// A token is per EGRESS: each start of a track egress draws a fresh random
// nonce, stored on the track's row, and the token is
//
//	<nonce>.<expiry, Unix seconds>.<HMAC-SHA256 under MEETINGS_INGEST_SECRET of
//	    "muster-meetings ingest v1", the meeting id, the track id, the nonce and the expiry>
//
// so a token opens one track of one meeting, until the meeting's own 24 h
// cap, and only while it is that track's CURRENT egress: once a newer egress
// is started for the track, an older egress's URL (Egress logs the full URL
// it was given) opens nothing. The MAC is compared in constant time
// (hmac.Equal).

// SecretBytes is the least MEETINGS_INGEST_SECRET may hold, decoded.
const SecretBytes = 32

// ErrSecret is a missing or short MEETINGS_INGEST_SECRET. Its text never
// carries the value.
var ErrSecret = errors.New("MEETINGS_INGEST_SECRET must be at least 32 random bytes, base64 (openssl rand -base64 32); its value is not shown")

// ParseSecret reads MEETINGS_INGEST_SECRET: base64 (standard or URL, padded
// or not) of at least SecretBytes bytes.
func ParseSecret(raw string) ([]byte, error) {
	v := strings.TrimSpace(raw)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) >= SecretBytes {
			return b, nil
		}
	}
	return nil, ErrSecret
}

// trackRE is a LiveKit track id: "TR_" and letters and digits.
var trackRE = regexp.MustCompile(`^TR_[A-Za-z0-9]{1,64}$`)

// ValidTrack says s is shaped like a LiveKit track id.
func ValidTrack(s string) bool { return trackRE.MatchString(s) }

// NewNonce is a fresh egress nonce: 16 random bytes, base64url.
func NewNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

const tokenDomain = "muster-meetings ingest v1"

func mac(key []byte, meetingID, track, nonce string, exp int64) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(tokenDomain + "\n" + meetingID + "\n" + track + "\n" + nonce + "\n" + strconv.FormatInt(exp, 10)))
	return m.Sum(nil)
}

// Token mints the token for one egress of track in meetingID, valid until
// exp.
func Token(key []byte, meetingID, track, nonce string, exp time.Time) string {
	e := exp.Unix()
	return nonce + "." + strconv.FormatInt(e, 10) + "." + base64.RawURLEncoding.EncodeToString(mac(key, meetingID, track, nonce, e))
}

// ErrToken is a token that does not open this track: one error for every
// reason.
var ErrToken = errors.New("ingest: the token does not open this track")

// Parsed is a token's parts, before it is checked against a track's row.
type Parsed struct {
	Nonce string
	Exp   int64
	mac   []byte
}

// Parse splits a token. A token that is not shaped like one is ErrToken.
func Parse(tok string) (Parsed, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || len(parts[0]) != 22 {
		return Parsed{}, ErrToken
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp <= 0 {
		return Parsed{}, ErrToken
	}
	m, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(m) != sha256.Size {
		return Parsed{}, ErrToken
	}
	return Parsed{Nonce: parts[0], Exp: exp, mac: m}, nil
}

// Check says p is a token for the egress whose current nonce is nonce, of
// track in meetingID, unexpired at now. 🔴 Every part is checked: the MAC
// (constant time) over the meeting, the track, the nonce and the expiry; the
// nonce is the track's current one (constant time); and now is before the
// expiry.
func (p Parsed) Check(key []byte, meetingID, track, nonce string, now time.Time) error {
	if !hmac.Equal(p.mac, mac(key, meetingID, track, p.Nonce, p.Exp)) {
		return ErrToken
	}
	if !hmac.Equal([]byte(p.Nonce), []byte(nonce)) {
		return ErrToken
	}
	if now.Unix() >= p.Exp {
		return ErrToken
	}
	return nil
}

// Path is the ingest route for a track and token: /stt/ingest/<track>/<token>.
func Path(track, token string) string { return "/stt/ingest/" + track + "/" + token }

// LogPath is the ingest path as the access log may record it: the track,
// never the token (design: "The plugin's access log records
// /stt/ingest/<track> only").
func LogPath(p string) string {
	rest, ok := strings.CutPrefix(p, "/stt/ingest/")
	if !ok {
		return "/{redacted}"
	}
	track, _, _ := strings.Cut(rest, "/")
	if !ValidTrack(track) {
		return "/stt/ingest/{track}"
	}
	return "/stt/ingest/" + track
}
