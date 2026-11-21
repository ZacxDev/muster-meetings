// Package mustertest is a fake muster for tests: the /plugin/v1 routes a
// server-installed app plugin calls, with muster's answers as
// docs/design/meetings.md ("Core seam: server-installed app plugins") and
// docs/plugins.md specify them, recording every request it was sent.
//
// It models what is the plugin's security boundary, faithfully:
//
//   - A server app is installed once (Install) with one token. Uninstall
//     makes the token 401 on every route and deletes every grant and unspent
//     code; Rotate gives the same app a new token, kills the old one, and
//     keeps every subject and grant.
//   - A person's browser mints a code for the app (MintApp). It lives 60 s,
//     is spent once, redeems only with the token of the app it was minted
//     for, and carries the minting session's sign-in time.
//   - The redeem re-checks the person (enabled, not hidden, signed in after
//     any sign-out everywhere, in whole seconds, at-or-before refused) and
//     upserts the grant: the subject is kept, or a new random 128-bit one is
//     drawn (pairwise: one per person per app); signin_at is the code's
//     sign-in time, never now; act_until = max(act_until, now + 8 h). The
//     answer names the subject and display name, never a user id or email.
//   - One predicate, grantState, decides "may this app act for this
//     subject" (live, expired or revoked); owners/check answers it, and
//     every act-for route admits only live. A subject the app never held is
//     absent from owners/check's answer.
//   - Act-for routes (notify, its DELETE, tasks) need exactly one
//     Muster-Act-For header (missing or doubled: 400); an unknown, another
//     app's, expired, hidden, disabled or signed-out subject is one 404 body.
//     A personal token sending the header is 400.
//   - Tasks are matched only through the app's own ledger
//     (app, person, key) → task, and only while that task is open.
//
// Personal (per-owner) tokens are kept only as far as the plugin must refuse
// them: their /self says scope "person", and owners/check is 403.
package mustertest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is the default server app's token (New installs it).
const Token = "mpl_test_server_token_0123456789"

// ActFor is the header a server token names its person with.
const ActFor = "Muster-Act-For"

// GrantLife is how long a redeem lets the app act for its person (fork 3).
const GrantLife = 8 * time.Hour

// CodeLife is a hand-off code's life.
const CodeLife = 60 * time.Second

// App is one server-installed app: what GET /plugin/v1/self says of it.
type App struct {
	Token        string
	PluginID     int64
	PluginName   string
	Capabilities []string
}

// DefaultApp is the app New installs under Token.
var DefaultApp = App{Token: Token, PluginID: 3, PluginName: "example-meetings", Capabilities: []string{"app"}}

// Personal is a per-owner plugin token: the old model, kept so a test can
// show the plugin refuses it.
type Personal struct {
	Token        string
	PluginID     int64
	OwnerID      int64
	OwnerLogin   string
	Capabilities []string
}

// Person is a muster account. ID and Email are what the plugin must never
// learn: the fake never sends either in any answer.
type Person struct {
	ID          int64
	Email       string
	DisplayName string
}

type person struct {
	Person
	disabled  bool
	signedIn  time.Time // the session the person's browser mints with
	signedOut time.Time // the last Sign out everywhere; zero: none
}

type grant struct {
	subject  string
	signinAt *time.Time // nil: a row only Hide made
	actUntil time.Time
	hidden   bool
}

type app struct {
	App
	revoked bool
	grants  map[int64]*grant // user id → grant
	// ledger is the app's own tasks: (user id, key) → task id.
	ledger map[string]int64
	// out is the knocks still out: (user id, ref).
	out map[string]bool
}

type code struct {
	pluginID int64
	userID   int64     // 0 for a MintRaw code
	answer   string    // a MintRaw code's answer, exactly
	signedIn time.Time // the minting session's sign-in
	target   map[string]any
	mintedAt time.Time
	spent    bool
}

// Push is a notification muster sent: a knock, or the withdrawal of one.
type Push struct {
	UserID int64
	Kind   string // "knock" or "resolved"
	Tag    string // app-<plugin id>-<ref>
	Count  int
}

// Task is a task in a person's list.
type Task struct {
	ID       int64
	UserID   int64
	Key      string
	Title    string
	Comments []string
	State    string // "open", "complete", "dismissed", "superseded"
	FiledBy  string // "plugin:<name>", or "person"
}

// Fake is a fake muster server.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Now is the fake's clock; default time.Now.
	Now func() time.Time
	// Off makes every route answer 404, as muster does with plugins off.
	Off bool
	// Failing makes every route answer 503.
	Failing bool
	// FailOwnersCheck makes owners/check alone answer 503.
	FailOwnersCheck bool

	apps     map[string]*app // token → app; a rotated-away token stays, revoked
	byID     map[int64]*app
	personal map[string]*Personal
	people   map[int64]*person
	codes    map[string]*code
	pushes   []Push
	tasks    []*Task
	nextTask int64
	calls    []string
}

// New starts a fake muster with DefaultApp installed; Close it when done.
func New() *Fake {
	f := &Fake{apps: map[string]*app{}, byID: map[int64]*app{}, personal: map[string]*Personal{}, people: map[int64]*person{},
		codes: map[string]*code{}, Now: time.Now, nextTask: 900}
	f.Install(DefaultApp)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /plugin/v1/self", f.gate(f.self))
	mux.HandleFunc("POST /plugin/v1/handoffs/redeem", f.gate(f.redeem))
	mux.HandleFunc("POST /plugin/v1/owners/check", f.gate(f.ownersCheck))
	mux.HandleFunc("POST /plugin/v1/notify", f.gate(f.notify))
	mux.HandleFunc("DELETE /plugin/v1/notify/{ref}", f.gate(f.withdraw))
	mux.HandleFunc("POST /plugin/v1/tasks", f.gate(f.task))
	f.Server = httptest.NewServer(mux)
	return f
}

// Install installs a server app.
func (f *Fake) Install(a App) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := &app{App: a, grants: map[int64]*grant{}, ledger: map[string]int64{}, out: map[string]bool{}}
	x.Capabilities = slices.Clone(a.Capabilities)
	f.apps[a.Token] = x
	f.byID[a.PluginID] = x
}

// Uninstall revokes the app: its token is 401 everywhere, and its grants,
// ledger and unspent codes are deleted.
func (f *Fake) Uninstall(pluginID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.byID[pluginID]
	a.revoked = true
	a.grants = map[int64]*grant{}
	a.ledger = map[string]int64{}
	for k, c := range f.codes {
		if c.pluginID == pluginID && !c.spent {
			delete(f.codes, k)
		}
	}
}

// Rotate gives the app a new token: the old one is 401 at once; subjects
// and grants stay.
func (f *Fake) Rotate(pluginID int64, newToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := f.byID[pluginID]
	dead := *old
	dead.revoked = true
	f.apps[old.Token] = &dead
	old.Token = newToken
	f.apps[newToken] = old
}

// AddPersonal registers a per-owner token (the old model).
func (f *Fake) AddPersonal(p Personal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.personal[p.Token] = &cp
}

// AddPerson adds an account, signed in now.
func (f *Fake) AddPerson(p Person) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[p.ID] = &person{Person: p, signedIn: f.Now()}
}

// SignIn is a new browser session for the person, at now: later mints carry
// this sign-in time.
func (f *Fake) SignIn(userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[userID].signedIn = f.Now()
}

// SignOutEverywhere ends every session the person has, at now.
func (f *Fake) SignOutEverywhere(userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[userID].signedOut = f.Now()
}

// Disable disables the account.
func (f *Fake) Disable(userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[userID].disabled = true
}

// Hide is the person's Hide on Account → Apps: hidden_at and act_until are
// now. A person who never opened the app gets a row with a new subject that
// can never act.
func (f *Fake) Hide(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.byID[pluginID]
	g := a.grants[userID]
	if g == nil {
		g = &grant{subject: newSubject()}
		a.grants[userID] = g
	}
	g.hidden, g.actUntil = true, f.Now()
}

// Show clears hidden_at only: the app may not act again until a redeem.
func (f *Fake) Show(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.byID[pluginID].grants[userID]; g != nil {
		g.hidden = false
	}
}

// Subject is the person's subject under the app, "" when there is none.
func (f *Fake) Subject(pluginID, userID int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if g := f.byID[pluginID].grants[userID]; g != nil {
		return g.subject
	}
	return ""
}

// MintApp mints a code for the app, as the person's browser does on
// muster's /apps/{id} page. ref "" is {"app": {}}.
func (f *Fake) MintApp(pluginID, userID int64, ref string) string {
	target := map[string]any{"app": map[string]any{}}
	if ref != "" {
		target["app"] = map[string]any{"ref": ref}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.people[userID]
	return f.putCode(&code{pluginID: pluginID, userID: userID, signedIn: p.signedIn, target: target})
}

// MintRaw mints a code for pluginID whose redeem answers exactly answer
// (stepped_up and minted_at are added when missing), creating no grant.
// Tests use it to play an answer the plugin must refuse: a view purpose, a
// mirror target, a personal scope, a malformed subject.
func (f *Fake) MintRaw(pluginID int64, answer map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := answer["stepped_up"]; !ok {
		answer["stepped_up"] = false
	}
	if _, ok := answer["minted_at"]; !ok {
		answer["minted_at"] = f.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	return f.putCode(&code{pluginID: pluginID, answer: string(raw)})
}

func (f *Fake) putCode(c *code) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	k := base64.RawURLEncoding.EncodeToString(b)
	c.mintedAt = f.Now()
	f.codes[k] = c
	return k
}

func newSubject() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Calls returns every request so far, as "METHOD path token", with
// " act-for=<v>" for each Muster-Act-For header it carried.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CountCalls counts the requests to path (any token).
func (f *Fake) CountCalls(method, path string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, method+" "+path+" ") {
			n++
		}
	}
	return n
}

// Pushes returns every notification sent so far.
func (f *Fake) Pushes() []Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes)
}

// Tasks returns the person's tasks.
func (f *Fake) Tasks(userID int64) []Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Task
	for _, t := range f.tasks {
		if t.UserID == userID {
			cp := *t
			cp.Comments = slices.Clone(t.Comments)
			out = append(out, cp)
		}
	}
	return out
}

// AddTask puts a task in a person's list that the app did not file (the
// person's own, or another plugin's), tagged key.
func (f *Fake) AddTask(userID int64, key, filedBy string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextTask++
	f.tasks = append(f.tasks, &Task{ID: f.nextTask, UserID: userID, Key: key, Title: "filed elsewhere", State: "open", FiledBy: filedBy})
	return f.nextTask
}

// SetTaskState completes, dismisses or merges away a task.
func (f *Fake) SetTaskState(id int64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tasks {
		if t.ID == id {
			t.State = state
		}
	}
}

// SetFailing makes every route answer 503 (on) or serve again (off).
func (f *Fake) SetFailing(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Failing = on
}

// SetFailOwnersCheck makes owners/check alone answer 503 (on) or serve.
func (f *Fake) SetFailOwnersCheck(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailOwnersCheck = on
}

func write(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	write(w, code, string(b))
}

// caller is the authenticated token: a server app or a personal token.
type caller struct {
	app      *app
	personal *Personal
}

// The one body every act-for refusal gets, whatever the reason.
const noSuchPerson = `{"error":"no such person for this app"}`

func (f *Fake) gate(next func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		line := r.Method + " " + r.URL.Path + " " + tok
		for _, v := range r.Header.Values(ActFor) {
			line += " act-for=" + v
		}
		f.calls = append(f.calls, line)
		off, failing := f.Off, f.Failing
		a := f.apps[tok]
		p := f.personal[tok]
		f.mu.Unlock()
		if off {
			http.NotFound(w, r)
			return
		}
		if failing {
			write(w, 503, `{"error":"unavailable"}`)
			return
		}
		if !bearer || (a == nil && p == nil) || (a != nil && a.revoked) {
			write(w, 401, `{"error":"this route takes a plugin token (mpl_…)"}`)
			return
		}
		// A personal token naming a person is the two models mixed: 400 on
		// every route.
		if p != nil && len(r.Header.Values(ActFor)) > 0 {
			write(w, 400, `{"error":"Muster-Act-For is for a server-installed app's token"}`)
			return
		}
		next(w, r, caller{app: a, personal: p})
	}
}

func (f *Fake) self(w http.ResponseWriter, _ *http.Request, c caller) {
	if c.personal != nil {
		p := c.personal
		writeJSON(w, 200, map[string]any{"plugin_id": p.PluginID, "plugin_name": "personal", "scope": "person", "owner_id": p.OwnerID,
			"owner_login": p.OwnerLogin, "owner_kind": "person", "capabilities": p.Capabilities, "deliver_to": nil})
		return
	}
	f.mu.Lock()
	a := c.app
	out := map[string]any{"plugin_id": a.PluginID, "plugin_name": a.PluginName, "scope": "server", "owner_id": nil, "owner_login": nil,
		"owner_kind": "server", "capabilities": slices.Clone(a.Capabilities), "deliver_to": nil, "act_for_hours": 8}
	f.mu.Unlock()
	writeJSON(w, 200, out)
}

// sec is a time in whole seconds: the cutoff rule compares these.
func sec(t time.Time) int64 { return t.Unix() }

// signedInAfterCutoff is the whole-second rule: the sign-in must be strictly
// after the last Sign out everywhere (at-or-before is refused).
func signedInAfterCutoff(signin time.Time, p *person) bool {
	return p.signedOut.IsZero() || sec(signin) > sec(p.signedOut)
}

// grantState is serverGrantLive, with the reason when it fails: the ONE
// predicate that decides whether the app may act for a person. owners/check
// answers it, and every act-for route admits only "live". "expired" is a
// grant whose only failing gate is act_until; every other failing gate is
// "revoked".
func (f *Fake) grantState(a *app, userID int64, g *grant, now time.Time) string {
	p := f.people[userID]
	if a.revoked || g.hidden || g.signinAt == nil || p == nil || p.disabled || !signedInAfterCutoff(*g.signinAt, p) {
		return "revoked"
	}
	if !g.actUntil.After(now) {
		return "expired"
	}
	return "live"
}

func (f *Fake) grantLive(a *app, userID int64, g *grant, now time.Time) bool {
	return f.grantState(a, userID, g, now) == "live"
}

// Expire ends the person's grant by its clock alone (act_until = now), as
// 8 hours without opening the tab does, while their owner session may still
// be live.
func (f *Fake) Expire(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].grants[userID].actUntil = f.Now()
}

func (f *Fake) redeem(w http.ResponseWriter, r *http.Request, c caller) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		write(w, 400, `{"error":"the body is {\"code\": \"<muster_code>\"}"}`)
		return
	}
	const missing = `{"error":"no such hand-off code (unknown, used, expired, or another plugin's)"}`
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	k, ok := f.codes[strings.TrimSpace(in.Code)]
	if c.personal != nil {
		// A personal token can spend no server app's code.
		write(w, 404, missing)
		return
	}
	a := c.app
	if !ok || k.spent || k.pluginID != a.PluginID || now.Sub(k.mintedAt) > CodeLife {
		write(w, 404, missing)
		return
	}
	k.spent = true
	if k.answer != "" {
		write(w, 200, k.answer)
		return
	}
	// The person, re-checked on a fresh read: the mint's checks are not
	// trusted.
	p := f.people[k.userID]
	g := a.grants[k.userID]
	if p == nil || p.disabled || (g != nil && g.hidden) || !signedInAfterCutoff(k.signedIn, p) {
		write(w, 404, missing)
		return
	}
	if g == nil {
		g = &grant{subject: newSubject()}
		a.grants[k.userID] = g
	}
	signin := k.signedIn
	g.signinAt = &signin // the code's sign-in, never now
	if until := now.Add(GrantLife); until.After(g.actUntil) {
		g.actUntil = until
	}
	writeJSON(w, 200, map[string]any{
		"owner": map[string]any{"subject": g.subject, "display_name": p.DisplayName},
		"scope": "server", "target": k.target, "purpose": "app", "stepped_up": false,
		"minted_at": k.mintedAt.UTC().Format(time.RFC3339), "act_until": g.actUntil.UTC().Format(time.RFC3339),
	})
}

var subjectRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

func (f *Fake) ownersCheck(w http.ResponseWriter, r *http.Request, c caller) {
	if c.personal != nil {
		write(w, 403, `{"error":"owners/check is for a server-installed app's token"}`)
		return
	}
	var in struct {
		Subjects []string `json:"subjects"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil || len(in.Subjects) < 1 || len(in.Subjects) > 200 {
		write(w, 400, `{"error":"the body is {\"subjects\": [1 to 200 subjects]}"}`)
		return
	}
	for _, s := range in.Subjects {
		if !subjectRE.MatchString(s) {
			write(w, 400, `{"error":"a subject is 22 base64url characters"}`)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOwnersCheck {
		write(w, 503, `{"error":"unavailable"}`)
		return
	}
	now := f.Now()
	// A subject the app never held is left out of the answer.
	states := map[string]string{}
	for _, s := range in.Subjects {
		for uid, g := range c.app.grants {
			if g.subject == s {
				states[s] = f.grantState(c.app, uid, g, now)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"states": states})
}

// actFor resolves the Muster-Act-For header to the person it names, or
// answers the refusal itself and returns 0.
func (f *Fake) actFor(w http.ResponseWriter, r *http.Request, c caller) int64 {
	if c.personal != nil {
		write(w, 403, `{"error":"this fake serves act-for routes to server apps only"}`)
		return 0
	}
	vs := r.Header.Values(ActFor)
	if len(vs) != 1 {
		write(w, 400, `{"error":"a server app names its person with exactly one Muster-Act-For header"}`)
		return 0
	}
	now := f.Now()
	for uid, g := range c.app.grants {
		// The person comes from the grant row, never from the header.
		if g.subject == vs[0] && f.grantLive(c.app, uid, g, now) {
			return uid
		}
	}
	write(w, 404, noSuchPerson)
	return 0
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (f *Fake) notify(w http.ResponseWriter, r *http.Request, c caller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.app != nil && !slices.Contains(c.app.Capabilities, "app") {
		write(w, 403, `{"error":"this plugin token lacks the app capability"}`)
		return
	}
	uid := f.actFor(w, r, c)
	if uid == 0 {
		return
	}
	var in map[string]json.RawMessage
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	if err := dec.Decode(&in); err != nil || len(in) != 3 || string(in["kind"]) != `"knock"` {
		write(w, 400, `{"error":"the body is {\"kind\": \"knock\", \"ref\", \"count\"}"}`)
		return
	}
	var ref string
	var count int
	if json.Unmarshal(in["ref"], &ref) != nil || !refRE.MatchString(ref) || json.Unmarshal(in["count"], &count) != nil || count < 1 || count > 99 {
		write(w, 400, `{"error":"ref is ^[A-Za-z0-9_-]{1,64}$ and count 1 to 99"}`)
		return
	}
	f.pushes = append(f.pushes, Push{UserID: uid, Kind: "knock", Tag: fmt.Sprintf("app-%d-%s", c.app.PluginID, ref), Count: count})
	c.app.out[strconv.FormatInt(uid, 10)+"/"+ref] = true
	write(w, 202, `{}`)
}

func (f *Fake) withdraw(w http.ResponseWriter, r *http.Request, c caller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid := f.actFor(w, r, c)
	if uid == 0 {
		return
	}
	ref := r.PathValue("ref")
	k := strconv.FormatInt(uid, 10) + "/" + ref // keyed per person: B cannot withdraw A's
	if !c.app.out[k] {
		w.WriteHeader(204)
		return
	}
	delete(c.app.out, k)
	f.pushes = append(f.pushes, Push{UserID: uid, Kind: "resolved", Tag: fmt.Sprintf("app-%d-%s", c.app.PluginID, ref)})
	write(w, 202, `{}`)
}

func (f *Fake) task(w http.ResponseWriter, r *http.Request, c caller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.app != nil && !slices.Contains(c.app.Capabilities, "tasks") {
		write(w, 403, `{"error":"this plugin token lacks the tasks capability"}`)
		return
	}
	uid := f.actFor(w, r, c)
	if uid == 0 {
		return
	}
	var in struct {
		Key    string `json:"key"`
		Create *struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"create"`
		Comment string `json:"comment"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields() // route_to_chief and reference are refused
	if err := dec.Decode(&in); err != nil || !strings.Contains(in.Key, ":") || (in.Create == nil && strings.TrimSpace(in.Comment) == "") ||
		(in.Create != nil && strings.TrimSpace(in.Create.Body) == "") {
		write(w, 400, `{"error":"the body is {\"key\", \"create\": {\"title\", \"body\"}, \"comment\"}"}`)
		return
	}
	lk := strconv.FormatInt(uid, 10) + "/" + in.Key
	var t *Task
	if id, ok := c.app.ledger[lk]; ok {
		for _, x := range f.tasks {
			if x.ID == id && x.State == "open" {
				t = x
			}
		}
	}
	comment := strings.TrimSpace(in.Comment)
	if t == nil {
		if in.Create == nil {
			write(w, 200, `{"id":null,"created":false,"commented":false}`)
			return
		}
		f.nextTask++
		t = &Task{ID: f.nextTask, UserID: uid, Key: in.Key, Title: in.Create.Title, State: "open", FiledBy: "plugin:" + c.app.PluginName}
		if comment != "" {
			t.Comments = append(t.Comments, comment)
		}
		f.tasks = append(f.tasks, t)
		c.app.ledger[lk] = t.ID // inserted, or re-pointed
		writeJSON(w, 201, map[string]any{"id": t.ID, "created": true, "commented": comment != ""})
		return
	}
	commented := false
	if comment != "" && !slices.Contains(t.Comments, comment) {
		t.Comments = append(t.Comments, comment)
		commented = true
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "created": false, "commented": commented})
}
