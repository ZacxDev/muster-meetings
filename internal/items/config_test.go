package items_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func TestNewRefusesProviderPrefsWithoutDeny(t *testing.T) {
	if _, err := items.New(items.Config{Model: "scaledown/extract", Adapter: items.AdapterExtract, Prefs: map[string]any{}, APIKey: "k"}, nil); err == nil {
		t.Error("New with no deny: want an error")
	}
	if _, err := items.New(items.Config{Model: "deepseek/deepseek-v4.1-flash-1017", Adapter: items.AdapterChatJSON, Prefs: map[string]any{"data_collection": "deny"}, APIKey: "k"}, nil); err == nil {
		t.Error("New with an unpinned chat_json model: want an error")
	}
}

// The provider object is checked on the request about to be sent, not only
// when the configuration is read: a Config whose Prefs changed after New,
// or that never came from ParsePrefs, sends nothing.
func TestTheProviderObjectIsCheckedWhenItIsSent(t *testing.T) {
	cfg := func(f *fakeOR, model string, p map[string]any) items.Adapter {
		a, err := items.New(items.Config{Model: model, Adapter: items.AdapterFor(model), Prefs: p, APIKey: "k", BaseURL: f.srv.URL}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ok := func(int, []byte) (int, map[string]string, string) { return content(`{"entities":[]}`) }

	f := newFake(t, ok)
	p, _ := items.ParsePrefs("", items.AdapterExtract)
	a := cfg(f, "scaledown/extract", p)
	delete(p, "data_collection")
	if _, err := a.Propose(context.Background(), hostile); f.calls() != 0 || items.ClassOf(err) != items.Fatal {
		t.Errorf("sent without deny: %d requests, err %v", f.calls(), err)
	}

	f = newFake(t, ok)
	p, _ = items.ParsePrefs(pin, items.AdapterChatJSON)
	a = cfg(f, "deepseek/deepseek-v4.1-flash-1017", p)
	delete(p, "order")
	if _, err := a.Propose(context.Background(), hostile); f.calls() != 0 || items.ClassOf(err) != items.Fatal {
		t.Errorf("a chat_json request sent without its pin: %d requests, err %v", f.calls(), err)
	}

	f = newFake(t, ok)
	a = cfg(f, "scaledown/extract", map[string]any{"data_collection": "deny", "max_price": math.Inf(1)})
	if _, err := a.Propose(context.Background(), hostile); f.calls() != 0 || items.ClassOf(err) != items.Fatal || !strings.Contains(fmt.Sprint(err), "do not encode") {
		t.Errorf("an unencodable provider object not refused as unencodable: %d requests, err %v", f.calls(), err)
	}
}
