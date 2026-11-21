package livekit

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxWebhookBody is the largest webhook body read.
const MaxWebhookBody = 1 << 20

// Event is the part of a LiveKit webhook event the plugin reads.
type Event struct {
	Event       string           `json:"event"`
	ID          string           `json:"id"`
	Room        *Room            `json:"room"`
	Participant *ParticipantInfo `json:"participant"`
	Track       *TrackInfo       `json:"track"`
	EgressInfo  *EgressInfo      `json:"egressInfo"`
}

// ErrWebhook is a webhook that does not verify: one error for every reason.
var ErrWebhook = errors.New("livekit: the webhook does not verify")

// VerifyWebhook reads r's body and checks it is LiveKit's (design: "the
// Authorization JWT is verified against the API secret and the body's
// SHA-256 inside it; anything else 401"): the Authorization header is a token
// made under these keys (Verify), and its sha256 claim is the SHA-256 of the
// exact body bytes. LiveKit sends the token bare; a "Bearer " prefix is
// accepted too.
func (k Keys) VerifyWebhook(r *http.Request, now time.Time) (Event, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil || len(body) > MaxWebhookBody {
		return Event{}, ErrWebhook
	}
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return Event{}, ErrWebhook
	}
	tok := strings.TrimPrefix(strings.TrimSpace(vals[0]), "Bearer ")
	c, err := k.Verify(tok, now)
	if err != nil || c.SHA256 == "" {
		return Event{}, ErrWebhook
	}
	want, err := base64.StdEncoding.DecodeString(c.SHA256)
	if err != nil {
		return Event{}, ErrWebhook
	}
	got := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return Event{}, ErrWebhook
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.Event == "" {
		return Event{}, ErrWebhook
	}
	return ev, nil
}

// SignWebhook is the Authorization value LiveKit would send with body: for
// the fake LiveKit and tests.
func (k Keys) SignWebhook(body []byte, now time.Time) (string, error) {
	sum := sha256.Sum256(body)
	return k.Sign(Claims{Issuer: k.Key, NotBefore: now.Unix(), Expires: now.Add(5 * time.Minute).Unix(),
		SHA256: base64.StdEncoding.EncodeToString(sum[:])})
}
