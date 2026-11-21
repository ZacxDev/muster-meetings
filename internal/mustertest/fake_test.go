package mustertest_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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
	// A subject the app never held is absent from the answer.
	if st, ok := x.states(app, "Unkn0wnSubjectXXXXXXXX")["Unkn0wnSubjectXXXXXXXX"]; ok {
		t.Fatalf("an unknown subject is answered %q", st)
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
