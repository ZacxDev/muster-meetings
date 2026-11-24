package items

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The adapters' names (MEETINGS_ITEMS_ADAPTER).
const (
	AdapterExtract  = "extract"
	AdapterChatJSON = "chat_json"
)

// Off is the MEETINGS_ITEMS_MODEL value that turns action items off.
const Off = "off"

// DefaultBaseURL is OpenRouter's API.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// Config is the action items' settings (design: "The worker", Config).
type Config struct {
	// Model is an OpenRouter slug, or Off.
	Model string
	// Adapter is AdapterExtract or AdapterChatJSON.
	Adapter string
	// Prefs is the OpenRouter `provider` object every request carries, as
	// ParsePrefs checked it: data_collection is always "deny".
	Prefs map[string]any
	// APIKey is OpenRouter's bearer.
	APIKey string
	// BaseURL is OpenRouter's API root (a test points it at a fake).
	BaseURL string
	// Timeout bounds one request (default 30 s).
	Timeout time.Duration
}

// AdapterFor is the adapter a slug implies: ScaleDown's extract models are
// extract, everything else chat_json.
func AdapterFor(model string) string {
	if strings.HasPrefix(model, "scaledown/extract") {
		return AdapterExtract
	}
	return AdapterChatJSON
}

// ParsePrefs reads MEETINGS_ITEMS_PROVIDER_PREFS for adapter. Empty is
// {"data_collection": "deny"}. A data_collection other than "deny" is
// refused, never overridden: only providers that do not retain prompts may
// see a meeting, whatever the account's own privacy setting. A chat_json
// model must carry a provider pin (`order` with `allow_fallbacks: false`,
// or `quantizations`): its weights are pinned by the dated slug, its
// serving is not (design: "The fallback").
func ParsePrefs(raw, adapter string) (map[string]any, error) {
	p := map[string]any{}
	if s := strings.TrimSpace(raw); s != "" {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		if err := d.Decode(&p); err != nil || p == nil {
			return nil, errors.New("MEETINGS_ITEMS_PROVIDER_PREFS must be a JSON object (OpenRouter's provider object)")
		}
		if d.More() {
			return nil, errors.New("MEETINGS_ITEMS_PROVIDER_PREFS must be one JSON object")
		}
	}
	switch dc, ok := p["data_collection"]; {
	case !ok:
		p["data_collection"] = "deny"
	case dc != "deny":
		return nil, fmt.Errorf(`MEETINGS_ITEMS_PROVIDER_PREFS: data_collection must be "deny", not %v (a meeting may reach only providers that do not retain prompts)`, dc)
	}
	if adapter == AdapterChatJSON && !pinned(p) {
		return nil, errors.New(`MEETINGS_ITEMS_PROVIDER_PREFS: a chat_json model must pin its provider: "order" with "allow_fallbacks": false, or "quantizations"`)
	}
	if adapter == AdapterExtract {
		if o, ok := p["options"]; ok {
			if _, isMap := o.(map[string]any); !isMap {
				return nil, errors.New("MEETINGS_ITEMS_PROVIDER_PREFS: options must be an object")
			}
		}
	}
	return p, nil
}

// pinned reports a provider pin in p.
func pinned(p map[string]any) bool {
	if q, ok := p["quantizations"].([]any); ok && len(q) > 0 {
		return true
	}
	o, ok := p["order"].([]any)
	return ok && len(o) > 0 && p["allow_fallbacks"] == false
}

// FromEnv reads the configuration; getenv is os.Getenv. A Model of Off
// answers a Config with only Model set and no error.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{Model: strings.TrimSpace(getenv("MEETINGS_ITEMS_MODEL"))}
	if c.Model == "" {
		return Config{}, errors.New("MEETINGS_ITEMS_MODEL is not set (an OpenRouter model slug, or off)")
	}
	if c.Model == Off {
		return c, nil
	}
	c.Adapter = strings.TrimSpace(getenv("MEETINGS_ITEMS_ADAPTER"))
	if c.Adapter == "" {
		c.Adapter = AdapterFor(c.Model)
	}
	if c.Adapter != AdapterExtract && c.Adapter != AdapterChatJSON {
		return Config{}, fmt.Errorf("MEETINGS_ITEMS_ADAPTER must be %s or %s", AdapterExtract, AdapterChatJSON)
	}
	var err error
	if c.Prefs, err = ParsePrefs(getenv("MEETINGS_ITEMS_PROVIDER_PREFS"), c.Adapter); err != nil {
		return Config{}, err
	}
	c.APIKey = strings.TrimSpace(getenv("OPENROUTER_API_KEY"))
	if c.APIKey == "" {
		return Config{}, errors.New("OPENROUTER_API_KEY is not set (required unless MEETINGS_ITEMS_MODEL=off; its value is not shown)")
	}
	c.BaseURL = DefaultBaseURL
	return c, nil
}

// New builds the configured adapter. hc nil is a client with c.Timeout.
func New(c Config, hc *http.Client) (Adapter, error) {
	if c.Model == "" || c.Model == Off {
		return nil, errors.New("items: no model (action items are off)")
	}
	if c.Prefs == nil || c.Prefs["data_collection"] != "deny" {
		return nil, errors.New(`items: the provider preferences must carry data_collection "deny" (build them with ParsePrefs)`)
	}
	cl := newClient(c, hc)
	switch c.Adapter {
	case AdapterExtract:
		return &Extract{c: cl}, nil
	case AdapterChatJSON:
		if !pinned(c.Prefs) {
			return nil, errors.New("items: a chat_json model must pin its provider (ParsePrefs)")
		}
		return &ChatJSON{c: cl}, nil
	}
	return nil, fmt.Errorf("items: unknown adapter %q", c.Adapter)
}

// prefsWith is a deep copy of the configured provider object, so an
// adapter's additions never reach the shared Config.
func prefsWith(p map[string]any) map[string]any {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(p)
	out := map[string]any{}
	d := json.NewDecoder(&b)
	d.UseNumber()
	_ = d.Decode(&out)
	return out
}
