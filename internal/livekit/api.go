package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	tok, err := c.keys.apiToken(grant, c.Now())
	if err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
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
	SID      string `json:"sid"`
	Identity string `json:"identity"`
	Name     string `json:"name"`
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
