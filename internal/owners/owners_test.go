package owners_test

import (
	"bytes"
	"context"
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
	// Fixture ids that equal no constant the code or the fake names (the
	// fake's Default is plugin 3, owner 1).
	pA = mustertest.Plugin{Token: "mpl_tokenA_7f3c", PluginID: 12, PluginName: "meet-a", OwnerID: 7, OwnerLogin: "a@example.test", OwnerKind: "person", Capabilities: []string{"app", "tasks"}}
	pB = mustertest.Plugin{Token: "mpl_tokenB_91e0", PluginID: 19, PluginName: "meet-b", OwnerID: 41, OwnerLogin: "b@example.test", OwnerKind: "person", Capabilities: []string{"app"}}
)

func setup(t *testing.T, ps ...mustertest.Plugin) (*mustertest.Fake, *store.Store, []*muster.Client, *bytes.Buffer) {
	t.Helper()
	f := mustertest.New()
	t.Cleanup(f.Close)
	var cs []*muster.Client
	for _, p := range ps {
		f.AddPlugin(p)
		c, err := muster.New(f.URL, p.Token)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return f, st, cs, &bytes.Buffer{}
}

func now() time.Time { return t0 }

// "Session revocation: a 401 from the fake /self deletes the owner's sessions
// and ends their meetings within one poll. Red if: the poll's handler is a
// no-op."
func TestAPollThatMusterAnswers401DeletesTheOwnersSessionsAndEndsTheirMeetings(t *testing.T) {
	f, st, cs, buf := setup(t, pA, pB)
	reg, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	sa, _, _ := st.CreateSession(ctx, pA.OwnerID, pA.PluginID, t0)
	sb, _, _ := st.CreateSession(ctx, pB.OwnerID, pB.PluginID, t0)
	ma, _ := st.ForOwner(pA.OwnerID).CreateMeeting(ctx, store.NewMeeting{}, t0)
	mb, _ := st.ForOwner(pB.OwnerID).CreateMeeting(ctx, store.NewMeeting{}, t0)

	reg.PollOnce(ctx) // both live: nothing changes
	if _, err := st.LookupSession(ctx, sa, t0); err != nil {
		t.Fatalf("a live token's session was dropped by a poll: %v", err)
	}

	f.Revoke(pA.Token)
	reg.PollOnce(ctx)

	if _, err := st.LookupSession(ctx, sa, t0); err != store.ErrNotFound {
		t.Fatalf("revoked owner's session after one poll = %v, want deleted", err)
	}
	if m, _, _ := st.ForOwner(pA.OwnerID).Meeting(ctx, ma.Meeting.ID); m.State != "ended" {
		t.Fatalf("revoked owner's meeting after one poll = %q, want ended", m.State)
	}
	if _, ok := reg.Lookup(pA.PluginID); ok {
		t.Fatal("the revoked token is still in the registry")
	}
	// The other owner is untouched.
	if _, err := st.LookupSession(ctx, sb, t0); err != nil {
		t.Fatalf("the other owner's session = %v", err)
	}
	if m, _, _ := st.ForOwner(pB.OwnerID).Meeting(ctx, mb.Meeting.ID); m.State != "open" {
		t.Fatalf("the other owner's meeting = %q", m.State)
	}
	if _, ok := reg.Lookup(pB.PluginID); !ok {
		t.Fatal("the other token left the registry")
	}
	if strings.Contains(buf.String(), pA.Token) {
		t.Fatal("the log names a token")
	}
}

// Only muster's 401 ends a token: a 5xx or no answer keeps it (the positive
// control for "a poll revokes": the poll is not simply revoking on error).
func TestAPollThatFailsOtherwiseKeepsTheToken(t *testing.T) {
	f, st, cs, buf := setup(t, pA)
	reg, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	sa, _, _ := st.CreateSession(ctx, pA.OwnerID, pA.PluginID, t0)
	f.Off = true // 404: plugins off on that server
	reg.PollOnce(ctx)
	f.Close() // no answer at all
	reg.PollOnce(ctx)
	if _, ok := reg.Lookup(pA.PluginID); !ok {
		t.Fatal("a token was dropped without muster's 401")
	}
	if _, err := st.LookupSession(ctx, sa, t0); err != nil {
		t.Fatalf("a session was dropped without muster's 401: %v", err)
	}
}

// Two tokens for one owner (two app tabs): revoking one deletes the sessions
// minted through it but leaves the owner's meetings to the other.
func TestRevokingOneOfAnOwnersTwoTokensKeepsTheirMeetings(t *testing.T) {
	p2 := pB
	p2.OwnerID, p2.OwnerLogin = pA.OwnerID, pA.OwnerLogin
	f, st, cs, buf := setup(t, pA, p2)
	reg, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	s1, _, _ := st.CreateSession(ctx, pA.OwnerID, pA.PluginID, t0)
	s2, _, _ := st.CreateSession(ctx, pA.OwnerID, p2.PluginID, t0)
	m, _ := st.ForOwner(pA.OwnerID).CreateMeeting(ctx, store.NewMeeting{}, t0)
	f.Revoke(pA.Token)
	reg.PollOnce(ctx)
	if _, err := st.LookupSession(ctx, s1, t0); err != store.ErrNotFound {
		t.Fatal("the revoked token's session survived")
	}
	if _, err := st.LookupSession(ctx, s2, t0); err != nil {
		t.Fatal("the other token's session was dropped")
	}
	if got, _, _ := st.ForOwner(pA.OwnerID).Meeting(ctx, m.Meeting.ID); got.State != "open" {
		t.Fatal("the owner's meeting ended while another token still serves them")
	}
}

func TestBootRefusesATokenWithoutAppOrATeamsToken(t *testing.T) {
	noApp := pA
	noApp.Capabilities = []string{"messages", "viewer"}
	team := pB
	team.OwnerKind = "team"
	for name, p := range map[string]mustertest.Plugin{"no app": noApp, "team": team} {
		_, st, cs, buf := setup(t, p)
		if _, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now); err == nil {
			t.Errorf("%s: Boot accepted it", name)
		} else if strings.Contains(err.Error(), p.Token) {
			t.Errorf("%s: the refusal names the token: %v", name, err)
		}
	}
}

// A token revoked while the plugin was down: Boot leaves it out, and deletes
// the sessions and ends the meetings it slept through.
func TestBootAppliesARevocationThePluginSleptThrough(t *testing.T) {
	f, st, cs, buf := setup(t, pA, pB)
	sa, _, _ := st.CreateSession(ctx, pA.OwnerID, pA.PluginID, t0)
	sb, _, _ := st.CreateSession(ctx, pB.OwnerID, pB.PluginID, t0)
	ma, _ := st.ForOwner(pA.OwnerID).CreateMeeting(ctx, store.NewMeeting{}, t0)
	f.Revoke(pA.Token)
	reg, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup(pA.PluginID); ok || reg.Live() != 1 {
		t.Fatalf("Boot serves the revoked token (live %d)", reg.Live())
	}
	if _, err := st.LookupSession(ctx, sa, t0); err != store.ErrNotFound {
		t.Fatal("a session of the revoked token survived the boot")
	}
	if _, err := st.LookupSession(ctx, sb, t0); err != nil {
		t.Fatal("a live token's session was dropped at boot")
	}
	if m, _, _ := st.ForOwner(pA.OwnerID).Meeting(ctx, ma.Meeting.ID); m.State != "ended" {
		t.Fatal("the revoked owner's meeting stayed open")
	}
	// Every token dead: nothing to serve.
	f.Revoke(pB.Token)
	if _, err := owners.Boot(ctx, cs, st, log.New(buf, "", 0), now); err == nil {
		t.Fatal("Boot with no live token succeeded")
	}
}

func TestBootStopsWhenMusterDoesNotAnswer(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c, _ := muster.New(url, pA.Token)
	_, st, _, buf := setup(t)
	if _, err := owners.Boot(ctx, []*muster.Client{c}, st, log.New(buf, "", 0), now); err == nil {
		t.Fatal("Boot without muster succeeded")
	}
}
