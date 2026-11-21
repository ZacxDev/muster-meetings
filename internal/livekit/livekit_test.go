package livekit_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
)

var (
	keys = livekit.Keys{Key: "APIk7q2", Secret: "a-secret-of-the-fixture-31f9c0de42"}
	// t0 is not on a minute boundary, so no expiry lands on one by chance.
	t0 = time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
)

// 🔴 "Grants, exactly" (design: "LiveKit tokens"). Each grant is pinned as a
// whole JSON string, so a flipped or dropped field fails, including the
// subtle one: LiveKit reads an ABSENT canPublishData as true, so omitting it
// would grant it. The room is the argument's, never a constant (the fixture
// room is no word the code names).
func TestGrantsAreExactlyAsDesigned(t *testing.T) {
	g, _ := json.Marshal(livekit.GuestGrant("meeting-Qx7_room"))
	wantGuest := `{"roomAdmin":false,"roomJoin":true,"room":"meeting-Qx7_room","canPublish":true,"canSubscribe":true,"canPublishData":false,` +
		`"canPublishSources":["camera","microphone","screen_share","screen_share_audio"],"canUpdateOwnMetadata":false,"hidden":false,"recorder":false}`
	if string(g) != wantGuest {
		t.Errorf("guest grant =\n%s\nwant\n%s", g, wantGuest)
	}
	o, _ := json.Marshal(livekit.OwnerGrant("meeting-Qx7_room"))
	wantOwner := `{"roomAdmin":true,"roomJoin":true,"room":"meeting-Qx7_room","canPublish":true,"canSubscribe":true,"canPublishData":true,` +
		`"canPublishSources":["camera","microphone","screen_share","screen_share_audio"],"canUpdateOwnMetadata":false,"hidden":false,"recorder":false}`
	if string(o) != wantOwner {
		t.Errorf("owner grant =\n%s\nwant\n%s", o, wantOwner)
	}
}

// A minted token carries the identity, the name, the grant and a 2-minute
// life, and verifies under its keys.
func TestMintIsATwoMinuteTokenForOneIdentity(t *testing.T) {
	tok, err := keys.Mint(livekit.Participant{Identity: "guest-Zt3", Name: "Grace H. (guest)"}, livekit.GuestGrant("meeting-Qx7_room"), t0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := keys.Verify(tok, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != keys.Key || c.Subject != "guest-Zt3" || c.Name != "Grace H. (guest)" || c.Attributes["display_name"] != "Grace H. (guest)" {
		t.Errorf("claims = %+v", c)
	}
	if c.NotBefore != t0.Unix() || c.Expires-c.NotBefore != 120 {
		t.Errorf("nbf %d exp %d: want a life of exactly 120 s from %d", c.NotBefore, c.Expires, t0.Unix())
	}
	if c.Video == nil || c.Video.RoomAdmin || *c.Video.CanPublishData || c.Video.Room != "meeting-Qx7_room" {
		t.Errorf("video = %+v", c.Video)
	}
	// Past its life (and the leeway) it no longer verifies.
	if _, err := keys.Verify(tok, t0.Add(livekit.TokenTTL+livekit.Leeway+time.Second)); err == nil {
		t.Error("the token verified after its life")
	}
	if _, err := keys.Mint(livekit.Participant{Name: "x"}, livekit.GuestGrant("r"), t0); err == nil {
		t.Error("a token with no identity was minted")
	}
}

func part(tok string, i int) string { return strings.Split(tok, ".")[i] }

// Verify refuses everything but an HS256 token signed by the secret, issued
// by the key, within its life.
func TestVerifyRefusesEveryOtherToken(t *testing.T) {
	good, _ := keys.Sign(livekit.Claims{Issuer: keys.Key, NotBefore: t0.Unix(), Expires: t0.Add(time.Minute).Unix()})
	if _, err := keys.Verify(good, t0); err != nil {
		t.Fatalf("the positive control does not verify: %v", err)
	}
	otherSecret, _ := livekit.Keys{Key: keys.Key, Secret: "another-secret-entirely-0000000000"}.Sign(livekit.Claims{Issuer: keys.Key, Expires: t0.Add(time.Minute).Unix()})
	otherKey, _ := keys.Sign(livekit.Claims{Issuer: "APIother", Expires: t0.Add(time.Minute).Unix()})
	expired, _ := keys.Sign(livekit.Claims{Issuer: keys.Key, Expires: t0.Add(-time.Minute).Unix()})
	future, _ := keys.Sign(livekit.Claims{Issuer: keys.Key, NotBefore: t0.Add(time.Minute).Unix(), Expires: t0.Add(2 * time.Minute).Unix()})
	noExp, _ := keys.Sign(livekit.Claims{Issuer: keys.Key})
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + part(good, 1) + "."
	// A header that names another algorithm over a signature that IS the
	// secret's HS256: only the algorithm check refuses it.
	hs512Unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS512","typ":"JWT"}`)) + "." + part(good, 1)
	mac := hmac.New(sha256.New, []byte(keys.Secret))
	mac.Write([]byte(hs512Unsigned))
	hs512 := hs512Unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	tamperedBody := part(good, 0) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"APIk7q2","exp":9999999999}`)) + "." + part(good, 2)
	for name, tok := range map[string]string{
		"another secret": otherSecret, "another issuer": otherKey, "expired": expired, "not yet valid": future, "no expiry": noExp,
		"alg none": none, "alg HS512": hs512, "a changed payload": tamperedBody, "garbage": "x.y.z", "empty": "",
	} {
		if _, err := keys.Verify(tok, t0); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func webhookReq(body []byte, auth ...string) *http.Request {
	r := httptest.NewRequest("POST", "/livekit/webhook", bytes.NewReader(body))
	for _, a := range auth {
		r.Header.Add("Authorization", a)
	}
	return r
}

// 🔴 "Webhook: a valid signature passes; a tampered body, a wrong secret and
// no header are 401" (design). Here at the verifier; the server test
// asserts the 401s and that nothing was acted on.
func TestVerifyWebhookChecksTheSignatureAndTheBodyHash(t *testing.T) {
	body := []byte(`{"event":"participant_joined","room":{"name":"meeting-Qx7_room"},"participant":{"identity":"guest-Zt3"}}`)
	auth, err := keys.SignWebhook(body, t0)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := keys.VerifyWebhook(webhookReq(body, auth), t0)
	if err != nil || ev.Event != "participant_joined" || ev.Room.Name != "meeting-Qx7_room" || ev.Participant.Identity != "guest-Zt3" {
		t.Fatalf("the valid webhook: %+v, %v", ev, err)
	}
	if _, err := keys.VerifyWebhook(webhookReq(body, "Bearer "+auth), t0); err != nil {
		t.Errorf("a Bearer-prefixed valid webhook: %v", err)
	}
	tampered := bytes.Replace(body, []byte("guest-Zt3"), []byte("guest-Zt4"), 1)
	wrong, _ := livekit.Keys{Key: keys.Key, Secret: "another-secret-entirely-0000000000"}.SignWebhook(body, t0)
	sum := sha256.Sum256(body)
	noHash, _ := keys.Sign(livekit.Claims{Issuer: keys.Key, Expires: t0.Add(time.Minute).Unix()})
	hexHash, _ := keys.Sign(livekit.Claims{Issuer: keys.Key, Expires: t0.Add(time.Minute).Unix(), SHA256: base64.StdEncoding.EncodeToString(sum[:16])})
	for name, req := range map[string]*http.Request{
		"a tampered body":        webhookReq(tampered, auth),
		"a wrong secret":         webhookReq(body, wrong),
		"no header":              webhookReq(body),
		"two headers":            webhookReq(body, auth, auth),
		"a token with no hash":   webhookReq(body, noHash),
		"half a hash":            webhookReq(body, hexHash),
		"an expired signature":   webhookReq(body, auth),
		"a body with no event":   webhookReq([]byte(`{}`), mustSign(t, []byte(`{}`))),
		"a body that is no JSON": webhookReq([]byte(`event`), mustSign(t, []byte(`event`))),
	} {
		at := t0
		if name == "an expired signature" {
			at = t0.Add(10 * time.Minute)
		}
		if _, err := keys.VerifyWebhook(req, at); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func mustSign(t *testing.T, body []byte) string {
	a, err := keys.SignWebhook(body, t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The server API: each call is signed with the grant it needs, and the fake
// (as strict as LiveKit) refuses a bearer signed by another secret.
func TestTheAPIClientSignsEachCallWithTheGrantItNeeds(t *testing.T) {
	f := livekittest.New()
	defer f.Close()
	c, err := livekit.NewClient(f.URL, livekittest.Keys)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.CreateRoom(ctx, "meeting-Qx7_room"); err != nil {
		t.Fatal(err)
	}
	rm, ok := f.Room("meeting-Qx7_room")
	if !ok || rm.EmptyTimeout != 600 || rm.DepartureTimeout != 600 || rm.MaxParticipants != 8 {
		t.Fatalf("room = %+v, %v; want timeouts 600 s and 8 participants", rm, ok)
	}
	f.Put("meeting-Qx7_room", "guest-Zt3")
	ps, err := c.ListParticipants(ctx, "meeting-Qx7_room")
	if err != nil || len(ps) != 1 || ps[0].Identity != "guest-Zt3" {
		t.Fatalf("participants = %+v, %v", ps, err)
	}
	if err := c.RemoveParticipant(ctx, "meeting-Qx7_room", "guest-Zt3"); err != nil || len(f.InRoom("meeting-Qx7_room")) != 0 {
		t.Fatalf("remove: %v, left %v", err, f.InRoom("meeting-Qx7_room"))
	}
	// Not there, or no room: the outcome is the same, so not an error.
	if err := c.RemoveParticipant(ctx, "meeting-Qx7_room", "guest-Zt3"); err != nil {
		t.Errorf("removing someone not there: %v", err)
	}
	if err := c.DeleteRoom(ctx, "meeting-Qx7_room"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Room("meeting-Qx7_room"); ok {
		t.Error("the room survived its deletion")
	}
	if err := c.DeleteRoom(ctx, "meeting-Qx7_room"); err != nil {
		t.Errorf("deleting a room that is gone: %v", err)
	}
	bad, _ := livekit.NewClient(f.URL, livekit.Keys{Key: livekittest.Keys.Key, Secret: "another-secret-entirely-0000000000"})
	var apiErr *livekit.APIError
	if _, err := bad.CreateRoom(ctx, "meeting-x"); err == nil || !asAPI(err, &apiErr) || apiErr.Status != 401 {
		t.Errorf("a client with another secret: %v", err)
	}
	f.SetDown(true)
	if _, err := c.CreateRoom(ctx, "meeting-y"); err == nil {
		t.Error("CreateRoom with LiveKit down succeeded")
	}
}

func asAPI(err error, target **livekit.APIError) bool {
	e, ok := err.(*livekit.APIError)
	if ok {
		*target = e
	}
	return ok
}
