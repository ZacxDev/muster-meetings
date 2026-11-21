package owners_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/mustertest"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

var (
	ctx = context.Background()
	t0  = time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
	// testLinkKey: 32 bytes, no pattern any code names.
	testLinkKey = []byte("fixture link key 7a6b5c4d3e2f109")
	// Fixtures that equal no constant the code or the fake names (the fake's
	// DefaultApp is plugin 3).
	app = mustertest.App{Token: "mpl_server_7f3c91e0", PluginID: 12, PluginName: "meet-srv", Capabilities: []string{"app", "tasks"}}
	ada = mustertest.Person{ID: 4093716825, Email: "ada.lovelace@example.test", DisplayName: "Ada L."}
	bea = mustertest.Person{ID: 2718281828, Email: "bea.k@example.test", DisplayName: "Bea K."}
)

func now() time.Time { return t0 }

func setup(t *testing.T) (*mustertest.Fake, *store.Store, *muster.Client, *bytes.Buffer) {
	t.Helper()
	f := mustertest.New()
	t.Cleanup(f.Close)
	f.Now = now
	f.Install(app)
	f.AddPerson(ada)
	f.AddPerson(bea)
	c, err := muster.New(f.URL, app.Token)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), testLinkKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return f, st, c, &bytes.Buffer{}
}

// held gives p a session and an open meeting, as the server would after a
// real redeem through the fake, and returns p's subject and the session.
func held(t *testing.T, f *mustertest.Fake, st *store.Store, c *muster.Client, p mustertest.Person) (string, string, string) {
	t.Helper()
	red, err := c.Redeem(ctx, f.MintApp(app.PluginID, p.ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := st.CreateSession(ctx, red.Owner.Subject, red.Owner.DisplayName, t0)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := st.LookupSession(ctx, tok, t0)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.ForSession(sess).CreateMeeting(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	return red.Owner.Subject, tok, m.Meeting.ID
}

func state(t *testing.T, st *store.Store, subject, id string) string {
	t.Helper()
	m, err := st.ForSubject(subject).Meeting(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return m.State
}

func TestBootReadsTheServerApp(t *testing.T) {
	_, st, c, buf := setup(t)
	a, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if a.PluginID != app.PluginID || a.Name != app.PluginName || !a.FilesTasks || !a.Live() {
		t.Fatalf("Boot = plugin %d %q tasks %v live %v", a.PluginID, a.Name, a.FilesTasks, a.Live())
	}
}

// fork 8: one server token. A person's own token (scope person), or a
// server app without `app`, refuses to boot.
func TestBootRefusesAPersonalTokenOrOneWithoutApp(t *testing.T) {
	f, st, _, buf := setup(t)
	personal := mustertest.Personal{Token: "mpl_personal_a81c", PluginID: 23, OwnerID: ada.ID, OwnerLogin: ada.Email, Capabilities: []string{"app"}}
	f.AddPersonal(personal)
	noApp := mustertest.App{Token: "mpl_server_noapp_33d0", PluginID: 29, PluginName: "no-app", Capabilities: []string{"tasks"}}
	f.Install(noApp)
	for name, tok := range map[string]string{"a personal token": personal.Token, "no app": noApp.Token} {
		c, _ := muster.New(f.URL, tok)
		if _, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now); err == nil {
			t.Errorf("%s: Boot accepted it", name)
		} else if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), ada.Email) {
			t.Errorf("%s: the refusal names the token or the owner: %v", name, err)
		}
	}
}

// An uninstall the plugin slept through: Boot's /self gets 401, ends every
// session and open meeting, and refuses to serve.
func TestBootThatMusterAnswers401EndsEverything(t *testing.T) {
	f, st, c, buf := setup(t)
	sa, ta, ma := held(t, f, st, c, ada)
	sb, tb, mb := held(t, f, st, c, bea)
	f.Uninstall(app.PluginID)
	if _, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now); !errors.Is(err, owners.ErrRevoked) {
		t.Fatalf("Boot after an uninstall = %v, want ErrRevoked", err)
	}
	for name, tok := range map[string]string{"Ada": ta, "Bea": tb} {
		if _, err := st.LookupSession(ctx, tok, t0); err != store.ErrNotFound {
			t.Errorf("%s's session survived the boot: %v", name, err)
		}
	}
	if state(t, st, sa, ma) != "ended" || state(t, st, sb, mb) != "ended" {
		t.Fatal("an open meeting survived a boot whose /self was 401")
	}
}

// 🔴 The boot reconcile: before serving anyone, Boot asks owners/check about
// every subject it holds and ends each one muster no longer answers live,
// so a Hide, a disable, or a reinstall's new subjects (fork 7) that the
// plugin slept through is applied first. Bea, still live, is untouched.
func TestBootEndsWhoMusterNoLongerAnswersLive(t *testing.T) {
	f, st, c, buf := setup(t)
	sa, ta, ma := held(t, f, st, c, ada)
	sb, tb, mb := held(t, f, st, c, bea)
	// A subject from a former install muster has never heard of.
	gone := "Gq3Ws8Ed1Rf6Tg0Yh5Uj2k"
	gtok, _, _ := st.CreateSession(ctx, gone, "Old", t0)
	f.Hide(app.PluginID, ada.ID)

	a, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupSession(ctx, ta, t0); err != store.ErrNotFound {
		t.Errorf("boot: Ada's session survived (%v): the reconcile did not run", err)
	}
	if state(t, st, sa, ma) != "ended" {
		t.Errorf("boot: Ada's meeting stayed open")
	}
	if _, err := st.LookupSession(ctx, gtok, t0); err != store.ErrNotFound {
		t.Errorf("boot: a former install's subject kept its session")
	}
	if _, err := st.LookupSession(ctx, tb, t0); err != nil {
		t.Errorf("boot: Bea's session was dropped: %v", err)
	}
	if state(t, st, sb, mb) != "open" {
		t.Errorf("boot: Bea's meeting was ended")
	}
	if !a.Live() {
		t.Fatal("one person's end revoked the app")
	}
	if strings.Contains(buf.String(), app.Token) || strings.Contains(buf.String(), sa) {
		t.Fatalf("the log names the token or a subject: %s", buf)
	}
}

// Boot will not serve on a reconcile it could not do.
func TestBootStopsWhenOwnersCheckFailsOrMusterDoesNotAnswer(t *testing.T) {
	f, st, c, buf := setup(t)
	_, ta, _ := held(t, f, st, c, ada)
	f.SetFailOwnersCheck(true)
	if _, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now); err == nil {
		t.Fatal("Boot with a failing owners/check succeeded")
	}
	if _, err := st.LookupSession(ctx, ta, t0); err != nil {
		t.Fatalf("a failing owners/check ended a session: %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c2, _ := muster.New(url, app.Token)
	if _, err := owners.Boot(ctx, c2, st, log.New(buf, "", 0), now); err == nil {
		t.Fatal("Boot without muster succeeded")
	}
}

// Rotate keeps the subjects (fork 7): the new token's owners/check still
// answers Ada live. The old token is dead for good.
func TestARotatedTokenEndsEverythingAndTheNewOneKeepsTheSubjects(t *testing.T) {
	f, st, c, buf := setup(t)
	a, err := owners.Boot(ctx, c, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	sa, ta, _ := held(t, f, st, c, ada)
	f.Rotate(app.PluginID, "mpl_server_rotated_4c2e")
	a.PollOnce(ctx)
	if a.Live() {
		t.Fatal("the rotated-away token is still live")
	}
	if _, err := st.LookupSession(ctx, ta, t0); err != store.ErrNotFound {
		t.Fatal("a session survived the rotation")
	}
	c2, _ := muster.New(f.URL, "mpl_server_rotated_4c2e")
	live, err := c2.OwnersCheck(ctx, []string{sa})
	if err != nil || !live[sa] {
		t.Fatalf("the new token's owners/check = %v, %v: the subject was lost", live, err)
	}
}
