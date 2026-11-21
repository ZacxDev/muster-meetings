package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The startup line names muster by scheme and host only. cfg is built here,
// not by LoadConfig (which refuses a path), so the line's own redaction is
// what is tested.
func TestTheStartupLineNamesTheHostOnly(t *testing.T) {
	got := startupLine(Config{URL: "https://muster.example.test:8443/mpl_s3cretT0ken?t=mpl_s3cretT0ken", Token: "mpl_s3cretT0ken",
		PublicURL: "https://meet.example.test", MusterOrigin: "https://example.test", Listen: ":8080", InternalListen: ":8081"}, "meet-srv", 12, true)
	want := `muster-meetings: talking to https://muster.example.test:8443 as server app "meet-srv" (plugin 12, with Files tasks); serving https://meet.example.test on :8080 (internal :8081), framed by https://example.test`
	if got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
	if strings.Contains(got, "s3cretT0ken") {
		t.Fatalf("startup line names the token: %q", got)
	}
}

// fixtureKey is 32 bytes with no pattern the code names.
var fixtureKey = []byte("main fixture key 5e4d3c2b1a09f8e")

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"MUSTER_URL": "http://localhost:8105/", "MUSTER_PLUGIN_TOKEN": "mpl_x",
		"MUSTER_ORIGIN": "https://example.test", "MEETINGS_PUBLIC_URL": "https://Meet.Example.Test/",
		"MEETINGS_LINK_KEY": base64.StdEncoding.EncodeToString(fixtureKey)}
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil || c.URL != "http://localhost:8105" || c.Token != "mpl_x" ||
		c.MusterOrigin != "https://example.test" || c.PublicURL != "https://meet.example.test" ||
		c.Listen != ":8080" || c.InternalListen != ":8081" || c.StateDir != "./state" || string(c.LinkKey) != string(fixtureKey) {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	m := base()
	m["MUSTER_PLUGIN_TOKEN"] = " mpl_a \n"
	m["MEETINGS_LISTEN"], m["MEETINGS_INTERNAL_LISTEN"], m["STATE_DIR"] = "127.0.0.1:9000", "127.0.0.1:9001", "/tmp/s"
	c, err = LoadConfig(env(m))
	if err != nil || c.Token != "mpl_a" || c.Listen != "127.0.0.1:9000" ||
		c.InternalListen != "127.0.0.1:9001" || c.StateDir != "/tmp/s" {
		t.Fatalf("set = %+v, %v", c, err)
	}
	for name, mut := range map[string]func(map[string]string){
		"no MUSTER_URL":           func(m map[string]string) { delete(m, "MUSTER_URL") },
		"http MUSTER_URL":         func(m map[string]string) { m["MUSTER_URL"] = "http://muster.example.test" },
		"no token":                func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = "  " },
		"a token of another kind": func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = "mst_s3cretT0ken" },
		// fork 8: exactly one server token; 2a's comma list refuses to boot,
		// in every shape, never served in part.
		"two tokens":             func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = "mpl_s3cretT0ken,mpl_s3cretT0ken2" },
		"a token and a comma":    func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = "mpl_s3cretT0ken," },
		"a comma and a token":    func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = " ,mpl_s3cretT0ken" },
		"no MUSTER_ORIGIN":       func(m map[string]string) { delete(m, "MUSTER_ORIGIN") },
		"no public URL":          func(m map[string]string) { delete(m, "MEETINGS_PUBLIC_URL") },
		"http public URL":        func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "http://meet.example.test" },
		"public URL with a path": func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "https://meet.example.test/app" },
		"origin with a query":    func(m map[string]string) { m["MUSTER_ORIGIN"] = "https://example.test/?x=1" },
		// The plugin served from muster's own origin would be muster, framed
		// with allow-same-origin.
		"public URL is muster's origin": func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "https://EXAMPLE.test" },
		// The link key: missing, short (16 bytes), long, not base64.
		"missing link key": func(m map[string]string) { delete(m, "MEETINGS_LINK_KEY") },
		"short link key": func(m map[string]string) {
			m["MEETINGS_LINK_KEY"] = base64.StdEncoding.EncodeToString([]byte("s3cretT0ken 16 b"))
		},
		"long link key": func(m map[string]string) {
			m["MEETINGS_LINK_KEY"] = base64.StdEncoding.EncodeToString([]byte("s3cretT0ken thirty-three bytes!!!"))
		},
		"malformed link key": func(m map[string]string) { m["MEETINGS_LINK_KEY"] = "s3cretT0ken-not-base64!!" },
	} {
		m := base()
		mut(m)
		_, err := LoadConfig(env(m))
		if err == nil {
			t.Errorf("%s: LoadConfig accepted it", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") || strings.Contains(err.Error(), "czNjcmV0VDBr") {
			t.Errorf("%s: the refusal echoes a token: %v", name, err)
		}
	}
}
