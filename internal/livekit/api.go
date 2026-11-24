package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Room settings the plugin creates every meeting's room with (design:
// "Meeting lifecycle": empty_timeout 10 min, max_participants 8).
const (
	// EmptyTimeout closes a room nobody has joined after this long, and
	// DepartureTimeout one everyone has left: both 10 minutes, so "the room
	// has been empty for 10 minutes" ends the meeting either way (LiveKit
	// fires room_finished, and the webhook ends the meeting).
	EmptyTimeout     = 10 * time.Minute
	DepartureTimeout = 10 * time.Minute
	// MaxParticipants is the 8-person cap, owner included (decision 13).
	MaxParticipants = 8
)

// Client calls LiveKit's RoomService over Twirp JSON with a short-lived
// bearer signed by the API secret.
type Client struct {
	base string
	keys Keys
	// HTTP is used for every call.
	HTTP *http.Client
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// NewClient returns a client for the server API at base (http or https, no
// path), e.g. http://livekit:7880.
func NewClient(base string, keys Keys) (*Client, error) {
	if !keys.Valid() {
		return nil, ErrKeys
	}
	return &Client{base: strings.TrimRight(base, "/"), keys: keys, HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}, nil
}

// Keys are the client's keys, for minting participant tokens.
func (c *Client) Keys() Keys { return c.keys }

// APIError is a Twirp error: its code ("not_found", "unauthenticated", …)
// and HTTP status. Its message is LiveKit's, which never holds the secret.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("livekit: %d %s: %s", e.Status, e.Code, e.Msg)
}

// NotFound says the room or participant does not exist.
func NotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Code == "not_found" || e.Status == http.StatusNotFound)
}

func (c *Client) call(ctx context.Context, method string, grant VideoGrant, in, out any) error {
	return c.callService(ctx, "livekit.RoomService", method, grant, in, out)
}

func (c *Client) callService(ctx context.Context, service, method string, grant VideoGrant, in, out any) error {
	tok, err := c.keys.apiToken(grant, c.Now())
	if err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/twirp/"+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		e := &APIError{Status: resp.StatusCode}
		var tw struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(raw, &tw) == nil {
			e.Code, e.Msg = tw.Code, tw.Msg
		}
		return e
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("livekit %s: unreadable answer: %w", method, err)
		}
	}
	return nil
}

// Room is the part of LiveKit's Room the plugin reads.
type Room struct {
	SID  string `json:"sid"`
	Name string `json:"name"`
}

// CreateRoom creates name with the meeting settings. LiveKit answers an
// existing room with that room, so it is safe to repeat.
func (c *Client) CreateRoom(ctx context.Context, name string) (Room, error) {
	var out Room
	err := c.call(ctx, "CreateRoom", VideoGrant{RoomCreate: true}, map[string]any{
		"name":              name,
		"empty_timeout":     int(EmptyTimeout / time.Second),
		"departure_timeout": int(DepartureTimeout / time.Second),
		"max_participants":  MaxParticipants,
	}, &out)
	return out, err
}

// DeleteRoom deletes name, disconnecting everyone in it. A room that does
// not exist is not an error: the outcome is the same.
func (c *Client) DeleteRoom(ctx context.Context, name string) error {
	err := c.call(ctx, "DeleteRoom", VideoGrant{RoomCreate: true}, map[string]any{"room": name}, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// ListRooms lists every room.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	var out struct {
		Rooms []Room `json:"rooms"`
	}
	err := c.call(ctx, "ListRooms", VideoGrant{RoomList: true}, map[string]any{}, &out)
	return out.Rooms, err
}

// ParticipantInfo is the part of LiveKit's ParticipantInfo the plugin reads.
type ParticipantInfo struct {
	SID      string      `json:"sid"`
	Identity string      `json:"identity"`
	Name     string      `json:"name"`
	Tracks   []TrackInfo `json:"tracks"`
	// Kind is what LiveKit says the participant is. The server takes it from
	// the `kind` claim of the access token the participant joined with, so
	// only a holder of the API secret can set it; the tokens the plugin
	// mints carry no such claim (Claims has no field for it), so every
	// owner and guest is STANDARD.
	Kind Kind `json:"kind"`
}

// Kind is LiveKit's ParticipantInfo.Kind, by its protobuf enum name. Both
// wire formats send the name: Twirp's JSON (ListParticipants) always, as
// "kind":"STANDARD" too; the webhook's protojson leaves the zero value out,
// so an absent kind is STANDARD. Measured against livekit-server 1.13.11
// (internal/livekit/testdata/real-*.json). A number is read as the enum
// value, should a server ever send one. Absent ("") is STANDARD.
type Kind string

// The participant kinds (livekit_models.proto, ParticipantInfo.Kind).
const (
	KindStandard Kind = "STANDARD"
	KindIngress  Kind = "INGRESS"
	KindEgress   Kind = "EGRESS"
	KindSIP      Kind = "SIP"
	KindAgent    Kind = "AGENT"
)

var kindByValue = map[int]Kind{0: KindStandard, 1: KindIngress, 2: KindEgress, 3: KindSIP, 4: KindAgent, 7: "CONNECTOR", 8: "BRIDGE"}

// UnmarshalJSON reads the enum's name or its number; null or absent is
// STANDARD.
func (k *Kind) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		*k = Kind(x)
	case float64:
		if n, ok := kindByValue[int(x)]; ok {
			*k = n
		} else {
			*k = Kind(fmt.Sprintf("KIND_%d", int(x)))
		}
	case nil:
		*k = KindStandard
	default:
		return fmt.Errorf("livekit: a participant kind that is neither a name nor a number")
	}
	return nil
}

// Egress says p is a LiveKit Egress worker: what LiveKit's own Egress
// service joins a room as to record or stream a track.
func (p ParticipantInfo) Egress() bool { return p.Kind == KindEgress }

// TrackInfo is the part of LiveKit's TrackInfo the plugin reads. Source is
// the protobuf enum's name ("MICROPHONE", "CAMERA", "SCREEN_SHARE",
// "SCREEN_SHARE_AUDIO"); protojson leaves out a zero value, so an absent
// Type is AUDIO.
type TrackInfo struct {
	SID    string `json:"sid"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

// SourceMicrophone is a microphone track's source: the only kind the
// plugin transcribes (design: "started by the plugin on every
// track_published webhook for a microphone track").
const SourceMicrophone = "MICROPHONE"

// SourceScreenShare is a shared screen's video track.
const SourceScreenShare = "SCREEN_SHARE"

// ScreenShare says t is a shared screen's video (not its audio).
func (t TrackInfo) ScreenShare() bool {
	return t.Source == SourceScreenShare && t.Type != "" && t.Type != "AUDIO"
}

// Microphone says t is a microphone's audio.
func (t TrackInfo) Microphone() bool {
	return t.Source == SourceMicrophone && (t.Type == "" || t.Type == "AUDIO")
}

// Published says identity has track published in ps.
func Published(ps []ParticipantInfo, identity, track string) bool {
	for _, p := range ps {
		if p.Identity != identity {
			continue
		}
		for _, t := range p.Tracks {
			if t.SID == track {
				return true
			}
		}
	}
	return false
}

// ListParticipants lists who is in room. A room that does not exist has
// nobody in it.
func (c *Client) ListParticipants(ctx context.Context, room string) ([]ParticipantInfo, error) {
	var out struct {
		Participants []ParticipantInfo `json:"participants"`
	}
	err := c.call(ctx, "ListParticipants", VideoGrant{RoomAdmin: true, Room: room}, map[string]any{"room": room}, &out)
	if NotFound(err) {
		return nil, nil
	}
	return out.Participants, err
}

// RemoveParticipant disconnects identity from room. One that is not there
// is not an error.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	err := c.call(ctx, "RemoveParticipant", VideoGrant{RoomAdmin: true, Room: room}, map[string]any{"room": room, "identity": identity}, nil)
	if NotFound(err) {
		return nil
	}
	return err
}

// ---- Egress (design: "Audio for STT: track Egress to a websocket") ----

// EgressInfo is the part of LiveKit's EgressInfo the plugin reads. LiveKit's
// Twirp answers use the protobuf field names (egress_id) and its webhooks
// protojson's (egressId): both are read.
type EgressInfo struct {
	EgressID string
	RoomName string
	Status   string // EGRESS_STARTING, EGRESS_ACTIVE, EGRESS_ENDING, EGRESS_COMPLETE, EGRESS_FAILED, EGRESS_ABORTED, EGRESS_LIMIT_REACHED
	Error    string
	// Files is a room composite's file results (FileInfo): empty for a
	// track egress, and until the egress's pipeline has started.
	Files []FileInfo
}

// FileInfo is the part of LiveKit's FileInfo the plugin reads: one file an
// egress wrote. Times are Unix NANOseconds and Duration nanoseconds, as
// Egress reports them (egress v1.15.0: FileInfo.started_at is set when its
// pipeline reaches PLAYING, the file's first frame; duration is its end
// minus that). protojson writes every int64 as a JSON string; a number is
// read too.
type FileInfo struct {
	Filename  string
	StartedAt int64
	EndedAt   int64
	Duration  int64
	Size      int64
}

// int64s reads a protojson int64: a string of digits, or a number.
type int64s int64

func (n *int64s) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			*n = 0
			return nil
		}
		i, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return fmt.Errorf("livekit: %q is not an int64", x)
		}
		*n = int64s(i)
	case float64:
		*n = int64s(x)
	case nil:
		*n = 0
	default:
		return fmt.Errorf("livekit: an int64 that is neither a string nor a number")
	}
	return nil
}

// UnmarshalJSON reads either spelling.
func (f *FileInfo) UnmarshalJSON(b []byte) error {
	var v struct {
		Filename string `json:"filename"`
		SA       int64s `json:"started_at"`
		SB       int64s `json:"startedAt"`
		EA       int64s `json:"ended_at"`
		EB       int64s `json:"endedAt"`
		D        int64s `json:"duration"`
		Size     int64s `json:"size"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = FileInfo{Filename: v.Filename, StartedAt: int64(v.SA + v.SB), EndedAt: int64(v.EA + v.EB), Duration: int64(v.D), Size: int64(v.Size)}
	return nil
}

// UnmarshalJSON reads either spelling.
func (e *EgressInfo) UnmarshalJSON(b []byte) error {
	var v struct {
		A  string     `json:"egress_id"`
		B  string     `json:"egressId"`
		RA string     `json:"room_name"`
		RB string     `json:"roomName"`
		S  any        `json:"status"`
		E  string     `json:"error"`
		FA []FileInfo `json:"file_results"`
		FB []FileInfo `json:"fileResults"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	e.EgressID, e.RoomName, e.Error = v.A+v.B, v.RA+v.RB, v.E
	e.Files = append(v.FA, v.FB...)
	switch s := v.S.(type) {
	case string:
		e.Status = s
	case float64:
		e.Status = egressStatus[int(s)]
	case nil:
		e.Status = "EGRESS_STARTING" // protojson leaves out the zero value
	}
	return nil
}

var egressStatus = map[int]string{0: "EGRESS_STARTING", 1: "EGRESS_ACTIVE", 2: "EGRESS_ENDING", 3: "EGRESS_COMPLETE",
	4: "EGRESS_FAILED", 5: "EGRESS_ABORTED", 6: "EGRESS_LIMIT_REACHED"}

// StartTrackEgress asks LiveKit to stream one track's raw audio to a
// websocket (Egress connects to wsURL and sends binary PCM frames). The
// grant is roomRecord, which only the plugin's own API bearer carries.
func (c *Client) StartTrackEgress(ctx context.Context, room, track, wsURL string) (EgressInfo, error) {
	var out EgressInfo
	err := c.callService(ctx, "livekit.Egress", "StartTrackEgress", VideoGrant{RoomRecord: true, Room: room},
		map[string]any{"room_name": room, "track_id": track, "websocket_url": wsURL}, &out)
	if err == nil && out.EgressID == "" {
		err = fmt.Errorf("livekit StartTrackEgress: an answer with no egress id")
	}
	return out, err
}

// StopEgress stops an egress. One that has already ended is not an error.
func (c *Client) StopEgress(ctx context.Context, egressID string) error {
	err := c.callService(ctx, "livekit.Egress", "StopEgress", VideoGrant{RoomRecord: true}, map[string]any{"egress_id": egressID}, nil)
	if NotFound(err) {
		return nil
	}
	var e *APIError
	if errors.As(err, &e) && e.Code == "failed_precondition" {
		return nil
	}
	return err
}

// Ended says the status is an egress's last: it will write nothing more.
func (e EgressInfo) Ended() bool {
	switch e.Status {
	case "EGRESS_COMPLETE", "EGRESS_FAILED", "EGRESS_ABORTED", "EGRESS_LIMIT_REACHED":
		return true
	}
	return false
}

// RecordingLayout is the room composite's layout: LiveKit's grid, which its
// template switches to the speaker layout (the shared screen large) while a
// screen is shared (egress's template-default, Room.tsx).
const RecordingLayout = "grid"

// StartRoomCompositeEgress asks LiveKit to record room (Egress's headless
// Chrome renders the layout) to ONE MP4 file at filepath in the bucket.
// 🔴 The request names no storage and carries no credential: Egress uploads
// with the S3 block of its OWN configuration (storage.s3; deploy/kubernetes
// egress.yaml), so the bucket key never passes through LiveKit or its Redis.
// filepath is used as the object key as it is (no {time} or {room_name}
// template in it).
func (c *Client) StartRoomCompositeEgress(ctx context.Context, room, filepath string) (EgressInfo, error) {
	var out EgressInfo
	err := c.callService(ctx, "livekit.Egress", "StartRoomCompositeEgress", VideoGrant{RoomRecord: true, Room: room},
		map[string]any{"room_name": room, "layout": RecordingLayout,
			"file_outputs": []map[string]any{{"file_type": "MP4", "filepath": filepath}}}, &out)
	if err == nil && out.EgressID == "" {
		err = fmt.Errorf("livekit StartRoomCompositeEgress: an answer with no egress id")
	}
	return out, err
}

// ListEgress is what LiveKit knows of one egress: none when it knows nothing
// of it.
func (c *Client) ListEgress(ctx context.Context, egressID string) ([]EgressInfo, error) {
	var out struct {
		Items []EgressInfo `json:"items"`
	}
	err := c.callService(ctx, "livekit.Egress", "ListEgress", VideoGrant{RoomRecord: true}, map[string]any{"egress_id": egressID}, &out)
	if NotFound(err) {
		return nil, nil
	}
	return out.Items, err
}
