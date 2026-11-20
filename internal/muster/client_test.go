package muster_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
)

func client(t *testing.T, base string) *muster.Client {
	t.Helper()
	c, err := muster.New(base, mustertest.Token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://muster.example.com":     "https://muster.example.com",
		"https://muster.example.com/":    "https://muster.example.com",
		"http://localhost:8105":          "http://localhost:8105",
		"http://127.0.0.1:8105":          "http://127.0.0.1:8105",
		"http://[::1]:8105":              "http://[::1]:8105",
		"  https://muster.example.com  ": "https://muster.example.com",
	}
	for in, want := range ok {
		got, err := muster.CheckBaseURL(in)
		if err != nil || got != want {
			t.Errorf("CheckBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "muster.example.com", "http://muster.example.com", "ftp://localhost",
		"https://user:pw@muster.example.com", "https://muster.example.com/?x=1",
		"https://muster.example.com/#frag", "http://localhost.example.com",
	} {
		if got, err := muster.CheckBaseURL(bad); err == nil {
			t.Errorf("CheckBaseURL(%q) = %q, want a refusal", bad, got)
		}
	}
}

// A mistaken MUSTER_URL may hold the token (pasted into the wrong variable,
// or into the path): the refusal must not echo it to the log.
func TestCheckBaseURLNeverEchoesTheValue(t *testing.T) {
	const secret = "mpl_s3cretT0ken"
	for _, bad := range []string{
		secret, "muster.example.com/" + secret, "http://muster.example.com/" + secret,
		"https://muster.example.com/" + secret, "http://localhost:8105/" + secret,
		"https://muster.example.com/?token=" + secret, "https://" + secret + "@muster.example.com",
		"https://muster.example.com/#" + secret, "%zz" + secret,
	} {
		_, err := muster.CheckBaseURL(bad)
		if err == nil {
			t.Errorf("CheckBaseURL(%q) accepted it", bad)
			continue
		}
		if strings.Contains(err.Error(), "s3cretT0ken") {
			t.Errorf("CheckBaseURL refusal echoes the value: %v", err)
		}
	}
}

// A token pasted into the URL's path would be sent, in the request line, on
// every call, and named by every network error: a path is refused.
func TestCheckBaseURLRefusesAPath(t *testing.T) {
	const secret = "mpl_s3cretT0ken"
	for _, bad := range []string{
		"https://muster.example.com/" + secret, "http://localhost:8105/" + secret,
		"https://example.com/muster", "https://example.com/muster/", "https://muster.example.com//",
		"https://muster.example.com/%2F",
	} {
		got, err := muster.CheckBaseURL(bad)
		if err == nil {
			t.Errorf("CheckBaseURL(%q) = %q: a URL with a path was accepted", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "no path") {
			t.Errorf("CheckBaseURL(%q) refused it for another reason: %v", bad, err)
		}
		if strings.Contains(err.Error(), "s3cretT0ken") {
			t.Errorf("CheckBaseURL refusal echoes the value: %v", err)
		}
	}
}

// Defence in depth: net/http's error names the whole request URL. A base
// with a token in its path (one that skipped CheckBaseURL) must not reach the
// error, and so the log, through it.
func TestANetworkErrorNamesTheSchemeAndHostOnly(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()
	c, err := muster.New(base+"/mpl_s3cretT0ken", mustertest.Token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Self(context.Background())
	e := muster.AsError(err)
	if e == nil || e.Kind != muster.KindNetwork {
		t.Fatalf("Self on a closed port = %v, want a network error", err)
	}
	if strings.Contains(err.Error(), "s3cretT0ken") || strings.Contains(err.Error(), "/plugin/v1") {
		t.Errorf("the network error names the request path: %v", err)
	}
	if !strings.Contains(err.Error(), base) {
		t.Errorf("the network error lost the scheme and host %s: %v", base, err)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://muster.example.com/mpl_s3cretT0ken?x=1#f": "https://muster.example.com",
		"https://u:p@muster.example.com:8443/a":            "https://muster.example.com:8443",
		"http://127.0.0.1:8105":                            "http://127.0.0.1:8105",
		"%zzmpl_s3cretT0ken":                               "(an address not shown)",
		"mpl_s3cretT0ken":                                  "(an address not shown)",
	} {
		if got := muster.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRefusesATokenOfAnotherKind(t *testing.T) {
	for _, tok := range []string{"", "mpl_", "mst_abc", "abc"} {
		if _, err := muster.New("https://muster.example.com", tok); err == nil {
			t.Errorf("New(%q) accepted a non-plugin token", tok)
		}
	}
}

func TestRedeemSendsTheCodeOnceAndReadsTheAnswer(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	c := client(t, f.URL)
	code := f.MintApp(mustertest.Default.PluginID, "m_ref-9")
	got, err := c.Redeem(context.Background(), code)
	if err != nil {
		t.Fatalf("Redeem = %v", err)
	}
	if got.OwnerID != 1 || got.Purpose != "app" || got.Target.App == nil || got.Target.App.Ref != "m_ref-9" || got.Target.Mirror != nil {
		t.Fatalf("Redeem = %+v", got)
	}
	// Single use: muster's one 404.
	_, err = c.Redeem(context.Background(), code)
	if e := muster.AsError(err); e == nil || e.Status != 404 || e.Kind != muster.KindOff {
		t.Fatalf("second redeem err = %v, want the 404", err)
	}
	calls := f.Calls()
	if len(calls) != 2 || calls[0] != "POST /plugin/v1/handoffs/redeem "+mustertest.Token {
		t.Fatalf("calls = %q", calls)
	}
}

func TestErrorsAreTyped(t *testing.T) {
	f := mustertest.New()
	defer f.Close()
	c := client(t, f.URL)
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"slow down"}`)
	}))
	defer rl.Close()
	_, err := client(t, rl.URL).Self(context.Background())
	e := muster.AsError(err)
	if e == nil || e.Status != 429 || e.Kind != muster.KindRateLimited || e.RetryAfter != 7*time.Second || !e.Retryable() || e.Message != "slow down" {
		t.Fatalf("429 err = %#v", e)
	}
	f.Revoke(mustertest.Token)
	_, err = c.Self(context.Background())
	if e := muster.AsError(err); e == nil || e.Status != 401 || e.Kind != muster.KindAuth || e.Retryable() {
		t.Fatalf("401 err = %#v", e)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err = client(t, srv.URL).Self(context.Background())
	if e := muster.AsError(err); e == nil || e.Kind != muster.KindOff {
		t.Fatalf("404 err = %#v", e)
	}
}

func TestKindForStatus(t *testing.T) {
	want := map[int]muster.Kind{400: "bad_request", 401: "auth", 403: "capability", 404: "not_found",
		409: "conflict", 413: "bad_request", 429: "rate_limited", 500: "server", 503: "server", 302: "protocol"}
	for s, k := range want {
		if got := muster.KindForStatus(s); got != k {
			t.Errorf("KindForStatus(%d) = %s, want %s", s, got, k)
		}
	}
}
