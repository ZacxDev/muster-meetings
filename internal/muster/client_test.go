package muster_test

import (
	"context"
	"encoding/json"
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

// owners/check answers a subset of what it was asked. More than 200 subjects
// go in batches of at most 200 (the fake refuses 201), and a subject the
// answer names but the plugin did not ask about is dropped.
func TestOwnersCheckAnswersOnlyTheLiveSubsetItWasAsked(t *testing.T) {
	f, c := fakeWithPeople(t)
	sa, sb := redeemed(t, f, c, ada), redeemed(t, f, c, bea)
	f.Hide(mustertest.DefaultApp.PluginID, bea.ID)
	asked := []string{sa, sb}
	for i := 0; len(asked) < 450; i++ { // subjects muster never gave: not live
		asked = append(asked, ("Nv" + strconv.Itoa(100000+i) + "xxxxxxxxxxxxxxx")[:22])
	}
	live, err := c.OwnersCheck(context.Background(), asked)
	if err != nil {
		t.Fatalf("450 subjects: %v", err)
	}
	if len(live) != 1 || !live[sa] {
		t.Fatalf("450 subjects: live = %v, want Ada's alone", live)
	}
	if n := f.CountCalls("POST", "/plugin/v1/owners/check"); n != 3 {
		t.Fatalf("450 subjects took %d calls, want 3", n)
	}
	// A muster that names more than it was asked.
	extra := "Ex7raSubjectNotAsked00"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"live": []string{sa, extra}})
	}))
	defer srv.Close()
	live, err = client(t, srv.URL).OwnersCheck(context.Background(), []string{sa})
	if err != nil || len(live) != 1 || !live[sa] || live[extra] {
		t.Fatalf("an answer naming a subject that was not asked: live = %v, %v", live, err)
	}
	if _, err := c.OwnersCheck(context.Background(), []string{"4093716825"}); err != muster.ErrBadSubject {
		t.Fatalf("a user id as a subject = %v, want ErrBadSubject", err)
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
