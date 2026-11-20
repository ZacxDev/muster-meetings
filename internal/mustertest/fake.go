// Package mustertest is a fake muster for tests: the two /plugin/v1 routes an
// app plugin calls (GET /plugin/v1/self and POST /plugin/v1/handoffs/redeem),
// with muster's answers (docs/plugins.md, "App tab" and "Viewer hand-off"),
// for any number of plugin tokens, recording every request it was sent.
//
// It follows muster where muster's rules are the plugin's security boundary:
// a code is single use, lives 60 s, and redeems only with the token of the
// plugin it was minted for; the purpose comes from the target; a token with
// neither viewer nor app is 403; a revoked token is 401 on every route.
package mustertest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// Token is the plugin token of the fake's default plugin (New adds it).
const Token = "mpl_test_token_0123456789"

// Plugin is one plugin token the fake serves: what GET /plugin/v1/self says.
type Plugin struct {
	Token        string
	PluginID     int64
	PluginName   string
	OwnerID      int64
	OwnerLogin   string
	OwnerKind    string
	Capabilities []string
}

// Default is the plugin New registers under Token.
var Default = Plugin{Token: Token, PluginID: 3, PluginName: "example-meetings", OwnerID: 1,
	OwnerLogin: "operator@example.test", OwnerKind: "person", Capabilities: []string{"app"}}

type code struct {
	pluginID int64
	answer   string // the redeem answer, exactly
	mintedAt time.Time
	spent    bool
}

// Fake is a fake muster server.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Now is the fake's clock (a code lives 60 s by it); default time.Now.
	Now func() time.Time
	// Off makes every route answer 404, as muster does with plugins off.
	Off bool

	plugins map[string]*Plugin // token → plugin
	revoked map[string]bool
	codes   map[string]*code
	// calls is every request, as "METHOD path token".
	calls []string
}

// New starts a fake muster with Default registered; Close it when done.
func New() *Fake {
	f := &Fake{plugins: map[string]*Plugin{}, revoked: map[string]bool{}, codes: map[string]*code{}, Now: time.Now}
	f.AddPlugin(Default)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /plugin/v1/self", f.gate(f.self))
	mux.HandleFunc("POST /plugin/v1/handoffs/redeem", f.gate(f.redeem))
	f.Server = httptest.NewServer(mux)
	return f
}

// AddPlugin registers another token.
func (f *Fake) AddPlugin(p Plugin) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.plugins[p.Token] = &cp
}

// Revoke makes token answer 401 on every route from now on.
func (f *Fake) Revoke(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[token] = true
}

// MintApp mints a code for an app target, as muster's /apps/{id} page does:
// purpose "app", the owner the plugin's own. ref "" is {"app": {}}.
func (f *Fake) MintApp(pluginID int64, ref string) string {
	f.mu.Lock()
	var owner int64
	for _, p := range f.plugins {
		if p.PluginID == pluginID {
			owner = p.OwnerID
		}
	}
	f.mu.Unlock()
	target := map[string]any{"app": map[string]any{}}
	if ref != "" {
		target["app"] = map[string]any{"ref": ref}
	}
	return f.MintRaw(pluginID, map[string]any{"owner_id": owner, "target": target, "purpose": "app"})
}

// MintRaw mints a code for pluginID whose redeem answers exactly answer
// (stepped_up and minted_at are added when missing). Tests use it to play a
// muster answer the plugin must refuse: a view purpose, a mirror target, an
// owner other than the token's.
func (f *Fake) MintRaw(pluginID int64, answer map[string]any) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	c := base64.RawURLEncoding.EncodeToString(b)
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	if _, ok := answer["stepped_up"]; !ok {
		answer["stepped_up"] = false
	}
	if _, ok := answer["minted_at"]; !ok {
		answer["minted_at"] = now.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	f.codes[c] = &code{pluginID: pluginID, answer: string(raw), mintedAt: now}
	return c
}

// Calls returns every request so far, as "METHOD path token".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CountCalls counts the requests to path (any token).
func (f *Fake) CountCalls(method, path string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, method+" "+path+" ") {
			n++
		}
	}
	return n
}

func write(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (f *Fake) gate(next func(http.ResponseWriter, *http.Request, Plugin)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+tok)
		off := f.Off
		p, ok := f.plugins[tok]
		revoked := f.revoked[tok]
		var cp Plugin
		if ok {
			cp = *p
		}
		f.mu.Unlock()
		if off {
			http.NotFound(w, r)
			return
		}
		if !ok || revoked || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			write(w, 401, `{"error":"this route takes a plugin token (mpl_…), minted at /plugins"}`)
			return
		}
		next(w, r, cp)
	}
}

func (f *Fake) self(w http.ResponseWriter, _ *http.Request, p Plugin) {
	b, _ := json.Marshal(map[string]any{"plugin_id": p.PluginID, "plugin_name": p.PluginName, "owner_id": p.OwnerID,
		"owner_login": p.OwnerLogin, "owner_kind": p.OwnerKind, "capabilities": p.Capabilities, "deliver_to": nil})
	write(w, 200, string(b))
}

func (f *Fake) redeem(w http.ResponseWriter, r *http.Request, p Plugin) {
	if !slices.Contains(p.Capabilities, "app") && !slices.Contains(p.Capabilities, "viewer") {
		write(w, 403, `{"error":"this plugin token lacks the viewer or app capability"}`)
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		write(w, 400, `{"error":"the body is {\"code\": \"<muster_code>\"}"}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[strings.TrimSpace(in.Code)]
	if !ok || c.spent || c.pluginID != p.PluginID || f.Now().Sub(c.mintedAt) > 60*time.Second {
		write(w, 404, `{"error":"no such hand-off code (unknown, used, expired, or another plugin's)"}`)
		return
	}
	c.spent = true
	write(w, 200, c.answer)
}
