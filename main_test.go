package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// The startup line names muster by scheme and host only. cfg is built here,
// not by LoadConfig (which refuses a path), so the line's own redaction is
// what is tested.
func TestTheStartupLineNamesTheHostOnly(t *testing.T) {
	got := startupLine(Config{URL: "https://muster.example.test:8443/mpl_s3cretT0ken?t=mpl_s3cretT0ken", Token: "mpl_s3cretT0ken",
		PublicURL: "https://meet.example.test", MusterOrigin: "https://example.test", Listen: ":8080", InternalListen: ":8081",
		LiveKitURL: "wss://meet-rtc.example.test", LiveKitAPI: "http://livekit:7880", ICE: "relay",
		LiveKitKeys: livekit.Keys{Key: "APIk3yS3cret", Secret: "lks3cretT0kenValue"}}, "meet-srv", 12, true)
	want := `muster-meetings: talking to https://muster.example.test:8443 as server app "meet-srv" (plugin 12, with Files tasks); serving https://meet.example.test on :8080 (internal :8081), framed by https://example.test; LiveKit wss://meet-rtc.example.test (API http://livekit:7880, ICE relay); transcript off`
	if got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
	if strings.Contains(got, "s3cretT0ken") || strings.Contains(got, "lks3cret") || strings.Contains(got, "APIk3y") {
		t.Fatalf("startup line names the token: %q", got)
	}
}

// fixtureKey is 32 bytes with no pattern the code names.
var fixtureKey = []byte("main fixture key 5e4d3c2b1a09f8e")

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"MUSTER_URL": "http://localhost:8105/", "MUSTER_PLUGIN_TOKEN": "mpl_x",
		"MUSTER_ORIGIN": "https://example.test", "MEETINGS_PUBLIC_URL": "https://Meet.Example.Test/",
		"MEETINGS_LINK_KEY":    base64.StdEncoding.EncodeToString(fixtureKey),
		"MEETINGS_LIVEKIT_URL": "wss://Meet-RTC.example.test/", "MEETINGS_LIVEKIT_API_URL": "http://livekit:7880",
		"LIVEKIT_API_KEY": "APIfixture", "LIVEKIT_API_SECRET": "s3cretT0ken-livekit-secret"}
}

func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil || c.URL != "http://localhost:8105" || c.Token != "mpl_x" ||
		c.MusterOrigin != "https://example.test" || c.PublicURL != "https://meet.example.test" ||
		c.Listen != ":8080" || c.InternalListen != ":8081" || c.StateDir != "./state" || string(c.LinkKey) != string(fixtureKey) ||
		c.LiveKitURL != "wss://meet-rtc.example.test" || c.LiveKitAPI != "http://livekit:7880" || c.ICE != "relay" ||
		c.LiveKitKeys != (livekit.Keys{Key: "APIfixture", Secret: "s3cretT0ken-livekit-secret"}) || c.TrustedProxies != nil {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	m0 := base()
	m0["MEETINGS_LIVEKIT_ICE"], m0["MEETINGS_TRUSTED_PROXIES"] = "all", " 10.1.2.3/8, fd00::/8 "
	m0["MEETINGS_LIVEKIT_URL"] = "ws://127.0.0.1:7880"
	c, err = LoadConfig(env(m0))
	if err != nil || c.ICE != "all" || c.LiveKitURL != "ws://127.0.0.1:7880" ||
		!slices.Equal(c.TrustedProxies, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}) {
		t.Fatalf("LiveKit set = %+v, %v", c, err)
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
		// LiveKit: the browser URL wss (ws only to loopback), the API http(s),
		// both keys, a known ICE policy, real CIDRs.
		"no LiveKit URL":             func(m map[string]string) { delete(m, "MEETINGS_LIVEKIT_URL") },
		"ws LiveKit URL off-host":    func(m map[string]string) { m["MEETINGS_LIVEKIT_URL"] = "ws://meet-rtc.example.test" },
		"https LiveKit URL":          func(m map[string]string) { m["MEETINGS_LIVEKIT_URL"] = "https://meet-rtc.example.test" },
		"LiveKit URL with a path":    func(m map[string]string) { m["MEETINGS_LIVEKIT_URL"] = "wss://meet-rtc.example.test/rtc" },
		"no LiveKit API URL":         func(m map[string]string) { delete(m, "MEETINGS_LIVEKIT_API_URL") },
		"LiveKit API URL with user":  func(m map[string]string) { m["MEETINGS_LIVEKIT_API_URL"] = "http://k:s3cretT0ken@livekit:7880" },
		"no LiveKit key":             func(m map[string]string) { delete(m, "LIVEKIT_API_KEY") },
		"no LiveKit secret":          func(m map[string]string) { m["LIVEKIT_API_SECRET"] = " " },
		"unknown ICE policy":         func(m map[string]string) { m["MEETINGS_LIVEKIT_ICE"] = "udp" },
		"a proxy that is no CIDR":    func(m map[string]string) { m["MEETINGS_TRUSTED_PROXIES"] = "ingress.example.test" },
		"every IPv4 address trusted": func(m map[string]string) { m["MEETINGS_TRUSTED_PROXIES"] = "10.0.0.0/8, 0.0.0.0/0" },
		"every IPv6 address trusted": func(m map[string]string) { m["MEETINGS_TRUSTED_PROXIES"] = "::/0" },
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

// 🔴 MEETINGS_CLIENT_IP_HEADER: unset changes nothing; set, it needs
// MEETINGS_TRUSTED_PROXIES (it is believed only from a trusted proxy, so
// without one it could never bind) and must name one header that is not
// X-Forwarded-For.
func TestLoadConfigClientIPHeader(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil || c.ClientIPHeader != "" {
		t.Fatalf("unset: ClientIPHeader = %q, %v; want empty", c.ClientIPHeader, err)
	}
	m := base()
	m["MEETINGS_CLIENT_IP_HEADER"], m["MEETINGS_TRUSTED_PROXIES"] = " cf-connecting-ip ", "10.20.0.0/16"
	if c, err = LoadConfig(env(m)); err != nil || c.ClientIPHeader != "Cf-Connecting-Ip" {
		t.Fatalf("with trusted proxies: ClientIPHeader = %q, %v; want Cf-Connecting-Ip", c.ClientIPHeader, err)
	}
	m = base()
	m["MEETINGS_CLIENT_IP_HEADER"] = "True-Client-IP"
	if _, err = LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "MEETINGS_TRUSTED_PROXIES is not") {
		t.Fatalf("set without trusted proxies: LoadConfig = %v, want the refusal", err)
	}
	for _, bad := range []string{"X-Forwarded-For", "x-forwarded-for", "CF-Connecting-IP, X-Real-IP", "CF Connecting IP", "-X"} {
		m = base()
		m["MEETINGS_CLIENT_IP_HEADER"], m["MEETINGS_TRUSTED_PROXIES"] = bad, "10.20.0.0/16"
		if c, err = LoadConfig(env(m)); err == nil {
			t.Errorf("MEETINGS_CLIENT_IP_HEADER=%q: LoadConfig accepted it as %q", bad, c.ClientIPHeader)
		}
	}
}

// MUSTER_URL may be plain http to muster's fully-qualified Service name in
// the cluster domain (default cluster.local, or MEETINGS_CLUSTER_DOMAIN), or
// to exactly the host MEETINGS_MUSTER_HTTP_HOST names; never the short or
// dotless form, and never a wildcard.
func TestLoadConfigMusterHTTP(t *testing.T) {
	for name, c := range map[string]struct {
		url, domain, host string
		ok                bool
	}{
		"fqdn, default domain":      {"http://muster-web.tenant-b.svc.cluster.local.:8105", "", "", true},
		"fqdn, its own domain":      {"http://muster-web.tenant-b.svc.corp-k8s.internal.:8105", "corp-k8s.internal", "", true},
		"fqdn, the default refused": {"http://muster-web.tenant-b.svc.cluster.local.:8105", "corp-k8s.internal", "", false},
		"dotless full form":         {"http://muster-web.tenant-b.svc.cluster.local:8105", "", "", false},
		"short form":                {"http://muster-web.tenant-b.svc:8105", "", "", false},
		"the named host":            {"http://muster-app:8105", "", "muster-app", true},
		"another host":              {"http://muster:8105", "", "muster-app", false},
		"a wildcard host":           {"http://muster-app:8105", "", "*", false},
		"a wildcard-ish host":       {"http://x.svc:8105", "", "*.svc", false},
		"a host with a port":        {"http://muster-app:8105", "", "muster-app:8105", false},
		"a cluster domain wildcard": {"http://muster-web.tenant-b.svc.cluster.local.:8105", "*", "", false},
	} {
		m := base()
		m["MUSTER_URL"], m["MEETINGS_CLUSTER_DOMAIN"], m["MEETINGS_MUSTER_HTTP_HOST"] = c.url, c.domain, c.host
		got, err := LoadConfig(env(m))
		if (err == nil) != c.ok {
			t.Errorf("%s: LoadConfig = %q, %v; want ok=%v", name, got.URL, err, c.ok)
		}
		// A malformed setting is refused as itself, not only because the
		// URL then happens not to match it.
		if strings.Contains(c.host, "*") || strings.Contains(c.host, ":") {
			if err == nil || !strings.Contains(err.Error(), "MEETINGS_MUSTER_HTTP_HOST names ONE host") {
				t.Errorf("%s: LoadConfig = %v, want MEETINGS_MUSTER_HTTP_HOST refused", name, err)
			}
		}
		if c.domain == "*" && (err == nil || !strings.Contains(err.Error(), "MEETINGS_CLUSTER_DOMAIN")) {
			t.Errorf("%s: LoadConfig = %v, want MEETINGS_CLUSTER_DOMAIN refused", name, err)
		}
	}
}

// The proxy settings reach the server: a header read by LoadConfig but
// dropped on the way would leave the knock limit counting the proxy.
func TestTheProxySettingsReachTheServer(t *testing.T) {
	m := base()
	m["MEETINGS_CLIENT_IP_HEADER"], m["MEETINGS_TRUSTED_PROXIES"] = "True-Client-IP", "10.30.0.0/16"
	c, err := LoadConfig(env(m))
	if err != nil {
		t.Fatal(err)
	}
	sc := c.serverConfig(nil, nil)
	if sc.ClientIPHeader != "True-Client-Ip" || !slices.Equal(sc.TrustedProxies, []netip.Prefix{netip.MustParsePrefix("10.30.0.0/16")}) {
		t.Fatalf("server config: header %q, proxies %v; want True-Client-Ip and 10.30.0.0/16", sc.ClientIPHeader, sc.TrustedProxies)
	}
}

// The transcript's settings (design: "Three engines, one interface"): off by
// default and needing nothing; utterance needs the STT service and the
// ingest's secret and URL; fake refuses to start unless
// MEETINGS_TEST_MODE=1; nemotron is TestLoadConfigNemotronNeedsAllFour's.
func TestLoadConfigTranscript(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil || c.Engine != "" || c.IngestListen != ":8082" || c.IngestKey != nil {
		t.Fatalf("unset: engine %q, listen %q, key %v, %v; want off", c.Engine, c.IngestListen, c.IngestKey, err)
	}
	if eng, err := c.engine(nil); eng != nil || err != nil {
		t.Fatalf("unset: engine() = %v, %v; want none", eng, err)
	}
	secret := base64.StdEncoding.EncodeToString([]byte("main fixture ingest secret 0a9b8c7d"))
	on := func(extra map[string]string) map[string]string {
		m := base()
		m["MEETINGS_STT_ENGINE"], m["MEETINGS_STT_URL"], m["MEETINGS_STT_TOKEN"] = "utterance", "http://stt-cohere:8000", "s3cretT0ken-stt"
		m["MEETINGS_INGEST_SECRET"], m["MEETINGS_INGEST_URL"] = secret, "ws://muster-meetings:8082"
		for k, v := range extra {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	c, err = LoadConfig(env(on(map[string]string{"MEETINGS_INGEST_LISTEN": "10.1.2.3:8082"})))
	if err != nil || c.Engine != "utterance" || c.STTURL != "http://stt-cohere:8000" || c.IngestURL != "ws://muster-meetings:8082" || c.IngestListen != "10.1.2.3:8082" || len(c.IngestKey) != 35 {
		t.Fatalf("utterance: %+v, %v", c, err)
	}
	if eng, err := c.engine(nil); err != nil || eng == nil || eng.Name() != "utterance" {
		t.Fatalf("utterance: engine() = %v, %v", eng, err)
	}
	if sc := c.serverConfig(nil, nil); sc.IngestURL != "ws://muster-meetings:8082" || len(sc.IngestKey) != 35 {
		t.Fatalf("the ingest settings do not reach the server: %q, %d", sc.IngestURL, len(sc.IngestKey))
	}
	c, err = LoadConfig(env(on(map[string]string{"MEETINGS_STT_ENGINE": "fake", "MEETINGS_TEST_MODE": "1", "MEETINGS_FAKE_STT_SCRIPT": "one|two"})))
	if err != nil || c.Engine != "fake" || !slices.Equal(c.FakeScript, []string{"one", "two"}) {
		t.Fatalf("fake in test mode: %+v, %v", c, err)
	}
	if c, err := LoadConfig(env(on(map[string]string{"MEETINGS_STT_ENGINE": "off"}))); err != nil || c.Engine != "" {
		t.Fatalf("off: %q, %v", c.Engine, err)
	}
	for name, extra := range map[string]map[string]string{
		"fake without test mode":    {"MEETINGS_STT_ENGINE": "fake"},
		"fake with test mode 0":     {"MEETINGS_STT_ENGINE": "fake", "MEETINGS_TEST_MODE": "0"},
		"an unknown engine":         {"MEETINGS_STT_ENGINE": "whisper"},
		"utterance without a URL":   {"MEETINGS_STT_URL": ""},
		"utterance with a path":     {"MEETINGS_STT_URL": "http://stt-cohere:8000/v1"},
		"utterance without a token": {"MEETINGS_STT_TOKEN": ""},
		"no ingest secret":          {"MEETINGS_INGEST_SECRET": ""},
		"a short ingest secret":     {"MEETINGS_INGEST_SECRET": base64.StdEncoding.EncodeToString([]byte("s3cretT0ken 16 b"))},
		"no ingest URL":             {"MEETINGS_INGEST_URL": ""},
		"an http ingest URL":        {"MEETINGS_INGEST_URL": "http://muster-meetings:8082"},
		"an ingest URL with a path": {"MEETINGS_INGEST_URL": "ws://muster-meetings:8082/stt"},
	} {
		_, err := LoadConfig(env(on(extra)))
		if err == nil {
			t.Errorf("%s: LoadConfig accepted it", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") || strings.Contains(err.Error(), "czNjcmV0VDBr") {
			t.Errorf("%s: the refusal echoes a secret: %v", name, err)
		}
	}
}

// 🔴 The boot check (design: the engines table): nemotron needs ALL FOUR of
// MEETINGS_STREAM_STT_URL, MEETINGS_STREAM_STT_TOKEN, and its fallback's
// MEETINGS_STT_URL and MEETINGS_STT_TOKEN, or the plugin refuses to boot;
// each one missing is refused BY NAME, and no refusal shows a token.
func TestLoadConfigNemotronNeedsAllFour(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("main fixture ingest secret 0a9b8c7d"))
	all := func(extra map[string]string) map[string]string {
		m := base()
		m["MEETINGS_STT_ENGINE"] = "nemotron"
		m["MEETINGS_STREAM_STT_URL"], m["MEETINGS_STREAM_STT_TOKEN"] = "ws://stt-nemotron:8000/v1/stream", "s3cretT0ken-stream"
		m["MEETINGS_STT_URL"], m["MEETINGS_STT_TOKEN"] = "http://stt-cohere:8000", "s3cretT0ken-stt"
		m["MEETINGS_INGEST_SECRET"], m["MEETINGS_INGEST_URL"] = secret, "ws://muster-meetings:8082"
		for k, v := range extra {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	c, err := LoadConfig(env(all(nil)))
	if err != nil || c.Engine != "nemotron" || c.StreamURL != "ws://stt-nemotron:8000/v1/stream" || c.StreamToken != "s3cretT0ken-stream" ||
		c.STTURL != "http://stt-cohere:8000" || c.STTToken != "s3cretT0ken-stt" {
		t.Fatalf("all four: %+v, %v", c, err)
	}
	if eng, err := c.engine(nil); err != nil || eng == nil || eng.Name() != "nemotron" {
		t.Fatalf("all four: engine() = %v, %v", eng, err)
	}
	if line := startupLine(c, "meet", 1, false); strings.Contains(line, "s3cretT0ken") || !strings.Contains(line, "nemotron engine via ws://stt-nemotron:8000") {
		t.Fatalf("the startup line: %s", line)
	}
	for name, c := range map[string]struct {
		extra map[string]string
		want  string
	}{
		"no stream URL":                   {map[string]string{"MEETINGS_STREAM_STT_URL": ""}, "MEETINGS_STREAM_STT_URL"},
		"no stream token":                 {map[string]string{"MEETINGS_STREAM_STT_TOKEN": ""}, "MEETINGS_STREAM_STT_TOKEN"},
		"no fallback URL":                 {map[string]string{"MEETINGS_STT_URL": ""}, "MEETINGS_STT_URL"},
		"no fallback token":               {map[string]string{"MEETINGS_STT_TOKEN": ""}, "MEETINGS_STT_TOKEN"},
		"an http stream URL":              {map[string]string{"MEETINGS_STREAM_STT_URL": "http://stt-nemotron:8000/v1/stream"}, "MEETINGS_STREAM_STT_URL"},
		"another path":                    {map[string]string{"MEETINGS_STREAM_STT_URL": "ws://stt-nemotron:8000/v1/other"}, "MEETINGS_STREAM_STT_URL"},
		"a query":                         {map[string]string{"MEETINGS_STREAM_STT_URL": "ws://stt-nemotron:8000/v1/stream?token=s3cretT0ken"}, "MEETINGS_STREAM_STT_URL"},
		"credentials in the URL":          {map[string]string{"MEETINGS_STREAM_STT_URL": "ws://u:s3cretT0ken@stt-nemotron:8000/v1/stream"}, "MEETINGS_STREAM_STT_URL"},
		"a stream token with a CR/LF":     {map[string]string{"MEETINGS_STREAM_STT_TOKEN": "s3cretT0ken\r\nX-Forged: 1"}, "MEETINGS_STREAM_STT_TOKEN"},
		"a stream token with a space":     {map[string]string{"MEETINGS_STREAM_STT_TOKEN": "s3cretT0ken two"}, "MEETINGS_STREAM_STT_TOKEN"},
		"no ingest secret (any engine's)": {map[string]string{"MEETINGS_INGEST_SECRET": ""}, "MEETINGS_INGEST_SECRET"},
	} {
		_, err := LoadConfig(env(all(c.extra)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: LoadConfig = %v, want a refusal naming %s", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") {
			t.Errorf("%s: the refusal echoes a secret: %v", name, err)
		}
	}
	// The path may be left off (it is added); wss to any host.
	if c, err := LoadConfig(env(all(map[string]string{"MEETINGS_STREAM_STT_URL": "wss://stt.example.test"}))); err != nil || c.StreamURL != "wss://stt.example.test/v1/stream" {
		t.Fatalf("no path: %q, %v", c.StreamURL, err)
	}
}

// 🔴 An origin written into the CSP names exactly one host: a wildcard, a
// directive separator, a source separator or a quote is refused, and the
// ordinary forms still pass.
func TestCheckOriginRefusesWildcardsAndCSPSeparators(t *testing.T) {
	for _, bad := range []string{"https://*", "https://*.example.test", "https://muster.example.test;script-src", "https://muster.example.test,evil.test",
		"https://muster.example.test'", "https://mu ster.example.test", "https://a;b"} {
		if got, err := CheckOrigin("MUSTER_ORIGIN", bad); err == nil {
			t.Errorf("CheckOrigin(%q) = %q, want a refusal", bad, got)
		}
	}
	for raw, want := range map[string]string{"https://Muster.Example.Test": "https://muster.example.test", "https://example.test:8443": "https://example.test:8443",
		"http://127.0.0.1:9000": "http://127.0.0.1:9000", "http://[::1]:9000": "http://[::1]:9000", "https://xn--mstr-kva.example.test": "https://xn--mstr-kva.example.test"} {
		if got, err := CheckOrigin("MUSTER_ORIGIN", raw); err != nil || got != want {
			t.Errorf("CheckOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// Contacts (phase 1b) is optional: neither variable is today's config, both
// configure the client, one alone refuses to start, and the token is never
// shown. The startup line gains a contacts clause only when configured (the
// no-contacts line is pinned literally by TestTheStartupLineNamesTheHostOnly).
func TestContactsConfigIsOptionalBothOrNeither(t *testing.T) {
	const tok = "ctk_main_fixture_token_00112233445566778899"
	c, err := LoadConfig(env(base()))
	if err != nil || c.Contacts != nil {
		t.Fatalf("no contacts variables: %v, %v; want no client", c.Contacts, err)
	}
	for _, x := range []struct {
		url, token string
		ok         bool
	}{
		{"http://muster-contacts:8081", tok, true},
		{"http://muster-contacts:8081", "", false},
		{"", tok, false},
		{"http://muster-contacts:8081/v1", tok, false},
		{"http://muster-contacts:8081", "mpl_x", false},
		{"http://muster-contacts:8081", "short", false},
	} {
		m := base()
		m["MEETINGS_CONTACTS_URL"], m["MEETINGS_CONTACTS_TOKEN"] = x.url, x.token
		c, err := LoadConfig(env(m))
		if (err == nil) != x.ok || (err == nil && c.Contacts == nil) {
			t.Errorf("(%q, %q): %v, %v; want ok %v", x.url, x.token, c.Contacts, err, x.ok)
		}
		if err != nil && strings.Contains(err.Error(), tok) {
			t.Errorf("the error shows the token: %v", err)
		}
		if x.ok {
			line := startupLine(c, "meet", 1, false)
			if !strings.HasSuffix(line, "; contacts: http://muster-contacts:8081 (the guest card is offered)") || strings.Contains(line, tok) {
				t.Errorf("startup line with contacts: %q", line)
			}
		}
	}
}

// The recording (phase 5) is optional: no MEETINGS_S3_* variable is today's
// config, all four configure the bucket, any part refuses to start, and no
// value is ever shown. The startup line gains a recording clause only when
// configured.
func TestRecordingConfigIsAllOrNothing(t *testing.T) {
	// Built at run time from short parts: a fixture, not a key (gitleaks).
	secret := strings.Join([]string{"s3", "fixture", "only"}, "-")
	c, err := LoadConfig(env(base()))
	if err != nil || c.S3 != nil {
		t.Fatalf("no S3 variables: %v, %v; want no bucket", c.S3, err)
	}
	if line := startupLine(c, "meet", 1, false); strings.Contains(line, "recording") {
		t.Fatalf("startup line without a bucket names a recording: %q", line)
	}
	full := map[string]string{"MEETINGS_S3_ENDPOINT": "http://minio.example.test:9000", "MEETINGS_S3_BUCKET": "meetings",
		"MEETINGS_S3_ACCESS_KEY": "meetings-key", "MEETINGS_S3_SECRET_KEY": secret}
	m := base()
	for k, v := range full {
		m[k] = v
	}
	c, err = LoadConfig(env(m))
	if err != nil || c.S3 == nil || c.S3.Bucket() != "meetings" || c.S3.Region() != "us-east-1" {
		t.Fatalf("all four: %v, %v", c.S3, err)
	}
	line := startupLine(c, "meet", 1, false)
	if !strings.Contains(line, `; recording: bucket "meetings" at http://minio.example.test:9000, `) || strings.Contains(line, secret) || strings.Contains(line, "meetings-key") {
		t.Errorf("startup line with a bucket: %q", line)
	}
	for k := range full {
		m := base()
		for k2, v := range full {
			if k2 != k {
				m[k2] = v
			}
		}
		if _, err := LoadConfig(env(m)); !errors.Is(err, ErrS3Half) {
			t.Errorf("without %s: %v, want ErrS3Half", k, err)
		}
	}
	m = base()
	m["MEETINGS_S3_REGION"] = "us-east-1"
	if c, err := LoadConfig(env(m)); err != nil || c.S3 != nil {
		t.Errorf("a region alone (the Kubernetes base's): %v, %v; want no bucket", c.S3, err)
	}
	m = base()
	for k, v := range full {
		m[k] = v
	}
	m["MEETINGS_S3_ENDPOINT"] = "http://minio.example.test:9000/" + secret
	if _, err := LoadConfig(env(m)); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("an endpoint with a path: %v", err)
	}
}

// The analysis and the notes are optional: no model is off whatever else is
// set (the key included); a model needs the key; the default vision model is
// Anthropic's, pinned; mail needs the analysis; data_collection is always
// deny; and no secret reaches an error or the startup line.
func TestPostCallConfigIsOptional(t *testing.T) {
	key := strings.Join([]string{"or", "fixture", "only"}, "-")
	pass := strings.Join([]string{"smtp", "fixture", "only"}, "-")
	m := base()
	m["OPENROUTER_API_KEY"] = key
	m["MEETINGS_SMTP_ADDR"] = ""
	c, err := LoadConfig(env(m))
	if err != nil || c.AI != nil || c.Mail != nil {
		t.Fatalf("a key and no model: %v %v %v; want off", c.AI, c.Mail, err)
	}
	if line := startupLine(c, "meet", 1, false); strings.Contains(line, "action items") || strings.Contains(line, "notes") {
		t.Errorf("startup line with the analysis off: %q", line)
	}
	m["MEETINGS_ITEMS_MODEL"] = "off"
	if c, err := LoadConfig(env(m)); err != nil || c.AI != nil {
		t.Errorf("off: %v %v", c.AI, err)
	}
	m = base()
	m["MEETINGS_ITEMS_MODEL"] = "deepseek/deepseek-v4.1-flash-1017"
	m["MEETINGS_ITEMS_PROVIDER_PREFS"] = `{"data_collection":"deny","order":["deepinfra/fp8"],"allow_fallbacks":false}`
	if _, err := LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("a model with no key: %v", err)
	}
	m["OPENROUTER_API_KEY"] = key
	c, err = LoadConfig(env(m))
	if err != nil || c.AI == nil || c.AI.Items.Adapter != "chat_json" || c.AI.Every.String() != "10s" || c.AI.Vision.Model != DefaultVisionModel ||
		c.AI.Vision.Prefs["order"].([]any)[0] != "anthropic" || c.AI.Vision.Prefs["allow_fallbacks"] != false {
		t.Fatalf("a model and a key: %+v %v", c.AI, err)
	}
	for k, v := range map[string]string{
		"MEETINGS_ITEMS_PROVIDER_PREFS":  `{"data_collection":"allow","order":["deepinfra/fp8"],"allow_fallbacks":false}`,
		"MEETINGS_VISION_PROVIDER_PREFS": `{"data_collection":"allow","order":["anthropic"],"allow_fallbacks":false}`,
		"MEETINGS_ITEMS_EVERY":           "100ms",
	} {
		m2 := maps(m)
		m2[k] = v
		if _, err := LoadConfig(env(m2)); err == nil || strings.Contains(err.Error(), key) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	m2 := maps(m)
	m2["MEETINGS_VISION_MODEL"] = "google/gemini-9-flash"
	if _, err := LoadConfig(env(m2)); err == nil || !strings.Contains(err.Error(), "MEETINGS_VISION_PROVIDER_PREFS") {
		t.Errorf("another vision model with no pin: %v", err)
	}
	m2["MEETINGS_VISION_MODEL"] = "off"
	if c, err := LoadConfig(env(m2)); err != nil || c.AI.Vision.Model != "" {
		t.Errorf("vision off: %+v %v", c.AI, err)
	}
	m["MEETINGS_SMTP_ADDR"] = "mail.example.test:587"
	if _, err := LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "MEETINGS_MAIL_FROM") {
		t.Errorf("mail with no sender: %v", err)
	}
	m["MEETINGS_MAIL_FROM"] = "meetings@mail.example.test"
	m["MEETINGS_SMTP_USER"] = "meetings"
	if _, err := LoadConfig(env(m)); err == nil || strings.Contains(err.Error(), pass) {
		t.Errorf("a user with no password: %v", err)
	}
	m["MEETINGS_SMTP_PASS"] = pass
	c, err = LoadConfig(env(m))
	if err != nil || c.Mail == nil {
		t.Fatalf("mail: %v", err)
	}
	line := startupLine(c, "meet", 1, false)
	if !strings.Contains(line, "; action items: deepseek/deepseek-v4.1-flash-1017 (chat_json, live every 10s), no frame analysis; notes: mail via mail.example.test:587 from meetings@mail.example.test") ||
		strings.Contains(line, key) || strings.Contains(line, pass) {
		t.Errorf("startup line: %q", line)
	}
	m3 := base()
	m3["MEETINGS_SMTP_ADDR"] = "mail.example.test:587"
	m3["MEETINGS_MAIL_FROM"] = "meetings@mail.example.test"
	if _, err := LoadConfig(env(m3)); err == nil || !strings.Contains(err.Error(), "MEETINGS_ITEMS_MODEL") {
		t.Errorf("mail with no model: %v", err)
	}
}

func maps(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// The reprocess command queues one post run per meeting that ended in the
// range (inclusive of -to's day), under the configured model or another
// with its own pin, and nothing for a meeting outside it.
func TestReprocessQueuesTheRange(t *testing.T) {
	dir := t.TempDir()
	m := base()
	m["STATE_DIR"] = dir
	m["OPENROUTER_API_KEY"] = strings.Join([]string{"or", "fixture", "only"}, "-")
	m["MEETINGS_ITEMS_MODEL"] = "deepseek/deepseek-v4.1-flash-1017"
	m["MEETINGS_ITEMS_PROVIDER_PREFS"] = `{"data_collection":"deny","order":["deepinfra/fp8"],"allow_fallbacks":false}`
	st, err := store.Open(filepath.Join(dir, "meetings.db"), fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var ids []string
	for _, day := range []string{"2031-03-01", "2031-03-02", "2031-03-05"} {
		at, _ := time.Parse("2006-01-02", day)
		at = at.Add(12 * time.Hour)
		tok, _, err := st.CreateSession(ctx, "Aa1Bb2Cc3Dd4Ee5Ff6Gg7h", "Ada", at)
		if err != nil {
			t.Fatal(err)
		}
		sess, _ := st.LookupSession(ctx, tok, at)
		r := st.ForSession(sess)
		c, err := r.CreateMeeting(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.End(ctx, c.Meeting.ID, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.Meeting.ID)
	}
	st.Close()
	var out strings.Builder
	if err := reprocess([]string{"-from", "2031-03-01", "-to", "2031-03-02"}, env(m), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "queued 2 post-call runs (items deepseek/deepseek-v4.1-flash-1017, vision none)") {
		t.Errorf("out = %q", out.String())
	}
	if err := reprocess([]string{"-from", "2031-03-05", "-to", "2031-03-05", "-items-model", "z-ai/glm-5-20261001"}, env(m), &out); err == nil || !strings.Contains(err.Error(), "-items-prefs") {
		t.Errorf("another model with no pin: %v", err)
	}
	if err := reprocess([]string{"-from", "2031-03-05", "-to", "2031-03-05", "-items-model", "z-ai/glm-5-20261001", "-items-prefs", `{"order":["z-ai"],"allow_fallbacks":false}`}, env(m), &out); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Open(filepath.Join(dir, "meetings.db"), fixtureKey)
	defer st.Close()
	want := map[string]string{ids[0]: "deepseek/deepseek-v4.1-flash-1017", ids[1]: "deepseek/deepseek-v4.1-flash-1017", ids[2]: "z-ai/glm-5-20261001"}
	for id, model := range want {
		runs, err := st.ForSubject("Aa1Bb2Cc3Dd4Ee5Ff6Gg7h").Runs(ctx, id)
		if err != nil || len(runs) != 1 || runs[0].ItemsModel != model || runs[0].RequestedBy != "admin" || runs[0].State != "queued" {
			t.Errorf("meeting %s runs = %+v, %v; want one queued %s run", id, runs, err, model)
		}
	}
}
