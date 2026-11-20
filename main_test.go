package main

import (
	"strings"
	"testing"
)

// The startup line names muster by scheme and host only. cfg is built here,
// not by LoadConfig (which refuses a path), so the line's own redaction is
// what is tested.
func TestTheStartupLineNamesTheHostOnly(t *testing.T) {
	got := startupLine(Config{URL: "https://muster.example.test:8443/mpl_s3cretT0ken?t=mpl_s3cretT0ken", Tokens: []string{"mpl_s3cretT0ken"},
		PublicURL: "https://meet.example.test", MusterOrigin: "https://example.test", Listen: ":8080", InternalListen: ":8081"}, 1)
	want := "muster-meetings: talking to https://muster.example.test:8443 for 1 owner token(s); serving https://meet.example.test on :8080 (internal :8081), framed by https://example.test"
	if got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
	if strings.Contains(got, "s3cretT0ken") {
		t.Fatalf("startup line names the token: %q", got)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"MUSTER_URL": "http://localhost:8105/", "MUSTER_PLUGIN_TOKEN": "mpl_x",
		"MUSTER_ORIGIN": "https://example.test", "MEETINGS_PUBLIC_URL": "https://Meet.Example.Test/"}
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil || c.URL != "http://localhost:8105" || len(c.Tokens) != 1 || c.Tokens[0] != "mpl_x" ||
		c.MusterOrigin != "https://example.test" || c.PublicURL != "https://meet.example.test" ||
		c.Listen != ":8080" || c.InternalListen != ":8081" || c.StateDir != "./state" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	m := base()
	m["MUSTER_PLUGIN_TOKEN"] = " mpl_a , mpl_b,"
	m["MEETINGS_LISTEN"], m["MEETINGS_INTERNAL_LISTEN"], m["STATE_DIR"] = "127.0.0.1:9000", "127.0.0.1:9001", "/tmp/s"
	c, err = LoadConfig(env(m))
	if err != nil || len(c.Tokens) != 2 || c.Tokens[0] != "mpl_a" || c.Tokens[1] != "mpl_b" || c.Listen != "127.0.0.1:9000" ||
		c.InternalListen != "127.0.0.1:9001" || c.StateDir != "/tmp/s" {
		t.Fatalf("set = %+v, %v", c, err)
	}
	for name, mut := range map[string]func(map[string]string){
		"no MUSTER_URL":           func(m map[string]string) { delete(m, "MUSTER_URL") },
		"http MUSTER_URL":         func(m map[string]string) { m["MUSTER_URL"] = "http://muster.example.test" },
		"no token":                func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = " , " },
		"a token of another kind": func(m map[string]string) { m["MUSTER_PLUGIN_TOKEN"] = "mpl_a,mst_s3cretT0ken" },
		"no MUSTER_ORIGIN":        func(m map[string]string) { delete(m, "MUSTER_ORIGIN") },
		"no public URL":           func(m map[string]string) { delete(m, "MEETINGS_PUBLIC_URL") },
		"http public URL":         func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "http://meet.example.test" },
		"public URL with a path":  func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "https://meet.example.test/app" },
		"origin with a query":     func(m map[string]string) { m["MUSTER_ORIGIN"] = "https://example.test/?x=1" },
		// The plugin served from muster's own origin would be muster, framed
		// with allow-same-origin.
		"public URL is muster's origin": func(m map[string]string) { m["MEETINGS_PUBLIC_URL"] = "https://EXAMPLE.test" },
	} {
		m := base()
		mut(m)
		_, err := LoadConfig(env(m))
		if err == nil {
			t.Errorf("%s: LoadConfig accepted it", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") {
			t.Errorf("%s: the refusal echoes a token: %v", name, err)
		}
	}
}
