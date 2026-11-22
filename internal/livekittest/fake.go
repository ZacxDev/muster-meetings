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

	"github.com/ZacxDev/muster-meetings/internal/egresstest"
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
	Tracks   []livekit.TrackInfo
	// Kind is LiveKit's participant kind ("" is STANDARD). The real server
	// takes it from the join token's `kind` claim; the fake's Join takes
	// tokens the plugin mints, which carry none, so only PutKind and the
	// fake's own Egress make another kind.
	Kind livekit.Kind
}

func kindName(k livekit.Kind) livekit.Kind {
	if k == "" {
		return livekit.KindStandard
	}
	return k
}

// participantJSON is a participant as LiveKit's webhook (protojson) carries
// it: the zero kind, STANDARD, is left out.
func participantJSON(identity string, kind livekit.Kind) map[string]any {
	p := map[string]any{"sid": "PA_" + identity, "identity": identity}
	if kindName(kind) != livekit.KindStandard {
		p["kind"] = string(kind)
	}
	return p
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

	// WebhookURL is where Publish, Unpublish and the egresses post their
	// webhooks (the plugin's /livekit/webhook); "" posts none.
	WebhookURL string
	// EgressAudio, when set, makes the fake act as Egress: each accepted
	// track egress dials its websocket URL and streams what EgressAudio
	// answers for the track (Egress's PCM: 1920-byte messages), one message
	// every EgressPace (default 10 ms, real time), then stays connected,
	// silent, until the egress is stopped or killed.
	EgressAudio func(room, identity, track string) []byte
	EgressPace  time.Duration
	// StartHook, when set, runs (outside the fake's lock) at each
	// StartTrackEgress with its 1-based number, and may block; true refuses
	// the start as LiveKit does with no Egress. A test orders an interleaving
	// with it.
	StartHook func(n int) bool
	starts    int
	// refuse is how many of the next StartTrackEgress calls are refused (no
	// Egress capacity, as LiveKit answers it).
	refuse   int
	egresses []*Egress
	nextID   int
}

// Egress is one track egress the fake accepted.
type Egress struct {
	ID, Room, Identity, Track, URL string
	// Streamed says the fake's Egress connected and sent its audio.
	Streamed bool
	// DialErr is why the websocket was not opened, if it was not.
	DialErr string
	// Removed says the egress's participant was removed from the room (a
	// RemoveParticipant of it), which ended the egress.
	Removed bool
	active  bool
	stop    chan string // the status it ends with
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
	mux.HandleFunc("POST /twirp/livekit.Egress/{method}", f.egressTwirp)
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

// PutKind is Put for a participant of a given kind (LiveKit's Egress worker
// is EGRESS): what the real server lists for a participant whose join token
// carried that `kind` claim.
func (f *Fake) PutKind(room, identity string, kind livekit.Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rooms[room]
	if r == nil {
		r = &Room{Name: room, Participants: map[string]Joined{}}
		f.rooms[room] = r
	}
	r.Participants[identity] = Joined{Identity: identity, Kind: kind}
}

// ParticipantJoined posts participant_joined for identity, of kind, in room,
// shaped as the real server's webhook (the kind left out when STANDARD).
func (f *Fake) ParticipantJoined(url, room, identity string, kind livekit.Kind) (int, error) {
	return f.PostEvent(url, map[string]any{"event": "participant_joined", "room": map[string]any{"sid": "RM_fake", "name": room},
		"participant": participantJSON(identity, kind)})
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
		// Deleting a room ends its egresses, as LiveKit does.
		for _, e := range f.egresses {
			if e.Room == in.Room && e.active {
				go f.endEgress(e, "EGRESS_COMPLETE")
			}
		}
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
				var ts []map[string]any
				for _, t := range p.Tracks {
					// Twirp's JSON: the field names, the enums by name.
					ts = append(ts, map[string]any{"sid": t.SID, "type": t.Type, "source": t.Source})
				}
				// Twirp's JSON sends every field, the kind by name, STANDARD too.
				ps = append(ps, map[string]any{"sid": "PA_" + p.Identity, "identity": p.Identity, "name": p.Name, "tracks": ts,
					"kind": string(kindName(p.Kind))})
			}
			writeOK(map[string]any{"participants": ps})
			return
		}
		p, ok := rm.Participants[in.Identity]
		if !ok {
			twirpErr(w, 404, "not_found", "participant not found")
			return
		}
		delete(rm.Participants, in.Identity)
		// Removing Egress's participant ends its egress, as the real Egress
		// does (disconnected: PARTICIPANT_REMOVED, then egress_ended
		// EGRESS_FAILED).
		if p.Kind == livekit.KindEgress {
			for _, e := range f.egresses {
				if e.ID == in.Identity && e.active {
					e.Removed = true
					go f.endEgress(e, "EGRESS_FAILED")
				}
			}
		}
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

// ---- tracks and Egress ----

// Publish makes identity publish a track from source ("MICROPHONE",
// "CAMERA", …) in room (identity is put in the room if it is not there), and
// posts the track_published webhook. It returns the track's id.
func (f *Fake) Publish(room, identity, source string) (string, int, error) {
	f.mu.Lock()
	r := f.rooms[room]
	if r == nil {
		r = &Room{Name: room, Participants: map[string]Joined{}}
		f.rooms[room] = r
	}
	p, ok := r.Participants[identity]
	if !ok {
		p = Joined{Identity: identity}
	}
	f.nextID++
	typ := "AUDIO"
	if source == "CAMERA" || source == "SCREEN_SHARE" {
		typ = "VIDEO"
	}
	t := livekit.TrackInfo{SID: fmt.Sprintf("TR_fake%04d%s", f.nextID, strings.ToUpper(fmt.Sprint(len(identity)))), Type: typ, Source: source}
	p.Tracks = append(p.Tracks, t)
	r.Participants[identity] = p
	url := f.WebhookURL
	f.mu.Unlock()
	if url == "" {
		return t.SID, 0, nil
	}
	// protojson, as LiveKit's webhook sends it: a zero enum (AUDIO) is left out.
	tr := map[string]any{"sid": t.SID, "source": t.Source, "name": "fake"}
	if typ != "AUDIO" {
		tr["type"] = typ
	}
	code, err := f.PostEvent(url, map[string]any{"event": "track_published", "room": map[string]any{"sid": "RM_fake", "name": room},
		"participant": map[string]any{"sid": "PA_" + identity, "identity": identity}, "track": tr})
	return t.SID, code, err
}

// Unpublish removes a track and posts track_published's twin,
// track_unpublished. A track egress of it ends (EGRESS_COMPLETE).
func (f *Fake) Unpublish(room, identity, track string) (int, error) {
	f.mu.Lock()
	if r := f.rooms[room]; r != nil {
		if p, ok := r.Participants[identity]; ok {
			p.Tracks = slices.DeleteFunc(p.Tracks, func(t livekit.TrackInfo) bool { return t.SID == track })
			r.Participants[identity] = p
		}
	}
	url := f.WebhookURL
	var ending []*Egress
	for _, e := range f.egresses {
		if e.Track == track && e.active {
			ending = append(ending, e)
		}
	}
	f.mu.Unlock()
	code := 0
	var err error
	if url != "" {
		code, err = f.PostEvent(url, map[string]any{"event": "track_unpublished", "room": map[string]any{"sid": "RM_fake", "name": room},
			"participant": map[string]any{"sid": "PA_" + identity, "identity": identity}, "track": map[string]any{"sid": track, "source": "MICROPHONE"}})
	}
	for _, e := range ending {
		f.endEgress(e, "EGRESS_COMPLETE")
	}
	return code, err
}

// RefuseEgress makes the next n StartTrackEgress calls fail as LiveKit does
// when no Egress has the capacity.
func (f *Fake) RefuseEgress(n int) { f.mu.Lock(); f.refuse = n; f.mu.Unlock() }

// Egresses is every track egress the fake accepted, oldest first.
func (f *Fake) Egresses() []Egress {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Egress
	for _, e := range f.egresses {
		out = append(out, *e)
	}
	return out
}

// Active says the egress is running.
func (e Egress) Active() bool { return e.active }

// KillEgress ends an egress as a dropped stream does: its websocket is cut
// without a close frame, and egress_ended (EGRESS_FAILED) is posted.
func (f *Fake) KillEgress(id string) {
	f.mu.Lock()
	var e *Egress
	for _, x := range f.egresses {
		if x.ID == id {
			e = x
		}
	}
	f.mu.Unlock()
	if e != nil {
		f.endEgress(e, "EGRESS_FAILED")
	}
}

// endEgress stops an egress's stream and posts egress_ended with status.
func (f *Fake) endEgress(e *Egress, status string) {
	f.mu.Lock()
	if !e.active {
		f.mu.Unlock()
		return
	}
	e.active = false
	stop, url, room := e.stop, f.WebhookURL, e.Room
	// Egress leaves the room when its egress ends.
	if r := f.rooms[room]; r != nil {
		if p, ok := r.Participants[e.ID]; ok && p.Kind == livekit.KindEgress {
			delete(r.Participants, e.ID)
		}
	}
	f.mu.Unlock()
	if stop != nil {
		select {
		case stop <- status:
		default:
		}
	}
	if url != "" {
		f.PostEvent(url, map[string]any{"event": "egress_ended", "room": map[string]any{"sid": "RM_fake", "name": room},
			"egressInfo": map[string]any{"egressId": e.ID, "roomName": room, "status": status}})
	}
}

// PostEvent signs ev as LiveKit does and posts it to url.
func (f *Fake) PostEvent(url string, ev map[string]any) (int, error) {
	ev["id"] = "EV_" + fmt.Sprint(time.Now().UnixNano())
	ev["createdAt"] = fmt.Sprint(f.Now().Unix())
	body, _ := json.Marshal(ev)
	auth, err := f.keys.SignWebhook(body, f.Now())
	if err != nil {
		return 0, err
	}
	return PostWebhook(url, body, auth)
}

func (f *Fake) egressTwirp(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		RoomName     string `json:"room_name"`
		TrackID      string `json:"track_id"`
		WebsocketURL string `json:"websocket_url"`
		EgressID     string `json:"egress_id"`
	}
	if json.Unmarshal(raw, &in) != nil {
		twirpErr(w, 400, "malformed", "the body is not JSON")
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimSpace(method+" "+in.RoomName+" "+in.TrackID+in.EgressID))
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := f.keys.Verify(tok, f.Now())
	if !ok || err != nil || c.Video == nil {
		f.mu.Unlock()
		twirpErr(w, 401, "unauthenticated", "invalid token")
		return
	}
	if !c.Video.RoomRecord {
		f.mu.Unlock()
		twirpErr(w, 403, "permission_denied", "permissions denied")
		return
	}
	writeOK := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	switch method {
	case "StartTrackEgress":
		if f.Down {
			f.mu.Unlock()
			twirpErr(w, 503, "unavailable", "the fake is down")
			return
		}
		if c.Video.Room != "" && c.Video.Room != in.RoomName {
			f.mu.Unlock()
			twirpErr(w, 403, "permission_denied", "permissions denied")
			return
		}
		f.starts++
		if hook := f.StartHook; hook != nil {
			n := f.starts
			f.mu.Unlock()
			refused := hook(n)
			f.mu.Lock()
			if refused {
				f.mu.Unlock()
				twirpErr(w, 503, "unavailable", "no response from servers")
				return
			}
		}
		if f.refuse > 0 {
			f.refuse--
			f.mu.Unlock()
			// What LiveKit answers when no Egress instance takes the job.
			twirpErr(w, 503, "unavailable", "no response from servers")
			return
		}
		rm := f.rooms[in.RoomName]
		identity := ""
		if rm != nil {
			for _, p := range rm.Participants {
				for _, t := range p.Tracks {
					if t.SID == in.TrackID {
						identity = p.Identity
					}
				}
			}
		}
		if identity == "" {
			f.mu.Unlock()
			twirpErr(w, 404, "not_found", "track not found")
			return
		}
		f.nextID++
		e := &Egress{ID: fmt.Sprintf("EG_fake%04d", f.nextID), Room: in.RoomName, Identity: identity, Track: in.TrackID, URL: in.WebsocketURL,
			active: true, stop: make(chan string, 1)}
		f.egresses = append(f.egresses, e)
		audio, pace := f.EgressAudio, f.EgressPace
		f.mu.Unlock()
		if audio != nil {
			go f.runEgress(e, audio(in.RoomName, identity, in.TrackID), pace)
		}
		writeOK(map[string]any{"egress_id": e.ID, "room_name": e.Room, "status": "EGRESS_STARTING"})
	case "StopEgress":
		var e *Egress
		for _, x := range f.egresses {
			if x.ID == in.EgressID {
				e = x
			}
		}
		f.mu.Unlock()
		if e == nil {
			twirpErr(w, 404, "not_found", "egress not found")
			return
		}
		if !e.Active() {
			twirpErr(w, 412, "failed_precondition", "egress is not active")
			return
		}
		f.endEgress(e, "EGRESS_COMPLETE")
		writeOK(map[string]any{"egress_id": e.ID, "status": "EGRESS_COMPLETE"})
	default:
		f.mu.Unlock()
		twirpErr(w, 404, "bad_route", "no such method")
	}
}

// runEgress is the fake's Egress for one track. First it joins the room as
// the real Egress does, as a participant of kind EGRESS whose identity is
// the egress id, and posts participant_joined; if the plugin removes it, the
// egress ends (EGRESS_FAILED) and nothing is streamed. Then it dials the
// websocket URL with no credential but the URL, streams pcm, and waits,
// silent, until the egress is stopped (a close frame) or killed (the
// connection cut).
func (f *Fake) runEgress(e *Egress, pcm []byte, pace time.Duration) {
	if pace == 0 {
		pace = 10 * time.Millisecond
	}
	f.mu.Lock()
	if r := f.rooms[e.Room]; r != nil && e.active {
		r.Participants[e.ID] = Joined{Identity: e.ID, Kind: livekit.KindEgress}
	}
	url := f.WebhookURL
	f.mu.Unlock()
	if url != "" {
		// The plugin answers participant_joined after acting on it, so a
		// removal has happened (Removed set) by the time this returns.
		f.ParticipantJoined(url, e.Room, e.ID, livekit.KindEgress)
	}
	f.mu.Lock()
	removed := e.Removed || !e.active
	f.mu.Unlock()
	if removed {
		return
	}
	cl, err := egresstest.Dial(e.URL)
	if err != nil {
		f.mu.Lock()
		e.DialErr = err.Error()
		f.mu.Unlock()
		f.endEgress(e, "EGRESS_FAILED")
		return
	}
	done := make(chan error, 1)
	go func() { done <- cl.Stream(pcm, pace) }()
	select {
	case err := <-done:
		f.mu.Lock()
		e.Streamed = err == nil
		f.mu.Unlock()
	case st := <-e.stop:
		f.stopClient(cl, st)
		return
	}
	st := <-e.stop
	f.stopClient(cl, st)
}

func (f *Fake) stopClient(cl *egresstest.Client, status string) {
	if status == "EGRESS_FAILED" {
		cl.Drop()
		return
	}
	cl.Close()
}
