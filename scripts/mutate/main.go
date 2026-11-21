// Command mutate is the security-gate mutation sweep (design: "Mutation and
// negative controls for the security gates"). Each mutant is one exact text
// replacement in one file; it is applied, the ONE test named to kill it is
// run, and the file is put back byte for byte. A mutant counts as killed only
// when that test fails AND its output carries the mutant's expected message,
// so a mutant that breaks the build, panics elsewhere, or trips a different
// assertion is reported, not counted.
//
// Two controls run in every sweep: a known-caught mutant (it must be killed)
// and a no-op edit (it must survive), so a sweep that cannot tell the two
// apart fails as a whole.
//
//	go run ./scripts/mutate             # every mutant
//	go run ./scripts/mutate -list       # names only
//	go run ./scripts/mutate -match re   # the mutants whose name matches re, and the two controls
//
// It exits non-zero unless every security mutant is killed and both controls
// behave. Run it from the repository root. It refuses a dirty tree (`git
// status --porcelain` not empty), so a mutant can never be left behind
// mixed into uncommitted work, and on SIGINT or SIGTERM it puts the file it
// is mutating back before it exits.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
)

// live is the files a mutant is applied to right now, and their original
// bytes: what a signal must put back.
var live struct {
	sync.Mutex
	orig map[string][]byte
}

func setLive(orig map[string][]byte) {
	live.Lock()
	live.orig = orig
	live.Unlock()
}

// restoreOnSignal puts the mutated file back on SIGINT or SIGTERM, then exits.
func restoreOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-ch
		live.Lock() // held to exit: no mutant is applied after this
		for f, b := range live.orig {
			if err := os.WriteFile(f, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "mutate: %v: COULD NOT RESTORE %s: %v\n", sig, f, err)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "mutate: %v: restored %s\n", sig, f)
		}
		os.Exit(130)
	}()
}

type mutant struct {
	name string
	file string
	from string
	to   string
	pkg  string
	test string // the killing test's name
	want string // a substring of the killing failure's message
	// survive marks the no-op control: it must NOT be killed.
	survive bool
	// realLiveKit marks a mutant killed by the integration test against a
	// real livekit-server: the sweep needs the binaries (and refuses to start
	// without them, so it can never pass by skipping), and runs the test with
	// MEETINGS_REQUIRE_LIVEKIT=1.
	realLiveKit bool
	// more are further replacements applied with this one, for a mutant
	// that needs a few lines in several files (in order; each from must
	// occur exactly once when it is applied).
	more []edit
}

type edit struct{ file, from, to string }

func (m mutant) edits() []edit { return append([]edit{{m.file, m.from, m.to}}, m.more...) }

const (
	srv    = "internal/server/server.go"
	repo   = "internal/store/repo.go"
	st     = "internal/store/store.go"
	secret = "internal/store/secret.go"
	own    = "internal/owners/owners.go"
	mainGo = "main.go"
	link   = "internal/store/link.go"
	client = "internal/muster/client.go"
	// phase 2b
	calls   = "internal/server/calls.go"
	guest   = "internal/server/guest.go"
	machine = "internal/server/machine.go"
	parts   = "internal/store/participants.go"
	lkToken = "internal/livekit/token.go"
	lkHook  = "internal/livekit/webhook.go"
	name    = "internal/lobby/name.go"
	knocker = "internal/lobby/knocker.go"
	limiter = "internal/lobby/limiter.go"
	appJS   = "internal/server/web/static/app.js"
	streams = "internal/server/streams.go"
	// phase 2c: the deployment files (killed by ./deploy, which needs kubectl)
	kLiveKit = "deploy/kubernetes/livekit.yaml"
	kPlugin  = "deploy/kubernetes/muster-meetings.yaml"
	kNetPol  = "deploy/kubernetes/networkpolicy.yaml"
	kBase    = "deploy/kubernetes/kustomization.yaml"
	composeF = "compose.meetings.yml"
)

var mutants = []mutant{
	// ---- controls ----
	{name: "control: known-caught (join code 64 bits)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 8",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", want: "is not 22 base64url characters"},
	{name: "control: a no-op edit (must survive)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 16 // no-op",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", survive: true},

	// ---- /enter: the frame gate (sign-in CSRF) ----
	{name: "frame gate: removed", file: srv, from: `if ok, why := framed(r); !ok {`, to: `if ok, why := framed(r); false && !ok {`,
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "Sec-Fetch-Dest document: enter = 200"},
	{name: "frame gate: a missing header passes", file: srv, from: `len(d) == 1 && d[0] == "iframe"`, to: `len(d) == 0 || (len(d) == 1 && d[0] == "iframe")`,
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "no Sec-Fetch at all: enter = 200"},
	{name: "frame gate: any value passes", file: srv, from: `len(d) == 1 && d[0] == "iframe"`, to: `len(d) == 1 && d[0] != ""`,
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "Sec-Fetch-Dest empty: enter = 200"},
	{name: "frame gate: a doubled header passes", file: srv, from: `len(d) == 1 && d[0] == "iframe"`, to: `len(d) >= 1 && d[0] == "iframe"`,
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "Sec-Fetch-Dest twice: enter = 200"},
	{name: "frame gate: the code is redeemed before the refusal", file: srv, from: "if ok, why := framed(r); !ok {\n",
		to:  "if ok, why := framed(r); !ok {\n\t\t_, _ = s.app.Client.Redeem(r.Context(), r.URL.Query().Get(\"muster_code\"))\n",
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "made 1 redeem call: the code was spent"},
	{name: "frame gate: an old browser gets no update message", file: srv, from: `return false, EnterOldBrowser`, to: `return false, EnterNotFramed`,
		pkg: "./internal/server", test: "TestEnterRefusesUnlessLoadedInAFrameBeforeAnyRedeem", want: "no Sec-Fetch at all: the page says"},

	// ---- /enter: the redeem ----
	{name: "redeem: accept any purpose", file: srv, from: `if red.Purpose != "app" {`, to: `if false && red.Purpose != "app" {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "purpose view, target app: enter = 200"},
	{name: "redeem: accept a mirror beside the app target", file: srv, from: `if red.Target.App == nil || len(red.Target.Mirror) > 0 {`, to: `if red.Target.App == nil {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "purpose app, both targets: enter = 200"},
	{name: "redeem: accept a mirror-only target", file: srv, from: `if red.Target.App == nil || len(red.Target.Mirror) > 0 {`, to: `if len(red.Target.Mirror) > 0 && red.Target.App != nil {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "purpose app, target mirror: enter = 200"},
	{name: "redeem: accept a personal-scope answer", file: srv, from: `if red.Scope != muster.ScopeServer {`, to: `if false {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "scope person: enter = 200"},
	{name: "redeem: accept an owner with no subject shape (server)", file: srv, from: `if !store.ValidSubject(red.Owner.Subject) {`, to: `if false {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAServerAppCodeNamingASubject", want: "subject a user id: enter = 500"},
	{name: "redeem: the store keys on a non-subject", file: st, from: "if !ValidSubject(subject) {", to: "if false {",
		pkg: "./internal/store", test: "TestCreateSessionRefusesAnythingButASubject", want: `CreateSession("ada.lovelace@example.t") = <nil>, want ErrBadSubject`},
	{name: "plugin bound: any muster_plugin is redeemed", file: srv, from: `if pluginID != s.app.PluginID || !s.app.Live() {`, to: `if !s.app.Live() {`,
		pkg: "./internal/server", test: "TestEnterRedeemsOnlyForThisServerApp", want: "another plugin id = 403"},
	{name: "redeem: pass another person's ref to the page", file: srv,
		from: `if _, err := s.store.ForSubject(subject).Meeting(r.Context(), red.Target.App.Ref); err == nil {`, to: `if true {`,
		pkg: "./internal/server", test: "TestEnterPassesTheRefOnlyForTheOwnersOwnMeeting", want: "another person's ref reached the page"},

	// ---- cookies and headers on the plugin's origin ----
	{name: "cookie: /enter sets a session cookie", file: srv, from: `	s.renderEnter(w, http.StatusOK, enterPage{`,
		to: `	http.SetCookie(w, &http.Cookie{Name: "mm_session", Value: session, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode})
	s.renderEnter(w, http.StatusOK, enterPage{`,
		pkg: "./internal/server", test: "TestEnterHeadersFrameAncestorsIsMustersOriginAndNoCookieIsSet", want: "enter set a cookie"},
	{name: "cookie: owner routes also read a cookie", file: srv, from: `tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")`,
		to: `tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if c, err := r.Cookie("session"); err == nil {
			tok, ok = c.Value, true
		}`,
		pkg: "./internal/server", test: "TestOwnerRoutesRefuseAnythingButALiveBearerSession", want: "with the token as cookie = 200"},
	{name: "frame-ancestors: any origin", file: srv, from: `func (s *Server) ownerPageCSP() string { return s.pageCSP(s.cfg.MusterOrigin) }`, to: `func (s *Server) ownerPageCSP() string { return s.pageCSP("*") }`,
		pkg: "./internal/server", test: "TestEnterHeadersFrameAncestorsIsMustersOriginAndNoCookieIsSet", want: "frame-ancestors = \"*\""},
	{name: "referrer: send the origin", file: srv, from: `h.Set("Referrer-Policy", "no-referrer")`, to: `h.Set("Referrer-Policy", "origin")`,
		pkg: "./internal/server", test: "TestEnterHeadersFrameAncestorsIsMustersOriginAndNoCookieIsSet", want: "Referrer-Policy = \"origin\""},

	// ---- owner sessions ----
	{name: "session: never expires", file: st, from: `WHERE token_sha256 = ? AND expires_at > ?`, to: `WHERE token_sha256 = ? AND ? IS NOT NULL`,
		pkg: "./internal/server", test: "TestAnOwnerSessionEndsAfterEightHours", want: "at 8 h = 200"},
	{name: "session: lives 24 h", file: st, from: `const SessionLifetime = 8 * time.Hour`, to: `const SessionLifetime = 24 * time.Hour`,
		pkg: "./internal/server", test: "TestAnOwnerSessionEndsAfterEightHours", want: "at 8 h = 200"},
	{name: "session: no app-liveness check per request", file: srv, from: "\t\tif !s.app.Live() {\n\t\t\trefuse()", to: "\t\tif false {\n\t\t\trefuse()",
		pkg: "./internal/server", test: "TestASessionIsRefusedOnceTheAppIsDeadEvenIfItsRowSurvives", want: "a session made after the uninstall = 200"},
	{name: "session: token stored in clear", file: st, from: `Hash(tok), subject, ms(now), ms(exp)`, to: `tok, subject, ms(now), ms(exp)`,
		pkg: "./internal/store", test: "TestSessionTokenIsStoredOnlyHashed", want: "the session token itself is in the database"},
	{name: "session: 16 random bytes", file: secret, from: "SessionTokenBytes = 32", to: "SessionTokenBytes = 16",
		pkg: "./internal/store", test: "TestSessionTokenAndMeetingIDShapes", want: "want mmo_ and 32 random bytes"},

	// ---- uninstall: /self's 401 ends everything ----
	{name: "uninstall: the poll ignores /self's 401", file: own, from: `a.Revoke(ctx, "/self answered 401: the app was uninstalled or its token rotated")`, to: `_ = 0`,
		pkg: "./internal/server", test: "TestUninstallEndsEverySessionAndMeeting", want: "Ada's session after the uninstall poll = 200"},
	{name: "uninstall: sessions not deleted", file: st, from: "`DELETE FROM owner_sessions WHERE 1`", to: "`DELETE FROM owner_sessions WHERE 0`",
		pkg: "./internal/server", test: "TestUninstallEndsEverySessionAndMeeting", want: "sessions left in the database after the uninstall"},
	{name: "uninstall: meetings not ended", file: st, from: "WHERE state = 'open'`, ms(now))", to: "WHERE state = 'open' AND 0`, ms(now))",
		pkg: "./internal/server", test: "TestUninstallEndsEverySessionAndMeeting", want: "Ada's meeting after the uninstall"},
	{name: "uninstall: the app stays live (enter still asks muster)", file: own, from: "a.dead.Store(true)", to: "_ = 0",
		pkg: "./internal/server", test: "TestUninstallEndsEverySessionAndMeeting", want: "(1 new redeems)"},
	{name: "uninstall: any /self error ends everything", file: own,
		from: "if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {\n\t\ta.Revoke(ctx, \"/self answered", to: "if err != nil {\n\t\ta.Revoke(ctx, \"/self answered",
		pkg: "./internal/server", test: "TestALivenessPollThatFailsEndsNoOne", want: "/self 503: Ada's session = 401"},

	// ---- owners/check: a subject muster no longer answers live ----
	{name: "owners/check: ignore a revoked answer", file: own, from: "revoked = append(revoked, s)", to: "_ = s",
		pkg: "./internal/server", test: "TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse", want: "hidden: Ada's session after the poll = 200"},
	{name: "owners/check: revoked treated as expired", file: own, from: "revoked = append(revoked, s)", to: "expired = append(expired, s)",
		pkg: "./internal/server", test: "TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse", want: `hidden: Ada's meeting after the poll = "open"`},
	{name: "owners/check: expired treated as revoked", file: own, from: "expired = append(expired, s)", to: "revoked = append(revoked, s)",
		pkg: "./internal/server", test: "TestAnExpiredGrantEndsOnlyTheOwnersSessionsAndKeepsTheirMeetingsOpen", want: `expired: Ada's meeting after the poll = "ended"`},
	{name: "owners/check: expired ends no session", file: st, from: "AS j) AND created_at <= ?`",
		to:  "AS j) AND 0 AND created_at <= ?`",
		pkg: "./internal/server", test: "TestAnExpiredGrantEndsOnlyTheOwnersSessionsAndKeepsTheirMeetingsOpen", want: "expired: Ada's session after the poll = 200"},
	{name: "owners/check: an absent or unknown state is live", file: client, from: "default:\n\t\t\t\tst = StateRevoked", to: "default:\n\t\t\t\tst = StateLive",
		pkg: "./internal/server", test: "TestASubjectOwnersCheckDoesNotAnswerIsTreatedAsRevoked", want: `absent: the meeting after the poll = "open"`},
	{name: "owners/check: the poll never asks", file: own, from: "_, _, _ = a.checkOwners(ctx) // it logs its own failures",
		to:  "_ = 0 // it logs its own failures",
		pkg: "./internal/server", test: "TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse", want: "hidden: Ada's session after the poll = 200"},
	{name: "owners/check: a failed check ends everyone held", file: own, from: "\t\t\tcontinue // its people are kept until muster answers", to: "\t\t\t_ = 0",
		pkg: "./internal/server", test: "TestALivenessPollThatFailsEndsNoOne", want: "owners/check 503: Ada's session = 401"},
	{name: "owners/check: one person's end deletes everyone's sessions", file: st,
		from: "WHERE subject IN (SELECT value FROM json_each(?)) AND created_at <= ?`, list", to: "WHERE ? IS NOT NULL AND created_at <= ?`, list",
		pkg: "./internal/server", test: "TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse", want: "Bea's session after Ada's end = 401"},
	{name: "owners/check: one person's end ends everyone's meetings", file: st,
		from: "AND subject IN (SELECT value FROM json_each(?)) AND created_at <= ? RETURNING", to: "AND ? IS NOT NULL AND created_at <= ? RETURNING",
		pkg: "./internal/server", test: "TestAPersonMusterNoLongerAnswersLiveLosesTheirSessionsAndMeetingsAndNoOneElse", want: `Bea's meeting after Ada's end = "ended"`},
	{name: "owners/check: batches of 201", file: client, from: "const MaxCheckSubjects = 200", to: "const MaxCheckSubjects = 201",
		pkg: "./internal/muster", test: "TestOwnersCheckAnswersOnlyTheLiveSubsetItWasAsked", want: "450 subjects:"},
	{name: "owners/check: trust a subject that was not asked", file: client, from: "\t\t\tstates[s] = st\n\t\t}\n", to: "\t\t\tstates[s] = st\n\t\t}\n\t\tfor s, st := range *out.States {\n\t\t\tstates[s] = st\n\t\t}\n",
		pkg: "./internal/muster", test: "TestOwnersCheckAnswersOnlyTheLiveSubsetItWasAsked", want: "an answer naming a subject that was not asked"},

	{name: "owners/check: no states object reads as everyone revoked (client)", file: client, from: "if err == nil && out.States == nil {",
		to:  "if err == nil && out.States == nil {\n\t\t\tout.States = &map[string]string{}\n\t\t}\n\t\tif false {",
		pkg: "./internal/muster", test: "TestOwnersCheckWithoutAStatesObjectIsAProtocolError", want: "want a protocol error"},
	{name: "owners/check: no states object reads as everyone revoked (poll)", file: client, from: "if err == nil && out.States == nil {",
		to:  "if err == nil && out.States == nil {\n\t\t\tout.States = &map[string]string{}\n\t\t}\n\t\tif false {",
		pkg: "./internal/server", test: "TestALivenessPollThatFailsEndsNoOne", want: "owners/check {}: Ada's session = 401"},
	{name: "owners/check: one failed batch discards the answered ones", file: client, from: "\t\t\tcontinue\n\t\t}\n\t\tfor _, s := range batch {",
		to:  "\t\t\treturn nil, e\n\t\t}\n\t\tfor _, s := range batch {",
		pkg: "./internal/muster", test: "TestOwnersCheckKeepsTheBatchesThatWereAnswered", want: "want the 250 of the first and third batches"},
	{name: "owners/check: no rotation (each poll restarts at the first subject)", file: own, from: "a.resume = order[done-1]", to: "_ = order[done-1]",
		pkg: "./internal/owners", test: "TestOwnersCheckRotatesAndHonoursRetryAfterSoNoSubjectIsStarved", want: "was never judged in three polls"},
	{name: "owners/check: Retry-After ignored", file: own, from: "a.retryAt = a.now().Add(wait)", to: "_ = wait",
		pkg: "./internal/owners", test: "TestOwnersCheckRotatesAndHonoursRetryAfterSoNoSubjectIsStarved", want: "a poll inside muster's Retry-After sent"},
	{name: "boot: gives up at owners/check's limit", file: own, from: "\t\tcovered += n\n", to: "\t\tcovered = len(held)\n",
		pkg: "./internal/owners", test: "TestBootWaitsOutOwnersChecksLimitAndJudgesEveryone", want: "sessions left (want 0)"},
	{name: "race: a revoked end deletes sessions made after the send", file: st, from: "json_each(?)) AND created_at <= ?`, list, ms(asOf))", to: "json_each(?)) AND ? IS NOT NULL`, list, ms(asOf))",
		pkg: "./internal/server", test: "TestAPersonWhoReopensTheTabWhileTheCheckIsOnItsWayKeepsTheFreshSession", want: "revoked: the session made after the check was sent = 401"},
	{name: "race: an expired end deletes sessions made after the send", file: st, from: "AS j) AND created_at <= ?`", to: "AS j) AND ? IS NOT NULL`",
		pkg: "./internal/server", test: "TestAPersonWhoReopensTheTabWhileTheCheckIsOnItsWayKeepsTheFreshSession", want: "expired: the session made after the check was sent = 401"},
	{name: "race: a revoked end ends meetings made after the send", file: st, from: "json_each(?)) AND created_at <= ? RETURNING", to: "json_each(?)) AND ? IS NOT NULL RETURNING",
		pkg: "./internal/store", test: "TestTheEndKeepsWhatWasMadeAfterTheCheckWasSent", want: "revoked: a meeting made after the send"},
	{name: "race: the cutoff taken when the answer arrives", file: own, from: "sent, done := a.now(), 0", to: "var sent time.Time\n\tdone := 0",
		more: []edit{{own, "\tif done > 0 {\n\t\ta.resume", "\tsent = a.now()\n\tif done > 0 {\n\t\ta.resume"}},
		pkg:  "./internal/server", test: "TestAPersonWhoReopensTheTabWhileTheCheckIsOnItsWayKeepsTheFreshSession", want: "the session made after the check was sent = 401"},

	// ---- boot ----
	{name: "boot: skip the reconcile", file: own, from: "for covered := 0; covered < len(held); {", to: "for covered := len(held); covered < len(held); {",
		pkg: "./internal/owners", test: "TestBootEndsWhoMusterNoLongerAnswersLive", want: "boot: Ada's session survived"},
	{name: "boot: a 401 leaves the sessions", file: own, from: `if _, err := st.EndAll(ctx, "boot: /self answered 401", now()); err != nil {`, to: `if _, err := (store.Ended{}), error(nil); err != nil {`,
		pkg: "./internal/owners", test: "TestBootThatMusterAnswers401EndsEverything", want: "session survived the boot"},
	{name: "boot: accept a personal token", file: own, from: "if s.Scope != muster.ScopeServer || s.OwnerKind != muster.ScopeServer {", to: "if false {",
		pkg: "./internal/owners", test: "TestBootRefusesAPersonalTokenOrOneWithoutApp", want: "a personal token: Boot accepted it"},
	{name: "boot: accept a token without app", file: own, from: "if !slices.Contains(s.Capabilities, Capability) {", to: "if !slices.Contains(s.Capabilities, Capability) && false {",
		pkg: "./internal/owners", test: "TestBootRefusesAPersonalTokenOrOneWithoutApp", want: "no app: Boot accepted it"},
	{name: "config: a comma-separated token list boots", file: mainGo, from: `if strings.Contains(c.Token, ",") {`, to: `if false {`,
		pkg: ".", test: "TestLoadConfig", want: "two tokens: LoadConfig accepted it"},

	// ---- Muster-Act-For (notify and tasks, for 2b) ----
	{name: "act-for: the header is not sent", file: client, from: "return http.Header{ActForHeader: {subject}}, nil", to: "return http.Header{}, nil",
		pkg: "./internal/muster", test: "TestNotifyAndTasksActForTheSubjectNamed", want: "Notify for Ada ="},
	{name: "act-for: the header's name is wrong", file: client, from: `const ActForHeader = "Muster-Act-For"`, to: `const ActForHeader = "Muster-Act-As"`,
		pkg: "./internal/muster", test: "TestNotifyAndTasksActForTheSubjectNamed", want: "Notify for Ada ="},
	{name: "act-for: a call with no subject is sent", file: client, from: "if !ValidSubject(subject) {\n\t\treturn nil, ErrBadSubject", to: "if false {\n\t\treturn nil, ErrBadSubject",
		pkg: "./internal/muster", test: "TestNotifyAndTasksActForTheSubjectNamed", want: `Notify("") =`},

	// ---- ownership: the subject filter ----
	{name: "ownership: list without the subject filter", file: repo, from: "` FROM meetings WHERE subject = ?`", to: "` FROM meetings WHERE ? IS NOT NULL`",
		pkg: "./internal/server", test: "TestAnotherPersonsMeetingsAreNeverVisible", want: "B's list ="},
	{name: "ownership: one meeting without the subject filter", file: repo,
		from: "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND subject = ?`",
		to:   "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND ? IS NOT NULL`",
		pkg:  "./internal/server", test: "TestAnotherPersonsMeetingsAreNeverVisible", want: "B reading A's"},
	{name: "ownership: repository list (store level)", file: repo, from: "` FROM meetings WHERE subject = ?`", to: "` FROM meetings WHERE ? IS NOT NULL`",
		pkg: "./internal/store", test: "TestOwnerRepoNeverReturnsAnotherOwnersMeetings", want: "A's list ="},

	// ---- nothing of muster's about a person but the subject and name ----
	{name: "storage: the display name read from the email", file: client, from: "DisplayName string `json:\"display_name\"`", to: "DisplayName string `json:\"email\"`",
		pkg: "./internal/server", test: "TestNoMusterUserIDOrEmailIsEverStored", want: "the database holds Cy W.'s email"},

	// The auditor's mutant: the redeem's owner id written to audit.at as an
	// INTEGER, which the byte scan cannot see.
	{name: "storage: an owner id written to audit.at as an INTEGER", file: client,
		from: "Subject     string `json:\"subject\"`", to: "Subject     string `json:\"subject\"`\n\tID          int64  `json:\"id\"`",
		more: []edit{
			{srv, "s.store.CreateSession(r.Context(), subject, red.Owner.DisplayName, now)", "s.store.CreateSession(r.Context(), subject, red.Owner.DisplayName, now, red.Owner.ID)"},
			{st, "func (s *Store) CreateSession(ctx context.Context, subject, displayName string, now time.Time) (string, time.Time, error) {",
				"func (s *Store) CreateSession(ctx context.Context, subject, displayName string, now time.Time, ownerID ...int64) (string, time.Time, error) {"},
			{st, "\tif err := audit(ctx, tx, now, subject, \"\", \"session.created\", \"\"); err != nil {",
				"\tat := now\n\tif len(ownerID) > 0 && ownerID[0] != 0 {\n\t\tat = time.UnixMilli(ownerID[0])\n\t}\n\tif err := audit(ctx, tx, at, subject, \"\", \"session.created\", \"\"); err != nil {"},
		},
		pkg: "./internal/server", test: "TestNoMusterUserIDOrEmailIsEverStored", want: "the database holds a muster user id as an integer: audit.at = 3141592653"},

	// ---- the link ----
	{name: "link: code stored in clear (not hashed)", file: repo, from: `Hash(code), ms(now)`, to: `code, ms(now)`,
		pkg: "./internal/store", test: "TestJoinCodeIsNeverStoredInClear", want: "the join code itself is in the database"},
	{name: "link: code 64 bits (API)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 8",
		pkg: "./internal/server", test: "TestNewMeetingAnswersItsIDAndALinkOnThePluginsOrigin", want: "join_url ="},
	{name: "link: the key is not in the HMAC", file: link, from: "hmac.New(sha256.New, s.linkKey)", to: "hmac.New(sha256.New, nil)",
		pkg: "./internal/store", test: "TestAnotherKeyGivesTheOwnerAnotherLink", want: "under another key"},
	{name: "link: the meeting id is not in the HMAC", file: link, from: "m.Write([]byte(linkDomain + meetingID))", to: "m.Write([]byte(linkDomain))",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", want: "drawn twice"},
	{name: "link: another person reads it (store)", file: repo,
		from: "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND subject = ?`",
		to:   "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND ? IS NOT NULL`",
		pkg:  "./internal/store", test: "TestOnlyTheOwnerReadsTheLink", want: "another owner reading the link"},
	{name: "link: an ended meeting gives its link", file: repo, from: `if m.State != "open" || !now.Before(m.LinkExpiresAt) {`, to: `if !now.Before(m.LinkExpiresAt) {`,
		pkg: "./internal/store", test: "TestAnEndedMeetingHasNoLink", want: "an ended meeting's link ="},
	{name: "link: no 24 h lifetime (store)", file: repo, from: `if m.State != "open" || !now.Before(m.LinkExpiresAt) {`, to: `if m.State != "open" {`,
		pkg: "./internal/store", test: "TestTheLinkIsGoneAt24Hours", want: "at 24 h ="},
	{name: "link: no 24 h lifetime (API)", file: repo, from: `if m.State != "open" || !now.Before(m.LinkExpiresAt) {`, to: `if m.State != "open" {`,
		pkg: "./internal/server", test: "TestTheLinkIsTheOwnersOnlyAndOnlyWhileOpen", want: "at 24 h = 200"},
	{name: "link key: missing key accepted", file: mainGo, from: `if c.LinkKey, err = store.ParseLinkKey(getenv("MEETINGS_LINK_KEY")); err != nil {`,
		to:  `if c.LinkKey, _ = store.ParseLinkKey(getenv("MEETINGS_LINK_KEY")); false {`,
		pkg: ".", test: "TestLoadConfig", want: "missing link key: LoadConfig accepted it"},
	{name: "link key: short key parsed", file: link, from: "err == nil && len(k) == LinkKeyBytes", to: "err == nil && len(k) > 0",
		pkg: ".", test: "TestLoadConfig", want: "short link key: LoadConfig accepted it"},
	{name: "link key: Open takes a short key", file: st, from: "if len(linkKey) != LinkKeyBytes {", to: "if len(linkKey) == 0 {",
		pkg: "./internal/store", test: "TestOpenAndParseLinkKeyRefuseAnythingButThirtyTwoBytes", want: "Open with a 16-byte key succeeded"},

	// ---- the revocation race ----
	{name: "race: create without the live-session check (API)", file: repo,
		from: "WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND subject = ? AND expires_at > ?)`",
		to:   "WHERE ? IS NOT NULL AND ? IS NOT NULL AND ? IS NOT NULL`",
		pkg:  "./internal/server", test: "TestANewMeetingRacingARevocationLeavesNoOpenMeeting", want: "an open meeting after the revocation"},
	{name: "race: create without the live-session check (store)", file: repo,
		from: "WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND subject = ? AND expires_at > ?)`",
		to:   "WHERE ? IS NOT NULL AND ? IS NOT NULL AND ? IS NOT NULL`",
		pkg:  "./internal/store", test: "TestCreatingAMeetingNeedsTheSessionStillLive", want: "after the session was revoked"},

	// ---- /enter fails closed ----
	{name: "enter: muster down or 5xx falls through", file: srv,
		from: "\t\t\ts.refuseEnter(w, http.StatusBadGateway, EnterNoMuster, \"muster: \"+kindOf(err))\n\t\t\treturn\n",
		to:   "",
		pkg:  "./internal/server", test: "TestEnterFailsClosedWhenMusterIsDownOrFailing", want: "muster failing: enter = 403"},

	// ---- logs ----
	{name: "log: no redaction of /j/<code>", file: srv, from: `return "/j/{code}"`, to: `return p`,
		pkg: "./internal/server", test: "TestTheLogCarriesNoCodeTokenOrQuery", want: "the log carries the join code"},
	{name: "log: a /j/ segment elsewhere is not redacted", file: srv, from: `if strings.Contains(p, "/j/") || strings.Contains(c+"/", "/j/") {`, to: `if false {`,
		pkg: "./internal/server", test: "TestRedactPath", want: `RedactPath("/api/j/AbC_-123") = "/api/j/AbC_-123"`},
	{name: "log: the path is not cleaned first", file: srv, from: `c := path.Clean("/" + p)`, to: `c, _ := p, path.Clean`,
		pkg: "./internal/server", test: "TestRedactPath", want: `RedactPath("/x/../j/AbC_-123/a") = "/{redacted}"`},
	{name: "log: the full URL with its query", file: srv, from: `RedactPath(r.URL.Path), sw.status`, to: `r.URL.String(), sw.status`,
		pkg: "./internal/server", test: "TestTheLogCarriesNoCodeTokenOrQuery", want: "the log carries the hand-off code"},

	// ==== phase 2b: the call, the lobby, the webhook, the notice ====

	// ---- 🔴 no guest token before admission ----
	{name: "lobby: mint a guest token on knock", file: guest, from: `writeJSON(w, http.StatusCreated, map[string]any{"ticket": k.Ticket})`,
		to:   "tok, _ := s.cfg.LiveKit.Keys().Mint(livekit.Participant{Identity: \"guest-\" + k.ParticipantID}, livekit.GuestGrant(store.RoomName(k.MeetingID)), now)\n\twriteJSON(w, http.StatusCreated, map[string]any{\"ticket\": k.Ticket, \"token\": tok})",
		more: []edit{{guest, "\t\"github.com/ZacxDev/muster-meetings/internal/lobby\"\n", "\t\"github.com/ZacxDev/muster-meetings/internal/livekit\"\n\t\"github.com/ZacxDev/muster-meetings/internal/lobby\"\n"}},
		pkg:  "./internal/server", test: "TestNoGuestTokenBeforeAdmissionAndItsGrantIsExact", want: "want 201 with the ticket and nothing else"},
	{name: "lobby: the token gate skips the admission predicate", file: calls, from: "if err != nil || !ok {\n\t\treturn tokenAnswer{}, false, err", to: "if err != nil || (!ok && false) {\n\t\treturn tokenAnswer{}, false, err",
		pkg: "./internal/server", test: "TestNoGuestTokenBeforeAdmissionAndItsGrantIsExact", want: `no "waiting" event (the stream so far: "event: admitted`},
	{name: "lobby: an event stream for a ticket that is not admitted", file: guest, from: `if g.State != store.StAdmitted || !g.MeetingLive {`, to: `if !g.MeetingLive {`,
		pkg: "./internal/server", test: "TestADeniedGuestGetsNoToken", want: "a denied ticket's event stream = 200"},

	// ---- 🔴 a ticket opens only its own meeting ----
	{name: "ticket: the link code is not checked", file: parts, from: "`m.join_code_sha256 = ? AND p.ticket_sha256 = ?`", to: "`? IS NOT NULL AND p.ticket_sha256 = ?`",
		pkg: "./internal/server", test: "TestATicketCannotWaitOnOrGetATokenForAnotherMeeting", want: "A's ticket on B's link /wait = 200"},
	{name: "ticket: a guest's ticket opens the owner routes", file: secret,
		from: "func ValidSessionToken(s string) bool { return sessionTokenRE.MatchString(s) }",
		to:   "func ValidSessionToken(s string) bool { return sessionTokenRE.MatchString(s) || joinCodeRE.MatchString(s) }",
		more: []edit{{st, "`SELECT subject, expires_at FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ?`,\n\t\tHash(token), ms(now))",
			"`SELECT subject, expires_at FROM owner_sessions WHERE token_sha256 = ? AND expires_at > ? UNION ALL SELECT m.subject, ? + 1 FROM participants p JOIN meetings m ON m.id = p.meeting_id WHERE p.ticket_sha256 = ?`,\n\t\tHash(token), ms(now), ms(now), Hash(token))"}},
		pkg: "./internal/server", test: "TestAGuestReachesNoOwnerRoute", want: "with a guest's ticket as the bearer = 200"},

	// ---- 🔴 a denied guest gets no token; the lobby state machine ----
	{name: "deny: a deny admits", file: parts, from: "EvDeny:   {[]string{StWaiting}, StDenied},", to: "EvDeny:   {[]string{StWaiting}, StAdmitted},",
		pkg: "./internal/server", test: "TestADeniedGuestGetsNoToken", want: `no "denied" event (the stream so far: "event: waiting`},
	{name: "deny: the predicate admits a denied guest", file: parts, from: "p.kind = 'guest' AND p.state = 'admitted'))`", to: "p.kind = 'guest' AND p.state IN ('admitted', 'denied')))`",
		pkg: "./internal/store", test: "TestMayBeInRoom", want: "a denied guest: true, want false"},
	{name: "state machine: remove a waiting guest", file: parts, from: "EvRemove: {[]string{StAdmitted}, StRemoved},", to: "EvRemove: {[]string{StAdmitted, StWaiting}, StRemoved},",
		pkg: "./internal/store", test: "TestTheLobbyStateMachine", want: `waiting/remove: ("removed", true), want ("", false)`},
	{name: "state machine: admit a denied guest", file: parts, from: "EvAdmit:  {[]string{StWaiting}, StAdmitted},", to: "EvAdmit:  {[]string{StWaiting, StDenied}, StAdmitted},",
		pkg: "./internal/store", test: "TestTheLobbyStateMachine", want: `denied/admit: ("admitted", true), want ("", false)`},
	{name: "state machine: deny an admitted guest", file: parts, from: "EvDeny:   {[]string{StWaiting}, StDenied},", to: "EvDeny:   {[]string{StWaiting, StAdmitted}, StDenied},",
		pkg: "./internal/store", test: "TestTheLobbyStateMachine", want: `admitted/deny: ("denied", true), want ("", false)`},
	{name: "state machine: a waiting guest leaves", file: parts, from: "EvLeave:  {[]string{StAdmitted}, StLeft},", to: "EvLeave:  {[]string{StAdmitted, StWaiting}, StLeft},",
		pkg: "./internal/store", test: "TestTheLobbyStateMachine", want: `waiting/leave: ("left", true), want ("", false)`},
	{name: "state machine: something leaves ended", file: parts, from: "EvEnd:    {[]string{StWaiting, StAdmitted}, StEnded},", to: "EvEnd:    {[]string{StWaiting, StAdmitted, StDenied}, StEnded},",
		pkg: "./internal/store", test: "TestTheLobbyStateMachine", want: `denied/end: ("ended", true), want ("", false)`},

	// ---- 🔴 grants, exactly ----
	{name: "grants: guest roomAdmin", file: lkToken, from: "RoomJoin: true, Room: room, RoomAdmin: false,", to: "RoomJoin: true, Room: room, RoomAdmin: true,",
		pkg: "./internal/livekit", test: "TestGrantsAreExactlyAsDesigned", want: "guest grant ="},
	{name: "grants: guest canPublishData left out (LiveKit reads it as true)", file: lkToken, from: "CanPublishData: ptr(false)", to: "CanPublishData: nil",
		pkg: "./internal/livekit", test: "TestGrantsAreExactlyAsDesigned", want: "guest grant ="},
	{name: "grants: guest recorder", file: lkToken, from: "RoomJoin: true, Room: room, RoomAdmin: false,", to: "RoomJoin: true, Room: room, RoomAdmin: false, Recorder: true,",
		pkg: "./internal/livekit", test: "TestGrantsAreExactlyAsDesigned", want: "guest grant ="},
	{name: "grants: hidden guest", file: lkToken, from: "RoomJoin: true, Room: room, RoomAdmin: false,", to: "RoomJoin: true, Room: room, RoomAdmin: false, Hidden: true,",
		pkg: "./internal/livekit", test: "TestGrantsAreExactlyAsDesigned", want: "guest grant ="},
	{name: "grants: a guest may rename itself", file: lkToken, from: "CanPublishData: ptr(false), CanUpdateOwnMetadata: ptr(false),", to: "CanPublishData: ptr(false), CanUpdateOwnMetadata: ptr(true),",
		pkg: "./internal/livekit", test: "TestGrantsAreExactlyAsDesigned", want: "guest grant ="},
	{name: "grants: a 10-minute token", file: lkToken, from: "const TokenTTL = 2 * time.Minute", to: "const TokenTTL = 10 * time.Minute",
		pkg: "./internal/livekit", test: "TestMintIsATwoMinuteTokenForOneIdentity", want: "want a life of exactly 120 s"},
	{name: "grants: the guest's room is not the meeting's", file: calls, from: "livekit.GuestGrant(g.Room)", to: "livekit.GuestGrant(\"meeting-\" + g.Identity)",
		pkg: "./internal/server", test: "TestNoGuestTokenBeforeAdmissionAndItsGrantIsExact", want: "the guest's grant ="},

	// ---- 🔴 removal: revoked and kicked ----
	{name: "removal: LiveKit is not called", file: calls, from: "err := s.cfg.LiveKit.RemoveParticipant(r.Context(), mv.Room, mv.Identity)", to: "err := error(nil)\n\t\t\t_ = mv",
		pkg: "./internal/server", test: "TestARemovedGuestIsRevokedKickedAndKeptOut", want: "after the removal the room holds"},
	{name: "removal: the identity is not revoked", file: parts, from: "EvRemove: {[]string{StAdmitted}, StRemoved},", to: "EvRemove: {[]string{StAdmitted}, StAdmitted},",
		pkg: "./internal/server", test: "TestARemovedGuestIsRevokedKickedAndKeptOut", want: `no "removed" event`},
	{name: "removal: a removed ticket may wait again", file: guest, from: "case store.StRemoved, store.StLeft:", to: "case store.StLeft:",
		pkg: "./internal/server", test: "TestARemovedGuestIsRevokedKickedAndKeptOut", want: "the removed ticket's /wait = 200"},
	{name: "rejoin: the participant_joined check is removed", file: machine, from: "\t\ts.admitOrRemove(ctx, room, ev.Participant.Identity)\n", to: "\t\t_ = 0\n",
		pkg: "./internal/server", test: "TestARemovedGuestIsRevokedKickedAndKeptOut", want: "is back in the LiveKit room after rejoining with the old token"},
	{name: "rejoin (real LiveKit): the participant_joined check is removed", file: machine, from: "\t\ts.admitOrRemove(ctx, room, ev.Participant.Identity)\n", to: "\t\t_ = 0\n", realLiveKit: true,
		pkg: "./internal/server", test: "TestAgainstARealLiveKitServer", want: "the rejoining removed guest is disconnected by the plugin (PARTICIPANT_REMOVED)"},
	{name: "rejoin: anyone joining is removed (control)", file: machine, from: "\tif ok {\n\t\treturn\n\t}\n\tif err := s.cfg.LiveKit.RemoveParticipant", to: "\tif ok && false {\n\t\treturn\n\t}\n\tif err := s.cfg.LiveKit.RemoveParticipant",
		pkg: "./internal/server", test: "TestARemovedGuestIsRevokedKickedAndKeptOut", want: "an admitted guest's join was removed (the positive control)"},

	// ---- 🔴 the webhook ----
	{name: "webhook: the body hash is not compared", file: lkHook, from: "if subtle.ConstantTimeCompare(got[:], want) != 1 {", to: "if subtle.ConstantTimeCompare(got[:], want) != 1 && false {",
		pkg: "./internal/server", test: "TestTheWebhookActsOnlyOnAVerifiedRequest", want: "a tampered body: 200"},
	{name: "webhook: the signature is not compared", file: lkToken, from: "if subtle.ConstantTimeCompare(m.Sum(nil), sig) != 1 {", to: "if subtle.ConstantTimeCompare(m.Sum(nil), sig) != 1 && false {",
		pkg: "./internal/server", test: "TestTheWebhookActsOnlyOnAVerifiedRequest", want: "a wrong secret: 200"},
	{name: "webhook: the handler ignores the verification", file: machine, from: "if err != nil {\n\t\ts.cfg.Log.Printf(\"webhook: refused: does not verify\")", to: "if err != nil && false {\n\t\ts.cfg.Log.Printf(\"webhook: refused: does not verify\")",
		pkg: "./internal/server", test: "TestTheWebhookActsOnlyOnAVerifiedRequest", want: "no header: 200"},
	{name: "token: the algorithm is not checked", file: lkToken, from: `json.Unmarshal(hb, &hdr) != nil || hdr.Alg != "HS256"`, to: `json.Unmarshal(hb, &hdr) != nil`,
		pkg: "./internal/livekit", test: "TestVerifyRefusesEveryOtherToken", want: "alg HS512: verified"},
	{name: "token: the issuer is not checked", file: lkToken, from: "if json.Unmarshal(pb, &c) != nil || c.Issuer != k.Key {", to: "if json.Unmarshal(pb, &c) != nil {",
		pkg: "./internal/livekit", test: "TestVerifyRefusesEveryOtherToken", want: "another issuer: verified"},
	{name: "token: the expiry is not checked", file: lkToken, from: "c.Expires == 0 || n > c.Expires+int64(Leeway/time.Second) ||", to: "c.Expires == 0 ||",
		pkg: "./internal/livekit", test: "TestVerifyRefusesEveryOtherToken", want: "expired: verified"},

	// ---- 🔴 ending a meeting ----
	{name: "end: the room is not deleted", file: calls, from: "if err := s.cfg.LiveKit.DeleteRoom(r.Context(), room); err != nil {\n\t\ts.cfg.Log.Printf(\"end: LiveKit",
		to:  "if err := error(nil); room == \"\" && err != nil {\n\t\ts.cfg.Log.Printf(\"end: LiveKit",
		pkg: "./internal/server", test: "TestEndingAMeetingKillsTheLinkDeletesTheRoomAndEndsEveryToken", want: "the room survived End meeting"},
	{name: "end: the meeting stays open", file: parts, from: "`UPDATE meetings SET state = 'ended', ended_at = ? WHERE id = ? AND subject = ? AND state = 'open' RETURNING room`",
		to:  "`UPDATE meetings SET state = 'open', ended_at = ? WHERE id = ? AND subject = ? AND state = 'open' RETURNING room`",
		pkg: "./internal/server", test: "TestEndingAMeetingKillsTheLinkDeletesTheRoomAndEndsEveryToken", want: `no "ended" event`},
	{name: "end: a held token may be in an ended meeting's room", file: parts, from: "`SELECT count(*) FROM meetings m WHERE m.room = ? AND `+live+` AND (",
		to:  "`SELECT count(*) FROM meetings m WHERE m.room = ? AND ? IS NOT NULL AND (",
		pkg: "./internal/server", test: "TestEndingAMeetingKillsTheLinkDeletesTheRoomAndEndsEveryToken", want: "the owner's held token re-created the ended meeting's room and it survived"},
	{name: "ended link: the guest link skips the state check", file: parts, from: "if state != \"open\" || created <= cutoff(now) {\n\t\treturn GuestMeeting{}, ErrGone", to: "if false {\n\t\treturn GuestMeeting{}, ErrGone",
		pkg: "./internal/server", test: "TestEndingAMeetingKillsTheLinkDeletesTheRoomAndEndsEveryToken", want: "the link after End = 200"},
	{name: "ended link: no 24 h cap on the guest link", file: parts, from: "if state != \"open\" || created <= cutoff(now) {\n\t\treturn GuestMeeting{}, ErrGone", to: "if state != \"open\" {\n\t\treturn GuestMeeting{}, ErrGone",
		pkg: "./internal/server", test: "TestTheLinkIs410After24Hours", want: "the page at 24 h = 200"},

	// ---- 🔴 the consent notice ----
	{name: "notice at knock: the knock ignores the notice", file: parts, from: "shown != cur {", to: "false && shown != cur {",
		pkg: "./internal/server", test: "TestAKnockWithAStaleNoticeIs409", want: "a knock with the stale notice = 201"},
	{name: "consent: only an admitted guest locks the switches", file: parts, from: "const knockedSQL = `SELECT count(*) FROM participants WHERE meeting_id = ? AND kind = 'guest'`",
		to:  "const knockedSQL = `SELECT count(*) FROM participants WHERE meeting_id = ? AND kind = 'guest' AND state = 'admitted'`",
		pkg: "./internal/store", test: "TestASwitchGoesBackOnOnlyBeforeAnyGuestKnocked", want: "back on with a guest waiting: <nil>, want ErrSwitchLocked"},
	{name: "consent: a switch never locks", file: parts, from: "\t\tif knocked > 0 {\n\t\t\treturn Notice{}, ErrSwitchLocked", to: "\t\tif false {\n\t\t\treturn Notice{}, ErrSwitchLocked",
		pkg: "./internal/store", test: "TestASwitchGoesBackOnOnlyBeforeAnyGuestKnocked", want: "want ErrSwitchLocked"},
	{name: "consent: Record defaults off", file: repo, from: "SELECT ?, ?, ?, ?, 'open', 1, 1, ?", to: "SELECT ?, ?, ?, ?, 'open', 0, 1, ?",
		pkg: "./internal/store", test: "TestASwitchGoesBackOnOnlyBeforeAnyGuestKnocked", want: "a new meeting: record false, ai true; want both on"},
	{name: "notice is current: the guest stream keeps the notice it opened with", file: guest, from: `snap := map[string]any{"notice": lobby.NoticeFor(cur.Notice.Record, cur.Notice.AI)}`,
		to:  `snap := map[string]any{"notice": lobby.NoticeFor(g.Notice.Record, g.Notice.AI)}`,
		pkg: "./internal/server", test: "TestTheAdmittedGuestSeesTheCurrentNotice", want: `no "meeting" event (the stream so far: "event: token`},

	// ---- 🔴 the guest name ----
	{name: "name: format (bidi) characters accepted", file: name, from: `case unicode.Is(unicode.Cf, r) && r != '\u200D':`, to: `case false:`,
		pkg: "./internal/lobby", test: "TestCleanNameTrimsAndRefusesControlAndBidiCharacters", want: "RIGHT-TO-LEFT OVERRIDE: CleanName"},
	{name: "name: control characters accepted", file: name, from: `case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):`, to: `case false:`,
		pkg: "./internal/lobby", test: "TestCleanNameTrimsAndRefusesControlAndBidiCharacters", want: "a NUL: CleanName"},
	{name: "name: 41 characters accepted", file: name, from: "n < 1 || n > MaxNameRunes", to: "n < 1 || n > MaxNameRunes+1",
		pkg: "./internal/lobby", test: "TestCleanNameTrimsAndRefusesControlAndBidiCharacters", want: "41 characters: CleanName"},
	{name: "name: not trimmed", file: name, from: "\ts = strings.TrimSpace(s)\n", to: "\t_ = strings.TrimSpace\n",
		pkg: "./internal/lobby", test: "TestCleanNameTrimsAndRefusesControlAndBidiCharacters", want: `CleanName("  Grace H.  ")`},
	{name: "name: the knock skips the check", file: guest, from: "name, err := lobby.CleanName(in.Name)", to: "name, err := in.Name, error(nil)",
		pkg: "./internal/server", test: "TestTheGuestNameIsCheckedAndShownAsText", want: "right-to-left override: knock = 201"},
	{name: "name: the owner's lobby renders it as markup", file: appJS, from: "name.textContent = p.name + ' (guest)';", to: "name.innerHTML = p.name + ' (guest)';",
		pkg: "./internal/server", test: "TestNoPageScriptInterpretsMarkup", want: "app.js uses innerHTML"},

	// ---- 🔴 knock limits ----
	{name: "knock limits: no per-meeting cap", file: parts, from: "if waiting >= MaxWaiting {", to: "if false {",
		pkg: "./internal/server", test: "TestKnockLimits", want: "the 21st waiting knock = 201"},
	{name: "knock limits: no per-address cap", file: guest, from: "if !s.limiter.Allow(s.clientAddr(r), now) {", to: "if false {",
		pkg: "./internal/server", test: "TestKnockLimits", want: "the 11th knock from one address = 201"},
	{name: "knock limits: an untrusted peer's X-Forwarded-For is believed", file: limiter, from: "\tif inAny(addr, trusted) {\n", to: "\tif true {\n",
		pkg: "./internal/lobby", test: "TestClientAddr", want: `peer 198.51.100.7:4000, XFF "203.0.113.9": 203.0.113.9, want 198.51.100.7`},

	// ---- 🔴 the knock notification ----
	{name: "knock push: no presence check", file: knocker, from: "case k.hub.OwnerWatching(id):", to: "case false:",
		pkg: "./internal/server", test: "TestTheKnockIsPushedOnlyWhileTheOwnerIsNotLooking", want: "after the owner opened the meeting"},
	{name: "knock push: only the first knock", file: knocker, from: "case waiting > s.sent:", to: "case s.sent == 0:",
		pkg: "./internal/server", test: "TestTheKnockIsPushedOnlyWhileTheOwnerIsNotLooking", want: "after the count rose to 2"},
	{name: "knock push: every lobby change pushes", file: knocker, from: "case waiting > s.sent:", to: "case waiting > 0:",
		pkg: "./internal/server", test: "TestTheKnockIsPushedOnlyWhileTheOwnerIsNotLooking", want: "after a deny"},
	{name: "knock push: an empty lobby withdraws nothing", file: knocker, from: "\t\tk.withdraw(ctx, subject, id, s)\n\t\ts.sent = 0", to: "\t\ts.sent = 0",
		pkg: "./internal/server", test: "TestTheKnockIsPushedOnlyWhileTheOwnerIsNotLooking", want: "after the lobby emptied"},

	// ---- 🔴 the reconcile loop ----
	{name: "reconcile: the ListParticipants loop is removed", file: machine, from: "\t\tfor _, p := range ps {\n\t\t\ts.admitOrRemove(ctx, room, p.Identity)\n\t\t}", to: "\t\t_ = ps",
		pkg: "./internal/server", test: "TestTheReconcileLoopRemovesWhoIsNotAdmittedAndDeletesEndedRooms", want: "after one pass the room holds"},
	{name: "reconcile: ended meetings' rooms are kept", file: machine, from: "if strings.HasPrefix(rm.Name, \"meeting-\") {", to: "if strings.HasPrefix(rm.Name, \"meeting-\") && false {",
		pkg: "./internal/server", test: "TestTheReconcileLoopRemovesWhoIsNotAdmittedAndDeletesEndedRooms", want: "the reconcile pass left an ended meeting's room"},

	// ---- 🔴 ownership of the call ----
	{name: "ownership: a lobby move without the owner filter", file: parts, from: "`SELECT room, state, created_at FROM meetings WHERE id = ? AND subject = ?`", to: "`SELECT room, state, created_at FROM meetings WHERE id = ? AND ? IS NOT NULL`",
		pkg: "./internal/server", test: "TestAnotherPersonCannotDriveTheCall", want: "/admit = 200"},
	{name: "ownership: End without the owner filter", file: parts, from: "WHERE id = ? AND subject = ? AND state = 'open' RETURNING room`", to: "WHERE id = ? AND ? IS NOT NULL AND state = 'open' RETURNING room`",
		pkg: "./internal/server", test: "TestAnotherPersonCannotDriveTheCall", want: "Bea's calls changed Ada's meeting: {ID:"},

	{name: "owner stream: the session is checked only when it opens", file: calls, from: "if ok, err := repo.SessionLive(r.Context(), s.cfg.Now()); err != nil || !ok || !s.app.Live() {", to: "if ok, err := repo.SessionLive(r.Context(), s.cfg.Now()); err != nil || (!ok && false) || !s.app.Live() {",
		pkg: "./internal/server", test: "TestTheOwnersStreamEndsWithTheSession", want: "the stream outlived its session"},

	// ---- 🔴 no ticket in a URL; the pages' policies ----
	{name: "ticket in query: accepted", file: guest, from: "if r.URL.RawQuery != \"\" || r.URL.ForceQuery {", to: "if false {",
		pkg: "./internal/server", test: "TestNoTicketCodeOrNameInAURLOrALog", want: "/wait with ?ticket= = 200"},
	{name: "csp: connect anywhere", file: srv, from: "connect := \"'self'\"", to: "connect := \"*\"",
		pkg: "./internal/server", test: "TestThePagesPoliciesArePinned", want: "the owner page's CSP ="},
	{name: "csp: the guest page may be framed", file: srv, from: "func (s *Server) guestPageCSP() string { return s.pageCSP(\"'none'\") }", to: "func (s *Server) guestPageCSP() string { return s.pageCSP(\"*\") }",
		pkg: "./internal/server", test: "TestThePagesPoliciesArePinned", want: "the guest page's CSP ="},

	// ---- 🔴 round 1: stream caps, reconnects, proxies, names, knock counts ----
	{name: "streams: a new stream of a ticket does not replace the old", file: streams, from: "if old := g.byTicket[ticket]; ticket != \"\" && old != nil {", to: "if old := g.byTicket[ticket]; false && ticket != \"\" && old != nil {",
		pkg: "./internal/server", test: "TestOneStreamPerTicket", want: "a replaced stream still holds its slot"},
	{name: "streams: no per-address cap", file: streams, from: "if g.total >= s.cfg.MaxStreams || g.perAddr[addr] >= s.cfg.StreamsPerAddress {", to: "if g.total >= s.cfg.MaxStreams {",
		pkg: "./internal/server", test: "TestStreamsPerAddressAreCapped", want: "the 4th stream from one address (the owner's) = 200, want 429"},
	{name: "streams: no total cap", file: streams, from: "if g.total >= s.cfg.MaxStreams || g.perAddr[addr] >= s.cfg.StreamsPerAddress {", to: "if g.perAddr[addr] >= s.cfg.StreamsPerAddress {",
		pkg: "./internal/server", test: "TestStreamsAreCappedInAll", want: "the 4th stream in all, from a fresh address = 200, want 429"},
	{name: "reconnect: participant_left revokes the admission", file: machine, from: "\tcase \"participant_left\":\n", to: "\tcase \"participant_left\":\n\t\tif ev.Participant != nil {\n\t\t\ts.store.MarkLeft(ctx, room, ev.Participant.Identity, s.cfg.Now())\n\t\t}\n",
		pkg: "./internal/server", test: "TestAnAdmittedGuestWhoDropsReconnectsWithoutKnocking", want: `no "token" event`},
	{name: "reconnect (real LiveKit): participant_left revokes the admission", file: machine, from: "\tcase \"participant_left\":\n", to: "\tcase \"participant_left\":\n\t\tif ev.Participant != nil {\n\t\t\ts.store.MarkLeft(ctx, room, ev.Participant.Identity, s.cfg.Now())\n\t\t}\n", realLiveKit: true,
		pkg: "./internal/server", test: "TestAgainstARealLiveKitServer", want: "the admitted guest"},
	{name: "reconnect: /events mints no fresh token", file: guest, from: "\t\tif !minted {", to: "\t\tif false && !minted {",
		pkg: "./internal/server", test: "TestAnAdmittedGuestWhoDropsReconnectsWithoutKnocking", want: `no "token" event`},
	{name: "leave: the guest's Leave does not spend the ticket", file: guest, from: "if _, _, err := s.store.MarkLeft(r.Context(), g.Room, g.Identity, s.cfg.Now()); err != nil {", to: "if err := error(nil); err != nil {",
		pkg: "./internal/server", test: "TestAGuestWhoLeavesMustKnockAgain", want: "a left ticket's /wait = 200"},
	{name: "proxies: no warning for an undeclared proxy", file: streams, from: "len(r.Header.Values(\"X-Forwarded-For\")) > 0 && s.xffUntrusted.CompareAndSwap(false, true) {", to: "len(r.Header.Values(\"X-Forwarded-For\")) > 0 && false {",
		pkg: "./internal/server", test: "TestXForwardedForWithoutTrustedProxiesIsWarnedOnce", want: "the warning was logged 0 times"},
	{name: "proxies: /healthz does not say so", file: srv, from: "\t\tif s.xffUntrusted.Load() {", to: "\t\tif false {",
		pkg: "./internal/server", test: "TestXForwardedForWithoutTrustedProxiesIsWarnedOnce", want: "/healthz ="},
	{name: "proxies: a catch-all trusted proxy boots", file: mainGo, from: "if p.Bits() == 0 {", to: "if false {",
		pkg: ".", test: "TestLoadConfig", want: "every IPv4 address trusted: LoadConfig accepted it"},
	{name: "proxies: X-Forwarded-For walked from the left", file: limiter, from: "for i := len(hops) - 1; i >= 0; i-- {", to: "for i := 0; i < len(hops); i++ {",
		pkg: "./internal/lobby", test: "TestClientAddr", want: `XFF "198.51.100.1, 203.0.113.9, 10.9.9.9": 198.51.100.1, want 203.0.113.9`},
	{name: "name: nothing visible is accepted", file: name, from: "\tif !seen {", to: "\tif false {",
		pkg: "./internal/lobby", test: "TestCleanNameTrimsAndRefusesControlAndBidiCharacters", want: "only zero-width joiners: CleanName"},
	{name: "knock push: a fall is not tracked (2 → 1 → 2 is silent)", file: knocker, from: "if waiting > 0 && waiting < s.sent {", to: "if false {",
		pkg: "./internal/server", test: "TestAKnockCountThatFallsAndRisesIsPushedAgain", want: "2 → 1 → 2 did not push again"},

	// ---- 🔴 phase 2c: plain http to muster only inside the cluster ----
	{name: "in-cluster http: allowed to any host", file: client, from: "(loopback || ClusterInternal(h))", to: "(loopback || true)",
		pkg: "./internal/muster", test: "TestCheckBaseURLAllowsHTTPOnlyInsideTheCluster", want: `not in-cluster: CheckBaseURL("http://muster.example.test")`},
	{name: "in-cluster http: refused to a Service name", file: client, from: "(loopback || ClusterInternal(h))", to: "(loopback || false)",
		pkg: "./internal/muster", test: "TestCheckBaseURLAllowsHTTPOnlyInsideTheCluster", want: `in-cluster: CheckBaseURL("http://muster-web.tenant-b.svc:8105")`},
	{name: "in-cluster http: .svc anywhere in the name", file: client, from: "case strings.HasSuffix(h, \".svc\"):", to: "case strings.Contains(h, \".svc\"):",
		pkg: "./internal/muster", test: "TestCheckBaseURLAllowsHTTPOnlyInsideTheCluster", want: `not in-cluster: CheckBaseURL("http://muster.svc.example.test")`},
	{name: "in-cluster http: the labels are not checked", file: client, from: "if !clusterLabel.MatchString(l) {", to: "if !clusterLabel.MatchString(l) && false {",
		pkg: "./internal/muster", test: "TestCheckBaseURLAllowsHTTPOnlyInsideTheCluster", want: `not in-cluster: CheckBaseURL("http://muster..svc")`},

	// ---- 🔴 phase 2c: MEETINGS_CLIENT_IP_HEADER ----
	{name: "client ip header: read from an untrusted peer", file: limiter, from: "\tif inAny(addr, trusted) {\n\t\tif header != \"\" {", to: "\tif header != \"\" || inAny(addr, trusted) {\n\t\tif header != \"\" {",
		pkg: "./internal/lobby", test: "TestClientAddrWithAClientIPHeader", want: "untrusted peer spoofs the header: peer 198.51.100.7:4000"},
	{name: "client ip header: read from an untrusted peer (the knock)", file: limiter, from: "\tif inAny(addr, trusted) {\n\t\tif header != \"\" {", to: "\tif header != \"\" || inAny(addr, trusted) {\n\t\tif header != \"\" {",
		pkg: "./internal/server", test: "TestTheClientIPHeaderIsBelievedOnlyFromATrustedPeer", want: "an untrusted peer's CF-Connecting-IP was believed"},
	{name: "client ip header: ignored, X-Forwarded-For read", file: limiter, from: "\t\tif header != \"\" {\n", to: "\t\tif false {\n",
		pkg: "./internal/server", test: "TestTheClientIPHeaderIsBelievedOnlyFromATrustedPeer", want: "trusted peer: the 11th knock from one CF-Connecting-IP = 201"},
	{name: "client ip header: a repeated header is believed", file: limiter, from: "len(vs) == 1 {", to: "len(vs) >= 1 {",
		pkg: "./internal/lobby", test: "TestClientAddrWithAClientIPHeader", want: "a repeated header leaves the peer"},
	{name: "client ip header: a missing header falls back to X-Forwarded-For", file: limiter, from: "\t\t} else {\n\t\t\taddr = forwardedFor(r, addr, trusted)\n\t\t}", to: "\t\t}\n\t\tif r.Header.Get(header) == \"\" {\n\t\t\taddr = forwardedFor(r, addr, trusted)\n\t\t}",
		pkg: "./internal/lobby", test: "TestClientAddrWithAClientIPHeader", want: "no header leaves the peer"},
	{name: "client ip header: boots without trusted proxies", file: mainGo, from: "if !haveTrusted {", to: "if false {",
		pkg: ".", test: "TestLoadConfigClientIPHeader", want: "set without trusted proxies: LoadConfig = <nil>"},
	{name: "client ip header: X-Forwarded-For accepted as its name", file: mainGo, from: `if v == "X-Forwarded-For" {`, to: `if false {`,
		pkg: ".", test: "TestLoadConfigClientIPHeader", want: `MEETINGS_CLIENT_IP_HEADER="X-Forwarded-For": LoadConfig accepted it`},
	{name: "client ip header: not passed to the server", file: mainGo, from: "\t\tClientIPHeader: cfg.ClientIPHeader}", to: "\t\tClientIPHeader: \"\"}",
		pkg: ".", test: "TestTheProxySettingsReachTheServer", want: `server config: header ""`},

	// ---- 🔴 phase 2c: the deployment files ----
	{name: "deploy: LiveKit auto-creates rooms (kubernetes)", file: kLiveKit, from: "auto_create: false", to: "auto_create: true",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "kubernetes: room.auto_create = true"},
	{name: "deploy: LiveKit auto-creates rooms (compose)", file: composeF, from: "auto_create: false", to: "auto_create: true",
		pkg: "./deploy", test: "TestComposeLiveKitConfig", want: "compose: room.auto_create = true"},
	{name: "deploy: auto_create left to LiveKit's default (true)", file: kLiveKit, from: "                  auto_create: false\n", to: "",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "room.auto_create = <nil> (set: false), want an explicit false"},
	{name: "deploy: TURN over UDP opened", file: kLiveKit, from: "                  tls_port: 5349\n", to: "                  tls_port: 5349\n                  udp_port: 3478\n",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "turn.udp_port = 3478"},
	{name: "deploy: the UDP mux opened", file: kLiveKit, from: "                  tcp_port: 7881\n", to: "                  tcp_port: 7881\n                  udp_port: 7882\n",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "rtc.udp_port = 7882"},
	{name: "deploy: the ICE range inside the NodePort range", file: kLiveKit, from: "port_range_start: 50000\n                  port_range_end: 50199", to: "port_range_start: 32000\n                  port_range_end: 32199",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "rtc.port_range 32000-32199 overlaps the NodePort range"},
	{name: "deploy: the relay range left to LiveKit's default (30000-40000)", file: kLiveKit, from: "                  relay_range_start: 40000\n                  relay_range_end: 40199\n", to: "",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "turn.relay_range = <nil>-<nil>, want a pinned range"},
	{name: "deploy: TURN may relay to every private address", file: kLiveKit, from: "allow_restricted_peer_cidrs: [$(LIVEKIT_NODE_IP)/32]", to: "allow_restricted_peer_cidrs: [10.0.0.0/8, $(LIVEKIT_NODE_IP)/32]",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "turn.allow_restricted_peer_cidrs = [10.0.0.0/8"},
	{name: "deploy: the webhook goes to the public port", file: kLiveKit, from: "urls: [http://muster-meetings:8081/livekit/webhook]", to: "urls: [http://muster-meetings:8080/livekit/webhook]",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "webhook.urls = [http://muster-meetings:8080/livekit/webhook]"},
	{name: "deploy: the TURN host's variable is defined after the config", file: kLiveKit, from: "            - name: TURN_DOMAIN\n", to: "            - name: TURN_HOST\n",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "LIVEKIT_CONFIG uses $(TURN_DOMAIN), which is not defined before it"},
	{name: "deploy: LiveKit's image by tag only", file: kLiveKit, from: "livekit-server:v1.13.11@sha256:", to: "livekit-server:v1.13.11-unpinned@sha256:",
		pkg: "./deploy", test: "TestKubernetesLiveKitConfig", want: "want livekit-server v1.13.11 pinned by digest"},
	{name: "deploy: a Service exposes the UDP mux", file: kLiveKit, from: "    - name: turn-tls\n      port: 5349\n      targetPort: turn-tls\n", to: "    - name: turn-tls\n      port: 5349\n      targetPort: turn-tls\n    - name: udp-mux\n      port: 7882\n      protocol: UDP\n",
		pkg: "./deploy", test: "TestKubernetesExposesNoDirectMedia", want: "Service livekit-nodeport exposes 7882/UDP"},
	{name: "deploy: LiveKit on the host's network", file: kLiveKit, from: "      automountServiceAccountToken: false\n      enableServiceLinks: false\n      securityContext:\n        runAsNonRoot: true\n        runAsUser: 65532\n        runAsGroup: 65532\n        seccompProfile:", to: "      hostNetwork: true\n      automountServiceAccountToken: false\n      enableServiceLinks: false\n      securityContext:\n        runAsNonRoot: true\n        runAsUser: 65532\n        runAsGroup: 65532\n        seccompProfile:",
		pkg: "./deploy", test: "TestKubernetesExposesNoDirectMedia", want: "Deployment livekit uses hostNetwork"},
	{name: "deploy: the plugin's root filesystem writable", file: kPlugin, from: "            readOnlyRootFilesystem: true", to: "            readOnlyRootFilesystem: false",
		pkg: "./deploy", test: "TestKubernetesExposesNoDirectMedia", want: "Deployment muster-meetings: the container may write its root filesystem"},
	{name: "deploy: two plugin replicas (two SQLite writers)", file: kPlugin, from: "  replicas: 1\n  strategy:\n    type: Recreate", to: "  replicas: 2\n  strategy:\n    type: Recreate",
		pkg: "./deploy", test: "TestKubernetesPlugin", want: "plugin replicas 2"},
	{name: "deploy: the plugin's internal port open to every pod", file: kNetPol, from: "    - from:\n        - podSelector:\n            matchLabels:\n              app.kubernetes.io/name: livekit\n      ports:\n        - {protocol: TCP, port: 8081}", to: "    - ports:\n        - {protocol: TCP, port: 8081}",
		pkg: "./deploy", test: "TestKubernetesNetworkPolicies", want: "the plugin's :8081 admits [ANYONE]"},
	{name: "deploy: LiveKit may reach the plugin's public port", file: kNetPol, from: "              app.kubernetes.io/name: muster-meetings\n      ports:\n        - {protocol: TCP, port: 8081}", to: "              app.kubernetes.io/name: muster-meetings\n      ports:\n        - {protocol: TCP, port: 8081}\n        - {protocol: TCP, port: 8080}",
		pkg: "./deploy", test: "TestKubernetesNetworkPolicies", want: "LiveKit may reach port 8080"},
	{name: "deploy: a param nobody reads", file: kBase, from: "      - client-ip-header=\n", to: "      - client-ip-header=\n      - unread=x\n",
		pkg: "./deploy", test: "TestKubernetesParamsAreAllUsed", want: "they must match"},
	{name: "deploy: the compose and kubernetes LiveKit configs drift", file: composeF, from: "          departure_timeout: 600", to: "          departure_timeout: 20",
		pkg: "./deploy", test: "TestKubernetesAndComposeLiveKitConfigsAgree", want: "the LiveKit configs differ"},
	{name: "deploy: compose node_ip is not LiveKit's address", file: composeF, from: "          node_ip: ${MEETINGS_LIVEKIT_IP:-172.31.250.10}", to: "          node_ip: 172.31.250.10",
		pkg: "./deploy", test: "TestComposeLiveKitConfig", want: "compose: rtc.node_ip = 172.31.250.10, want \"172.31.250.77\""},
	{name: "deploy: compose publishes the media port", file: composeF, from: "      - \"${MEETINGS_BIND:-127.0.0.1}:${MEETINGS_TURN_PORT:-8108}:5349\"\n", to: "      - \"${MEETINGS_BIND:-127.0.0.1}:${MEETINGS_TURN_PORT:-8108}:5349\"\n      - \"127.0.0.1:7882:7882/udp\"\n",
		pkg: "./deploy", test: "TestComposeLiveKitConfig", want: "compose livekit publishes 127.0.0.1:7882:7882/udp"},
	{name: "deploy: a real hostname in the base", file: kBase, from: "muster-origin=https://muster.example.test", to: "muster-origin=https://muster.example.com",
		pkg: "./deploy", test: "TestTheDeploymentFilesNameNoRealHost", want: `names the host "muster.example.com"`},
	{name: "deploy: a real address in the base", file: kBase, from: "      - client-ip-header=\n", to: "      - client-ip-header=\n# 8.8.4.4\n",
		pkg: "./deploy", test: "TestTheDeploymentFilesNameNoRealHost", want: "names the address 8.8.4.4"},

	// ---- configuration ----
	{name: "config: allow the plugin on muster's own origin", file: mainGo, from: `if c.PublicURL == c.MusterOrigin {`, to: `if false {`,
		pkg: ".", test: "TestLoadConfig", want: "public URL is muster's origin: LoadConfig accepted it"},
}

func main() {
	list := flag.Bool("list", false, "print the mutants' names and stop")
	match := flag.String("match", "", "run only the mutants whose name matches this regular expression (the controls always run)")
	flag.Parse()
	if *match != "" {
		re, err := regexp.Compile(*match)
		if err != nil {
			fatalf("-match: %v", err)
		}
		var keep []mutant
		for _, m := range mutants {
			if strings.HasPrefix(m.name, "control: ") || re.MatchString(m.name) {
				keep = append(keep, m)
			}
		}
		mutants = keep
	}
	if *list {
		for _, m := range mutants {
			fmt.Println(m.name)
		}
		return
	}
	for _, m := range mutants {
		if m.realLiveKit && (lookup("MEETINGS_LIVEKIT_SERVER", "livekit-server") == "" || lookup("MEETINGS_LK_CLI", "lk") == "") {
			fatalf("mutant %q is killed by the test against a real LiveKit: set MEETINGS_LIVEKIT_SERVER and MEETINGS_LK_CLI, or run under `nix-shell -p livekit livekit-cli` (the sweep never passes a mutant by skipping its test)", m.name)
		}
	}
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		fatalf("git status: %v (run it from the repository root)", err)
	}
	if len(bytes.TrimSpace(out)) > 0 {
		fatalf("the tree is dirty; commit or set your work aside first (a mutant must never mix with uncommitted work):\n%s", out)
	}
	restoreOnSignal()
	sums := map[string][32]byte{}
	for _, m := range mutants {
		for _, e := range m.edits() {
			b, err := os.ReadFile(e.file)
			if err != nil {
				fatalf("%s: %v", e.file, err)
			}
			sums[e.file] = sha256.Sum256(b)
		}
	}
	failed := 0
	fmt.Printf("%-58s %-9s %s\n", "mutant", "result", "killing test: message")
	for _, m := range mutants {
		res, detail := run(m)
		ok := (res == "KILLED" && !m.survive) || (res == "SURVIVED" && m.survive)
		if !ok {
			failed++
		}
		mark := ""
		if !ok {
			mark = "  <-- FAIL"
		}
		fmt.Printf("%-58s %-9s %s: %s%s\n", m.name, res, m.test, detail, mark)
	}
	for f, want := range sums {
		b, _ := os.ReadFile(f)
		if sha256.Sum256(b) != want {
			fatalf("%s was not restored byte for byte", f)
		}
	}
	if failed > 0 {
		fatalf("%d of %d mutants did not behave", failed, len(mutants))
	}
	fmt.Printf("all %d mutants behaved (every security mutant killed, the no-op survived)\n", len(mutants))
}

// run applies one mutant, runs its killing test, and restores the file.
func run(m mutant) (string, string) {
	orig := map[string][]byte{}
	cur := map[string][]byte{}
	for _, e := range m.edits() {
		if _, ok := orig[e.file]; !ok {
			b, err := os.ReadFile(e.file)
			if err != nil {
				return "ERROR", err.Error()
			}
			orig[e.file], cur[e.file] = b, b
		}
		if n := bytes.Count(cur[e.file], []byte(e.from)); n != 1 {
			return "ERROR", fmt.Sprintf("the text to replace occurs %d times in %s, want exactly 1", n, e.file)
		}
		cur[e.file] = bytes.Replace(cur[e.file], []byte(e.from), []byte(e.to), 1)
	}
	setLive(orig)
	defer func() {
		live.Lock()
		defer live.Unlock()
		for f, b := range orig {
			if err := os.WriteFile(f, b, 0o644); err != nil {
				fatalf("restoring %s: %v", f, err)
			}
		}
		live.orig = nil
	}()
	for f, b := range cur {
		if err := os.WriteFile(f, b, 0o644); err != nil {
			return "ERROR", err.Error()
		}
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "^"+m.test+"$", m.pkg)
	if m.realLiveKit {
		cmd.Env = append(os.Environ(), "MEETINGS_REQUIRE_LIVEKIT=1")
	}
	if m.pkg == "./deploy" {
		// The deployment tests render with kubectl: fail, never skip.
		cmd.Env = append(os.Environ(), "MEETINGS_REQUIRE_KUBECTL=1")
	}
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err == nil {
		// The positive check that the test ran at all: "ok" for the package
		// and no "no tests to run".
		if strings.Contains(s, "no tests to run") {
			return "ERROR", "the -run filter matched no test"
		}
		return "SURVIVED", "test passed"
	}
	if !strings.Contains(s, "--- FAIL: "+m.test) {
		first := firstLine(s)
		return "ERROR", "failed without the named test failing (a build error?): " + first
	}
	if m.want != "" && !strings.Contains(s, m.want) {
		return "ERROR", fmt.Sprintf("the test failed, but not with %q: %s", m.want, failLine(s))
	}
	return "KILLED", failLine(s)
}

// lookup is a binary named by env, or found on PATH; "" for neither.
func lookup(env, name string) string {
	if p := os.Getenv(env); p != "" {
		return p
	}
	p, _ := exec.LookPath(name)
	return p
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// failLine is the first assertion line of the failure output.
func failLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "_test.go:") {
			if len(l) > 160 {
				l = l[:160] + "…"
			}
			return l
		}
	}
	return firstLine(s)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mutate: "+f+"\n", a...)
	os.Exit(1)
}
