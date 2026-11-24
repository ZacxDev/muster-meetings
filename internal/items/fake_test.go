package items_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

// fakeOR is a fake OpenRouter: it records every request and answers what
// reply says.
type fakeOR struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	reply   func(n int, body []byte) (status int, hdr map[string]string, out string)
}

func newFake(t *testing.T, reply func(n int, body []byte) (int, map[string]string, string)) *fakeOR {
	t.Helper()
	f := &fakeOR{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, b)
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		st, hdr, out := f.reply(n, b)
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(st)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOR) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// content answers 200 with a chat completion whose message content is c.
func content(c string) (int, map[string]string, string) {
	b, _ := json.Marshal(map[string]any{
		"id": "gen-1", "model": "test/model-20260101", "provider": "TestProvider",
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": c}}},
		"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "cost": 0.00042},
	})
	return 200, nil, string(b)
}

func adapter(t *testing.T, f *fakeOR, model, prefs string) items.Adapter {
	t.Helper()
	ad := items.AdapterFor(model)
	p, err := items.ParsePrefs(prefs, ad)
	if err != nil {
		t.Fatalf("ParsePrefs(%s): %v", prefs, err)
	}
	a, err := items.New(items.Config{Model: model, Adapter: ad, Prefs: p, APIKey: "sk-test-key", BaseURL: f.srv.URL + "/api/v1", Timeout: 2 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
