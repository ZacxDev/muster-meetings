// Package owners holds the plugin's one muster token, a server-installed
// app's, and learns from muster when it may no longer act: for anyone (the
// app uninstalled, or its token rotated away) or for one person (they hid the
// app, were disabled, signed out everywhere, or their 8-hour grant ran out).
//
// muster never calls the plugin, so all of it is learned by asking (design:
// "Revocation, uninstall and the alive poll"). Every PollInterval:
//
//   - GET /plugin/v1/self. A 401 ends everything: every owner session is
//     deleted and every open meeting ended, and the app refuses every owner
//     request and every /enter from then on.
//   - POST /plugin/v1/owners/check for every subject with a live owner
//     session or an open meeting. "expired" ends that person's sessions and
//     keeps their meetings; 🔴 "revoked", any other state, or no answer for
//     the subject ends their sessions and meetings. Only an answer ends
//     anything: a 5xx or no answer at all keeps what is held.
//
// Boot does the same once, before the plugin serves anyone, so a revocation
// it slept through is applied first.
package owners

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// PollInterval is how often /self and owners/check are asked (design: 60 s).
const PollInterval = 60 * time.Second

// Capability is the muster capability the token must hold.
const Capability = "app"

// TasksCapability decides whether Make task is drawn (phase 6).
const TasksCapability = "tasks"

// App is the plugin's one server token and what muster said of it. It is
// safe for concurrent use.
type App struct {
	// PluginID is the app's id on muster: muster's frame names it in
	// muster_plugin, and /enter refuses any other.
	PluginID int64
	// Name is the app's name on muster.
	Name string
	// FilesTasks says the admin installed it with Files tasks.
	FilesTasks bool
	// Client calls muster with the token.
	Client *muster.Client

	store *store.Store
	log   *log.Logger
	now   func() time.Time
	dead  atomic.Bool
}

// ErrRevoked is muster's 401 for the token: the app is uninstalled, or its
// token was rotated away.
var ErrRevoked = errors.New("muster refused MUSTER_PLUGIN_TOKEN (401): the app was uninstalled, or its token rotated; " +
	"every owner session was deleted and every open meeting ended")

// Boot asks /self, checks the token is a server app's with `app`, and
// reconciles the database with muster before the plugin serves anyone.
//
// A 401 ends everything the database holds and returns ErrRevoked. A token
// that is not a server app's (a person's own token, or a team's), or lacks
// `app`, is refused. Then every subject the database holds a session or an
// open meeting for is sent to owners/check, and each one muster does not
// answer live is ended.
func Boot(ctx context.Context, c *muster.Client, st *store.Store, logger *log.Logger, now func() time.Time) (*App, error) {
	a := &App{Client: c, store: st, log: logger, now: now}
	s, err := c.Self(ctx)
	if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
		if _, err := st.EndAll(ctx, "boot: /self answered 401", now()); err != nil {
			return nil, fmt.Errorf("%w (and the database could not be cleared: %v)", ErrRevoked, err)
		}
		return nil, ErrRevoked
	}
	if err != nil {
		return nil, fmt.Errorf("GET /plugin/v1/self: %w", err)
	}
	if s.Scope != muster.ScopeServer || s.OwnerKind != muster.ScopeServer {
		return nil, fmt.Errorf("MUSTER_PLUGIN_TOKEN is a %q token (plugin %d), not a server-installed app's: an admin installs Meetings on muster's /admin/apps", s.OwnerKind, s.PluginID)
	}
	if !slices.Contains(s.Capabilities, Capability) {
		return nil, fmt.Errorf("the server app (plugin %d) lacks the %q capability", s.PluginID, Capability)
	}
	a.PluginID, a.Name, a.FilesTasks = s.PluginID, s.PluginName, slices.Contains(s.Capabilities, TasksCapability)
	if err := a.checkOwners(ctx); err != nil {
		if errors.Is(err, ErrRevoked) {
			return nil, err
		}
		return nil, fmt.Errorf("boot: POST /plugin/v1/owners/check: %w", err)
	}
	return a, nil
}

// Live says muster has not refused the token. Once false it stays false:
// a rotated-away or uninstalled token never comes back.
func (a *App) Live() bool { return !a.dead.Load() }

// Revoke ends everything: the app refuses every request from now on, every
// owner session is deleted and every open meeting ended.
func (a *App) Revoke(ctx context.Context, why string) {
	a.dead.Store(true)
	res, err := a.store.EndAll(ctx, why, a.now())
	if err != nil {
		// The app already refuses every owner request; the rows go at the
		// next boot, which fails on the same 401.
		a.log.Printf("owners: %s, but the sessions could not be deleted: %v", why, err)
		return
	}
	a.log.Printf("owners: %s: %d sessions deleted, %d meetings ended", why, res.Sessions, res.Meetings)
}

// checkOwners asks owners/check about every held subject and acts on each
// state: live keeps it; expired ends the person's owner sessions only, so
// their open meetings keep running for the guests (they end by End meeting,
// the 24 h link lifetime or the sweep, and the owner reopens the tab to see
// them); revoked ends their sessions and open meetings. 🔴 Anything else,
// including a subject absent from the answer, is revoked: fail closed. A
// 401 revokes the app and returns ErrRevoked; any other failure returns it
// and ends nothing.
func (a *App) checkOwners(ctx context.Context) error {
	held, err := a.store.HeldSubjects(ctx, a.now())
	if err != nil || len(held) == 0 {
		return err
	}
	// Only what existed when the check was sent is judged by its answer.
	sent := a.now()
	states, checkErr := a.Client.OwnersCheck(ctx, held)
	if e := muster.AsError(checkErr); e != nil && e.Kind == muster.KindAuth {
		a.Revoke(ctx, "owners/check answered 401")
		return ErrRevoked
	}
	var revoked, expired []string
	for _, s := range held {
		st, answered := states[s]
		if !answered {
			continue // its batch failed: kept until muster answers
		}
		switch st {
		case muster.StateLive:
		case muster.StateExpired:
			expired = append(expired, s)
		default: // revoked, another state, or absent from an answered batch
			revoked = append(revoked, s)
		}
	}
	if len(expired) > 0 {
		n, err := a.store.EndSessions(ctx, expired, sent, a.now())
		if err != nil {
			return err
		}
		a.log.Printf("owners: %d of %d people's grants expired: %d sessions deleted, their meetings kept", len(expired), len(held), n)
	}
	if len(revoked) > 0 {
		res, err := a.store.EndSubjects(ctx, revoked, sent, a.now())
		if err != nil {
			return err
		}
		a.log.Printf("owners: muster revoked the app for %d of %d people: %d sessions deleted, %d meetings ended",
			len(revoked), len(held), res.Sessions, res.Meetings)
	}
	if checkErr != nil {
		a.log.Printf("owners: POST /plugin/v1/owners/check: %v (those people are kept: only an answer ends a person)", checkErr)
		return checkErr
	}
	return nil
}

// PollOnce asks /self, then owners/check. A 401 from either ends
// everything; any other failure is logged and changes nothing, since only
// muster's answer says what is dead.
func (a *App) PollOnce(ctx context.Context) {
	if !a.Live() {
		return
	}
	_, err := a.Client.Self(ctx)
	if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
		a.Revoke(ctx, "/self answered 401: the app was uninstalled or its token rotated")
		return
	}
	if err != nil {
		a.log.Printf("owners: GET /plugin/v1/self: %v (keeping everything: only a 401 ends it)", err)
		return
	}
	_ = a.checkOwners(ctx) // it logs its own failures
}

// Run polls every interval until ctx ends.
func (a *App) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.PollOnce(ctx)
		}
	}
}
