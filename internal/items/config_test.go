package items_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/items"
)

func asError(err error, e **items.Error) bool        { return errors.As(err, e) }
func asDetail(err error, d **items.DetailError) bool { return errors.As(err, d) }

func TestProviderPrefsAlwaysDenyDataCollection(t *testing.T) {
	cases := []struct {
		raw, adapter, want, err string
	}{
		{"", items.AdapterExtract, `{"data_collection":"deny"}`, ""},
		{`{"sort":"price"}`, items.AdapterExtract, `{"data_collection":"deny","sort":"price"}`, ""},
		{`{"data_collection":"deny"}`, items.AdapterExtract, `{"data_collection":"deny"}`, ""},
		{`{"data_collection":"allow"}`, items.AdapterExtract, "", `data_collection must be "deny"`},
		{`{"data_collection":null}`, items.AdapterExtract, "", `data_collection must be "deny"`},
		{`[1]`, items.AdapterExtract, "", "must be a JSON object"},
		{`{} {}`, items.AdapterExtract, "", "must be one JSON object"},
		{`{"options":[1]}`, items.AdapterExtract, "", "options must be an object"},
		// chat_json: a provider pin is required
		{"", items.AdapterChatJSON, "", "must pin its provider"},
		{`{"order":["deepinfra/fp8"]}`, items.AdapterChatJSON, "", "must pin its provider"},
		{`{"order":["deepinfra/fp8"],"allow_fallbacks":true}`, items.AdapterChatJSON, "", "must pin its provider"},
		{`{"order":[],"allow_fallbacks":false}`, items.AdapterChatJSON, "", "must pin its provider"},
		{`{"order":["deepinfra/fp8"],"allow_fallbacks":false}`, items.AdapterChatJSON, `{"allow_fallbacks":false,"data_collection":"deny","order":["deepinfra/fp8"]}`, ""},
		{`{"quantizations":["fp8"]}`, items.AdapterChatJSON, `{"data_collection":"deny","quantizations":["fp8"]}`, ""},
		{`{"quantizations":["fp8"],"data_collection":"allow"}`, items.AdapterChatJSON, "", `data_collection must be "deny"`},
	}
	for _, c := range cases {
		p, err := items.ParsePrefs(c.raw, c.adapter)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("ParsePrefs(%s, %s) err = %v, want %q", c.raw, c.adapter, err, c.err)
			}
			continue
		}
		b, _ := json.Marshal(p)
		if err != nil || string(b) != c.want {
			t.Errorf("ParsePrefs(%s, %s) = %s, %v; want %s", c.raw, c.adapter, b, err, c.want)
		}
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "off"}))
	if err != nil || c.Model != "off" || c.APIKey != "" {
		t.Errorf("off: %+v %v", c, err)
	}
	if _, err := items.FromEnv(env(map[string]string{})); err == nil {
		t.Error("no model: want an error")
	}
	if _, err := items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "scaledown/extract"})); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("no key: %v", err)
	}
	c, err = items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "scaledown/extract", "OPENROUTER_API_KEY": "k"}))
	if err != nil || c.Adapter != items.AdapterExtract || c.Prefs["data_collection"] != "deny" || c.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("extract: %+v %v", c, err)
	}
	if _, err := items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "deepseek/deepseek-v4.1-flash-1017", "OPENROUTER_API_KEY": "k"})); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Errorf("deepseek with no pin: %v", err)
	}
	c, err = items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "deepseek/deepseek-v4.1-flash-1017", "OPENROUTER_API_KEY": "k",
		"MEETINGS_ITEMS_PROVIDER_PREFS": `{"quantizations":["fp8"]}`}))
	if err != nil || c.Adapter != items.AdapterChatJSON {
		t.Errorf("deepseek pinned: %+v %v", c, err)
	}
	if _, err := items.FromEnv(env(map[string]string{"MEETINGS_ITEMS_MODEL": "x/y", "MEETINGS_ITEMS_ADAPTER": "regex", "OPENROUTER_API_KEY": "k"})); err == nil {
		t.Error("unknown adapter: want an error")
	}
	if _, err := items.New(items.Config{Model: "scaledown/extract", Adapter: items.AdapterExtract, Prefs: map[string]any{}, APIKey: "k"}, nil); err == nil {
		t.Error("New with no deny: want an error")
	}
}
