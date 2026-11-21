package mustertest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/mustertest"
)

// The fake is the plugin tests' muster, so its own gates are pinned here
// against the design's rules ("Core seam: server-installed app plugins").

var (
	t0  = time.Date(2031, 3, 14, 15, 9, 26, 400_000_000, time.UTC) // not on a second or hour boundary
	app = mustertest.App{Token: "mpl_server_7f3c91e0", PluginID: 12, PluginName: "meet-srv", Capabilities: []string{"app", "tasks"}}
	two = mustertest.App{Token: "mpl_server_two_8b1d", PluginID: 19, PluginName: "meet-two", Capabilities: []string{"app", "tasks"}}
	ada = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
	bea = mustertest.Person{ID: 2718281828, Email: "bea.k@example.test", DisplayName: "Bea K."}
)

type fx struct {
	t   *testing.T
	f   *mustertest.Fake
	now time.Time
}

func newFx(t *testing.T) *fx {
	x := &fx{t: t, f: mustertest.New(), now: t0}
	t.Cleanup(x.f.Close)
	x.f.Now = func() time.Time { return x.now }
	x.f.Install(app)
	x.f.Install(two)
	x.f.AddPerson(ada)
	x.f.AddPerson(bea)
	return x
}

func (x *fx) call(method, path, token string, body any, hdr ...string) (int, string) {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, x.f.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// redeem spends a fresh code of p for a and returns the answer's subject
// ("" on a refusal) and the status.
func (x *fx) redeem(a mustertest.App, p mustertest.Person) (string, int) {
	x.t.Helper()
	code := x.f.MintApp(a.PluginID, p.ID, "")
	st, body := x.call("POST", "/plugin/v1/handoffs/redeem", a.Token, map[string]string{"code": code})
	var out struct {
		Owner struct{ Subject string }
	}
	json.Unmarshal([]byte(body), &out)
	return out.Owner.Subject, st
}

func (x *fx) knock(a mustertest.App, subject string) int {
	x.t.Helper()
	st, _ := x.call("POST", "/plugin/v1/notify", a.Token, map[string]any{"kind": "knock", "ref": "r1", "count": 1}, mustertest.ActFor, subject)
	return st
}

// states is owners/check's answer: subject → live, expired or revoked.
func (x *fx) states(a mustertest.App, subjects ...string) map[string]string {
	x.t.Helper()
	st, body := x.call("POST", "/plugin/v1/owners/check", a.Token, map[string]any{"subjects": subjects})
	if st != 200 {
		x.t.Fatalf("owners/check = %d %s", st, body)
	}
	var out struct{ States map[string]string }
	json.Unmarshal([]byte(body), &out)
	return out.States
}

func TestTheRedeemAnswerNamesASubjectAndNoUserIDOrEmail(t *testing.T) {
	x := newFx(t)
	code := x.f.MintApp(app.PluginID, ada.ID, "")
	st, body := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": code})
	sub := x.f.Subject(app.PluginID, ada.ID)
	want := `{"act_until":"2031-03-14T23:09:26Z","minted_at":"2031-03-14T15:09:26Z","owner":{"display_name":"Ada L.","subject":"` + sub +
		`"},"purpose":"app","scope":"server","stepped_up":false,"target":{"app":{}}}`
	if st != 200 || body != want {
		t.Fatalf("redeem = %d %s\nwant %s", st, body, want)
	}
	// Pairwise, and not derived from the user id: another app's subject for
	// the same person differs.
	other, _ := x.redeem(two, ada)
	if other == sub || other == "" || sub == strconv.FormatInt(ada.ID, 10) {
		t.Fatalf("subjects under two apps: %q and %q", sub, other)
	}
	// Re-redeeming keeps the subject.
	if again, _ := x.redeem(app, ada); again != sub {
		t.Fatalf("a second redeem gave subject %q, want %q", again, sub)
	}
}

// A code lives 60 s: expires_at > now(), so it is refused at exactly 60 s
// and spent one millisecond before.
func TestACodeIsRefusedAtExactlySixtySeconds(t *testing.T) {
	x := newFx(t)
	late := x.f.MintApp(app.PluginID, ada.ID, "")
	ok := x.f.MintApp(app.PluginID, bea.ID, "")
	x.now = x.now.Add(time.Minute - time.Millisecond)
	if st, _ := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": ok}); st != 200 {
		t.Fatalf("at 60 s - 1 ms = %d, want 200", st)
	}
	x.now = x.now.Add(time.Millisecond)
	if st, _ := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": late}); st != 404 {
		t.Fatalf("at exactly 60 s = %d, want 404", st)
	}
}

// One predicate, two consumers: each gate closed alone is 404 on act-for,
// and owners/check names it: "expired" when act_until alone has passed,
// "revoked" for every other gate. The open case is 202 and "live".
func TestEveryGrantGateEndsActForAndOwnersCheckAlike(t *testing.T) {
	cases := map[string]struct {
		close func(x *fx)
		state string
	}{
		"open":                  {func(x *fx) {}, "live"},
		"act_until passed":      {func(x *fx) { x.now = x.now.Add(8 * time.Hour) }, "expired"},
		"Expire":                {func(x *fx) { x.f.Expire(app.PluginID, ada.ID) }, "expired"},
		"hidden":                {func(x *fx) { x.f.Hide(app.PluginID, ada.ID) }, "revoked"},
		"disabled":              {func(x *fx) { x.f.Disable(ada.ID) }, "revoked"},
		"signed out everywhere": {func(x *fx) { x.now = x.now.Add(time.Second); x.f.SignOutEverywhere(ada.ID) }, "revoked"},
		// Core's other revoke causes (internal/plugins/server.go serverGrantStanding).
		"sessions_not_before cut off": {func(x *fx) { x.now = x.now.Add(time.Second); x.f.SetSessionsNotBefore(ada.ID) }, "revoked"},
		"origin retired":              {func(x *fx) { x.f.RetireOrigin(app.PluginID) }, "revoked"},
		"a team, not a person":        {func(x *fx) { x.f.MakeTeam(ada.ID) }, "revoked"},
		"scope no longer server":      {func(x *fx) { x.f.SetScope(app.PluginID, "person") }, "revoked"},
		// Expired AND hidden is revoked: expiry is only the clock alone.
		"expired and hidden": {func(x *fx) { x.now = x.now.Add(9 * time.Hour); x.f.Hide(app.PluginID, ada.ID) }, "revoked"},
	}
	for name, c := range cases {
		x := newFx(t)
		sub, _ := x.redeem(app, ada)
		c.close(x)
		if got := x.states(app, sub)[sub]; got != c.state {
			t.Errorf("%s: owners/check state = %q, want %q", name, got, c.state)
		}
		want := 404
		if c.state == "live" {
			want = 202
		}
		if st := x.knock(app, sub); st != want {
			t.Errorf("%s: knock = %d, want %d", name, st, want)
		}
	}
	// The boundary: one second short of 8 h passes.
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	x.now = x.now.Add(8*time.Hour - time.Second)
	if x.states(app, sub)[sub] != "live" || x.knock(app, sub) != 202 {
		t.Fatal("at 8 h - 1 s the grant is not live")
	}
}

// A code minted before a Sign out everywhere and redeemed after it is
// refused, and no grant comes of it; the whole-second rule refuses a sign-in
// in the cutoff's own second and passes one a second later.
func TestTheCutoffIsComparedWithTheMintingSignIn(t *testing.T) {
	x := newFx(t)
	code := x.f.MintApp(app.PluginID, ada.ID, "")
	x.now = x.now.Add(5 * time.Second)
	x.f.SignOutEverywhere(ada.ID)
	x.now = x.now.Add(5 * time.Second)
	if st, _ := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": code}); st != 404 {
		t.Fatalf("minted before the sign-out, redeemed after = %d, want 404", st)
	}
	if x.f.Subject(app.PluginID, ada.ID) != "" {
		t.Fatal("the refused redeem made a grant")
	}
	x.f.SignIn(ada.ID) // a new sign-in, 5 s after the cutoff
	if _, st := x.redeem(app, ada); st != 200 {
		t.Fatalf("a sign-in after the cutoff = %d", st)
	}
	y := newFx(t)
	y.now = y.now.Add(time.Second)
	y.f.SignOutEverywhere(ada.ID)
	y.now = y.now.Add(300 * time.Millisecond) // still the cutoff's second
	y.f.SignIn(ada.ID)
	if _, st := y.redeem(app, ada); st != 404 {
		t.Fatalf("a sign-in in the cutoff's second = %d, want 404", st)
	}
	y.now = y.now.Add(time.Second)
	y.f.SignIn(ada.ID)
	if _, st := y.redeem(app, ada); st != 200 {
		t.Fatalf("a sign-in a second after the cutoff = %d, want 200", st)
	}
}

func TestHideThenShowNeedsAFreshRedeemAndKeepsTheSubject(t *testing.T) {
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	x.f.Hide(app.PluginID, ada.ID)
	x.f.Show(app.PluginID, ada.ID)
	if x.knock(app, sub) != 404 {
		t.Fatal("Show revived the old grant")
	}
	if again, st := x.redeem(app, ada); st != 200 || again != sub || x.knock(app, sub) != 202 {
		t.Fatalf("Show then redeem = %d %q (want the same subject %q)", st, again, sub)
	}
	// Hide by a person who never opened the app: a row that never acts.
	x.f.Hide(app.PluginID, bea.ID)
	if sb := x.f.Subject(app.PluginID, bea.ID); sb == "" || x.states(app, sb)[sb] != "revoked" {
		t.Fatal("a Hide-only row is not revoked")
	}
	// A subject the app never redeemed is answered, as revoked.
	if st := x.states(app, "Unkn0wnSubjectXXXXXXXX")["Unkn0wnSubjectXXXXXXXX"]; st != "revoked" {
		t.Fatalf("an unknown subject is answered %q, want revoked", st)
	}
}

func TestUninstallAndRotate(t *testing.T) {
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	x.f.Rotate(app.PluginID, "mpl_server_rotated_4c2e")
	if st, _ := x.call("GET", "/plugin/v1/self", app.Token, nil); st != 401 {
		t.Fatalf("the old token after Rotate = %d", st)
	}
	rot := app
	rot.Token = "mpl_server_rotated_4c2e"
	if x.knock(rot, sub) != 202 {
		t.Fatal("Rotate lost the grant")
	}
	x.f.Uninstall(app.PluginID)
	if st, _ := x.call("GET", "/plugin/v1/self", rot.Token, nil); st != 401 {
		t.Fatalf("/self after Uninstall = %d", st)
	}
	if st, _ := x.call("POST", "/plugin/v1/owners/check", rot.Token, map[string]any{"subjects": []string{sub}}); st != 401 {
		t.Fatalf("owners/check after Uninstall = %d", st)
	}
}

func TestActForShapeAndOneBodyForEveryRefusal(t *testing.T) {
	x := newFx(t)
	sa, _ := x.redeem(app, ada)
	sb2, _ := x.redeem(two, bea)
	body := map[string]any{"kind": "knock", "ref": "r1", "count": 1}
	if st, _ := x.call("POST", "/plugin/v1/notify", app.Token, body); st != 400 {
		t.Fatalf("no Muster-Act-For = %d", st)
	}
	if st, _ := x.call("POST", "/plugin/v1/notify", app.Token, body, mustertest.ActFor, sa, mustertest.ActFor, sa); st != 400 {
		t.Fatalf("two Muster-Act-For = %d", st)
	}
	x.f.AddPersonal(mustertest.Personal{Token: "mpl_personal_a81c", PluginID: 23, OwnerID: ada.ID, Capabilities: []string{"app"}})
	if st, _ := x.call("GET", "/plugin/v1/self", "mpl_personal_a81c", nil, mustertest.ActFor, sa); st != 400 {
		t.Fatalf("a personal token with Muster-Act-For = %d", st)
	}
	var bodies []string
	for _, s := range []string{"Unkn0wnSubjectXXXXXXXX", strconv.FormatInt(bea.ID, 10), sb2} {
		st, b := x.call("POST", "/plugin/v1/notify", app.Token, body, mustertest.ActFor, s)
		if st != 404 {
			t.Fatalf("act-for %q = %d", s, st)
		}
		bodies = append(bodies, b)
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("the 404 bodies differ: %q", bodies)
	}
	if x.knock(app, sa) != 202 || len(x.f.Pushes()) != 1 || x.f.Pushes()[0].UserID != ada.ID {
		t.Fatal("positive control: Ada's knock")
	}
	// B cannot withdraw A's knock.
	sb, _ := x.redeem(app, bea)
	if st, _ := x.call("DELETE", "/plugin/v1/notify/r1", app.Token, nil, mustertest.ActFor, sb); st != 204 {
		t.Fatalf("Bea withdrawing Ada's ref = %d, want 204", st)
	}
	if st, _ := x.call("DELETE", "/plugin/v1/notify/r1", app.Token, nil, mustertest.ActFor, sa); st != 202 {
		t.Fatalf("Ada withdrawing her own = %d, want 202", st)
	}
}

func TestOwnersCheckShape(t *testing.T) {
	x := newFx(t)
	many := make([]string, 201)
	for i := range many {
		many[i] = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7h"
	}
	if st, _ := x.call("POST", "/plugin/v1/owners/check", app.Token, map[string]any{"subjects": many}); st != 400 {
		t.Fatalf("201 subjects = %d", st)
	}
	if st, _ := x.call("POST", "/plugin/v1/owners/check", app.Token, map[string]any{"subjects": []string{}}); st != 400 {
		t.Fatalf("0 subjects = %d", st)
	}
	x.f.AddPersonal(mustertest.Personal{Token: "mpl_personal_a81c", PluginID: 23, OwnerID: ada.ID, Capabilities: []string{"app"}})
	if st, _ := x.call("POST", "/plugin/v1/owners/check", "mpl_personal_a81c", map[string]any{"subjects": many[:1]}); st != 403 {
		t.Fatalf("a personal token = %d", st)
	}
}

// Tasks only through the app's own ledger, and only while the task is open.
func TestTasksMatchOnlyTheAppsOwnLedger(t *testing.T) {
	x := newFx(t)
	sa, _ := x.redeem(app, ada)
	task := func(b map[string]any) (int, string) {
		return x.call("POST", "/plugin/v1/tasks", app.Token, b, mustertest.ActFor, sa)
	}
	own := x.f.AddTask(ada.ID, "meetings:x", "person")
	x.f.AddTask(ada.ID, "meetings:x", "plugin:other")
	st, notLedgered := task(map[string]any{"key": "meetings:x", "comment": "c1"})
	_, unknown := task(map[string]any{"key": "meetings:nobody", "comment": "c1"})
	if st != 200 || notLedgered != unknown || notLedgered != `{"id":null,"created":false,"commented":false}` {
		t.Fatalf("a comment on a key only others carry = %d %s (unknown key: %s)", st, notLedgered, unknown)
	}
	for _, tk := range x.f.Tasks(ada.ID) {
		if len(tk.Comments) != 0 {
			t.Fatalf("task %d (filed by %s) took a comment", tk.ID, tk.FiledBy)
		}
	}
	st, b := task(map[string]any{"key": "meetings:x", "create": map[string]any{"title": "t", "body": "b"}})
	var r struct{ ID int64 }
	json.Unmarshal([]byte(b), &r)
	if st != 201 || r.ID == own {
		t.Fatalf("create = %d %s", st, b)
	}
	if st, b := task(map[string]any{"key": "meetings:x", "comment": "c2"}); st != 200 || b != `{"commented":true,"created":false,"id":`+strconv.FormatInt(r.ID, 10)+`}` {
		t.Fatalf("positive control: a comment on the app's own task = %d %s", st, b)
	}
	// Completed: the next create files a new one and the ledger re-points.
	x.f.SetTaskState(r.ID, "complete")
	st, b = task(map[string]any{"key": "meetings:x", "create": map[string]any{"title": "t", "body": "b"}})
	var r2 struct{ ID int64 }
	json.Unmarshal([]byte(b), &r2)
	if st != 201 || r2.ID == r.ID {
		t.Fatalf("create after complete = %d %s", st, b)
	}
	if st, _ := task(map[string]any{"key": "meetings:x", "route_to_chief": true, "comment": "c"}); st != 400 {
		t.Fatalf("route_to_chief = %d", st)
	}
}

// ---- core's behaviour, pinned (muster PR #936) ----

// callH is call with the response headers too.
func (x *fx) callH(method, path, token string, body any, hdr ...string) (int, string, http.Header) {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, x.f.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), resp.Header
}

// 1. owners/check answers EVERY asked subject (one this app never redeemed
// as revoked) and is 60 calls a minute per app: 429 with Retry-After.
func TestOwnersCheckAnswersEverySubjectAndIsSixtyCallsAMinute(t *testing.T) {
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	unknown := "Unkn0wnSubjectXXXXXXXX"
	got := x.states(app, sub, unknown)
	if len(got) != 2 || got[sub] != "live" || got[unknown] != "revoked" {
		t.Fatalf("states = %v; want Ada live and the unknown subject revoked", got)
	}
	for i := 2; i <= 60; i++ { // the first call was above
		x.states(app, sub)
	}
	st, body, h := x.callH("POST", "/plugin/v1/owners/check", app.Token, map[string]any{"subjects": []string{sub}})
	if st != 429 || h.Get("Retry-After") != "60" || !strings.Contains(body, `"retry_after":60`) {
		t.Fatalf("the 61st call in a minute = %d %s (Retry-After %q); want 429, 60", st, body, h.Get("Retry-After"))
	}
	// Another app has its own minute; this one is served again after it.
	sub2, _ := x.redeem(two, ada)
	if x.states(two, sub2)[sub2] != "live" {
		t.Fatal("another app's owners/check was limited by this one's")
	}
	x.now = x.now.Add(time.Minute)
	if x.states(app, sub)[sub] != "live" {
		t.Fatal("a minute later the app is still limited")
	}
}

// 2. Every revoke cause core has also refuses the redeem (the re-check).
func TestTheRedeemRefusesEveryRevokeCause(t *testing.T) {
	for name, close := range map[string]func(x *fx){
		"sessions_not_before cut off": func(x *fx) { x.now = x.now.Add(time.Second); x.f.SetSessionsNotBefore(ada.ID) },
		"origin retired":              func(x *fx) { x.f.RetireOrigin(app.PluginID) },
		"a team, not a person":        func(x *fx) { x.f.MakeTeam(ada.ID) },
		"scope no longer server":      func(x *fx) { x.f.SetScope(app.PluginID, "person") },
		"disabled":                    func(x *fx) { x.f.Disable(ada.ID) },
	} {
		x := newFx(t)
		code := x.f.MintApp(app.PluginID, ada.ID, "")
		close(x)
		if st, _ := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": code}); st != 404 {
			t.Errorf("%s: redeem = %d, want 404", name, st)
		}
	}
}

// 3. Muster-Act-For on an acts-as-nobody route is 400.
func TestActForIsRefusedOnSelfRedeemAndOwnersCheck(t *testing.T) {
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	code := x.f.MintApp(app.PluginID, bea.ID, "")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/plugin/v1/self", nil},
		{"POST", "/plugin/v1/handoffs/redeem", map[string]string{"code": code}},
		{"POST", "/plugin/v1/owners/check", map[string]any{"subjects": []string{sub}}},
	} {
		if st, body := x.call(c.method, c.path, app.Token, c.body, mustertest.ActFor, sub); st != 400 || !strings.Contains(body, "exactly one Muster-Act-For") {
			t.Errorf("%s %s with Muster-Act-For = %d %s; want 400", c.method, c.path, st, body)
		}
	}
	// The code was not spent by the refused redeem.
	if st, _ := x.call("POST", "/plugin/v1/handoffs/redeem", app.Token, map[string]string{"code": code}); st != 200 {
		t.Fatalf("the same code without the header = %d", st)
	}
}

// 4. The door's order: an unserved route is 403, then a missing capability
// 403, then the header's shape 400, then a lookup error 503.
func TestTheDoorChecksInCoresOrder(t *testing.T) {
	x := newFx(t)
	sub, _ := x.redeem(app, ada)
	if st, body := x.call("POST", "/plugin/v1/messages", app.Token, nil, mustertest.ActFor, sub, mustertest.ActFor, sub); st != 403 || !strings.Contains(body, "server app's token") {
		t.Fatalf("an unserved route with a doubled header = %d %s; want the route refusal first", st, body)
	}
	noTasks := mustertest.App{Token: "mpl_server_notasks_51e0", PluginID: 31, PluginName: "no-tasks", Capabilities: []string{"app"}}
	x.f.Install(noTasks)
	if st, body := x.call("POST", "/plugin/v1/tasks", noTasks.Token, nil); st != 403 || !strings.Contains(body, "tasks capability") {
		t.Fatalf("tasks without the capability and no header = %d %s; want the capability refusal before the header's", st, body)
	}
	tasksOnly := mustertest.App{Token: "mpl_server_tasksonly_7a2d", PluginID: 37, PluginName: "tasks-only", Capabilities: []string{"tasks"}}
	x.f.Install(tasksOnly)
	if st, body := x.call("DELETE", "/plugin/v1/notify/r1", tasksOnly.Token, nil, mustertest.ActFor, sub); st != 403 || !strings.Contains(body, "app capability") {
		t.Fatalf("a withdrawal without app = %d %s; want 403", st, body)
	}
	x.f.SetFailActForLookup(true)
	if st, _ := x.call("POST", "/plugin/v1/notify", app.Token, map[string]any{"kind": "knock", "ref": "r1", "count": 1}, mustertest.ActFor, sub); st != 503 {
		t.Fatalf("a lookup error = %d, want 503", st)
	}
	if st, _ := x.call("POST", "/plugin/v1/notify", app.Token, map[string]any{"kind": "knock", "ref": "r1", "count": 1}); st != 400 {
		t.Fatalf("no header, lookup failing = %d, want the shape's 400 first", st)
	}
}

// 5. Notify and its withdrawal: core's answers and caps.
func TestNotifyAnswersAndCapsAreCores(t *testing.T) {
	x := newFx(t)
	sa, _ := x.redeem(app, ada)
	knock := func(sub, ref string) (int, string, http.Header) {
		return x.callH("POST", "/plugin/v1/notify", app.Token, map[string]any{"kind": "knock", "ref": ref, "count": 1}, mustertest.ActFor, sub)
	}
	if st, body, _ := knock(sa, "r1"); st != 202 || body != `{"queued":true,"tag":"app-12-r1"}` {
		t.Fatalf("a knock = %d %s", st, body)
	}
	if st, _, h := knock(sa, "r1"); st != 429 || h.Get("Retry-After") != "30" {
		t.Fatalf("the same ref within 30 s = %d (Retry-After %q); want 429, 30", st, h.Get("Retry-After"))
	}
	x.f.Mute(ada.ID, true)
	if st, _, _ := knock(sa, "r2"); st != 204 {
		t.Fatalf("muted = %d, want 204", st)
	}
	x.f.Mute(ada.ID, false)
	x.f.SetDevices(ada.ID, 0)
	if st, _, _ := knock(sa, "r2"); st != 204 {
		t.Fatalf("no device = %d, want 204", st)
	}
	x.f.SetDevices(ada.ID, 1)
	x.f.SetPushConfigured(false)
	if st, _, _ := knock(sa, "r2"); st != 503 {
		t.Fatalf("push not configured = %d, want 503", st)
	}
	if st, _, _ := x.callH("DELETE", "/plugin/v1/notify/r1", app.Token, nil, mustertest.ActFor, sa); st != 503 {
		t.Fatalf("a withdrawal, push not configured = %d, want 503", st)
	}
	x.f.SetPushConfigured(true)
	if st, body, _ := x.callH("DELETE", "/plugin/v1/notify/r1", app.Token, nil, mustertest.ActFor, sa); st != 202 || body != `{"queued":true,"tag":"app-12-r1"}` {
		t.Fatalf("a withdrawal of a knock that is out = %d %s", st, body)
	}
	if st, _, _ := x.callH("DELETE", "/plugin/v1/notify/a.b", app.Token, nil, mustertest.ActFor, sa); st != 400 {
		t.Fatalf("a bad ref = %d, want 400", st)
	}
	// 30 an hour per person (one is out already): the 31st is 429.
	for i := 2; i <= 30; i++ {
		if st, _, _ := knock(sa, "p"+strconv.Itoa(i)); st != 202 {
			t.Fatalf("knock %d = %d", i, st)
		}
	}
	if st, _, _ := knock(sa, "p31"); st != 429 {
		t.Fatalf("the 31st knock an hour for one person = %d, want 429", st)
	}
	// 60 withdrawals of nothing an hour per person: the 61st is 429.
	for i := 1; i <= 60; i++ {
		if st, _, _ := x.callH("DELETE", "/plugin/v1/notify/g"+strconv.Itoa(i), app.Token, nil, mustertest.ActFor, sa); st != 204 {
			t.Fatalf("withdrawal of nothing %d = %d", i, st)
		}
	}
	if st, _, h := x.callH("DELETE", "/plugin/v1/notify/g61", app.Token, nil, mustertest.ActFor, sa); st != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("the 61st withdrawal of nothing = %d, want 429 with Retry-After", st)
	}
	// 600 an hour per app, across people each under their own 30.
	y := newFx(t)
	subs := y.f.AddGrants(app.PluginID, 21)
	n := 0
	for _, s := range subs {
		for j := 0; j < 30 && n < 600; j++ {
			st, _ := y.call("POST", "/plugin/v1/notify", app.Token, map[string]any{"kind": "knock", "ref": "k" + strconv.Itoa(j), "count": 1}, mustertest.ActFor, s)
			if st != 202 {
				t.Fatalf("app knock %d = %d", n+1, st)
			}
			n++
		}
	}
	if st, _ := y.call("POST", "/plugin/v1/notify", app.Token, map[string]any{"kind": "knock", "ref": "last", "count": 1}, mustertest.ActFor, subs[20]); st != 429 {
		t.Fatalf("the app's 601st knock an hour = %d, want 429", st)
	}
}

// 6. Tasks: namespaces, the key's shape, trimming, and the caps.
func TestTasksKeysTrimmingAndCapsAreCores(t *testing.T) {
	x := newFx(t)
	sa, _ := x.redeem(app, ada)
	task := func(sub string, b map[string]any) (int, string, http.Header) {
		return x.callH("POST", "/plugin/v1/tasks", app.Token, b, mustertest.ActFor, sub)
	}
	create := func(key string) map[string]any {
		return map[string]any{"key": key, "create": map[string]any{"title": "t", "body": "b"}}
	}
	for _, key := range []string{"Meetings:x", "other:x", "meetings:", "meetings", "meetings: x", ""} {
		if st, body, _ := task(sa, create(key)); st != 400 {
			t.Errorf("key %q = %d %s, want 400", key, st, body)
		}
	}
	long := strings.Repeat("é", 600)
	big := strings.Repeat("x", 20<<10)
	if st, _, _ := task(sa, map[string]any{"key": "meetings:long", "create": map[string]any{"title": long, "body": big}}); st != 201 {
		t.Fatalf("an over-long title and body = %d, want 201 (trimmed)", st)
	}
	tk := x.f.Tasks(ada.ID)[0]
	if tk.Title != strings.Repeat("é", 512) || len(tk.Body) != 16<<10 {
		t.Fatalf("trimmed to %d runes and %d bytes; want 512 and 16384", len([]rune(tk.Title)), len(tk.Body))
	}
	// A repeat comment wrote nothing and spends nothing; 60 writes an hour
	// per person (one above), and the 61st is 429.
	task(sa, map[string]any{"key": "meetings:long", "comment": "c1"})
	for i := 0; i < 3; i++ {
		if st, body, _ := task(sa, map[string]any{"key": "meetings:long", "comment": "c1"}); st != 200 || !strings.Contains(body, `"commented":false`) {
			t.Fatalf("a repeat comment = %d %s", st, body)
		}
	}
	for i := 3; i <= 60; i++ {
		if st, _, _ := task(sa, create("meetings:k"+strconv.Itoa(i))); st != 201 {
			t.Fatalf("write %d = %d", i, st)
		}
	}
	if st, _, h := task(sa, create("meetings:k61")); st != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("the 61st write an hour for one person = %d (Retry-After %q), want 429", st, h.Get("Retry-After"))
	}
	// 600 an hour per app, across 11 people.
	y := newFx(t)
	subs := y.f.AddGrants(app.PluginID, 11)
	n := 0
	for _, s := range subs {
		for j := 0; j < 60 && n < 600; j++ {
			if st, _ := y.call("POST", "/plugin/v1/tasks", app.Token, create("meetings:a"+strconv.Itoa(j)), mustertest.ActFor, s); st != 201 {
				t.Fatalf("app write %d = %d", n+1, st)
			}
			n++
		}
	}
	if st, _ := y.call("POST", "/plugin/v1/tasks", app.Token, create("meetings:last"), mustertest.ActFor, subs[10]); st != 429 {
		t.Fatalf("the app's 601st write an hour = %d, want 429", st)
	}
	// No namespaces configured: 503.
	x.f.SetTaskNamespaces()
	if st, _, _ := task(sa, create("meetings:x")); st != 503 {
		t.Fatalf("no namespaces = %d, want 503", st)
	}
}

// 7. A personal token's /self has no scope field.
func TestAPersonalTokensSelfHasNoScope(t *testing.T) {
	x := newFx(t)
	x.f.AddPersonal(mustertest.Personal{Token: "mpl_personal_a81c", PluginID: 23, OwnerID: ada.ID, Capabilities: []string{"app"}})
	st, body := x.call("GET", "/plugin/v1/self", "mpl_personal_a81c", nil)
	var m map[string]any
	json.Unmarshal([]byte(body), &m)
	if _, has := m["scope"]; st != 200 || has || m["owner_kind"] != "person" {
		t.Fatalf("a personal /self = %d %s; want no scope field", st, body)
	}
}
