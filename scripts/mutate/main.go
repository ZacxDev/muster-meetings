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
//	go run ./scripts/mutate          # every mutant
//	go run ./scripts/mutate -list    # names only
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
	{name: "frame-ancestors: any origin", file: srv, from: `form-action 'none'; frame-ancestors " + s.cfg.MusterOrigin`, to: `form-action 'none'; frame-ancestors *"`,
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

	// ---- configuration ----
	{name: "config: allow the plugin on muster's own origin", file: mainGo, from: `if c.PublicURL == c.MusterOrigin {`, to: `if false {`,
		pkg: ".", test: "TestLoadConfig", want: "public URL is muster's origin: LoadConfig accepted it"},
}

func main() {
	list := flag.Bool("list", false, "print the mutants' names and stop")
	flag.Parse()
	if *list {
		for _, m := range mutants {
			fmt.Println(m.name)
		}
		return
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
