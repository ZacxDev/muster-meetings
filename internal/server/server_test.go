package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/livekittest"
	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
)

// Fixtures: ids, hosts and origins that equal no constant the code or the
// fake names (the fake's DefaultApp is plugin 3). The people's user ids,
// emails and display names are pairwise distinct, and the ids are long
// enough that no timestamp or counter could hold one by chance.
const (
	publicURL    = "https://meet.example.test"
	musterOrigin = "https://hub.example.test"
)

var (
	app   = mustertest.App{Token: "mpl_server_7f3c91e0", PluginID: 12, PluginName: "meet-srv", Capabilities: []string{"app", "tasks"}}
	other = mustertest.App{Token: "mpl_server_other_5d21", PluginID: 19, PluginName: "meet-other", Capabilities: []string{"app"}}
	ada   = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
	bea   = mustertest.Person{ID: 2718281828, Email: "bea.k@example.test", DisplayName: "Bea K."}
	t0    = time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
	// testLinkKey: 32 bytes, no pattern any code names.
	testLinkKey = []byte("fixture link key 0f9e1d2c3b4a596")
)

type clock struct {
	mu sync.Mutex
	t  time.Time
	// real: the wall clock (the integration test, against a real LiveKit
	// that checks token times against its own).
	real bool
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.real {
		return time.Now()
	}
	return c.t
}
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t        *testing.T
	fake     *mustertest.Fake
	lk       *livekittest.Fake
	store    *store.Store
	dbPath   string
	app      *owners.App
	server   *server.Server
	srv      *httptest.Server
	internal *httptest.Server
	// ingest is the ingest listener (Egress's websocket streams).
	ingest *httptest.Server
	clock  *clock
	log    *syncBuf
}

// fakeScript is the fake engine's lines in every env: pairwise distinct,
// and one that looks like markup.
var fakeScript = []string{"Budget goes to Sam by Friday.", "<b>Bold</b> & <i>sly</i>", "Same time next week."}

// ingestKey is the env's MEETINGS_INGEST_SECRET.
var ingestKey = []byte("env ingest key 0a1b2c3d4e5f6a7b8c9d")

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// liveKitAt points an env at a real LiveKit instead of the fake.
type liveKitAt struct {
	API, WS   string
	Keys      livekit.Keys
	RealClock bool
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

func newEnvWith(t *testing.T, real *liveKitAt) *env { return newEnvOpts(t, real, nil) }

// newEnvOpts is newEnvWith with a last word on the server's config: by
// default the fake engine transcribes (fakeScript) and a refused egress
// start is retried after 20, 40 and 80 ms.
func newEnvOpts(t *testing.T, real *liveKitAt, opt func(*server.Config)) *env {
	t.Helper()
	e := &env{t: t, fake: mustertest.New(), clock: &clock{t: t0, real: real != nil && real.RealClock}, log: &syncBuf{}}
	t.Cleanup(e.fake.Close)
	e.fake.Now = e.clock.Now
	e.fake.Install(app)
	e.fake.Install(other)
	e.fake.AddPerson(ada)
	e.fake.AddPerson(bea)
	c, err := muster.New(e.fake.URL, app.Token)
	if err != nil {
		t.Fatal(err)
	}
	e.dbPath = filepath.Join(t.TempDir(), "meetings.db")
	st, err := store.Open(e.dbPath, testLinkKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st
	lg := log.New(e.log, "", 0)
	if e.app, err = owners.Boot(context.Background(), c, st, lg, e.clock.Now); err != nil {
		t.Fatal(err)
	}
	lkAPI, lkWS, lkKeys := "", "", livekittest.Keys
	if real == nil {
		e.lk = livekittest.New()
		t.Cleanup(e.lk.Close)
		e.lk.Now = e.clock.Now
		lkAPI, lkWS = e.lk.URL, e.lk.WSURL()
	} else {
		lkAPI, lkWS, lkKeys = real.API, real.WS, real.Keys
	}
	lkc, err := livekit.NewClient(lkAPI, lkKeys)
	if err != nil {
		t.Fatal(err)
	}
	lkc.Now = e.clock.Now
	// The ingest listener exists before the server, so its URL can be the
	// server's IngestURL; it serves the server's Ingest once there is one.
	var ingestH atomic.Value
	e.ingest = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ingestH.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(e.ingest.CloseClientConnections)
	t.Cleanup(e.ingest.Close)
	cfg := server.Config{PublicURL: publicURL, MusterOrigin: musterOrigin, Now: e.clock.Now, Log: lg,
		LiveKit: lkc, LiveKitURL: lkWS, ICE: "relay",
		// The test client is the "ingress": a knock's address comes from
		// X-Forwarded-For, so a test can knock from many addresses.
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
		// The streams re-read the database often and send no heartbeat a
		// test waits on; the reconcile loop runs only when a test calls it.
		StreamRecheck: 50 * time.Millisecond, ReconcileEvery: time.Hour, Heartbeat: time.Hour,
		Engine: stt.NewFake(fakeScript), IngestURL: "ws" + strings.TrimPrefix(e.ingest.URL, "http"), IngestKey: ingestKey,
		EgressBackoff: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}}
	if opt != nil {
		opt(&cfg)
	}
	e.server = server.New(cfg, st, e.app)
	ingestH.Store(e.server.Ingest())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.server.Run(ctx)
	e.srv = httptest.NewServer(e.server)
	t.Cleanup(e.srv.Close)
	e.internal = httptest.NewServer(e.server.Internal())
	t.Cleanup(e.internal.Close)
	if e.lk != nil {
		e.lk.WebhookURL = e.internal.URL + "/livekit/webhook"
	}
	return e
}

func (e *env) do(method, path, bearer string, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			rd = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// An event stream does not end: its status is the answer. (A mutant
		// that opens a stream where a refusal belongs must fail its test,
		// not hang it.)
		return resp, []byte("<an event stream>")
	}
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// inFrame is what a current browser sends when muster's page frames /enter.
var inFrame = []string{"Sec-Fetch-Dest", "iframe", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "same-site"}

// framed is GET path as muster's frame loads it.
func (e *env) framed(path string) (*http.Response, []byte) {
	e.t.Helper()
	return e.do("GET", path, "", nil, inFrame...)
}

func enterPath(code string, pluginID int64) string {
	return "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(pluginID, 10)
}

var sessionAttr = regexp.MustCompile(`data-session="(mmo_[A-Za-z0-9_-]{43})"`)

// enter opens the app as muster's frame does, for p, and returns the
// session.
func (e *env) enter(p mustertest.Person) string {
	e.t.Helper()
	resp, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, p.ID, ""), app.PluginID))
	m := sessionAttr.FindSubmatch(body)
	if resp.StatusCode != 200 || m == nil {
		e.t.Fatalf("enter as %s = %d %s", p.DisplayName, resp.StatusCode, body)
	}
	return string(m[1])
}

func (e *env) subject(p mustertest.Person) string { return e.fake.Subject(app.PluginID, p.ID) }

type created struct {
	ID      string `json:"id"`
	JoinURL string `json:"join_url"`
}

// create is New meeting as the owner app sends it.
func (e *env) create(sess string) created {
	e.t.Helper()
	resp, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
	var c created
	if err := json.Unmarshal(raw, &c); err != nil || resp.StatusCode != 201 {
		e.t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	return c
}

func (e *env) sessions() int {
	e.t.Helper()
	n, err := e.store.CountSessions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// meetingState is the state of the person's meeting id; subject is theirs
// (taken before an uninstall, which deletes the fake's grants).
func (e *env) meetingState(subject, id string) string {
	e.t.Helper()
	m, err := e.store.ForSubject(subject).Meeting(context.Background(), id)
	if err != nil {
		e.t.Fatalf("meeting %s of %s: %v", id, subject, err)
	}
	return m.State
}

func (e *env) redeems() int { return e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem") }

// ---- /enter: the redeem ----

func TestEnterRedeemsAnAppCodeAndRendersTheAppWithABearerSession(t *testing.T) {
	e := newEnv(t)
	resp, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, bea.ID, ""), app.PluginID))
	if resp.StatusCode != 200 {
		t.Fatalf("enter = %d %s", resp.StatusCode, body)
	}
	m := sessionAttr.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no session in the page: %s", body)
	}
	// The redeem went to muster with the one server token.
	if calls := e.fake.Calls(); calls[len(calls)-1] != "POST /plugin/v1/handoffs/redeem "+app.Token {
		t.Fatalf("last muster call = %q", calls[len(calls)-1])
	}
	// The session is Bea's: her subject's meetings, made as her.
	resp, raw := e.do("POST", "/api/meetings", string(m[1]), map[string]any{})
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	list, _, _ := e.store.ForSubject(e.subject(bea)).ListMeetings(context.Background(), nil, 10)
	if len(list) != 1 {
		t.Fatalf("Bea has %d meetings, want the one just made", len(list))
	}
}

// 🔴 The sign-in CSRF gate (design: "/enter refuses unless Sec-Fetch-Dest:
// iframe"). With one server token, a link to /enter carrying someone else's
// unspent code would sign its opener in as that someone. Every load that is
// not a frame's is refused BEFORE muster is asked, so the code is not spent:
// the same code then still works from the frame it was minted for.
func TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem(t *testing.T) {
	cases := []struct {
		name string
		hdr  []string
		msg  string
	}{
		{"Sec-Fetch-Dest document", []string{"Sec-Fetch-Dest", "document", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest empty", []string{"Sec-Fetch-Dest", "empty", "Sec-Fetch-Mode", "cors", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest frame", []string{"Sec-Fetch-Dest", "frame", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest IFRAME", []string{"Sec-Fetch-Dest", "IFRAME", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"Sec-Fetch-Dest twice", []string{"Sec-Fetch-Dest", "iframe", "Sec-Fetch-Dest", "document", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"no Sec-Fetch-Dest beside other Sec-Fetch headers", []string{"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Site", "cross-site"}, server.EnterNotFramed},
		{"no Sec-Fetch at all", nil, server.EnterOldBrowser},
	}
	for _, c := range cases {
		e := newEnv(t)
		code := e.fake.MintApp(app.PluginID, ada.ID, "")
		resp, body := e.do("GET", enterPath(code, app.PluginID), "", nil, c.hdr...)
		if resp.StatusCode == 200 || sessionAttr.Match(body) {
			t.Errorf("%s: enter = %d (sessions %d): a session was made outside a frame", c.name, resp.StatusCode, e.sessions())
			continue
		}
		if n := e.redeems(); n != 0 {
			t.Errorf("%s: refused, but made %d redeem call: the code was spent", c.name, n)
		}
		if resp.StatusCode != 403 || !strings.Contains(string(body), c.msg) {
			t.Errorf("%s: the page says %d %s; want 403 %q", c.name, resp.StatusCode, body, c.msg)
		}
		if directive(resp.Header.Get("Content-Security-Policy"), "frame-ancestors") != musterOrigin {
			t.Errorf("%s: the refusal lost frame-ancestors", c.name)
		}
		// Positive control: the same code, untouched, opens the app from
		// muster's frame.
		if resp, body := e.framed(enterPath(code, app.PluginID)); resp.StatusCode != 200 || !sessionAttr.Match(body) {
			t.Errorf("%s: the frame's own load of the same code = %d %s", c.name, resp.StatusCode, body)
		}
	}
}

// 🔴 "/enter accepts only purpose app, an app target and scope: server with
// an owner.subject". Each fixture breaks exactly one, so each check is
// reached on its own.
func TestEnterRefusesAnythingButAServerAppCodeNamingASubject(t *testing.T) {
	appT := map[string]any{"app": map[string]any{}}
	mirror := map[string]any{"mirror": map[string]any{"host": "ws", "session": "main", "window": "@3"}}
	both := map[string]any{"app": map[string]any{}, "mirror": map[string]any{"host": "ws", "session": "main", "window": "@3"}}
	subj := "Zq9wE1rT5yU3iO7pA2sD4f" // 22 base64url characters, not drawn by the fake
	owner := map[string]any{"subject": subj, "display_name": "Cy"}
	cases := map[string]map[string]any{
		"purpose view, target app":   {"owner": owner, "scope": "server", "purpose": "view", "target": appT},
		"purpose empty, target app":  {"owner": owner, "scope": "server", "purpose": "", "target": appT},
		"purpose app, target mirror": {"owner": owner, "scope": "server", "purpose": "app", "target": mirror},
		"purpose app, both targets":  {"owner": owner, "scope": "server", "purpose": "app", "target": both},
		"purpose app, no target":     {"owner": owner, "scope": "server", "purpose": "app", "target": map[string]any{}},
		"scope person":               {"owner": owner, "scope": "person", "purpose": "app", "target": appT},
		"no scope":                   {"owner": owner, "purpose": "app", "target": appT},
		// A personal token's answer: a user id, no subject.
		"a personal answer": {"owner_id": ada.ID, "purpose": "app", "target": appT},
		"subject missing":   {"owner": map[string]any{"display_name": "Cy"}, "scope": "server", "purpose": "app", "target": appT},
		"subject a user id": {"owner": map[string]any{"subject": strconv.FormatInt(ada.ID, 10), "display_name": "Cy"}, "scope": "server", "purpose": "app", "target": appT},
		"subject 21 chars":  {"owner": map[string]any{"subject": subj[:21], "display_name": "Cy"}, "scope": "server", "purpose": "app", "target": appT},
		"subject an email":  {"owner": map[string]any{"subject": "ada.lovelace@example.t", "display_name": "Cy"}, "scope": "server", "purpose": "app", "target": appT},
	}
	for name, answer := range cases {
		e := newEnv(t)
		resp, body := e.framed(enterPath(e.fake.MintRaw(app.PluginID, answer), app.PluginID))
		if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterWrongKind) {
			t.Errorf("%s: enter = %d %s; want 403 %q", name, resp.StatusCode, body, server.EnterWrongKind)
		}
		if sessionAttr.Match(body) || e.sessions() != 0 {
			t.Errorf("%s: a session was made (%d)", name, e.sessions())
		}
	}
	// Positive control: the same harness with the right answer admits.
	e := newEnv(t)
	resp, body := e.framed(enterPath(e.fake.MintRaw(app.PluginID, map[string]any{"owner": owner, "scope": "server", "purpose": "app", "target": appT}), app.PluginID))
	if resp.StatusCode != 200 || !sessionAttr.Match(body) || e.sessions() != 1 {
		t.Fatalf("positive control: enter = %d, sessions %d", resp.StatusCode, e.sessions())
	}
}

// muster_plugin no longer picks a token: it must be this app's own id, and a
// code redeems only with the app it was minted for.
func TestEnterRedeemsOnlyForThisServerApp(t *testing.T) {
	e := newEnv(t)
	// Another plugin id in the frame's URL never reaches muster, whatever
	// the code.
	resp, body := e.framed(enterPath(e.fake.MintApp(other.PluginID, ada.ID, ""), other.PluginID))
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterNotSetUp) || sessionAttr.Match(body) {
		t.Fatalf("another plugin id = %d %s", resp.StatusCode, body)
	}
	if n := e.redeems(); n != 0 {
		t.Fatalf("another plugin id: %d redeem calls, want none", n)
	}
	// Another app's code presented as this app's: muster's one 404.
	resp, body = e.framed(enterPath(e.fake.MintApp(other.PluginID, ada.ID, ""), app.PluginID))
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterRefused) || e.sessions() != 0 {
		t.Fatalf("another app's code = %d %s (sessions %d); want 403 %q", resp.StatusCode, body, e.sessions(), server.EnterRefused)
	}
	// A code is single use: a reload of the same URL is refused.
	code := e.fake.MintApp(app.PluginID, bea.ID, "")
	if resp, _ := e.framed(enterPath(code, app.PluginID)); resp.StatusCode != 200 {
		t.Fatalf("first use = %d", resp.StatusCode)
	}
	if resp, body := e.framed(enterPath(code, app.PluginID)); resp.StatusCode != 403 || sessionAttr.Match(body) {
		t.Fatalf("second use = %d", resp.StatusCode)
	}
	// So is an expired one (muster's 60 s).
	code = e.fake.MintApp(app.PluginID, bea.ID, "")
	e.clock.Add(61 * time.Second)
	if resp, _ := e.framed(enterPath(code, app.PluginID)); resp.StatusCode != 403 {
		t.Fatalf("an expired code = %d", resp.StatusCode)
	}
}

func TestEnterWithoutACodeSaysOpenItFromMusterAgain(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/enter", "/enter?muster_plugin=12", "/enter?muster_code=short&muster_plugin=12",
		"/enter?muster_code=" + strings.Repeat("a", 43) + "&muster_plugin=x", "/enter?muster_code=" + strings.Repeat("a", 43) + "&muster_plugin=-12"} {
		resp, body := e.framed(p)
		if resp.StatusCode != 400 || !strings.Contains(string(body), server.EnterAgain) || sessionAttr.Match(body) {
			t.Errorf("GET %s = %d %s", p, resp.StatusCode, body)
		}
		if directive(resp.Header.Get("Content-Security-Policy"), "frame-ancestors") != musterOrigin {
			t.Errorf("GET %s: the refusal cannot be shown in muster's frame", p)
		}
	}
	if n := e.redeems(); n != 0 {
		t.Fatalf("a malformed request reached muster %d times", n)
	}
}

// Fail closed: a muster that is down, or answers 5xx, admits nobody.
func TestEnterFailsClosedWhenMusterIsDownOrFailing(t *testing.T) {
	e := newEnv(t)
	code := e.fake.MintApp(app.PluginID, ada.ID, "")
	e.fake.SetFailing(true)
	resp, body := e.framed(enterPath(code, app.PluginID))
	if resp.StatusCode != 502 || !strings.Contains(string(body), server.EnterNoMuster) || sessionAttr.Match(body) || e.sessions() != 0 {
		t.Fatalf("muster failing: enter = %d %s (sessions %d); want 502", resp.StatusCode, body, e.sessions())
	}
	e.fake.SetFailing(false)
	e.fake.Close()
	resp, body = e.framed(enterPath(code, app.PluginID))
	if resp.StatusCode != 502 || sessionAttr.Match(body) || e.sessions() != 0 {
		t.Fatalf("muster down: enter = %d %s (sessions %d); want 502", resp.StatusCode, body, e.sessions())
	}
	if !e.app.Live() {
		t.Fatal("a muster outage revoked the token")
	}
}

// muster's 401 on the redeem is the token dying (uninstalled, or rotated
// away): everything ends at once, for everyone.
func TestEnterThatMusterAnswers401EndsEverything(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	code := e.fake.MintApp(app.PluginID, ada.ID, "")
	e.fake.Rotate(app.PluginID, "mpl_server_rotated_4c2e")
	resp, _ := e.framed(enterPath(code, app.PluginID))
	if resp.StatusCode != 403 {
		t.Fatalf("enter with a rotated-away token = %d", resp.StatusCode)
	}
	for name, s := range map[string]string{"Ada": sa, "Bea": sb} {
		if resp, _ := e.do("GET", "/api/meetings", s, nil); resp.StatusCode != 401 {
			t.Errorf("%s's session after the 401 = %d", name, resp.StatusCode)
		}
	}
	if n := e.sessions(); n != 0 {
		t.Fatalf("%d sessions left", n)
	}
}

// The ref is a hint: passed to the page only when it is one of this
// person's own meetings.
func TestEnterPassesTheRefOnlyForTheOwnersOwnMeeting(t *testing.T) {
	e := newEnv(t)
	ownID, otherID := e.create(e.enter(ada)).ID, e.create(e.enter(bea)).ID
	_, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ownID), app.PluginID))
	if !strings.Contains(string(body), `data-ref="`+ownID+`"`) {
		t.Fatalf("the person's own ref was dropped: %s", body)
	}
	_, body = e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, otherID), app.PluginID))
	if strings.Contains(string(body), "data-ref") || !sessionAttr.Match(body) {
		t.Fatalf("another person's ref reached the page: %s", body)
	}
}

// 🔴 The owner app's response on the plugin's own origin: framed by muster's
// exact origin and nothing else, no referrer, not cached, and no cookie.
func TestEnterHeadersFrameAncestorsIsMustersOriginAndNoCookieIsSet(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
	h := resp.Header
	csp := h.Get("Content-Security-Policy")
	if got := directive(csp, "frame-ancestors"); got != musterOrigin {
		t.Fatalf("frame-ancestors = %q, want exactly %q (CSP %q)", got, musterOrigin, csp)
	}
	for _, d := range []string{"script-src 'self'", "connect-src 'self'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("CSP lacks %q: %q", d, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "*") {
		t.Errorf("CSP is wide: %q", csp)
	}
	if h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", h.Get("Referrer-Policy"))
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", h.Get("Cache-Control"))
	}
	if h.Get("X-Frame-Options") != "" {
		t.Errorf("X-Frame-Options = %q: muster could not frame the app", h.Get("X-Frame-Options"))
	}
	if h.Get("Permissions-Policy") != "camera=(self), microphone=(self), display-capture=(self)" {
		t.Errorf("Permissions-Policy = %q", h.Get("Permissions-Policy"))
	}
	if sc := h.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("enter set a cookie: %q", sc)
	}
}

func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if v, ok := strings.CutPrefix(d, name+" "); ok {
			return v
		}
	}
	return ""
}

// ---- owner sessions: bearer only ----

func TestOwnerRoutesRefuseAnythingButALiveBearerSession(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	bad := map[string][]string{
		"no credential":        nil,
		"the token as cookie":  {"Cookie", "session=" + sess},
		"Basic":                {"Authorization", "Basic " + sess},
		"bare token":           {"Authorization", sess},
		"another prefix":       {"Authorization", "Bearer mpl_" + sess[4:]},
		"unknown":              {"Authorization", "Bearer " + store.NewSessionToken()},
		"its hash":             {"Authorization", "Bearer " + store.Hash(sess)},
		"truncated":            {"Authorization", "Bearer " + sess[:len(sess)-1]},
		"the muster token":     {"Authorization", "Bearer " + app.Token},
		"the subject":          {"Authorization", "Bearer " + e.subject(ada)},
		"two spaces":           {"Authorization", "Bearer  " + sess},
		"lower-case scheme...": {"Authorization", "bearer " + sess},
	}
	for name, hdr := range bad {
		for _, route := range [][2]string{{"GET", "/api/meetings"}, {"POST", "/api/meetings"}, {"GET", "/api/meetings/" + store.NewMeetingID()}} {
			var body any
			if route[0] == "POST" {
				body = map[string]any{}
			}
			resp, raw := e.do(route[0], route[1], "", body, hdr...)
			if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s with %s = %d %s; want 401", route[0], route[1], name, resp.StatusCode, raw)
			}
		}
	}
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 200 {
		t.Fatalf("positive control: the live session = %d", resp.StatusCode)
	}
}

// 🔴 The revocation race: a New meeting admitted just before muster said
// the app may no longer act (for anyone, or for this person), whose insert
// runs just after it, must not leave an open meeting. The hook holds the
// request between its admission and its insert while the revocation runs to
// the end, deterministically.
func TestANewMeetingRacingARevocationLeavesNoOpenMeeting(t *testing.T) {
	for name, revoke := range map[string]func(e *env){
		"uninstall":   func(e *env) { e.fake.Uninstall(app.PluginID) },
		"Hide by Ada": func(e *env) { e.fake.Hide(app.PluginID, ada.ID) },
	} {
		e := newEnv(t)
		sess := e.enter(ada)
		reached, release := make(chan struct{}), make(chan struct{})
		server.SetBeforeCreateHookForTest(t, func() {
			close(reached)
			<-release
		})
		type result struct {
			status int
			body   string
		}
		done := make(chan result, 1)
		go func() {
			resp, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
			done <- result{resp.StatusCode, string(raw)}
		}()
		<-reached // admitted: the grant was live a moment ago
		revoke(e)
		e.app.PollOnce(context.Background()) // the whole revocation, while the create waits
		server.SetBeforeCreateHookForTest(t, nil)
		close(release)
		r := <-done

		list, _, _ := e.store.ForSubject(e.subject(ada)).ListMeetings(context.Background(), nil, 10)
		for _, m := range list {
			if m.State == "open" {
				t.Errorf("%s: an open meeting after the revocation: %+v (create answered %d %s)", name, m, r.status, r.body)
			}
		}
		if r.status != 401 {
			t.Errorf("%s: the raced create = %d %s, want 401", name, r.status, r.body)
		}
	}
}

// Once muster refused the token, the app refuses every owner request at once,
// even a session whose row exists (one made after the uninstall's delete,
// here): the app's liveness is checked on every request, not only by the
// poll's delete.
func TestASessionIsRefusedOnceTheAppIsDeadEvenIfItsRowSurvives(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.enter(ada)
	subA := e.subject(ada)
	before, _, err := e.store.CreateSession(ctx, subA, "Ada L.", t0)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do("GET", "/api/meetings", before, nil); resp.StatusCode != 200 {
		t.Fatalf("positive control: a session row while the app is live = %d", resp.StatusCode)
	}
	e.fake.Uninstall(app.PluginID)
	e.app.PollOnce(ctx)
	after, _, err := e.store.CreateSession(ctx, subA, "Ada L.", t0)
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.do("GET", "/api/meetings", after, nil); resp.StatusCode != 401 {
		t.Fatalf("a session made after the uninstall = %d, want 401", resp.StatusCode)
	}
}

// Eight hours (decision 12), by the server's clock.
func TestAnOwnerSessionEndsAfterEightHours(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	e.clock.Add(8*time.Hour - time.Second)
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 200 {
		t.Fatalf("one second before 8 h = %d", resp.StatusCode)
	}
	e.clock.Add(time.Second)
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 401 {
		t.Fatalf("at 8 h = %d, want 401", resp.StatusCode)
	}
}

// ---- liveness: the /self poll and owners/check ----

// 🔴 Uninstall ends everything: /self's 401 within one poll deletes every
// person's sessions and ends every open meeting, and /enter refuses without
// asking muster.
func TestUninstallEndsEverySessionAndMeeting(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	subA, subB := e.subject(ada), e.subject(bea)
	ma, mb := e.create(sa), e.create(sb)

	e.app.PollOnce(context.Background()) // both live: nothing changes
	if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 200 {
		t.Fatalf("positive control: Ada's session after a live poll = %d", resp.StatusCode)
	}

	e.fake.Uninstall(app.PluginID)
	e.app.PollOnce(context.Background())

	for name, s := range map[string]string{"Ada": sa, "Bea": sb} {
		if resp, _ := e.do("GET", "/api/meetings", s, nil); resp.StatusCode != 401 {
			t.Errorf("%s's session after the uninstall poll = %d, want 401", name, resp.StatusCode)
		}
	}
	if n := e.sessions(); n != 0 {
		t.Errorf("%d sessions left in the database after the uninstall, want 0", n)
	}
	if st := e.meetingState(subA, ma.ID); st != "ended" {
		t.Errorf("Ada's meeting after the uninstall = %q, want ended", st)
	}
	if st := e.meetingState(subB, mb.ID); st != "ended" {
		t.Errorf("Bea's meeting after the uninstall = %q, want ended", st)
	}
	before := e.redeems()
	resp, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterNotSetUp) || e.redeems() != before {
		t.Fatalf("enter after the uninstall = %d %s (%d new redeems)", resp.StatusCode, body, e.redeems()-before)
	}
}

// 🔴 A person owners/check answers "revoked" (each reason alone) loses their
// sessions and open meetings within one poll; the other person keeps theirs.
func TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse(t *testing.T) {
	for name, end := range map[string]func(e *env){
		"hidden":                func(e *env) { e.fake.Hide(app.PluginID, ada.ID) },
		"disabled":              func(e *env) { e.fake.Disable(ada.ID) },
		"signed out everywhere": func(e *env) { e.clock.Add(3 * time.Second); e.fake.SignOutEverywhere(ada.ID) },
		// Expired and then hidden: muster answers revoked, so both end.
		"expired, then hidden": func(e *env) { e.fake.Expire(app.PluginID, ada.ID); e.fake.Hide(app.PluginID, ada.ID) },
		// An expired grant alone keeps the meetings:
		// TestAnExpiredGrantEndsOnlyTheOwnersSessionsAndKeepsTheirMeetingsOpen.
		// The app uninstalled: TestUninstallEndsEverySessionAndMeeting.
	} {
		e := newEnv(t)
		sa := e.enter(ada)
		ma := e.create(sa)
		e.clock.Add(time.Minute)
		sb := e.enter(bea) // Bea's grant runs a minute past Ada's
		mb := e.create(sb)

		end(e)
		e.app.PollOnce(context.Background())

		if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 401 {
			t.Errorf("%s: Ada's session after the poll = %d, want 401", name, resp.StatusCode)
		}
		if st := e.meetingState(e.subject(ada), ma.ID); st != "ended" {
			t.Errorf("%s: Ada's meeting after the poll = %q, want ended", name, st)
		}
		if st := e.meetingState(e.subject(bea), mb.ID); st != "open" {
			t.Errorf("%s: Bea's meeting after Ada's end = %q, want open", name, st)
		}
		if resp, _ := e.do("GET", "/api/meetings", sb, nil); resp.StatusCode != 200 {
			t.Errorf("%s: Bea's session after Ada's end = %d, want 200", name, resp.StatusCode)
		}
		if !e.app.Live() {
			t.Errorf("%s: one person's end revoked the app", name)
		}
	}
}

// "expired" (8 hours since the person last opened the tab, nothing else)
// ends only their owner sessions: their open meeting keeps running for the
// guests, and when they reopen the tab they find it still open, with its
// link. The other person is untouched.
func TestAnExpiredGrantEndsOnlyTheOwnersSessionsAndKeepsTheirMeetingsOpen(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	ma, mb := e.create(sa), e.create(sb)
	e.fake.Expire(app.PluginID, ada.ID)
	e.app.PollOnce(context.Background())

	if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 401 {
		t.Errorf("expired: Ada's session after the poll = %d, want 401", resp.StatusCode)
	}
	if st := e.meetingState(e.subject(ada), ma.ID); st != "open" {
		t.Errorf("expired: Ada's meeting after the poll = %q, want open", st)
	}
	if resp, _ := e.do("GET", "/api/meetings", sb, nil); resp.StatusCode != 200 {
		t.Errorf("expired: Bea's session after Ada's expiry = %d, want 200", resp.StatusCode)
	}
	if st := e.meetingState(e.subject(bea), mb.ID); st != "open" {
		t.Errorf("expired: Bea's meeting = %q", st)
	}
	// Ada reopens the tab: a fresh session, and the meeting is still open
	// with its link; the next poll keeps it.
	sa = e.enter(ada)
	e.app.PollOnce(context.Background())
	if st, link, body := e.meetingLink(sa, ma.ID); st != 200 || link == nil || *link != ma.JoinURL || !strings.Contains(body, `"state":"open"`) {
		t.Fatalf("expired: Ada reopening the tab = %d %s; want her open meeting and its link", st, body)
	}
}

// 🔴 A held subject owners/check does not answer at all (one muster never
// gave this app, say) is treated as revoked: fail closed.
func TestASubjectOwnersCheckDoesNotAnswerIsTreatedAsRevoked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	gone := "Gq3Ws8Ed1Rf6Tg0Yh5Uj2k" // never drawn by the fake
	tok, _, err := e.store.CreateSession(ctx, gone, "Old", t0)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := e.store.LookupSession(ctx, tok, t0)
	m, err := e.store.ForSession(sess).CreateMeeting(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	sa := e.enter(ada)
	e.app.PollOnce(ctx)
	if st := e.meetingState(gone, m.Meeting.ID); st != "ended" {
		t.Errorf("absent: the meeting after the poll = %q, want ended", st)
	}
	if resp, _ := e.do("GET", "/api/meetings", tok, nil); resp.StatusCode != 401 {
		t.Errorf("absent: the session after the poll = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 200 {
		t.Errorf("absent: Ada (live) after the poll = %d, want 200", resp.StatusCode)
	}
}

// 🔴 The reopen race, deterministically: owners/check decides Ada's state
// and, before its answer reaches the plugin, Ada reopens the tab (a fresh
// redeem, a fresh session). The answer is about what existed when the check
// was sent, so it ends her old session (and, for revoked, the meeting she
// had then) and keeps the fresh session.
func TestAPersonWhoReopensTheTabWhileTheCheckIsOnItsWayKeepsTheFreshSession(t *testing.T) {
	for name, end := range map[string]func(e *env){
		"revoked": func(e *env) { e.fake.Hide(app.PluginID, ada.ID) },
		"expired": func(e *env) { e.fake.Expire(app.PluginID, ada.ID) },
	} {
		e := newEnv(t)
		old := e.enter(ada)
		m := e.create(old)
		end(e)
		var fresh []byte
		var freshStatus int
		e.fake.SetOwnersCheck(0, "", func() {
			e.clock.Add(time.Second)
			e.fake.Show(app.PluginID, ada.ID)
			e.fake.SignIn(ada.ID)
			resp, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
			freshStatus, fresh = resp.StatusCode, body
		})
		e.app.PollOnce(context.Background())
		e.fake.SetOwnersCheck(0, "", nil)
		mm := sessionAttr.FindSubmatch(fresh)
		if freshStatus != 200 || mm == nil {
			t.Fatalf("%s: the reopened tab = %d %s", name, freshStatus, fresh)
		}
		if resp, _ := e.do("GET", "/api/meetings", string(mm[1]), nil); resp.StatusCode != 200 {
			t.Errorf("%s: the session made after the check was sent = %d, want 200", name, resp.StatusCode)
		}
		if resp, _ := e.do("GET", "/api/meetings", old, nil); resp.StatusCode != 401 {
			t.Errorf("%s: the session from before the check = %d, want 401", name, resp.StatusCode)
		}
		want := map[string]string{"revoked": "ended", "expired": "open"}[name]
		if st := e.meetingState(e.subject(ada), m.ID); st != want {
			t.Errorf("%s: the meeting from before the check = %q, want %q", name, st, want)
		}
	}
}

// Only muster's answer ends a person or the app: an owners/check that fails,
// a /self that fails, and plugins off on that muster each keep everything.
func TestALivenessPollThatFailsEndsNoOne(t *testing.T) {
	for name, fail := range map[string]func(e *env){
		"owners/check 503": func(e *env) { e.fake.SetFailOwnersCheck(true) },
		"/self 503":        func(e *env) { e.fake.SetFailing(true) },
		"no muster":        func(e *env) { e.fake.Close() },
		// A 2xx with no states object is not an answer.
		"owners/check the old live list": func(e *env) { e.fake.SetOwnersCheck(0, `{"live":[]}`, nil) },
		"owners/check {}":                func(e *env) { e.fake.SetOwnersCheck(0, `{}`, nil) },
		"owners/check states null":       func(e *env) { e.fake.SetOwnersCheck(0, `{"states":null}`, nil) },
	} {
		e := newEnv(t)
		sa := e.enter(ada)
		ma := e.create(sa)
		e.fake.Hide(app.PluginID, ada.ID) // were muster asked, Ada would end
		fail(e)
		e.app.PollOnce(context.Background())
		if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 200 {
			t.Errorf("%s: Ada's session = %d, want 200 (no answer ends no one)", name, resp.StatusCode)
		}
		if st := e.meetingState(e.subject(ada), ma.ID); st != "open" {
			t.Errorf("%s: Ada's meeting = %q, want open", name, st)
		}
		if !e.app.Live() {
			t.Errorf("%s: the app was revoked without muster's 401", name)
		}
	}
}

// ---- another person's meetings are never visible ----

func TestAnotherPersonsMeetingsAreNeverVisible(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	ids := []string{e.create(sa).ID, e.create(sa).ID}
	cb := e.create(sb)

	_, raw := e.do("GET", "/api/meetings", sb, nil)
	var list struct {
		Meetings []struct{ ID string }
	}
	json.Unmarshal(raw, &list)
	if len(list.Meetings) != 1 || list.Meetings[0].ID != cb.ID || strings.Contains(string(raw), ids[0]) || strings.Contains(string(raw), ids[1]) {
		t.Fatalf("B's list = %s; want B's one meeting only", raw)
	}
	_, unknown := e.do("GET", "/api/meetings/"+store.NewMeetingID(), sb, nil)
	for _, id := range ids {
		resp, raw := e.do("GET", "/api/meetings/"+id, sb, nil)
		if resp.StatusCode != 404 || !bytes.Equal(raw, unknown) {
			t.Fatalf("B reading A's %s = %d %s; want the same 404 as an unknown id (%s)", id, resp.StatusCode, raw, unknown)
		}
	}
	// Positive control: Ada reads her own.
	if resp, raw := e.do("GET", "/api/meetings/"+ids[0], sa, nil); resp.StatusCode != 200 || !strings.Contains(string(raw), ids[0]) {
		t.Fatalf("A reading her own = %d %s", resp.StatusCode, raw)
	}
}

// ---- nothing of muster's about a person but the subject and display name ----

// 🔴 No muster user id and no email is ever stored. An invariant guard: the
// plugin's client has no field for either, so the plugin never receives them
// from a muster that follows the design. The scan also covers a muster that
// misbehaves and sends both anyway, beside the subject. Every write path
// runs first (sessions, meetings, a person's end, the sweep, the audit), and
// the database file and its WAL are read as bytes.
func TestNoMusterUserIDOrEmailIsEverStored(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	e.create(sa)
	e.create(sb)
	cy := mustertest.Person{ID: 3141592653, Email: "cy.leaky@example.test", DisplayName: "Cy W."}
	leakySubject := "Lk7Yq2Rf8Hs1Jd4Ng6Pv0w"
	resp, body := e.framed(enterPath(e.fake.MintRaw(app.PluginID, map[string]any{
		"owner_id": cy.ID, "owner_login": cy.Email, "email": cy.Email,
		"owner": map[string]any{"subject": leakySubject, "display_name": cy.DisplayName, "id": cy.ID, "email": cy.Email},
		"scope": "server", "purpose": "app", "target": map[string]any{"app": map[string]any{}}}), app.PluginID))
	m := sessionAttr.FindSubmatch(body)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("the leaky answer = %d %s", resp.StatusCode, body)
	}
	e.create(string(m[1]))
	e.fake.Hide(app.PluginID, bea.ID)
	e.app.PollOnce(context.Background())
	e.clock.Add(24 * time.Hour)
	if _, err := e.store.Sweep(context.Background(), e.clock.Now()); err != nil {
		t.Fatal(err)
	}

	var all []byte
	for _, f := range []string{e.dbPath, e.dbPath + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	for _, p := range []mustertest.Person{ada, bea, cy} {
		if bytes.Contains(all, []byte(strconv.FormatInt(p.ID, 10))) {
			t.Errorf("the database holds %s's muster user id %d", p.DisplayName, p.ID)
		}
		if bytes.Contains(all, []byte(p.Email)) {
			t.Errorf("the database holds %s's email %s", p.DisplayName, p.Email)
		}
	}
	// Positive controls: the scan sees what IS stored, from the same redeems.
	for _, want := range []string{e.subject(ada), e.subject(bea), leakySubject, ada.DisplayName, bea.DisplayName, cy.DisplayName} {
		if !bytes.Contains(all, []byte(want)) {
			t.Errorf("positive control: %q is not in the database files, so the scan cannot see rows", want)
		}
	}

	// A user id stored as an INTEGER is a varint, invisible to the byte
	// scan: every INTEGER column of every table is also queried for the ids.
	ids := []int64{ada.ID, bea.ID, cy.ID}
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hits, cols := integerHits(t, db, ids)
	for _, h := range hits {
		t.Errorf("the database holds a muster user id as an integer: %s", h)
	}
	// The enumeration is not stale: it reaches every table's INTEGER columns.
	for _, want := range []string{"owners.updated_at", "owner_sessions.created_at", "owner_sessions.expires_at", "meetings.created_at", "meetings.ended_at", "audit.at"} {
		if !slices.Contains(cols, want) {
			t.Errorf("positive control: the integer scan does not reach %s (it reached %v)", want, cols)
		}
	}
	// And it can see one: an id written as an integer is caught.
	if _, err := db.Exec(`UPDATE audit SET at = ? WHERE rowid = (SELECT min(rowid) FROM audit)`, cy.ID); err != nil {
		t.Fatal(err)
	}
	if hits, _ := integerHits(t, db, ids); len(hits) != 1 || !strings.HasPrefix(hits[0], "audit.at") {
		t.Fatalf("positive control: an id stored as an integer in audit.at was not caught: %v", hits)
	}
}

// integerHits queries every INTEGER column of every table (enumerated from
// sqlite_master and pragma table_info) for each id, and returns
// "table.column = id" for each hit, and the columns it scanned.
func integerHits(t *testing.T, db *sql.DB, ids []int64) (hits, cols []string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tb := range tables {
		ci, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, tb)
		if err != nil {
			t.Fatal(err)
		}
		var ints []string
		for ci.Next() {
			var name, typ string
			ci.Scan(&name, &typ)
			if strings.Contains(strings.ToUpper(typ), "INT") {
				ints = append(ints, name)
			}
		}
		ci.Close()
		for _, c := range ints {
			cols = append(cols, tb+"."+c)
			for _, id := range ids {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM "`+tb+`" WHERE "`+c+`" = ?`, id).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					hits = append(hits, fmt.Sprintf("%s.%s = %d", tb, c, id))
				}
			}
		}
	}
	return hits, cols
}

// ---- the link, again, for its owner only, while open ----

func (e *env) meetingLink(sess, id string) (int, *string, string) {
	e.t.Helper()
	resp, raw := e.do("GET", "/api/meetings/"+id, sess, nil)
	var one struct {
		Link *string `json:"link"`
	}
	json.Unmarshal(raw, &one)
	return resp.StatusCode, one.Link, string(raw)
}

func TestTheLinkIsTheOwnersOnlyAndOnlyWhileOpen(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	c := e.create(sa)
	code := joinURL.FindStringSubmatch(c.JoinURL)[1]
	// Another person: the same 404 as any unknown meeting, no code anywhere.
	if st, link, body := e.meetingLink(sb, c.ID); st != 404 || link != nil || strings.Contains(body, code) {
		t.Fatalf("B reading A's meeting = %d %s", st, body)
	}
	if _, raw := e.do("GET", "/api/meetings", sb, nil); strings.Contains(string(raw), code) {
		t.Fatal("B's list names A's code")
	}
	// The owner, while open: the link.
	if st, link, _ := e.meetingLink(sa, c.ID); st != 200 || link == nil || *link != c.JoinURL {
		t.Fatalf("A's own open meeting: %d, link %v", st, link)
	}
	// At 24 h, before any sweep: no link (the 8 h session is gone, so the
	// owner enters again). The fake's session sign-in is renewed, as a
	// browser that stayed signed in has it.
	e.clock.Add(24 * time.Hour)
	sa = e.enter(ada)
	if st, link, body := e.meetingLink(sa, c.ID); st != 200 || link != nil || strings.Contains(body, code) {
		t.Fatalf("at 24 h = %d %s; want 200 with link null", st, body)
	}
}

func TestAnEndedMeetingAnswersNoLink(t *testing.T) {
	e := newEnv(t)
	sb := e.enter(bea)
	c := e.create(sb)
	e.clock.Add(24 * time.Hour)
	if _, err := e.store.Sweep(context.Background(), e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	sb = e.enter(bea)
	st, link, body := e.meetingLink(sb, c.ID)
	if st != 200 || link != nil || !strings.Contains(body, `"state":"ended"`) || strings.Contains(body, joinURL.FindStringSubmatch(c.JoinURL)[1]) {
		t.Fatalf("an ended meeting = %d %s; want 200, ended, link null", st, body)
	}
}

// ---- New meeting and its link ----

var joinURL = regexp.MustCompile(`^https://meet\.example\.test/j/([A-Za-z0-9_-]{22})$`)

func TestNewMeetingAnswersItsIDAndALinkOnThePluginsOrigin(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	resp, raw := e.do("POST", "/api/meetings", sess, nil)
	var c struct {
		ID      string `json:"id"`
		JoinURL string `json:"join_url"`
		Meeting struct {
			Title         string
			State         string
			Record, AI    bool
			CreatedAt     time.Time `json:"created_at"`
			LinkExpiresAt time.Time `json:"link_expires_at"`
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil || resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	m := joinURL.FindStringSubmatch(c.JoinURL)
	if m == nil {
		t.Fatalf("join_url = %q", c.JoinURL)
	}
	if !store.ValidMeetingID(c.ID) || c.Meeting.Title != "" || c.Meeting.State != "open" || !c.Meeting.Record || !c.Meeting.AI ||
		!c.Meeting.CreatedAt.Equal(t0) || !c.Meeting.LinkExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("created = %s", raw)
	}
	if _, raw := e.do("GET", "/api/meetings", sess, nil); strings.Contains(string(raw), m[1]) {
		t.Fatalf("the list names the join code: %s", raw)
	}
	var one struct {
		Link *string `json:"link"`
	}
	_, raw = e.do("GET", "/api/meetings/"+c.ID, sess, nil)
	if json.Unmarshal(raw, &one); one.Link == nil || *one.Link != c.JoinURL {
		t.Fatalf("GET one meeting's link = %s, want %q", raw, c.JoinURL)
	}
	if c2 := e.create(sess); c2.JoinURL == c.JoinURL || !joinURL.MatchString(c2.JoinURL) {
		t.Fatalf("second link = %q, first %q", c2.JoinURL, c.JoinURL)
	}
}

// New meeting takes no options (no title, no record switch): no body, or
// {}. Anything else is refused, so a client can't believe it set one.
func TestNewMeetingTakesNoOptions(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	e.enter(bea)
	for name, body := range map[string]string{
		"a title":         `{"title":"x"}`,
		"a record flag":   `{"record":false}`,
		"another subject": `{"subject":"` + e.subject(bea) + `"}`,
		"not JSON":        `title=x`,
		"too big":         `{` + strings.Repeat(" ", 5000) + `}`,
	} {
		resp, raw := e.do("POST", "/api/meetings", sess, body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, raw)
		}
	}
	for _, body := range []string{``, `{}`, ` {} `} {
		if resp, raw := e.do("POST", "/api/meetings", sess, body); resp.StatusCode != 201 {
			t.Errorf("body %q = %d %s", body, resp.StatusCode, raw)
		}
	}
	if list, _, _ := e.store.ForSubject(e.subject(bea)).ListMeetings(context.Background(), nil, 10); len(list) != 0 {
		t.Fatal("a body naming another subject made a meeting for them")
	}
}

// The API's shapes, pinned: one meeting is exactly {"meeting", "link"}, a
// list item never has a link, and a created meeting answers id and join_url.
func TestTheMeetingAPIShapes(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	c := e.create(sess)
	keys := func(raw []byte) []string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("not a JSON object: %s", raw)
		}
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	_, raw := e.do("GET", "/api/meetings/"+c.ID, sess, nil)
	if got := strings.Join(keys(raw), ","); got != "link,meeting" {
		t.Fatalf("one meeting's keys = %s (%s)", got, raw)
	}
	var one struct {
		Meeting map[string]json.RawMessage
		Link    *string
	}
	json.Unmarshal(raw, &one)
	if one.Link == nil || *one.Link != c.JoinURL {
		t.Fatalf("link = %v, want %q", one.Link, c.JoinURL)
	}
	if got := strings.Join(keys(mustJSON(t, one.Meeting)), ","); got != "ai,created_at,ended_at,id,link_expires_at,record,started_at,state,title" {
		t.Fatalf("meeting keys = %s", got)
	}
	_, raw = e.do("GET", "/api/meetings", sess, nil)
	var list struct{ Meetings []map[string]json.RawMessage }
	json.Unmarshal(raw, &list)
	if len(list.Meetings) != 1 {
		t.Fatalf("list = %s", raw)
	}
	if _, ok := list.Meetings[0]["link"]; ok || strings.Contains(string(raw), "/j/") {
		t.Fatalf("a list item carries a link: %s", raw)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheListIsNewestFirstAndPaged(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, e.create(sess).ID)
		e.clock.Add(time.Second)
	}
	type page struct {
		Meetings []struct{ ID string }
		Next     *string
	}
	var p1, p2 page
	_, raw := e.do("GET", "/api/meetings?limit=2", sess, nil)
	json.Unmarshal(raw, &p1)
	if len(p1.Meetings) != 2 || p1.Meetings[0].ID != ids[2] || p1.Meetings[1].ID != ids[1] || p1.Next == nil {
		t.Fatalf("page 1 = %s", raw)
	}
	_, raw = e.do("GET", "/api/meetings?limit=2&cursor="+url.QueryEscape(*p1.Next), sess, nil)
	json.Unmarshal(raw, &p2)
	if len(p2.Meetings) != 1 || p2.Meetings[0].ID != ids[0] || p2.Next != nil {
		t.Fatalf("page 2 = %s", raw)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=x", "?cursor=nope"} {
		if resp, _ := e.do("GET", "/api/meetings"+q, sess, nil); resp.StatusCode != 400 {
			t.Errorf("GET /api/meetings%s = %d", q, resp.StatusCode)
		}
	}
}

// ---- no cookie anywhere; logs carry no secret ----

func TestNoResponseSetsACookie(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	c := e.create(sess)
	for _, r := range [][3]string{
		{"GET", enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID), ""},
		{"GET", "/enter", ""},
		{"GET", "/api/meetings", sess},
		{"POST", "/api/meetings", sess},
		{"GET", "/api/meetings/" + c.ID, sess},
		{"GET", "/api/meetings", ""},
		{"GET", "/static/app.js", ""},
		{"GET", "/nothing-here", ""},
	} {
		var body any
		if r[0] == "POST" {
			body = map[string]any{}
		}
		resp, _ := e.do(r[0], r[1], r[2], body, inFrame...)
		if sc := resp.Header.Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("%s %s set a cookie: %q", r[0], r[1], sc)
		}
		if resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy %q", r[0], r[1], resp.Header.Get("Referrer-Policy"))
		}
	}
}

// Only the owner app may be framed, and only by muster.
func TestEveryOtherResponseRefusesToBeFramed(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	for _, p := range []string{"/api/meetings", "/static/app.js", "/nothing-here"} {
		resp, _ := e.do("GET", p, sess, nil)
		if fa := directive(resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); fa != "'none'" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s: frame-ancestors %q, X-Frame-Options %q", p, fa, resp.Header.Get("X-Frame-Options"))
		}
	}
}

func TestTheLogCarriesNoCodeTokenOrQuery(t *testing.T) {
	e := newEnv(t)
	code := e.fake.MintApp(app.PluginID, ada.ID, "")
	_, body := e.framed(enterPath(code, app.PluginID))
	sess := string(sessionAttr.FindSubmatch(body)[1])
	c := e.create(sess)
	join := joinURL.FindStringSubmatch(c.JoinURL)[1]
	e.do("GET", "/api/meetings", sess, nil)
	e.do("GET", "/api/meetings/"+c.ID, sess, nil)
	e.do("GET", "/j/"+join, "", nil)
	e.do("GET", "/j/"+join+"/wait", "", nil)
	// A refused enter logs its reason: still no code.
	e.framed(enterPath(code, app.PluginID))
	e.fake.Hide(app.PluginID, ada.ID)
	e.app.PollOnce(context.Background())
	got := e.log.String()
	// Positive controls: the lines are there to be searched. Each is
	// independent of the redaction under test, so a broken redaction fails
	// below, on the secret, not here.
	for _, want := range []string{"http GET /enter", "http POST /api/meetings 201", "http GET /j/", "/wait 404", "enter: refused 403", "muster revoked the app for 1 of 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log lacks %q (so this scan proves nothing):\n%s", want, got)
		}
	}
	for name, secret := range map[string]string{"hand-off code": code, "session": sess[4:], "join code": join, "muster token": app.Token,
		"query": "muster_code", "subject": e.subject(ada), "display name": ada.DisplayName} {
		if strings.Contains(got, secret) {
			t.Errorf("the log carries the %s:\n%s", name, got)
		}
	}
}

func TestRedactPath(t *testing.T) {
	for in, want := range map[string]string{
		"/j/AbC_-123":         "/j/{code}",
		"/j/AbC_-123/knock":   "/j/{code}/knock",
		"//j/AbC_-123":        "/j/{code}",
		"/./j/AbC_-123":       "/j/{code}",
		"/x/../j/AbC_-123":    "/j/{code}",
		"/x/../j/AbC_-123/a":  "/j/{code}/a",
		"/j/":                 "/{redacted}",
		"/api/j/AbC_-123":     "/{redacted}",
		"/static/../../j/AbC": "/j/{code}",
		"/api/meetings":       "/api/meetings",
		"/jx/abc":             "/jx/abc",
		"/enter":              "/enter",
	} {
		if got := server.RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheAppPageCarriesNoInlineScript(t *testing.T) {
	e := newEnv(t)
	_, body := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
	if !sessionAttr.Match(body) {
		t.Fatalf("not the app page: %s", body)
	}
	c := e.meeting(bea)
	_, guest := e.do("GET", "/j/"+c.Code, "", nil)
	if !bytes.Contains(guest, []byte(`id="knock-form"`)) {
		t.Fatalf("not the guest page: %s", guest)
	}
	for name, page := range map[string][]byte{"app": body, "guest": guest} {
		scripts := regexp.MustCompile(`<script[^>]*>([^<]*)</script>`).FindAllSubmatch(page, -1)
		if len(scripts) != 6 {
			t.Errorf("%s page: %d scripts, want the kit's kit.js, ui.js, prefs.js, the LiveKit client, call.js and its own", name, len(scripts))
		}
		// The UI kit, from this origin's /kit/v1/ (kit.go), first.
		// (Each URL carries its version: assets_test.go.)
		if len(scripts) == 0 || !regexp.MustCompile(`^<script src="/kit/v1/kit\.js\?v=[0-9a-f]{16}"></script>$`).Match(scripts[0][0]) {
			t.Errorf("%s page: the first script is not /kit/v1/kit.js", name)
		}
		kitCSS := `<link rel="stylesheet" href="/kit/v1/kit.css?v=`
		if !bytes.Contains(page, []byte(kitCSS)) || bytes.Index(page, []byte(kitCSS)) > bytes.Index(page, []byte(`href="/static/app.css?v=`)) {
			t.Errorf("%s page: kit.css is not linked from /kit/v1/ before app.css", name)
		}
		for _, m := range scripts {
			if !regexp.MustCompile(`^<script src="/(static|kit/v1)/[a-z0-9./-]+\.js\?v=[0-9a-f]{16}">$`).Match(m[0][:len(m[0])-len(m[1])-len("</script>")]) || len(bytes.TrimSpace(m[1])) != 0 {
				t.Errorf("%s page: script %s is not a file on this origin with no inline body: the CSP would block it", name, m[0])
			}
		}
		if regexp.MustCompile(`\son[a-z]+=`).Match(page) {
			t.Errorf("%s page: an inline event handler: the CSP would block it", name)
		}
	}
	resp, js := e.do("GET", "/static/app.js", "", nil)
	if resp.StatusCode != 200 || !bytes.Contains(js, []byte("removeAttribute('data-session')")) || !bytes.Contains(js, []byte("history.replaceState")) {
		t.Fatalf("app.js = %d", resp.StatusCode)
	}
	for _, f := range []string{"/static/app.js", "/static/guest.js", "/static/call.js", "/static/ui.js"} {
		_, js := e.do("GET", f, "", nil)
		// No script parses HTML: a guest's name and what anyone SAID (the
		// transcript) are rendered with textContent only. (A word check;
		// the behavioural one is e2e/transcript.spec.ts, whose scripted
		// line is markup and must come out as text.)
		for _, bad := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "DOMParser", "createContextualFragment"} {
			if bytes.Contains(js, []byte(bad)) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	srv := e.internal
	get := func() int {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := get(); st != 200 {
		t.Fatalf("healthz = %d", st)
	}
	// The public listener does not serve it.
	if resp, _ := e.do("GET", "/healthz", "", nil); resp.StatusCode != 404 {
		t.Fatalf("public /healthz = %d", resp.StatusCode)
	}
	// A dead token is unhealthy: the operator redeploys with a new one.
	e.fake.Uninstall(app.PluginID)
	e.app.PollOnce(context.Background())
	if st := get(); st != 503 {
		t.Fatalf("healthz after the uninstall = %d, want 503", st)
	}
}
