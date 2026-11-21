// Package mustertest is a fake muster for tests: the /plugin/v1 routes a
// server-installed app plugin calls, answered as muster core answers them
// (core PR 1h: internal/api/plugin_server.go, plugin_notify.go,
// plugin_tasks.go and internal/plugins/server.go), recording every request it
// was sent. Where a 2b or phase-6 test could pass against a lenient fake and
// fail against core, the fake is as strict as core.
//
//   - A server app is installed once (Install) with one token. Uninstall
//     makes the token 401 on every route and deletes every grant, ledger row
//     and unspent code; Rotate gives the same app a new token, kills the old
//     one, and keeps every subject and grant.
//   - THE DOOR, in core's order: 401 for a dead or unknown token; a personal
//     token carrying Muster-Act-For is 400 on every route; for a server
//     token, a route the door does not serve is 403, then a capability the
//     route needs and the app lacks is 403, then the header's shape (exactly
//     one on an act-for route, none on any other) is 400, then the act-for
//     lookup (a failure is 503) admits only a live grant: anything else is
//     the one 404 body.
//   - A person's browser mints a code for the app (MintApp). It lives 60 s
//     (refused at exactly 60 s), is spent once, redeems only with the token
//     of the app it was minted for, and carries the minting session's
//     sign-in time. The redeem re-checks the person against every revoke
//     cause and upserts the grant: the subject is kept, or a new random
//     128-bit one is drawn (pairwise); signin_at is the code's sign-in time,
//     never now; act_until = max(act_until, now + 8 h).
//   - One predicate, grantState, decides "may this app act for this
//     subject": live, expired (act_until alone has passed) or revoked
//     (hidden, a Hide-only row, the app uninstalled or no longer a server
//     app, its origin retired, the account disabled or not a person, signed
//     out everywhere or cut off by sessions_not_before since the sign-in,
//     whole seconds, at-or-before refused). owners/check answers EVERY asked
//     subject, one that this app never redeemed as revoked, 60 calls a minute
//     per app (429 with Retry-After).
//   - Notify and its withdrawal: core's answers and caps (body 400, push not
//     configured 503, muted or no device 204, 429 for the same ref in 30 s,
//     past 30 an hour per person and 600 an hour per app; withdrawals of
//     nothing capped at 60 an hour per person; 202 {"queued", "tag"}).
//   - Tasks: 503 with no namespaces configured; the key a normalised
//     <namespace>:<value> in one of them, else 400; title and body trimmed,
//     never refused; 60 writes an hour per person and 600 per app (429 with
//     Retry-After, a write that wrote nothing spends nothing); matched only
//     through the app's own ledger (app, person, key) → open task.
//
// Personal (per-owner) tokens are kept only as far as the plugin must refuse
// them: their /self has no scope field, and owners/check is 403.
package mustertest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Token is the default server app's token (New installs it).
const Token = "mpl_test_server_token_0123456789"

// ActFor is the header a server token names its person with.
const ActFor = "Muster-Act-For"

// Core's numbers.
const (
	GrantLife            = 8 * time.Hour    // how long a redeem lets the app act (fork 3)
	CodeLife             = 60 * time.Second // a hand-off code's life
	OwnersCheckPerMinute = 60               // owners/check calls per app per minute
	MaxOwnersCheck       = 200              // subjects per owners/check
	KnockRefWindow       = 30 * time.Second // one knock per (app, person, ref)
	KnockPerPersonHour   = 30               // knocks per (app, person) an hour
	KnockPerAppHour      = 600              // knocks per server app an hour
	GhostWithdrawalsHour = 60               // withdrawals of nothing per (app, person) an hour
	TaskPerPersonHour    = 60               // task writes per (app, person) an hour
	TaskPerAppHour       = 600              // task writes per server app an hour
	MaxTaskTitleRunes    = 512
	MaxTaskBodyBytes     = 16 << 10
)

// The refusal bodies core writes.
const (
	ServerRouteRefusal = "this is a server app's token: it reads /self, redeems its own hand-offs, checks its people and knocks or files tasks for one of them, and nothing else"
	ActForPersonal     = "Muster-Act-For is a server app's header: a personal plugin token acts as its own owner and names no one"
	ActForShape        = "this route needs exactly one Muster-Act-For header naming a subject (and only an act-for route takes one)"
	NoSuchPerson       = "no such person for this app"
)

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
	kind      string // "person", or "team"
	disabled  bool
	signedIn  time.Time // the session the person's browser mints with
	signedOut time.Time // the last Sign out everywhere; zero: none
	notBefore time.Time // sessions_not_before; zero: none
	muted     bool      // "An app needs you" turned off
	devices   int
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
	scope   string // "server"; anything else revokes every grant
	retired bool   // its origin left MUSTER_PLUGIN_ORIGINS
	grants  map[int64]*grant
	ledger  map[string]int64 // (user id, key) → task id
	// The caps' windows and the knocks still out.
	checks  []time.Time
	refs    map[string]time.Time  // (user, ref) → last knock
	sends   map[int64][]time.Time // user → knocks
	ceiling []time.Time           // the app's knocks
	out     map[string]time.Time  // (user, ref) → knock still out
	ghosts  map[int64][]time.Time // user → withdrawals of nothing
	tasksBy map[int64][]time.Time // user → task writes
	tasksAt []time.Time           // the app's task writes
}

type code struct {
	pluginID int64
	userID   int64
	answer   string
	signedIn time.Time
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
	Body     string
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
	// FailOwnersCheckCall makes the nth owners/check call (1-based, counted
	// from New) answer 503, and only it.
	FailOwnersCheckCall int
	// OwnersCheckBody, when set, is owners/check's 200 answer, verbatim.
	OwnersCheckBody string
	// OnOwnersCheck, when set, runs after owners/check's answer is decided
	// and before it is written, with the fake unlocked.
	OnOwnersCheck func()
	// OwnersCheckLimit is owners/check's calls per app per minute (core:
	// OwnersCheckPerMinute, New's default). A test lowers it to reach the
	// limit without thousands of subjects.
	OwnersCheckLimit int
	// PushUnconfigured is a muster without native push: notify and its
	// withdrawal answer 503.
	PushUnconfigured bool
	// TaskNamespaces is MUSTER_PLUGIN_TASK_NAMESPACES. New sets ["meetings"],
	// the namespace the plugin files under; empty makes POST /plugin/v1/tasks
	// 503, as core is with the variable unset.
	TaskNamespaces []string
	// FailActForLookup makes the act-for door's grant lookup fail: 503.
	FailActForLookup bool

	ownersChecks int
	checked      map[string]int
	apps         map[string]*app
	byID         map[int64]*app
	personal     map[string]*Personal
	people       map[int64]*person
	codes        map[string]*code
	pushes       []Push
	tasks        []*Task
	nextTask     int64
	nextPerson   int64
	calls        []string
}

// New starts a fake muster with DefaultApp installed; Close it when done.
func New() *Fake {
	f := &Fake{apps: map[string]*app{}, byID: map[int64]*app{}, personal: map[string]*Personal{}, people: map[int64]*person{},
		codes: map[string]*code{}, checked: map[string]int{}, Now: time.Now, nextTask: 900, nextPerson: 7_000_000_000,
		OwnersCheckLimit: OwnersCheckPerMinute, TaskNamespaces: []string{"meetings"}}
	f.Install(DefaultApp)
	mux := http.NewServeMux()
	served := map[string]struct {
		need   string // "" any; "app"; "tasks"
		actFor bool
		h      func(http.ResponseWriter, *http.Request, caller, int64)
	}{
		"GET /plugin/v1/self":             {"", false, f.self},
		"POST /plugin/v1/handoffs/redeem": {"app", false, f.redeem},
		"POST /plugin/v1/owners/check":    {"", false, f.ownersCheck},
		"POST /plugin/v1/notify":          {"app", true, f.notify},
		"DELETE /plugin/v1/notify/{ref}":  {"app", true, f.withdraw},
		"POST /plugin/v1/tasks":           {"tasks", true, f.task},
	}
	for pattern, r := range served {
		mux.HandleFunc(pattern, f.gate(true, r.need, r.actFor, r.h))
	}
	// Routes core serves personal tokens only: a server token is 403.
	for _, pattern := range []string{"POST /plugin/v1/messages", "GET /plugin/v1/tasks", "POST /plugin/v1/drafts/claim",
		"PUT /plugin/v1/chief/instructions", "POST /plugin/v1/task-writes/claim"} {
		mux.HandleFunc(pattern, f.gate(false, "", false, nil))
	}
	f.Server = httptest.NewServer(mux)
	return f
}

func newApp(a App) *app {
	x := &app{App: a, scope: "server", grants: map[int64]*grant{}, ledger: map[string]int64{},
		refs: map[string]time.Time{}, sends: map[int64][]time.Time{}, out: map[string]time.Time{},
		ghosts: map[int64][]time.Time{}, tasksBy: map[int64][]time.Time{}}
	x.Capabilities = slices.Clone(a.Capabilities)
	return x
}

// Install installs a server app.
func (f *Fake) Install(a App) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x := newApp(a)
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
	f.apps[old.Token] = &app{App: old.App, revoked: true}
	old.Token = newToken
	f.apps[newToken] = old
}

// RetireOrigin takes the app's origin out of MUSTER_PLUGIN_ORIGINS: every
// grant is revoked (the token still authenticates).
func (f *Fake) RetireOrigin(pluginID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].retired = true
}

// SetScope changes the app row's scope: anything but "server" revokes every
// grant.
func (f *Fake) SetScope(pluginID int64, scope string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].scope = scope
}

// AddPersonal registers a per-owner token (the old model).
func (f *Fake) AddPersonal(p Personal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.personal[p.Token] = &cp
}

// AddPerson adds an account, signed in now, with one device.
func (f *Fake) AddPerson(p Person) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[p.ID] = &person{Person: p, kind: "person", signedIn: f.Now(), devices: 1}
}

// AddGrants adds n people, each with a live grant under the app (as if
// each had just opened the tab), and returns their subjects.
func (f *Fake) AddGrants(pluginID int64, n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	a := f.byID[pluginID]
	out := make([]string, n)
	for i := range out {
		f.nextPerson++
		f.people[f.nextPerson] = &person{Person: Person{ID: f.nextPerson}, kind: "person", signedIn: now, devices: 1}
		in := now
		g := &grant{subject: newSubject(), signinAt: &in, actUntil: now.Add(GrantLife)}
		a.grants[f.nextPerson] = g
		out[i] = g.subject
	}
	return out
}

func (f *Fake) withPerson(userID int64, fn func(p *person)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.people[userID])
}

// SignIn is a new browser session for the person, at now.
func (f *Fake) SignIn(userID int64) { f.withPerson(userID, func(p *person) { p.signedIn = f.Now() }) }

// SignOutEverywhere ends every session the person has, at now.
func (f *Fake) SignOutEverywhere(userID int64) {
	f.withPerson(userID, func(p *person) { p.signedOut = f.Now() })
}

// SetSessionsNotBefore moves the person's session cutoff to now (core's
// sessions_not_before).
func (f *Fake) SetSessionsNotBefore(userID int64) {
	f.withPerson(userID, func(p *person) { p.notBefore = f.Now() })
}

// Disable disables the account.
func (f *Fake) Disable(userID int64) { f.withPerson(userID, func(p *person) { p.disabled = true }) }

// MakeTeam makes the account a team's: not a person.
func (f *Fake) MakeTeam(userID int64) { f.withPerson(userID, func(p *person) { p.kind = "team" }) }

// Mute turns the person's "An app needs you" notifications off (on false).
func (f *Fake) Mute(userID int64, on bool) { f.withPerson(userID, func(p *person) { p.muted = on }) }

// SetDevices sets how many devices the person has for push.
func (f *Fake) SetDevices(userID int64, n int) {
	f.withPerson(userID, func(p *person) { p.devices = n })
}

// Hide is the person's Hide on Account → Apps: hidden_at and act_until are
// now. A person who never opened the app gets a row that can never act.
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

// Expire ends the person's grant by its clock alone (act_until = now).
func (f *Fake) Expire(pluginID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[pluginID].grants[userID].actUntil = f.Now()
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
	return f.putCode(&code{pluginID: pluginID, userID: userID, signedIn: f.people[userID].signedIn, target: target})
}

// MintRaw mints a code for pluginID whose redeem answers exactly answer
// (stepped_up and minted_at are added when missing), creating no grant.
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

// Checked returns how many times owners/check answered each subject.
func (f *Fake) Checked() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.checked))
	for k, v := range f.checked {
		out[k] = v
	}
	return out
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

// AddTask puts a task in a person's list that the app did not file.
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
func (f *Fake) SetFailing(on bool) { f.set(func() { f.Failing = on }) }

// SetFailOwnersCheck makes owners/check alone answer 503 (on) or serve.
func (f *Fake) SetFailOwnersCheck(on bool) { f.set(func() { f.FailOwnersCheck = on }) }

// SetOwnersCheck sets how owners/check answers: failCall fails the nth call
// (0: none), body is a verbatim 200 answer ("": the real one), and hook runs
// before the answer is written (nil: none).
func (f *Fake) SetOwnersCheck(failCall int, body string, hook func()) {
	f.set(func() { f.FailOwnersCheckCall, f.OwnersCheckBody, f.OnOwnersCheck = failCall, body, hook })
}

// SetOwnersCheckLimit sets owners/check's calls per app per minute.
func (f *Fake) SetOwnersCheckLimit(n int) { f.set(func() { f.OwnersCheckLimit = n }) }

// SetPushConfigured turns native push on (true) or off.
func (f *Fake) SetPushConfigured(on bool) { f.set(func() { f.PushUnconfigured = !on }) }

// SetTaskNamespaces sets MUSTER_PLUGIN_TASK_NAMESPACES.
func (f *Fake) SetTaskNamespaces(ns ...string) { f.set(func() { f.TaskNamespaces = ns }) }

// SetFailActForLookup makes the act-for door's lookup fail (503).
func (f *Fake) SetFailActForLookup(on bool) { f.set(func() { f.FailActForLookup = on }) }

func (f *Fake) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
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

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// tooMany answers 429 with Retry-After, core's shape.
func tooMany(w http.ResponseWriter, until time.Duration, msg string) {
	wait := int(math.Ceil(until.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	writeJSON(w, 429, map[string]any{"error": msg, "retry_after": wait})
}

// trim drops the times at least window before now (oldest first).
func trim(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	return ts[i:]
}

// over is how long until ts (trimmed) holds fewer than limit, or 0.
func over(ts []time.Time, now time.Time, window time.Duration, limit int) time.Duration {
	if len(ts) < limit {
		return 0
	}
	return window - now.Sub(ts[len(ts)-limit])
}

// caller is the authenticated token: a server app or a personal token.
type caller struct {
	app      *app
	personal *Personal
}

// gate is the door. served is whether a server token is served the route;
// need is the capability the route needs (any, "app", "tasks"); actFor is
// whether the route acts for one person.
func (f *Fake) gate(served bool, need string, actFor bool, next func(http.ResponseWriter, *http.Request, caller, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hdr := r.Header.Values(ActFor)
		f.mu.Lock()
		line := r.Method + " " + r.URL.Path + " " + tok
		for _, v := range hdr {
			line += " act-for=" + v
		}
		f.calls = append(f.calls, line)
		off, failing, lookupFails := f.Off, f.Failing, f.FailActForLookup
		a := f.apps[tok]
		p := f.personal[tok]
		f.mu.Unlock()
		if off {
			http.NotFound(w, r)
			return
		}
		if failing {
			writeErr(w, 503, "unavailable")
			return
		}
		if !bearer || (a == nil && p == nil) || (a != nil && a.revoked) {
			writeErr(w, 401, "this route takes a plugin token (mpl_…)")
			return
		}
		if p != nil {
			if len(hdr) > 0 {
				writeErr(w, 400, ActForPersonal)
				return
			}
			if next == nil {
				writeErr(w, 501, "the fake serves this route to no personal token")
				return
			}
			next(w, r, caller{personal: p}, 0)
			return
		}
		if !served {
			writeErr(w, 403, ServerRouteRefusal)
			return
		}
		if need != "" && !slices.Contains(a.Capabilities, need) {
			writeErr(w, 403, "this plugin token lacks the "+need+" capability")
			return
		}
		if actFor != (len(hdr) == 1) || len(hdr) > 1 {
			writeErr(w, 400, ActForShape)
			return
		}
		var uid int64
		if actFor {
			if lookupFails {
				writeErr(w, 503, "could not check who this app acts for")
				return
			}
			f.mu.Lock()
			if subjectRE.MatchString(hdr[0]) {
				now := f.Now()
				for id, g := range a.grants {
					// The person comes from the grant row, never from the header.
					if g.subject == hdr[0] && f.grantState(a, id, g, now) == "live" {
						uid = id
					}
				}
			}
			f.mu.Unlock()
			if uid == 0 {
				writeErr(w, 404, NoSuchPerson)
				return
			}
		}
		next(w, r, caller{app: a}, uid)
	}
}

func (f *Fake) self(w http.ResponseWriter, _ *http.Request, c caller, _ int64) {
	if c.personal != nil {
		p := c.personal
		// A personal token's /self as core writes it: no scope field.
		writeJSON(w, 200, map[string]any{"plugin_id": p.PluginID, "plugin_name": "personal", "owner_id": p.OwnerID,
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

// after is the whole-second rule: a sign-in strictly after the cutoff
// passes (at-or-before is refused); no cutoff passes.
func after(signin, cutoff time.Time) bool { return cutoff.IsZero() || sec(signin) > sec(cutoff) }

// standing is everything but the clock that must hold for a person: the
// account enabled and a person, and the sign-in after both cutoffs.
func standing(p *person, signin time.Time) bool {
	return p != nil && !p.disabled && p.kind == "person" && after(signin, p.signedOut) && after(signin, p.notBefore)
}

// grantState is core's serverGrantLive, split as core splits it: the ONE
// predicate that decides whether the app may act for a person. "expired" is
// a grant whose only failing gate is act_until; every other failing gate is
// "revoked". owners/check answers it; the act-for door admits only "live".
func (f *Fake) grantState(a *app, userID int64, g *grant, now time.Time) string {
	if a.revoked || a.scope != "server" || a.retired || g.hidden || g.signinAt == nil || !standing(f.people[userID], *g.signinAt) {
		return "revoked"
	}
	if !g.actUntil.After(now) {
		return "expired"
	}
	return "live"
}

func (f *Fake) redeem(w http.ResponseWriter, r *http.Request, c caller, _ int64) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in); err != nil {
		writeErr(w, 400, `the body is {"code": "<muster_code>"}`)
		return
	}
	const missing = "no such hand-off code (unknown, used, expired, or another plugin's)"
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	k, ok := f.codes[strings.TrimSpace(in.Code)]
	if c.personal != nil {
		writeErr(w, 404, missing) // a personal token can spend no server app's code
		return
	}
	a := c.app
	// expires_at > now(): a code is refused at exactly CodeLife.
	if !ok || k.spent || k.pluginID != a.PluginID || !k.mintedAt.Add(CodeLife).After(now) {
		writeErr(w, 404, missing)
		return
	}
	k.spent = true
	if k.answer != "" {
		write(w, 200, k.answer)
		return
	}
	// The person, re-checked on a fresh read against every revoke cause.
	p := f.people[k.userID]
	g := a.grants[k.userID]
	if a.scope != "server" || a.retired || (g != nil && g.hidden) || !standing(p, k.signedIn) {
		writeErr(w, 404, missing)
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

func (f *Fake) ownersCheck(w http.ResponseWriter, r *http.Request, c caller, _ int64) {
	if c.personal != nil {
		writeErr(w, 403, "owners/check is for a server-installed app's token")
		return
	}
	var in struct {
		Subjects []string `json:"subjects"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxOwnersCheck*26+64))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || len(in.Subjects) < 1 || len(in.Subjects) > MaxOwnersCheck {
		writeErr(w, 400, `the body is {"subjects": [1-200 subjects]}`)
		return
	}
	for _, s := range in.Subjects {
		if !subjectRE.MatchString(s) {
			writeErr(w, 400, "each subject is the 22-character subject a redeem gave")
			return
		}
	}
	f.mu.Lock()
	now := f.Now()
	a := c.app
	a.checks = trim(a.checks, now, time.Minute)
	if until := over(a.checks, now, time.Minute, f.OwnersCheckLimit); until > 0 {
		f.mu.Unlock()
		tooMany(w, until, "too many owners checks this minute")
		return
	}
	a.checks = append(a.checks, now)
	f.ownersChecks++
	if f.FailOwnersCheck || f.ownersChecks == f.FailOwnersCheckCall {
		f.mu.Unlock()
		writeErr(w, 503, "could not check the subjects")
		return
	}
	if f.OwnersCheckBody != "" {
		body := f.OwnersCheckBody
		f.mu.Unlock()
		write(w, 200, body)
		return
	}
	// EVERY asked subject, once: one this app never redeemed is "revoked",
	// as any revoked one is, so the answer cannot tell "never seen" from
	// "ended".
	states := make(map[string]string, len(in.Subjects))
	for _, s := range in.Subjects {
		states[s] = "revoked"
		for uid, g := range a.grants {
			if g.subject == s {
				states[s] = f.grantState(a, uid, g, now)
			}
		}
		f.checked[s]++
	}
	hook := f.OnOwnersCheck
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	writeJSON(w, 200, map[string]any{"states": states})
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func tag(a *app, ref string) string { return fmt.Sprintf("app-%d-%s", a.PluginID, ref) }

func (f *Fake) notify(w http.ResponseWriter, r *http.Request, c caller, uid int64) {
	var in struct {
		Kind  string `json:"kind"`
		Ref   string `json:"ref"`
		Count int    `json:"count"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, 400, `the body is {"kind": "knock", "ref": "<id>", "count": 1-99} and nothing else`)
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeErr(w, 400, "the body is one JSON object")
		return
	}
	switch {
	case in.Kind != "knock":
		writeErr(w, 400, `kind must be "knock"`)
		return
	case !refRE.MatchString(in.Ref):
		writeErr(w, 400, "ref must be 1-64 of A-Z a-z 0-9 _ -")
		return
	case in.Count < 1 || in.Count > 99:
		writeErr(w, 400, "count must be 1-99")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PushUnconfigured {
		writeErr(w, 503, "push is not configured on this server")
		return
	}
	a, p, now := c.app, f.people[uid], f.Now()
	if p.muted || p.devices == 0 {
		w.WriteHeader(204) // muted, or no device: dropped, spends no cap
		return
	}
	key := strconv.FormatInt(uid, 10) + "/" + in.Ref
	a.sends[uid] = trim(a.sends[uid], now, time.Hour)
	a.ceiling = trim(a.ceiling, now, time.Hour)
	var until time.Duration
	if at, seen := a.refs[key]; seen && now.Sub(at) < KnockRefWindow {
		until = max(until, KnockRefWindow-now.Sub(at))
	}
	until = max(until, over(a.sends[uid], now, time.Hour, KnockPerPersonHour), over(a.ceiling, now, time.Hour, KnockPerAppHour))
	if until > 0 {
		tooMany(w, until, "the app's knock cap is reached")
		return
	}
	a.refs[key] = now
	a.sends[uid] = append(a.sends[uid], now)
	a.ceiling = append(a.ceiling, now)
	a.out[key] = now
	f.pushes = append(f.pushes, Push{UserID: uid, Kind: "knock", Tag: tag(a, in.Ref), Count: in.Count})
	writeJSON(w, 202, map[string]any{"queued": true, "tag": tag(a, in.Ref)})
}

func (f *Fake) withdraw(w http.ResponseWriter, r *http.Request, c caller, uid int64) {
	ref := r.PathValue("ref")
	if !refRE.MatchString(ref) {
		writeErr(w, 400, "ref must be 1-64 of A-Z a-z 0-9 _ -")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PushUnconfigured {
		writeErr(w, 503, "push is not configured on this server")
		return
	}
	a, now := c.app, f.Now()
	key := strconv.FormatInt(uid, 10) + "/" + ref // per person: B cannot withdraw A's
	if at, out := a.out[key]; out && now.Sub(at) < time.Hour {
		delete(a.out, key)
		f.pushes = append(f.pushes, Push{UserID: uid, Kind: "resolved", Tag: tag(a, ref)})
		writeJSON(w, 202, map[string]any{"queued": true, "tag": tag(a, ref)})
		return
	}
	a.ghosts[uid] = trim(a.ghosts[uid], now, time.Hour)
	if until := over(a.ghosts[uid], now, time.Hour, GhostWithdrawalsHour); until > 0 {
		tooMany(w, until, "too many withdrawals of notifications that are not out")
		return
	}
	a.ghosts[uid] = append(a.ghosts[uid], now)
	w.WriteHeader(204)
}

// normalTag is core's NormalizeTag: whitespace runs to "-", lower-cased.
func normalTag(raw string) string { return strings.ToLower(strings.Join(strings.Fields(raw), "-")) }

func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (f *Fake) task(w http.ResponseWriter, r *http.Request, c caller, uid int64) {
	f.mu.Lock()
	namespaces := slices.Clone(f.TaskNamespaces)
	f.mu.Unlock()
	if len(namespaces) == 0 {
		writeErr(w, 503, "this server files no plugin tasks: MUSTER_PLUGIN_TASK_NAMESPACES is empty")
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
	dec := json.NewDecoder(io.LimitReader(r.Body, 128<<10))
	dec.DisallowUnknownFields() // route_to_chief and reference: refused for a server app
	if err := dec.Decode(&in); err != nil {
		writeErr(w, 400, `the body is {"key", "create": {"title", "body"}, "comment"}`)
		return
	}
	// Trimmed, never refused.
	in.Comment = truncBytes(strings.TrimSpace(in.Comment), MaxTaskBodyBytes)
	if in.Create != nil {
		in.Create.Body = truncBytes(in.Create.Body, MaxTaskBodyBytes)
		in.Create.Title = capRunes(in.Create.Title, MaxTaskTitleRunes)
	}
	ns, val, _ := strings.Cut(in.Key, ":")
	switch {
	case in.Key == "" || normalTag(in.Key) != in.Key:
		writeErr(w, 400, "key is required and must be a normalized tag (lower-case namespace:value)")
		return
	case !strings.Contains(in.Key, ":") || !slices.Contains(namespaces, ns) || val == "":
		writeErr(w, 400, "key must be <namespace>:<id> in one of this server's plugin task namespaces ("+strings.Join(namespaces, ", ")+")")
		return
	case in.Create == nil && in.Comment == "":
		writeErr(w, 400, "give create, a non-blank comment, or both")
		return
	case in.Create != nil && strings.TrimSpace(in.Create.Body) == "":
		writeErr(w, 400, "create.body must not be blank")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a, now := c.app, f.Now()
	a.tasksBy[uid] = trim(a.tasksBy[uid], now, time.Hour)
	a.tasksAt = trim(a.tasksAt, now, time.Hour)
	if until := max(over(a.tasksBy[uid], now, time.Hour, TaskPerPersonHour), over(a.tasksAt, now, time.Hour, TaskPerAppHour)); until > 0 {
		tooMany(w, until, "too many task writes this hour")
		return
	}
	wrote := func() { // a write that wrote nothing spends nothing
		a.tasksBy[uid] = append(a.tasksBy[uid], now)
		a.tasksAt = append(a.tasksAt, now)
	}
	lk := strconv.FormatInt(uid, 10) + "/" + in.Key
	var t *Task
	if id, ok := a.ledger[lk]; ok {
		for _, x := range f.tasks {
			if x.ID == id && x.State == "open" {
				t = x
			}
		}
	}
	if t == nil {
		if in.Create == nil {
			write(w, 200, `{"id":null,"created":false,"commented":false}`)
			return
		}
		f.nextTask++
		t = &Task{ID: f.nextTask, UserID: uid, Key: in.Key, Title: in.Create.Title, Body: in.Create.Body, State: "open", FiledBy: "plugin:" + a.PluginName}
		if in.Comment != "" {
			t.Comments = append(t.Comments, in.Comment)
		}
		f.tasks = append(f.tasks, t)
		a.ledger[lk] = t.ID // inserted, or re-pointed
		wrote()
		writeJSON(w, 201, map[string]any{"id": t.ID, "created": true, "commented": in.Comment != ""})
		return
	}
	commented := false
	if in.Comment != "" && !slices.Contains(t.Comments, in.Comment) {
		t.Comments = append(t.Comments, in.Comment)
		commented = true
		wrote()
	}
	writeJSON(w, 200, map[string]any{"id": t.ID, "created": false, "commented": commented})
}
