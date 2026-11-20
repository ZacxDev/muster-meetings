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

// live is the file a mutant is applied to right now, and its original bytes:
// what a signal must put back.
var live struct {
	sync.Mutex
	file string
	orig []byte
}

func setLive(file string, orig []byte) {
	live.Lock()
	live.file, live.orig = file, orig
	live.Unlock()
}

// restoreOnSignal puts the mutated file back on SIGINT or SIGTERM, then exits.
func restoreOnSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-ch
		live.Lock() // held to exit: no mutant is applied after this
		if live.file != "" {
			if err := os.WriteFile(live.file, live.orig, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "mutate: %v: COULD NOT RESTORE %s: %v\n", sig, live.file, err)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "mutate: %v: restored %s\n", sig, live.file)
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
}

const (
	srv    = "internal/server/server.go"
	repo   = "internal/store/repo.go"
	st     = "internal/store/store.go"
	secret = "internal/store/secret.go"
	own    = "internal/owners/owners.go"
	mainGo = "main.go"
	link   = "internal/store/link.go"
)

var mutants = []mutant{
	// ---- controls ----
	{name: "control: known-caught (join code 64 bits)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 8",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", want: "is not 22 base64url characters"},
	{name: "control: a no-op edit (must survive)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 16 // no-op",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", survive: true},

	// ---- /enter: the redeem ----
	{name: "redeem: accept any purpose", file: srv, from: `if red.Purpose != "app" {`, to: `if false && red.Purpose != "app" {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAnAppCodeForTheTokensOwner", want: "purpose view, target app: enter = 200"},
	{name: "redeem: accept a mirror beside the app target", file: srv, from: `if red.Target.App == nil || len(red.Target.Mirror) > 0 {`, to: `if red.Target.App == nil {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAnAppCodeForTheTokensOwner", want: "purpose app, both targets: enter = 200"},
	{name: "redeem: accept a mirror-only target", file: srv, from: `if red.Target.App == nil || len(red.Target.Mirror) > 0 {`, to: `if len(red.Target.Mirror) > 0 && red.Target.App != nil {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAnAppCodeForTheTokensOwner", want: "purpose app, target mirror: enter = 200"},
	{name: "redeem: accept another owner's code", file: srv, from: `if red.OwnerID != tok.OwnerID {`, to: `if false {`,
		pkg: "./internal/server", test: "TestEnterRefusesAnythingButAnAppCodeForTheTokensOwner", want: "another owner: enter = 200"},
	{name: "plugin bound: ignore muster_plugin (always plugin 12)", file: srv, from: `pluginID, err := strconv.ParseInt(pid, 10, 64)`, to: `pluginID, err := strconv.ParseInt("12", 10, 64)`,
		pkg: "./internal/server", test: "TestEnterRedeemsWithTheTokenMusterPluginNamesAndNoOther", want: "an unserved plugin ="},
	{name: "redeem: pass another owner's ref to the page", file: srv,
		from: `if _, err := s.store.ForOwner(tok.OwnerID).Meeting(r.Context(), red.Target.App.Ref); err == nil {`, to: `if true {`,
		pkg: "./internal/server", test: "TestEnterPassesTheRefOnlyForTheOwnersOwnMeeting", want: "another owner's ref reached the page"},

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
	{name: "session: no liveness check per request", file: srv, from: `if t, live := s.reg.Lookup(sess.PluginID); !live || t.OwnerID != sess.OwnerID {`, to: `if false {`,
		pkg: "./internal/server", test: "TestASessionOfATokenNotServedIsRefused", want: "an unserved token's session = 200"},
	{name: "session: liveness ignores the owner", file: srv, from: `if t, live := s.reg.Lookup(sess.PluginID); !live || t.OwnerID != sess.OwnerID {`, to: `if _, live := s.reg.Lookup(sess.PluginID); !live {`,
		pkg: "./internal/server", test: "TestASessionOfATokenNotServedIsRefused", want: "another owner of a served token session = 200"},
	{name: "session: token stored in clear", file: st, from: `Hash(tok), ownerID, pluginID, ms(now), ms(exp)`, to: `tok, ownerID, pluginID, ms(now), ms(exp)`,
		pkg: "./internal/store", test: "TestSessionTokenIsStoredOnlyHashed", want: "the session token itself is in the database"},
	{name: "session: 16 random bytes", file: secret, from: "SessionTokenBytes = 32", to: "SessionTokenBytes = 16",
		pkg: "./internal/store", test: "TestSessionTokenAndMeetingIDShapes", want: "want mmo_ and 32 random bytes"},

	// ---- the /self poll ----
	{name: "revocation: the poll ignores /self's 401", file: own, from: "r.Revoke(ctx, t.PluginID)", to: "_ = 0",
		pkg: "./internal/owners", test: "TestAPollThatMusterAnswers401DeletesTheOwnersSessionsAndEndsTheirMeetings", want: "revoked owner's session after one poll"},
	{name: "revocation: sessions not deleted", file: st, from: "`DELETE FROM owner_sessions WHERE plugin_id = ?`", to: "`DELETE FROM owner_sessions WHERE plugin_id = ? AND 0`",
		pkg: "./internal/server", test: "TestTheSelfPollEndsARevokedOwnersSessionsAndMeetingsAndNoOneElses", want: "sessions left in the database"},
	{name: "revocation: meetings not ended", file: st, from: "WHERE owner_id = ? AND state = 'open'`, ms(now), ownerID)", to: "WHERE owner_id = ? AND state = 'open' AND 0`, ms(now), ownerID)",
		pkg: "./internal/server", test: "TestTheSelfPollEndsARevokedOwnersSessionsAndMeetingsAndNoOneElses", want: "the revoked owner's meeting"},
	{name: "revocation: deletes every plugin's sessions", file: st, from: "`DELETE FROM owner_sessions WHERE plugin_id = ?`", to: "`DELETE FROM owner_sessions WHERE plugin_id = ? OR 1`",
		pkg: "./internal/owners", test: "TestAPollThatMusterAnswers401DeletesTheOwnersSessionsAndEndsTheirMeetings", want: "the other owner's session"},
	{name: "revocation: any poll error revokes", file: own, from: "if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {\n\t\t\tr.Revoke", to: "if true {\n\t\t\tr.Revoke",
		pkg: "./internal/owners", test: "TestAPollThatFailsOtherwiseKeepsTheToken", want: "a token was dropped without muster's 401"},
	{name: "boot: skip the reconcile", file: own, from: "rec, err := st.ReconcileLive(ctx, plugins, owners, now())", to: "_, _ = plugins, owners\n\trec, err := store.Revoked{}, error(nil)",
		pkg: "./internal/owners", test: "TestBootAppliesARevocationThePluginSleptThrough", want: "a session of the revoked token survived the boot"},
	{name: "boot: accept a token without app", file: own, from: "if !slices.Contains(s.Capabilities, Capability) {", to: "if !slices.Contains(s.Capabilities, Capability) && false {",
		pkg: "./internal/owners", test: "TestBootRefusesATokenWithoutAppOrATeamsToken", want: "no app: Boot accepted it"},

	// ---- ownership ----
	{name: "ownership: list without the owner filter", file: repo, from: "` FROM meetings WHERE owner_id = ?`", to: "` FROM meetings WHERE ? IS NOT NULL`",
		pkg: "./internal/server", test: "TestAnotherOwnersMeetingsAreNeverVisible", want: "B's list ="},
	{name: "ownership: one meeting without the owner filter", file: repo,
		from: "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND owner_id = ?`",
		to:   "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND ? IS NOT NULL`",
		pkg:  "./internal/server", test: "TestAnotherOwnersMeetingsAreNeverVisible", want: "B reading A's"},
	{name: "ownership: repository list (store level)", file: repo, from: "` FROM meetings WHERE owner_id = ?`", to: "` FROM meetings WHERE ? IS NOT NULL`",
		pkg: "./internal/store", test: "TestOwnerRepoNeverReturnsAnotherOwnersMeetings", want: "A's list ="},

	// ---- the link ----
	{name: "link: code stored in clear (not hashed)", file: repo, from: `Hash(code), ms(now)`, to: `code, ms(now)`,
		pkg: "./internal/store", test: "TestJoinCodeIsNeverStoredInClear", want: "the join code itself is in the database"},
	{name: "link: code 64 bits (API)", file: secret, from: "JoinCodeBytes = 16", to: "JoinCodeBytes = 8",
		pkg: "./internal/server", test: "TestNewMeetingAnswersItsIDAndALinkOnThePluginsOrigin", want: "join_url ="},
	{name: "link: the key is not in the HMAC", file: link, from: "hmac.New(sha256.New, s.linkKey)", to: "hmac.New(sha256.New, nil)",
		pkg: "./internal/store", test: "TestAnotherKeyGivesTheOwnerAnotherLink", want: "under another key"},
	{name: "link: the meeting id is not in the HMAC", file: link, from: "m.Write([]byte(linkDomain + meetingID))", to: "m.Write([]byte(linkDomain))",
		pkg: "./internal/store", test: "TestJoinCodeIs128BitsIn22Base64urlCharacters", want: "drawn twice"},
	{name: "link: another owner reads it (store)", file: repo,
		from: "m, err := scanMeeting(r.s.db.QueryRowContext(ctx, `SELECT `+meetingCols+` FROM meetings WHERE id = ? AND owner_id = ?`",
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
	{name: "race: create without the live-session check (the pre-fix code)", file: repo,
		from: "WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND owner_id = ? AND expires_at > ?)`",
		to:   "WHERE ? IS NOT NULL AND ? IS NOT NULL AND ? IS NOT NULL`",
		pkg:  "./internal/server", test: "TestANewMeetingRacingARevocationLeavesNoOpenMeeting", want: "an open meeting for the revoked owner"},
	{name: "race: create without the live-session check (store)", file: repo,
		from: "WHERE EXISTS (SELECT 1 FROM owner_sessions WHERE token_sha256 = ? AND owner_id = ? AND expires_at > ?)`",
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
		b, err := os.ReadFile(m.file)
		if err != nil {
			fatalf("%s: %v", m.file, err)
		}
		sums[m.file] = sha256.Sum256(b)
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
	orig, err := os.ReadFile(m.file)
	if err != nil {
		return "ERROR", err.Error()
	}
	if n := bytes.Count(orig, []byte(m.from)); n != 1 {
		return "ERROR", fmt.Sprintf("the text to replace occurs %d times in %s, want exactly 1", n, m.file)
	}
	mut := bytes.Replace(orig, []byte(m.from), []byte(m.to), 1)
	setLive(m.file, orig)
	if err := os.WriteFile(m.file, mut, 0o644); err != nil {
		return "ERROR", err.Error()
	}
	defer func() {
		live.Lock()
		defer live.Unlock()
		if err := os.WriteFile(m.file, orig, 0o644); err != nil {
			fatalf("restoring %s: %v", m.file, err)
		}
		live.file, live.orig = "", nil
	}()
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
