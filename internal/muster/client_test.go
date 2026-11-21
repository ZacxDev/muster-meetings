package muster_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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
		got, err := muster.CheckBaseURL(in, muster.HTTPAllow{ClusterDomain: "cluster.local"})
		if err != nil || got != want {
			t.Errorf("CheckBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "muster.example.com", "http://muster.example.com", "ftp://localhost",
		"https://user:pw@muster.example.com", "https://muster.example.com/?x=1",
		"https://muster.example.com/#frag", "http://localhost.example.com",
	} {
		if got, err := muster.CheckBaseURL(bad, muster.HTTPAllow{ClusterDomain: "cluster.local"}); err == nil {
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
		_, err := muster.CheckBaseURL(bad, muster.HTTPAllow{ClusterDomain: "cluster.local"})
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
		got, err := muster.CheckBaseURL(bad, muster.HTTPAllow{ClusterDomain: "cluster.local"})
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

// Fixtures: people whose user ids, emails and display names are pairwise
// distinct; the fake's DefaultApp (plugin 3) serves them.
var (
	ada = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
	bea = mustertest.Person{ID: 2718281828, Email: "bea.k@example.test", DisplayName: "Bea K."}
)

func fakeWithPeople(t *testing.T) (*mustertest.Fake, *muster.Client) {
	t.Helper()
	f := mustertest.New()
	t.Cleanup(f.Close)
	f.AddPerson(ada)
	f.AddPerson(bea)
	return f, client(t, f.URL)
}

// redeemed is p's subject, after a redeem made the grant.
func redeemed(t *testing.T, f *mustertest.Fake, c *muster.Client, p mustertest.Person) string {
	t.Helper()
	r, err := c.Redeem(context.Background(), f.MintApp(mustertest.DefaultApp.PluginID, p.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	return r.Owner.Subject
}

func TestRedeemSendsTheCodeOnceAndReadsTheSubject(t *testing.T) {
	f, c := fakeWithPeople(t)
	code := f.MintApp(mustertest.DefaultApp.PluginID, ada.ID, "m_ref-9")
	got, err := c.Redeem(context.Background(), code)
	if err != nil {
		t.Fatalf("Redeem = %v", err)
	}
	if !muster.ValidSubject(got.Owner.Subject) || got.Owner.Subject != f.Subject(mustertest.DefaultApp.PluginID, ada.ID) ||
		got.Owner.DisplayName != ada.DisplayName || got.Scope != "server" || got.Purpose != "app" ||
		got.Target.App == nil || got.Target.App.Ref != "m_ref-9" || got.Target.Mirror != nil {
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

func TestSelfReadsAServerAppAndNoPerson(t *testing.T) {
	_, c := fakeWithPeople(t)
	s, err := c.Self(context.Background())
	if err != nil || s.PluginID != mustertest.DefaultApp.PluginID || s.Scope != "server" || s.OwnerKind != "server" ||
		!slices.Equal(s.Capabilities, []string{"app"}) || s.ActForHours != 8 {
		t.Fatalf("Self = %+v, %v", s, err)
	}
}

// owners/check answers each subject it was asked with live, expired or
// revoked. 🔴 It fails closed: a subject absent from the answer, or given a
// state the client does not know, is revoked; a subject the answer names but
// the plugin did not ask about is dropped. More than 200 subjects go in
// batches of at most 200 (the fake refuses 201).
func TestOwnersCheckAnswersOnlyTheLiveSubsetItWasAsked(t *testing.T) {
	f, c := fakeWithPeople(t)
	sa, sb := redeemed(t, f, c, ada), redeemed(t, f, c, bea)
	f.Hide(mustertest.DefaultApp.PluginID, bea.ID)
	asked := []string{sa, sb}
	for i := 0; len(asked) < 450; i++ { // subjects muster never gave: absent
		asked = append(asked, ("Nv" + strconv.Itoa(100000+i) + "xxxxxxxxxxxxxxx")[:22])
	}
	states, err := c.OwnersCheck(context.Background(), asked)
	if err != nil {
		t.Fatalf("450 subjects: %v", err)
	}
	if len(states) != 450 || states[sa] != muster.StateLive || states[sb] != muster.StateRevoked || states[asked[449]] != muster.StateRevoked {
		t.Fatalf("450 subjects: Ada %q, Bea %q, an absent one %q (%d answered); want live, revoked, revoked (450)",
			states[sa], states[sb], states[asked[449]], len(states))
	}
	if n := f.CountCalls("POST", "/plugin/v1/owners/check"); n != 3 {
		t.Fatalf("450 subjects took %d calls, want 3", n)
	}
	// A muster that names more than it was asked, and states the client
	// does not know.
	extra := "Ex7raSubjectNotAsked00"
	odd := "OddStateSubject0000000"
	exp := "ExpiredSubject00000000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"states": map[string]string{sa: "live", extra: "live", odd: "paused", exp: "expired"}})
	}))
	defer srv.Close()
	states, err = client(t, srv.URL).OwnersCheck(context.Background(), []string{sa, odd, exp})
	if err != nil || len(states) != 3 || states[sa] != muster.StateLive || states[exp] != muster.StateExpired {
		t.Fatalf("an answer naming a subject that was not asked: states = %v, %v", states, err)
	}
	if _, ok := states[extra]; ok {
		t.Fatalf("an answer naming a subject that was not asked: states = %v", states)
	}
	if states[odd] != muster.StateRevoked {
		t.Fatalf("an unknown state %q = %q, want revoked", "paused", states[odd])
	}
	if _, err := c.OwnersCheck(context.Background(), []string{"4093716825"}); err != muster.ErrBadSubject {
		t.Fatalf("a user id as a subject = %v, want ErrBadSubject", err)
	}
}

// A 2xx owners/check answer with no "states" object is not an answer: the
// design's old {"live": [...]} shape, an empty body, {} and {"states":
// null} are each a KindProtocol error, never "every subject revoked", so the
// poll and the boot keep everything and log it.
func TestOwnersCheckWithoutAStatesObjectIsAProtocolError(t *testing.T) {
	const sa = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7h"
	for name, body := range map[string]string{
		"the old live list": `{"live":["` + sa + `"]}`,
		"an empty body":     ``,
		"{}":                `{}`,
		"states null":       `{"states":null}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, body)
		}))
		states, err := client(t, srv.URL).OwnersCheck(context.Background(), []string{sa})
		srv.Close()
		if e := muster.AsError(err); e == nil || e.Kind != muster.KindProtocol {
			t.Errorf("%s: OwnersCheck = %v, %v; want a protocol error", name, states, err)
		}
		if states[sa] != "" {
			t.Errorf("%s: Ada was answered %q from no states object", name, states[sa])
		}
	}
	// Positive control: an empty states object is an answer (Ada absent:
	// revoked).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"states":{}}`)
	}))
	defer srv.Close()
	if states, err := client(t, srv.URL).OwnersCheck(context.Background(), []string{sa}); err != nil || states[sa] != muster.StateRevoked {
		t.Fatalf("positive control: {\"states\":{}} = %v, %v; want Ada revoked", states, err)
	}
}

// One failed batch does not discard the answered ones: 450 subjects, the
// second call fails, and the first and third batches come back.
func TestOwnersCheckKeepsTheBatchesThatWereAnswered(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 2 {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":"unavailable"}`)
			return
		}
		var in struct{ Subjects []string }
		json.NewDecoder(r.Body).Decode(&in)
		st := map[string]string{}
		for _, s := range in.Subjects {
			st[s] = "live"
		}
		json.NewEncoder(w).Encode(map[string]any{"states": st})
	}))
	defer srv.Close()
	var asked []string
	for i := 0; i < 450; i++ {
		asked = append(asked, fmt.Sprintf("K%021d", i))
	}
	states, err := client(t, srv.URL).OwnersCheck(context.Background(), asked)
	if e := muster.AsError(err); e == nil || e.Kind != muster.KindServer || !strings.Contains(e.Message, "batch 2 of 3") {
		t.Errorf("err = %v, want the second batch's 503 named", err)
	}
	if len(states) != 250 || states[asked[0]] != muster.StateLive || states[asked[449]] != muster.StateLive {
		t.Fatalf("%d answered; want the 250 of the first and third batches", len(states))
	}
	if _, ok := states[asked[300]]; ok {
		t.Fatal("a subject of the failed batch was answered")
	}
}

// A state other than live, expired or revoked, for a subject that was asked,
// is revoked: fail closed.
func TestOwnersCheckAnUnknownStateIsRevoked(t *testing.T) {
	const sa = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7h"
	for _, st := range []string{"paused", "LIVE", "", "live "} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"states": map[string]string{sa: st}})
		}))
		states, err := client(t, srv.URL).OwnersCheck(context.Background(), []string{sa})
		srv.Close()
		if err != nil || states[sa] != muster.StateRevoked {
			t.Errorf("state %q = %q, %v; want revoked", st, states[sa], err)
		}
	}
}

// 🔴 notify, its withdrawal and tasks act for the person the subject names,
// with exactly one Muster-Act-For header: Ada's knock reaches Ada and nobody
// else, and a call with no subject is never sent.
func TestNotifyAndTasksActForTheSubjectNamed(t *testing.T) {
	f, c := fakeWithPeople(t)
	f.Install(mustertest.App{Token: mustertest.Token, PluginID: mustertest.DefaultApp.PluginID, PluginName: "example-meetings", Capabilities: []string{"app", "tasks"}})
	sa, _ := redeemed(t, f, c, ada), redeemed(t, f, c, bea)
	queued, err := c.Notify(context.Background(), sa, "mtgRef01", 2)
	if err != nil || !queued {
		t.Fatalf("Notify for Ada = %v, %v", queued, err)
	}
	if p := f.Pushes(); len(p) != 1 || p[0].UserID != ada.ID || p[0].Kind != "knock" || p[0].Count != 2 || p[0].Tag != "app-3-mtgRef01" {
		t.Fatalf("pushes after Ada's knock = %+v, want one knock to Ada", p)
	}
	if w, err := c.Withdraw(context.Background(), sa, "mtgRef01"); err != nil || !w {
		t.Fatalf("Withdraw for Ada = %v, %v", w, err)
	}
	if p := f.Pushes(); len(p) != 2 || p[1].UserID != ada.ID || p[1].Kind != "resolved" {
		t.Fatalf("pushes after the withdrawal = %+v", p)
	}
	res, err := c.FileTask(context.Background(), sa, muster.TaskRequest{Key: "meetings:item-7", Create: &muster.TaskCreate{Title: "Send the deck", Body: "From the meeting."}})
	if err != nil || !res.Created || res.ID == nil {
		t.Fatalf("FileTask for Ada = %+v, %v", res, err)
	}
	if len(f.Tasks(ada.ID)) != 1 || len(f.Tasks(bea.ID)) != 0 {
		t.Fatalf("tasks: Ada %d, Bea %d; want 1, 0", len(f.Tasks(ada.ID)), len(f.Tasks(bea.ID)))
	}
	n := 0
	for _, call := range f.Calls() {
		if strings.Contains(call, "/notify") || strings.Contains(call, "/tasks") {
			n++
			if !strings.HasSuffix(call, " act-for="+sa) || strings.Count(call, "act-for=") != 1 {
				t.Errorf("call %q does not name Ada once", call)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d act-for calls, want 3", n)
	}
	// No subject, or a user id in its place: refused here, nothing sent.
	before := len(f.Calls())
	for _, bad := range []string{"", "4093716825"} {
		if _, err := c.Notify(context.Background(), bad, "mtgRef01", 1); err != muster.ErrBadSubject {
			t.Errorf("Notify(%q) = %v", bad, err)
		}
		if _, err := c.Withdraw(context.Background(), bad, "mtgRef01"); err != muster.ErrBadSubject {
			t.Errorf("Withdraw(%q) = %v", bad, err)
		}
		if _, err := c.FileTask(context.Background(), bad, muster.TaskRequest{Key: "meetings:x", Comment: "c"}); err != muster.ErrBadSubject {
			t.Errorf("FileTask(%q) = %v", bad, err)
		}
	}
	if after := len(f.Calls()); after != before {
		t.Fatalf("a call with no subject made %d requests", after-before)
	}
}

// A person muster may no longer act for is muster's one 404: "cannot reach
// this person now".
func TestActingForAPersonWhoHidTheAppIsA404(t *testing.T) {
	f, c := fakeWithPeople(t)
	sa := redeemed(t, f, c, ada)
	f.Hide(mustertest.DefaultApp.PluginID, ada.ID)
	_, err := c.Notify(context.Background(), sa, "mtgRef01", 1)
	if e := muster.AsError(err); e == nil || e.Kind != muster.KindOff || e.Message != "no such person for this app" {
		t.Fatalf("Notify for a hidden person = %v", err)
	}
	if len(f.Pushes()) != 0 {
		t.Fatal("a knock reached a person who hid the app")
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
	f.Uninstall(mustertest.DefaultApp.PluginID)
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

// 🔴 Plain http to muster is allowed only to a loopback host or to a
// FULLY-QUALIFIED Service name, `<svc>.<ns>.svc.<cluster domain>.` with its
// trailing dot: without the dot, a resolver with ndots:5 tries the node's
// search domains first, and a wildcard there would receive the token in
// clear text. Every short or dotless form, every other domain and every
// other host is refused; https is accepted everywhere. The cluster domain
// here differs from the default, so a hardcoded "cluster.local" is caught.
func TestCheckBaseURLAllowsHTTPOnlyInsideTheCluster(t *testing.T) {
	allow := muster.HTTPAllow{ClusterDomain: "corp-k8s.internal"}
	for in, want := range map[string]string{
		"http://muster-web.tenant-b.svc.corp-k8s.internal.:8105": "http://muster-web.tenant-b.svc.corp-k8s.internal.:8105",
		"http://Muster-Web.Tenant-B.SVC.Corp-K8s.Internal.":      "http://Muster-Web.Tenant-B.SVC.Corp-K8s.Internal.",
		"https://muster-web.tenant-b.svc":                        "https://muster-web.tenant-b.svc",
		"https://muster.example.test":                            "https://muster.example.test",
	} {
		got, err := muster.CheckBaseURL(in, allow)
		if err != nil || got != want {
			t.Errorf("in-cluster: CheckBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://muster-web.tenant-b.svc.corp-k8s.internal:8105", // no trailing dot: the search domains first
		"http://muster-web.tenant-b.svc:8105",                   // the short form
		"http://muster-web.tenant-b.svc.:8105",                  // short, with a dot
		"http://muster-web.svc.corp-k8s.internal.",              // no namespace
		"http://a.muster-web.tenant-b.svc.corp-k8s.internal.",   // a label too many
		"http://muster-web.tenant-b.svc.cluster.local.",         // another cluster domain
		"http://muster-web.tenant-b.svc.corp-k8s.internal.evil.test.",
		"http://muster.example.test",                        // plain http over a network
		"http://muster.example.test.",                       // an absolute public name
		"http://.tenant-b.svc.corp-k8s.internal.",           // an empty label
		"http://muster_web.tenant-b.svc.corp-k8s.internal.", // not a DNS label
		"http://192.0.2.10",                                 // an address that is not loopback
		"ws://muster-web.tenant-b.svc.corp-k8s.internal.",   // the wrong scheme
	} {
		if got, err := muster.CheckBaseURL(bad, allow); err == nil {
			t.Errorf("not in-cluster: CheckBaseURL(%q) = %q, want a refusal", bad, got)
		} else if !strings.Contains(err.Error(), "must be https://") {
			t.Errorf("not in-cluster: CheckBaseURL(%q) refused for another reason: %v", bad, err)
		}
	}
	// No cluster domain: no cluster name at all.
	if got, err := muster.CheckBaseURL("http://muster-web.tenant-b.svc.corp-k8s.internal.", muster.HTTPAllow{}); err == nil {
		t.Errorf("no cluster domain: CheckBaseURL accepted %q", got)
	}
}

// 🔴 MEETINGS_MUSTER_HTTP_HOST: plain http to exactly that one host (case
// aside). Another host, a host that merely starts or ends with it, or one
// under it is still refused; empty allows none.
func TestCheckBaseURLAllowsHTTPToTheNamedHostOnly(t *testing.T) {
	allow := muster.HTTPAllow{Host: "muster-app"}
	for in, want := range map[string]string{
		"http://muster-app:8105": "http://muster-app:8105",
		"http://MUSTER-APP:8105": "http://MUSTER-APP:8105",
	} {
		if got, err := muster.CheckBaseURL(in, allow); err != nil || got != want {
			t.Errorf("named host: CheckBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://muster:8105",             // another host
		"http://muster-app.example.test", // the name as a prefix
		"http://evil-muster-app:8105",    // the name as a suffix
		"http://x.muster-app",            // under it
		"http://muster-app.:8105",        // the absolute form: a different lookup
		"http://muster-app.attacker.test.",
	} {
		if got, err := muster.CheckBaseURL(bad, allow); err == nil {
			t.Errorf("not the named host: CheckBaseURL(%q) = %q, want a refusal", bad, got)
		}
	}
	if got, err := muster.CheckBaseURL("http://muster-app:8105", muster.HTTPAllow{}); err == nil {
		t.Errorf("no named host: CheckBaseURL accepted %q", got)
	}
}
