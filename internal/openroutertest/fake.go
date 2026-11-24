// Package openroutertest is a fake OpenRouter for the plugin's tests and the
// e2e harness: one chat completions route that tells the two requests the
// plugin makes apart by their system message (the action items, the frame
// analysis), records every request, and answers what its scripts say. The default scripts are deterministic and read the request:
//
//   - items: every segment whose text holds "I will " or "I'll " is an item
//     ("<the rest of the line>", owner the speaker), citing that segment;
//   - vision: one note per frame ("Frame at <ms> ms: the screen shows the
//     shared document.") citing the excerpt's first segment, and no item.
package openroutertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// Kinds of request.
const (
	KindItems  = "items"
	KindVision = "vision"
)

// Request is one request the fake received.
type Request struct {
	Kind   string
	Model  string
	Body   []byte
	Auth   string
	Images int
}

// Doc is the JSON document a request's user message carries.
type Doc struct {
	Frames   []int64 `json:"frames"`
	Segments []struct {
		ID      int64  `json:"id"`
		Speaker string `json:"speaker"`
		Guest   bool   `json:"guest"`
		Text    string `json:"text"`
	} `json:"segments"`
	Current []struct {
		Text string `json:"text"`
	} `json:"current_items"`
}

// Reply is a script's answer: Status 0 is 200, and Content the message
// content (a JSON string the plugin parses).
type Reply struct {
	Status  int
	Content string
}

// Fake is the fake OpenRouter.
type Fake struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []Request
	// scripts, by kind; nil is the default.
	scripts map[string]func(Doc) Reply
}

// New starts a fake.
func New() *Fake {
	f := &Fake{scripts: map[string]func(Doc) Reply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the API root to configure (BaseURL).
func (f *Fake) URL() string { return f.srv.URL + "/api/v1" }

// Close stops it.
func (f *Fake) Close() { f.srv.Close() }

// Script replaces kind's answer (nil restores the default).
func (f *Fake) Script(kind string, fn func(Doc) Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[kind] = fn
}

// Requests is every request so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request{}, f.got...)
}

// Count is how many requests of kind so far.
func (f *Fake) Count(kind string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

type msg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1"+items.ChatPath {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model    string         `json:"model"`
		Messages []msg          `json:"messages"`
		Provider map[string]any `json:"provider"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) != 2 {
		http.Error(w, `{"error":{"code":400,"message":"bad request"}}`, http.StatusBadRequest)
		return
	}
	var system string
	_ = json.Unmarshal(req.Messages[0].Content, &system)
	kind := ""
	switch system {
	case items.ChatJSONSystem:
		kind = KindItems
	case items.VisionSystem:
		kind = KindVision
	}
	var doc Doc
	images := 0
	var user string
	if json.Unmarshal(req.Messages[1].Content, &user) == nil {
		_ = json.Unmarshal([]byte(user), &doc)
	} else {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(req.Messages[1].Content, &parts)
		for _, p := range parts {
			if p.Type == "text" {
				_ = json.Unmarshal([]byte(p.Text), &doc)
			} else if p.Type == "image_url" {
				images++
			}
		}
	}
	f.mu.Lock()
	f.got = append(f.got, Request{Kind: kind, Model: req.Model, Body: body, Auth: r.Header.Get("Authorization"), Images: images})
	fn := f.scripts[kind]
	f.mu.Unlock()
	if kind == "" || req.Provider["data_collection"] != "deny" {
		http.Error(w, `{"error":{"code":400,"message":"unknown request"}}`, http.StatusBadRequest)
		return
	}
	if fn == nil {
		fn = defaults[kind]
	}
	rep := fn(doc)
	if rep.Status != 0 && rep.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.Status)
		fmt.Fprintf(w, `{"error":{"code":%d,"message":"scripted failure"}}`, rep.Status)
		return
	}
	out, _ := json.Marshal(map[string]any{
		"id": "gen-fake", "model": req.Model, "provider": "FakeProvider",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": rep.Content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "cost": 0.0001},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// JSON is v encoded, for a script's Content.
func JSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var defaults = map[string]func(Doc) Reply{
	KindItems: func(d Doc) Reply {
		out := []map[string]any{}
		for _, s := range d.Segments {
			for _, cue := range []string{"I will ", "I'll "} {
				if i := strings.Index(s.Text, cue); i >= 0 {
					task := strings.TrimSpace(strings.TrimRight(s.Text[i+len(cue):], ".!"))
					out = append(out, map[string]any{"task": strings.ToUpper(task[:1]) + task[1:], "owner": s.Speaker, "due": "", "segment_ids": []int64{s.ID}})
					break
				}
			}
		}
		return Reply{Content: JSON(map[string]any{"items": out})}
	},
	KindVision: func(d Doc) Reply {
		notes := []map[string]any{}
		for _, ms := range d.Frames {
			var cite []int64
			if len(d.Segments) > 0 {
				cite = []int64{d.Segments[0].ID}
			}
			notes = append(notes, map[string]any{"frame_ms": ms, "note": fmt.Sprintf("Frame at %d ms: the screen shows the shared document.", ms), "segment_ids": orEmpty(cite)})
		}
		return Reply{Content: JSON(map[string]any{"frames": notes, "items": []any{}})}
	},
}

func orEmpty(v []int64) []int64 {
	if v == nil {
		return []int64{}
	}
	return v
}
