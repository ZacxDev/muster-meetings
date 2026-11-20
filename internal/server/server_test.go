package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Fixtures: ids, hosts and origins that equal no constant the code or the
// fake names (the fake's Default is plugin 3, owner 1).
const (
	publicURL    = "https://meet.example.test"
	musterOrigin = "https://hub.example.test"
)

var (
	pA = mustertest.Plugin{Token: "mpl_tokenA_7f3c", PluginID: 12, PluginName: "meet-a", OwnerID: 7, OwnerLogin: "a@example.test", OwnerKind: "person", Capabilities: []string{"app", "tasks"}}
	pB = mustertest.Plugin{Token: "mpl_tokenB_91e0", PluginID: 19, PluginName: "meet-b", OwnerID: 41, OwnerLogin: "b@example.test", OwnerKind: "person", Capabilities: []string{"app"}}
	t0 = time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	fake  *mustertest.Fake
	store *store.Store
	reg   *owners.Registry
	srv   *httptest.Server
	clock *clock
	log   *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, fake: mustertest.New(), clock: &clock{t: t0}, log: &syncBuf{}}
	t.Cleanup(e.fake.Close)
	e.fake.Now = e.clock.Now
	var cs []*muster.Client
	for _, p := range []mustertest.Plugin{pA, pB} {
		e.fake.AddPlugin(p)
		c, err := muster.New(e.fake.URL, p.Token)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "meetings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.store = st
	lg := log.New(e.log, "", 0)
	if e.reg, err = owners.Boot(context.Background(), cs, st, lg, e.clock.Now); err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Config{PublicURL: publicURL, MusterOrigin: musterOrigin, Now: e.clock.Now, Log: lg}, st, e.reg)
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
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
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func enterPath(code string, pluginID int64) string {
	return "/enter?muster_code=" + url.QueryEscape(code) + "&muster_plugin=" + strconv.FormatInt(pluginID, 10)
}

var sessionAttr = regexp.MustCompile(`data-session="(mmo_[A-Za-z0-9_-]{43})"`)

// enter opens the app as muster's frame does, and returns the session.
func (e *env) enter(p mustertest.Plugin) string {
	e.t.Helper()
	resp, body := e.do("GET", enterPath(e.fake.MintApp(p.PluginID, ""), p.PluginID), "", nil)
	m := sessionAttr.FindSubmatch(body)
	if resp.StatusCode != 200 || m == nil {
		e.t.Fatalf("enter as plugin %d = %d %s", p.PluginID, resp.StatusCode, body)
	}
	return string(m[1])
}

// sessions is the number of owner sessions in the database.
func (e *env) sessions() int {
	e.t.Helper()
	n, err := e.store.CountSessions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---- /enter: the redeem ----

func TestEnterRedeemsAnAppCodeAndRendersTheAppWithABearerSession(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do("GET", enterPath(e.fake.MintApp(pB.PluginID, ""), pB.PluginID), "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("enter = %d %s", resp.StatusCode, body)
	}
	m := sessionAttr.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no session in the page: %s", body)
	}
	// The redeem went to muster with the token muster_plugin names.
	if calls := e.fake.Calls(); calls[len(calls)-1] != "POST /plugin/v1/handoffs/redeem "+pB.Token {
		t.Fatalf("last muster call = %q", calls[len(calls)-1])
	}
	// The session is B's: it lists B's meetings, and makes them as B.
	resp, raw := e.do("POST", "/api/meetings", string(m[1]), map[string]any{})
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	list, _, _ := e.store.ForOwner(pB.OwnerID).ListMeetings(context.Background(), nil, 10)
	if len(list) != 1 {
		t.Fatalf("owner %d has %d meetings, want the one just made", pB.OwnerID, len(list))
	}
}

// 🔴 The owner app's response on the plugin's own origin: framed by muster's
// exact origin and nothing else, no referrer, not cached, and no cookie.
func TestEnterHeadersFrameAncestorsIsMustersOriginAndNoCookieIsSet(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.do("GET", enterPath(e.fake.MintApp(pA.PluginID, ""), pA.PluginID), "", nil)
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

// "/enter refuses any answer whose purpose is not app or whose target is not
// {"app": {}}", and one whose owner is not the token's. Each fixture breaks
// exactly one of the three, so each check is reached on its own.
func TestEnterRefusesAnythingButAnAppCodeForTheTokensOwner(t *testing.T) {
	app := map[string]any{"app": map[string]any{}}
	mirror := map[string]any{"mirror": map[string]any{"host": "ws", "session": "main", "window": "@3"}}
	both := map[string]any{"app": map[string]any{}, "mirror": map[string]any{"host": "ws", "session": "main", "window": "@3"}}
	cases := map[string]map[string]any{
		"purpose view, target app":   {"owner_id": pA.OwnerID, "purpose": "view", "target": app},
		"purpose empty, target app":  {"owner_id": pA.OwnerID, "purpose": "", "target": app},
		"purpose app, target mirror": {"owner_id": pA.OwnerID, "purpose": "app", "target": mirror},
		"purpose app, both targets":  {"owner_id": pA.OwnerID, "purpose": "app", "target": both},
		"purpose app, no target":     {"owner_id": pA.OwnerID, "purpose": "app", "target": map[string]any{}},
		"another owner":              {"owner_id": pB.OwnerID, "purpose": "app", "target": app},
	}
	for name, answer := range cases {
		e := newEnv(t)
		resp, body := e.do("GET", enterPath(e.fake.MintRaw(pA.PluginID, answer), pA.PluginID), "", nil)
		if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterWrongKind) {
			t.Errorf("%s: enter = %d %s; want 403 %q", name, resp.StatusCode, body, server.EnterWrongKind)
		}
		if sessionAttr.Match(body) || e.sessions() != 0 {
			t.Errorf("%s: a session was made (%d)", name, e.sessions())
		}
	}
	// Positive control: the same harness with the right answer admits.
	e := newEnv(t)
	resp, body := e.do("GET", enterPath(e.fake.MintRaw(pA.PluginID, map[string]any{"owner_id": pA.OwnerID, "purpose": "app", "target": app}), pA.PluginID), "", nil)
	if resp.StatusCode != 200 || !sessionAttr.Match(body) || e.sessions() != 1 {
		t.Fatalf("positive control: enter = %d, sessions %d", resp.StatusCode, e.sessions())
	}
}

// The plugin is bound: muster_plugin picks the token the code is redeemed
// with, and a code redeems only with the token it was minted for.
func TestEnterRedeemsWithTheTokenMusterPluginNamesAndNoOther(t *testing.T) {
	e := newEnv(t)
	codeB := e.fake.MintApp(pB.PluginID, "")
	// B's code presented as A's: redeemed with A's token, which muster refuses.
	resp, body := e.do("GET", enterPath(codeB, pA.PluginID), "", nil)
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterRefused) || e.sessions() != 0 {
		t.Fatalf("B's code as plugin A = %d %s (sessions %d); want 403 %q", resp.StatusCode, body, e.sessions(), server.EnterRefused)
	}
	if calls := e.fake.Calls(); calls[len(calls)-1] != "POST /plugin/v1/handoffs/redeem "+pA.Token {
		t.Fatalf("the redeem used another token: %q", calls[len(calls)-1])
	}
	// A plugin id the server holds no token for never reaches muster.
	before := e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem")
	resp, body = e.do("GET", enterPath(e.fake.MintApp(mustertest.Default.PluginID, ""), mustertest.Default.PluginID), "", nil)
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterNotSetUp) {
		t.Fatalf("an unserved plugin = %d %s", resp.StatusCode, body)
	}
	if e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem") != before {
		t.Fatal("an unserved plugin's code was sent to muster")
	}
	// A code is single use: a reload of the same URL is refused.
	code := e.fake.MintApp(pB.PluginID, "")
	if resp, _ := e.do("GET", enterPath(code, pB.PluginID), "", nil); resp.StatusCode != 200 {
		t.Fatalf("first use = %d", resp.StatusCode)
	}
	resp, body = e.do("GET", enterPath(code, pB.PluginID), "", nil)
	if resp.StatusCode != 403 || sessionAttr.Match(body) {
		t.Fatalf("second use = %d", resp.StatusCode)
	}
	// So is an expired one (muster's 60 s).
	code = e.fake.MintApp(pB.PluginID, "")
	e.clock.Add(61 * time.Second)
	if resp, _ := e.do("GET", enterPath(code, pB.PluginID), "", nil); resp.StatusCode != 403 {
		t.Fatalf("an expired code = %d", resp.StatusCode)
	}
}

func TestEnterWithoutACodeSaysOpenItFromMusterAgain(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/enter", "/enter?muster_plugin=12", "/enter?muster_code=short&muster_plugin=12",
		"/enter?muster_code=" + strings.Repeat("a", 43) + "&muster_plugin=x", "/enter?muster_code=" + strings.Repeat("a", 43) + "&muster_plugin=-12"} {
		resp, body := e.do("GET", p, "", nil)
		if resp.StatusCode != 400 || !strings.Contains(string(body), server.EnterAgain) || sessionAttr.Match(body) {
			t.Errorf("GET %s = %d %s", p, resp.StatusCode, body)
		}
		if directive(resp.Header.Get("Content-Security-Policy"), "frame-ancestors") != musterOrigin {
			t.Errorf("GET %s: the refusal cannot be shown in muster's frame", p)
		}
	}
	if n := e.fake.CountCalls("POST", "/plugin/v1/handoffs/redeem"); n != 0 {
		t.Fatalf("a malformed request reached muster %d times", n)
	}
}

// muster's 401 on the redeem is the token dying: it is revoked at once.
func TestEnterThatMusterAnswers401RevokesTheToken(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	code := e.fake.MintApp(pA.PluginID, "")
	e.fake.Revoke(pA.Token)
	resp, _ := e.do("GET", enterPath(code, pA.PluginID), "", nil)
	if resp.StatusCode != 403 {
		t.Fatalf("enter with a revoked token = %d", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 401 {
		t.Fatalf("the revoked token's session still works: %d", resp.StatusCode)
	}
}

// The ref is a hint: passed to the page only when it is one of this owner's
// own meetings.
func TestEnterPassesTheRefOnlyForTheOwnersOwnMeeting(t *testing.T) {
	e := newEnv(t)
	own, _ := e.store.ForOwner(pA.OwnerID).CreateMeeting(context.Background(), store.NewMeeting{}, t0)
	other, _ := e.store.ForOwner(pB.OwnerID).CreateMeeting(context.Background(), store.NewMeeting{}, t0)
	_, body := e.do("GET", enterPath(e.fake.MintApp(pA.PluginID, own.Meeting.ID), pA.PluginID), "", nil)
	if !strings.Contains(string(body), `data-ref="`+own.Meeting.ID+`"`) {
		t.Fatalf("the owner's own ref was dropped: %s", body)
	}
	_, body = e.do("GET", enterPath(e.fake.MintApp(pA.PluginID, other.Meeting.ID), pA.PluginID), "", nil)
	if strings.Contains(string(body), "data-ref") || !sessionAttr.Match(body) {
		t.Fatalf("another owner's ref reached the page: %s", body)
	}
}

// ---- owner sessions: bearer only ----

func TestOwnerRoutesRefuseAnythingButALiveBearerSession(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	bad := map[string][]string{
		"no credential":        nil,
		"the token as cookie":  {"Cookie", "session=" + sess},
		"Basic":                {"Authorization", "Basic " + sess},
		"bare token":           {"Authorization", sess},
		"another prefix":       {"Authorization", "Bearer mpl_" + sess[4:]},
		"unknown":              {"Authorization", "Bearer " + store.NewSessionToken()},
		"its hash":             {"Authorization", "Bearer " + store.Hash(sess)},
		"truncated":            {"Authorization", "Bearer " + sess[:len(sess)-1]},
		"the muster token":     {"Authorization", "Bearer " + pA.Token},
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

// A session whose token is no longer served is refused even while its row
// survives (the revocation's own delete failed, say): liveness is checked
// on every request, not only by the poll's delete.
func TestASessionOfATokenNotServedIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Plugin 3 (the fake's Default) is not in this server's registry.
	unserved, _, _ := e.store.CreateSession(ctx, mustertest.Default.OwnerID, mustertest.Default.PluginID, t0)
	// Plugin 12 is served, but for owner 7, not 41.
	crossed, _, _ := e.store.CreateSession(ctx, pB.OwnerID, pA.PluginID, t0)
	for name, tok := range map[string]string{"an unserved token's": unserved, "a session naming another owner of a served token": crossed} {
		if resp, _ := e.do("GET", "/api/meetings", tok, nil); resp.StatusCode != 401 {
			t.Errorf("%s session = %d, want 401", name, resp.StatusCode)
		}
	}
	ok, _, _ := e.store.CreateSession(ctx, pA.OwnerID, pA.PluginID, t0)
	if resp, _ := e.do("GET", "/api/meetings", ok, nil); resp.StatusCode != 200 {
		t.Fatalf("positive control: a served token's session = %d", resp.StatusCode)
	}
}

// Eight hours (decision 12), by the server's clock.
func TestAnOwnerSessionEndsAfterEightHours(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	e.clock.Add(8*time.Hour - time.Second)
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 200 {
		t.Fatalf("one second before 8 h = %d", resp.StatusCode)
	}
	e.clock.Add(time.Second)
	if resp, _ := e.do("GET", "/api/meetings", sess, nil); resp.StatusCode != 401 {
		t.Fatalf("at 8 h = %d, want 401", resp.StatusCode)
	}
}

// ---- the /self poll ends sessions on revoke ----

func TestTheSelfPollEndsARevokedOwnersSessionsAndMeetingsAndNoOneElses(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(pA), e.enter(pB)
	_, ra := e.do("POST", "/api/meetings", sa, map[string]any{})
	_, rb := e.do("POST", "/api/meetings", sb, map[string]any{})
	var ma, mb struct{ ID string }
	json.Unmarshal(ra, &ma)
	json.Unmarshal(rb, &mb)

	e.fake.Revoke(pA.Token)
	e.reg.PollOnce(context.Background())

	if resp, _ := e.do("GET", "/api/meetings", sa, nil); resp.StatusCode != 401 {
		t.Fatalf("the revoked owner's session after one poll = %d, want 401", resp.StatusCode)
	}
	if n := e.sessions(); n != 1 {
		t.Fatalf("%d sessions left in the database, want only B's", n)
	}
	if m, _, _ := e.store.ForOwner(pA.OwnerID).Meeting(context.Background(), ma.ID); m.State != "ended" || m.EndedAt == nil {
		t.Fatalf("the revoked owner's meeting = %+v, want ended", m)
	}
	resp, raw := e.do("GET", "/api/meetings/"+mb.ID, sb, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"state":"open"`) {
		t.Fatalf("the other owner after A's revoke = %d %s", resp.StatusCode, raw)
	}
	// And the revoked tab cannot come back in.
	resp, body := e.do("GET", enterPath(e.fake.MintApp(pA.PluginID, ""), pA.PluginID), "", nil)
	if resp.StatusCode != 403 || !strings.Contains(string(body), server.EnterNotSetUp) {
		t.Fatalf("enter after revoke = %d %s", resp.StatusCode, body)
	}
}

// ---- another owner's meetings are never visible ----

func TestAnotherOwnersMeetingsAreNeverVisible(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(pA), e.enter(pB)
	var ids []string
	for _, title := range []string{"A first", "A second"} {
		_, raw := e.do("POST", "/api/meetings", sa, map[string]any{"title": title})
		var c struct{ ID string }
		json.Unmarshal(raw, &c)
		ids = append(ids, c.ID)
	}
	_, raw := e.do("POST", "/api/meetings", sb, map[string]any{"title": "B only"})
	var cb struct{ ID string }
	json.Unmarshal(raw, &cb)

	_, raw = e.do("GET", "/api/meetings", sb, nil)
	var list struct {
		Meetings []struct{ ID, Title string }
	}
	json.Unmarshal(raw, &list)
	if len(list.Meetings) != 1 || list.Meetings[0].ID != cb.ID || strings.Contains(string(raw), "A first") {
		t.Fatalf("B's list = %s; want B's one meeting only", raw)
	}
	_, unknown := e.do("GET", "/api/meetings/"+store.NewMeetingID(), sb, nil)
	for _, id := range ids {
		resp, raw := e.do("GET", "/api/meetings/"+id, sb, nil)
		if resp.StatusCode != 404 || !bytes.Equal(raw, unknown) {
			t.Fatalf("B reading A's %s = %d %s; want the same 404 as an unknown id (%s)", id, resp.StatusCode, raw, unknown)
		}
	}
	// Positive control: A reads its own.
	if resp, raw := e.do("GET", "/api/meetings/"+ids[0], sa, nil); resp.StatusCode != 200 || !strings.Contains(string(raw), "A first") {
		t.Fatalf("A reading its own = %d %s", resp.StatusCode, raw)
	}
}

// ---- New meeting and its link ----

var joinURL = regexp.MustCompile(`^https://meet\.example\.test/j/([A-Za-z0-9_-]{22})$`)

func TestNewMeetingAnswersItsIDAndALinkOnThePluginsOrigin(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	resp, raw := e.do("POST", "/api/meetings", sess, map[string]any{"title": "  Budget   review "})
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
	if !store.ValidMeetingID(c.ID) || c.Meeting.Title != "Budget review" || c.Meeting.State != "open" || !c.Meeting.Record || !c.Meeting.AI ||
		!c.Meeting.CreatedAt.Equal(t0) || !c.Meeting.LinkExpiresAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("created = %s", raw)
	}
	// The code is in no later answer: not the list, not the meeting.
	for _, p := range []string{"/api/meetings", "/api/meetings/" + c.ID} {
		if _, raw := e.do("GET", p, sess, nil); strings.Contains(string(raw), m[1]) {
			t.Fatalf("GET %s names the join code", p)
		}
	}
	// Two meetings, two links.
	_, raw = e.do("POST", "/api/meetings", sess, map[string]any{"record": false})
	var c2 struct {
		JoinURL string `json:"join_url"`
		Meeting struct{ Record, AI bool }
	}
	json.Unmarshal(raw, &c2)
	if c2.JoinURL == c.JoinURL || !joinURL.MatchString(c2.JoinURL) || c2.Meeting.Record || !c2.Meeting.AI {
		t.Fatalf("second = %s", raw)
	}
}

func TestNewMeetingRefusesABadBody(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	for name, body := range map[string]string{
		"unknown field":    `{"title":"x","owner_id":41}`,
		"not JSON":         `title=x`,
		"title too long":   `{"title":"` + strings.Repeat("é", 81) + `"}`,
		"control in title": `{"title":"a\u0007b"}`,
		"bidi in title":    `{"title":"a\u202eb"}`,
		"record a string":  `{"record":"no"}`,
		"too big":          `{"title":"` + strings.Repeat(" ", 5000) + `"}`,
	} {
		resp, raw := e.do("POST", "/api/meetings", sess, body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, raw)
		}
	}
	if resp, _ := e.do("POST", "/api/meetings", sess, `{}`, "Content-Type", "text/plain"); resp.StatusCode != 415 {
		t.Errorf("text/plain = %d", resp.StatusCode)
	}
	// Boundary, positive side: 80 characters and an empty body pass.
	if resp, raw := e.do("POST", "/api/meetings", sess, `{"title":"`+strings.Repeat("é", 80)+`"}`); resp.StatusCode != 201 {
		t.Errorf("80 characters = %d %s", resp.StatusCode, raw)
	}
	if resp, raw := e.do("POST", "/api/meetings", sess, ``); resp.StatusCode != 201 {
		t.Errorf("an empty body = %d %s", resp.StatusCode, raw)
	}
	list, _, _ := e.store.ForOwner(pB.OwnerID).ListMeetings(context.Background(), nil, 10)
	if len(list) != 0 {
		t.Fatal("a body naming another owner made a meeting for them")
	}
}

func TestTheListIsNewestFirstAndPaged(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(pA)
	var ids []string
	for i := 0; i < 3; i++ {
		_, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
		var c struct{ ID string }
		json.Unmarshal(raw, &c)
		ids = append(ids, c.ID)
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
	sess := e.enter(pA)
	_, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
	var c struct{ ID string }
	json.Unmarshal(raw, &c)
	for _, r := range [][3]string{
		{"GET", enterPath(e.fake.MintApp(pA.PluginID, ""), pA.PluginID), ""},
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
		resp, _ := e.do(r[0], r[1], r[2], body)
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
	sess := e.enter(pA)
	for _, p := range []string{"/api/meetings", "/static/app.js", "/nothing-here"} {
		resp, _ := e.do("GET", p, sess, nil)
		if fa := directive(resp.Header.Get("Content-Security-Policy"), "frame-ancestors"); fa != "'none'" || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s: frame-ancestors %q, X-Frame-Options %q", p, fa, resp.Header.Get("X-Frame-Options"))
		}
	}
}

func TestTheLogCarriesNoCodeTokenOrQuery(t *testing.T) {
	e := newEnv(t)
	code := e.fake.MintApp(pA.PluginID, "")
	_, body := e.do("GET", enterPath(code, pA.PluginID), "", nil)
	sess := string(sessionAttr.FindSubmatch(body)[1])
	_, raw := e.do("POST", "/api/meetings", sess, map[string]any{})
	var c struct {
		JoinURL string `json:"join_url"`
	}
	json.Unmarshal(raw, &c)
	join := joinURL.FindStringSubmatch(c.JoinURL)[1]
	e.do("GET", "/j/"+join, "", nil)
	e.do("GET", "/j/"+join+"/wait", "", nil)
	// A refused enter logs its reason: still no code.
	e.do("GET", enterPath(code, pA.PluginID), "", nil)
	got := e.log.String()
	// Positive controls: the lines are there to be searched.
	// Each is independent of the redaction under test, so a broken redaction
	// fails below, on the secret, not here.
	for _, want := range []string{"http GET /enter", "http POST /api/meetings 201", "http GET /j/", "/wait 404", "enter: refused 403"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log lacks %q (so this scan proves nothing):\n%s", want, got)
		}
	}
	for name, secret := range map[string]string{"hand-off code": code, "session": sess[4:], "join code": join, "muster token": pA.Token, "query": "muster_code"} {
		if strings.Contains(got, secret) {
			t.Errorf("the log carries the %s:\n%s", name, got)
		}
	}
}

func TestRedactPath(t *testing.T) {
	for in, want := range map[string]string{
		"/j/AbC_-123":       "/j/{code}",
		"/j/AbC_-123/knock": "/j/{code}/knock",
		"/j/":               "/j/{code}",
		"/api/meetings":     "/api/meetings",
		"/jx/abc":           "/jx/abc",
	} {
		if got := server.RedactPath(in); got != want {
			t.Errorf("RedactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheAppPageCarriesNoInlineScript(t *testing.T) {
	e := newEnv(t)
	_, body := e.do("GET", enterPath(e.fake.MintApp(pA.PluginID, ""), pA.PluginID), "", nil)
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAll(body, -1) {
		if !bytes.Contains(m, []byte(`src="/static/app.js"`)) {
			t.Errorf("inline script %s: the CSP would block it", m)
		}
	}
	if regexp.MustCompile(`\son[a-z]+=`).Match(body) {
		t.Error("an inline event handler: the CSP would block it")
	}
	resp, js := e.do("GET", "/static/app.js", "", nil)
	if resp.StatusCode != 200 || !bytes.Contains(js, []byte("removeAttribute('data-session')")) || !bytes.Contains(js, []byte("history.replaceState")) {
		t.Fatalf("app.js = %d", resp.StatusCode)
	}
	for _, bad := range []string{"localStorage", "sessionStorage", "document.cookie", "innerHTML"} {
		if bytes.Contains(js, []byte(bad)) {
			t.Errorf("app.js uses %s", bad)
		}
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(server.Internal(e.store, e.reg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz = %v %v", resp, err)
	}
	resp.Body.Close()
	// The public listener does not serve it.
	if resp, _ := e.do("GET", "/healthz", "", nil); resp.StatusCode != 404 {
		t.Fatalf("public /healthz = %d", resp.StatusCode)
	}
}
