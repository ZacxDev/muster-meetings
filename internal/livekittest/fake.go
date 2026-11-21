// Package livekittest is a fake LiveKit server for tests (design: "Plugin
// unit tests … a fake LiveKit server API"): RoomService over Twirp JSON
// (create, delete and list rooms, list and remove participants), token
// verification as LiveKit does it, and webhook signing.
//
// It is as strict as the real server where the plugin could pass against a
// lenient fake: every API call needs a bearer signed by the API secret with
// the grant that call needs (roomCreate, roomList, or roomAdmin for the named
// room), and a join needs a valid token whose room grant names the room. The
// answers' shapes are the real server's, measured against livekit-server
// 1.13.11 (snake_case room fields, Twirp's {"code", "msg"} errors).
package livekittest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
)

// Keys are the fake's API key and secret (New uses them).
var Keys = livekit.Keys{Key: "APIfakeKey0", Secret: "fake-livekit-secret-for-tests-9f1c2e"}

// Room is a room the fake holds.
type Room struct {
	Name             string
	EmptyTimeout     int
	DepartureTimeout int
	MaxParticipants  int
	Participants     map[string]Joined // identity → who
}

// Joined is a participant in a room, from the token it joined with.
type Joined struct {
	Identity string
	Name     string
	Grant    livekit.VideoGrant
}

// Fake is a fake LiveKit server.
type Fake struct {
	*httptest.Server

	mu    sync.Mutex
	keys  livekit.Keys
	rooms map[string]*Room
	calls []string
	// Down makes every API call answer 503.
	Down bool
	// Now is the fake's clock; default time.Now.
	Now func() time.Time
	// rtc records each token a browser sent to /rtc or /rtc/validate.
	rtc []RTC
}

// RTC is one connection attempt a client made with a token.
type RTC struct {
	Path   string
	Valid  bool
	Claims livekit.Claims
}

// New starts a fake with Keys; Close it when done.
func New() *Fake {
	f := &Fake{keys: Keys, rooms: map[string]*Room{}, Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /twirp/livekit.RoomService/{method}", f.twirp)
	mux.HandleFunc("GET /rtc", f.rtcHandler)
	mux.HandleFunc("GET /rtc/validate", f.rtcHandler)
	mux.HandleFunc("GET /rtc/v1", f.rtcHandler)
	mux.HandleFunc("GET /rtc/v1/validate", f.rtcHandler)
	f.Server = httptest.NewServer(mux)
	return f
}

// WSURL is the URL a browser client is given: the fake's address as ws://.
func (f *Fake) WSURL() string { return "ws" + strings.TrimPrefix(f.URL, "http") }

// SetDown makes every API call fail (503), or stop failing.
func (f *Fake) SetDown(on bool) { f.mu.Lock(); f.Down = on; f.mu.Unlock() }

// Calls is every API call made, as "<Method> <room> <identity>".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CountCalls counts the calls that start with prefix.
func (f *Fake) CountCalls(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Room returns a copy of a room, and whether it exists.
func (f *Fake) Room(name string) (Room, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rooms[name]
	if !ok {
		return Room{}, false
	}
	c := *r
	c.Participants = map[string]Joined{}
	for k, v := range r.Participants {
		c.Participants[k] = v
	}
	return c, true
}

// InRoom lists the identities in a room, sorted.
func (f *Fake) InRoom(name string) []string {
	r, _ := f.Room(name)
	var out []string
	for id := range r.Participants {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RTCs is every token a client presented to /rtc.
func (f *Fake) RTCs() []RTC {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rtc)
}

// ErrJoin is a join LiveKit would refuse: the token does not verify, or
// grants no join of a room.
var ErrJoin = errors.New("livekittest: the token does not admit a join")

// Join connects a client with token, as LiveKit does: the token must verify
// under the keys and grant roomJoin for a room, which is created if it does
// not exist (LiveKit's auto_create). It returns the identity and room.
func (f *Fake) Join(token string) (identity, room string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.keys.Verify(token, f.Now())
	if err != nil || c.Video == nil || !c.Video.RoomJoin || c.Video.Room == "" || c.Subject == "" {
		return "", "", ErrJoin
	}
	r := f.rooms[c.Video.Room]
	if r == nil {
		r = &Room{Name: c.Video.Room, Participants: map[string]Joined{}}
		f.rooms[c.Video.Room] = r
	}
	r.Participants[c.Subject] = Joined{Identity: c.Subject, Name: c.Name, Grant: *c.Video}
	return c.Subject, c.Video.Room, nil
}

// Put places identity in room directly, bypassing tokens: a participant the
// plugin's webhook never heard of (the reconcile loop's case).
func (f *Fake) Put(room, identity string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	if r == nil {
		r = &Room{Name: room, Participants: map[string]Joined{}}
		f.rooms[room] = r
	}
	r.Participants[identity] = Joined{Identity: identity}
}

// Webhook posts a signed LiveKit webhook to url: event about room, and
// participant (may be ""). It returns the HTTP status the plugin answered.
func (f *Fake) Webhook(url, event, room, participant string) (int, error) {
	ev := map[string]any{"event": event, "id": "EV_" + fmt.Sprint(time.Now().UnixNano()), "createdAt": fmt.Sprint(f.Now().Unix()),
		"room": map[string]any{"sid": "RM_fake", "name": room}}
	if participant != "" {
		ev["participant"] = map[string]any{"sid": "PA_fake", "identity": participant}
	}
	body, _ := json.Marshal(ev)
	auth, err := f.keys.SignWebhook(body, f.Now())
	if err != nil {
		return 0, err
	}
	return PostWebhook(url, body, auth)
}

// PostWebhook posts body with the Authorization value auth to url, as
// LiveKit does (the token bare, application/webhook+json).
func PostWebhook(url string, body []byte, auth string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/webhook+json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func twirpErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "msg": msg})
}

func (f *Fake) twirp(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		Name             string `json:"name"`
		Room             string `json:"room"`
		Identity         string `json:"identity"`
		EmptyTimeout     int    `json:"empty_timeout"`
		DepartureTimeout int    `json:"departure_timeout"`
		MaxParticipants  int    `json:"max_participants"`
	}
	if json.Unmarshal(raw, &in) != nil {
		twirpErr(w, 400, "malformed", "the body is not JSON")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace(method+" "+in.Name+in.Room+" "+in.Identity))
	if f.Down {
		twirpErr(w, 503, "unavailable", "the fake is down")
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := f.keys.Verify(tok, f.Now())
	if !ok || err != nil || c.Video == nil {
		twirpErr(w, 401, "unauthenticated", "invalid token")
		return
	}
	g := c.Video
	need := func(ok bool) bool {
		if !ok {
			twirpErr(w, 403, "permission_denied", "permissions denied")
		}
		return ok
	}
	writeOK := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch method {
	case "CreateRoom":
		if !need(g.RoomCreate) {
			return
		}
		rm := f.rooms[in.Name]
		if rm == nil {
			rm = &Room{Name: in.Name, Participants: map[string]Joined{}}
			f.rooms[in.Name] = rm
		}
		rm.EmptyTimeout, rm.DepartureTimeout, rm.MaxParticipants = in.EmptyTimeout, in.DepartureTimeout, in.MaxParticipants
		writeOK(map[string]any{"sid": "RM_" + in.Name, "name": in.Name, "empty_timeout": rm.EmptyTimeout,
			"departure_timeout": rm.DepartureTimeout, "max_participants": rm.MaxParticipants})
	case "DeleteRoom":
		if !need(g.RoomCreate) {
			return
		}
		if f.rooms[in.Room] == nil {
			twirpErr(w, 404, "not_found", "requested room does not exist")
			return
		}
		delete(f.rooms, in.Room)
		writeOK(map[string]any{})
	case "ListRooms":
		if !need(g.RoomList) {
			return
		}
		var rooms []map[string]any
		for _, rm := range f.rooms {
			rooms = append(rooms, map[string]any{"sid": "RM_" + rm.Name, "name": rm.Name})
		}
		writeOK(map[string]any{"rooms": rooms})
	case "ListParticipants", "RemoveParticipant":
		if !need(g.RoomAdmin && g.Room == in.Room) {
			return
		}
		rm := f.rooms[in.Room]
		if rm == nil {
			twirpErr(w, 404, "not_found", "requested room does not exist")
			return
		}
		if method == "ListParticipants" {
			var ps []map[string]any
			for _, p := range rm.Participants {
				ps = append(ps, map[string]any{"sid": "PA_" + p.Identity, "identity": p.Identity, "name": p.Name})
			}
			writeOK(map[string]any{"participants": ps})
			return
		}
		if _, ok := rm.Participants[in.Identity]; !ok {
			twirpErr(w, 404, "not_found", "participant not found")
			return
		}
		delete(rm.Participants, in.Identity)
		writeOK(map[string]any{})
	default:
		twirpErr(w, 404, "bad_route", "no such method")
	}
}

// rtcHandler records the token a browser client presents, verified, and
// refuses the connection: the fake carries no media.
func (f *Fake) rtcHandler(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("access_token")
	f.mu.Lock()
	c, err := f.keys.Verify(tok, f.Now())
	f.rtc = append(f.rtc, RTC{Path: r.URL.Path, Valid: err == nil, Claims: c})
	f.mu.Unlock()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.Error(w, "the fake LiveKit carries no media", http.StatusServiceUnavailable)
}
