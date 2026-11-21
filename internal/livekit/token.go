// Package livekit is the plugin's side of LiveKit (design: "Media: LiveKit,
// TURN, Egress" and "LiveKit tokens"): the access tokens it mints for the
// owner and admitted guests, the server API it calls (RoomService, over
// Twirp), and the verification of the webhooks LiveKit sends it.
//
// It is written against LiveKit's wire formats directly (an HS256 JWT, Twirp
// JSON) rather than LiveKit's Go SDK, so the plugin carries no WebRTC stack
// it never uses. The integration test (livekit_integration_test.go) runs
// every call, token and webhook here against a real livekit-server.
//
// 🔴 The API secret never leaves the plugin: it signs tokens and the API's
// own bearer, and verifies webhooks. Nothing here returns or logs it.
package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// TokenTTL is how long a participant's access token is valid: only to
// connect (design: "2 min to connect"). LiveKit refreshes the token of a
// connected client itself, so the plugin's own state, not this expiry, is
// what ends access (the participant_joined check and the reconcile loop).
const TokenTTL = 2 * time.Minute

// The publish sources both kinds of participant may use (design: "canPublish
// (camera, microphone, screen_share, screen_share_audio)").
var PublishSources = []string{"camera", "microphone", "screen_share", "screen_share_audio"}

// VideoGrant is LiveKit's `video` claim. Every boolean that decides a power
// is a *bool and written explicitly: in LiveKit an absent canPublish,
// canSubscribe or canPublishData means TRUE, so leaving one out would grant it.
type VideoGrant struct {
	RoomCreate           bool     `json:"roomCreate,omitempty"`
	RoomList             bool     `json:"roomList,omitempty"`
	RoomRecord           bool     `json:"roomRecord,omitempty"`
	RoomAdmin            bool     `json:"roomAdmin"`
	RoomJoin             bool     `json:"roomJoin"`
	Room                 string   `json:"room,omitempty"`
	CanPublish           *bool    `json:"canPublish,omitempty"`
	CanSubscribe         *bool    `json:"canSubscribe,omitempty"`
	CanPublishData       *bool    `json:"canPublishData,omitempty"`
	CanPublishSources    []string `json:"canPublishSources,omitempty"`
	CanUpdateOwnMetadata *bool    `json:"canUpdateOwnMetadata,omitempty"`
	Hidden               bool     `json:"hidden"`
	Recorder             bool     `json:"recorder"`
}

func ptr(b bool) *bool { return &b }

// OwnerGrant is the owner's grant for room, exactly as the design's table
// has it: join, publish the four sources, subscribe, publish data, and
// roomAdmin. Not hidden, not a recorder, and it may not rename itself.
func OwnerGrant(room string) VideoGrant {
	return VideoGrant{
		RoomJoin: true, Room: room, RoomAdmin: true,
		CanPublish: ptr(true), CanPublishSources: append([]string(nil), PublishSources...),
		CanSubscribe: ptr(true), CanPublishData: ptr(true), CanUpdateOwnMetadata: ptr(false),
	}
}

// GuestGrant is an admitted guest's grant for room: join, publish the four
// sources, subscribe. 🔴 NOT roomAdmin, NOT canPublishData (so no guest can
// publish a fake transcript line or item), NOT hidden, NOT a recorder, and it
// may not change its own name or metadata (the name the owner admitted is the
// name everyone sees).
func GuestGrant(room string) VideoGrant {
	return VideoGrant{
		RoomJoin: true, Room: room, RoomAdmin: false,
		CanPublish: ptr(true), CanPublishSources: append([]string(nil), PublishSources...),
		CanSubscribe: ptr(true), CanPublishData: ptr(false), CanUpdateOwnMetadata: ptr(false),
	}
}

// Claims is a LiveKit access token's payload.
type Claims struct {
	Issuer     string            `json:"iss"`
	Subject    string            `json:"sub,omitempty"` // the participant identity
	NotBefore  int64             `json:"nbf"`
	Expires    int64             `json:"exp"`
	Name       string            `json:"name,omitempty"`
	Video      *VideoGrant       `json:"video,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
	// SHA256 is a webhook token's body hash (base64, standard encoding).
	SHA256 string `json:"sha256,omitempty"`
}

// Keys is an API key and its secret.
type Keys struct {
	Key    string
	Secret string
}

// ErrKeys is a key or secret that is missing. Its text never carries either.
var ErrKeys = errors.New("LIVEKIT_API_KEY and LIVEKIT_API_SECRET must both be set (their values are not shown)")

// Valid says both halves are present. LiveKit refuses a secret shorter than
// 32 characters in production; the dev server's "secret" is shorter, so the
// length is the deployment's to keep, not checked here.
func (k Keys) Valid() bool { return k.Key != "" && k.Secret != "" }

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// Sign makes an HS256 JWT of claims under the secret.
func (k Keys) Sign(c Claims) (string, error) {
	if !k.Valid() {
		return "", ErrKeys
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(body)
	m := hmac.New(sha256.New, []byte(k.Secret))
	m.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// Participant is who a token is for.
type Participant struct {
	Identity string
	Name     string
}

// Mint is a participant's access token for grant, valid TokenTTL from now.
// The display name is the token's `name` (what every client shows) and the
// `display_name` attribute; the participant may not change either
// (canUpdateOwnMetadata is false in both grants).
func (k Keys) Mint(p Participant, grant VideoGrant, now time.Time) (string, error) {
	if p.Identity == "" || grant.Room == "" {
		return "", errors.New("livekit: a token needs an identity and a room")
	}
	g := grant
	return k.Sign(Claims{
		Issuer: k.Key, Subject: p.Identity, Name: p.Name,
		NotBefore: now.Unix(), Expires: now.Add(TokenTTL).Unix(),
		Video: &g, Attributes: map[string]string{"display_name": p.Name},
	})
}

// apiToken is the plugin's own bearer for one server API call: short-lived,
// for the one room when the call names one.
func (k Keys) apiToken(g VideoGrant, now time.Time) (string, error) {
	return k.Sign(Claims{Issuer: k.Key, NotBefore: now.Unix(), Expires: now.Add(time.Minute).Unix(), Video: &g})
}

// ErrToken is a token that does not verify. One error for every reason, so
// nothing about why is answered to a caller.
var ErrToken = errors.New("livekit: the token does not verify")

// Leeway is the clock skew Verify allows on nbf and exp.
const Leeway = 10 * time.Second

// Verify checks an HS256 JWT made under these keys and returns its claims:
// the algorithm is exactly HS256, the signature is the secret's (compared in
// constant time), the issuer is the key, and now is within nbf and exp (with
// Leeway). Anything else is ErrToken.
func (k Keys) Verify(token string, now time.Time) (Claims, error) {
	if !k.Valid() {
		return Claims{}, ErrToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrToken
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrToken
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256" {
		return Claims{}, ErrToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrToken
	}
	m := hmac.New(sha256.New, []byte(k.Secret))
	m.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(m.Sum(nil), sig) != 1 {
		return Claims{}, ErrToken
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrToken
	}
	var c Claims
	if json.Unmarshal(pb, &c) != nil || c.Issuer != k.Key {
		return Claims{}, ErrToken
	}
	n := now.Unix()
	if c.Expires == 0 || n > c.Expires+int64(Leeway/time.Second) || (c.NotBefore != 0 && n+int64(Leeway/time.Second) < c.NotBefore) {
		return Claims{}, ErrToken
	}
	return c, nil
}
