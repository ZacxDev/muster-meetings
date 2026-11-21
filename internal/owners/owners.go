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
	"sort"
	"strings"
	"sync"
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

	// The owners/check rotation: each pass resumes after the last subject
	// the previous one judged, and none is sent before retryAt (muster's
	// Retry-After), so past muster's 60 calls a minute no tail of the
	// subjects is starved.
	checkMu sync.Mutex
	resume  string
	retryAt time.Time
}

// sleep waits d, or until ctx ends: Boot waits out a Retry-After with it.
// Tests replace it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
	// Every held subject is judged once before the plugin serves anyone:
	// pass after pass, waiting out muster's Retry-After between them.
	held, err := st.HeldSubjects(ctx, now())
	if err != nil {
		return nil, err
	}
	for covered := 0; covered < len(held); {
		n, wait, err := a.checkOwners(ctx)
		if errors.Is(err, ErrRevoked) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("boot: POST /plugin/v1/owners/check: %w", err)
		}
		covered += n
		if covered < len(held) && wait > 0 {
			logger.Printf("owners: boot: %d of %d people checked; muster asks to wait %s", covered, len(held), wait)
			if err := sleep(ctx, wait); err != nil {
				return nil, fmt.Errorf("boot: waiting for owners/check: %w", err)
			}
		} else if n == 0 {
			break // nothing left held (each judged subject may have ended)
		}
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

// checkOwners is one owners/check pass. It sends the held subjects in
// batches of muster.MaxCheckSubjects, starting after the last subject the
// previous pass judged (wrapping around), until every one is sent or muster
// answers 429; then it records muster's Retry-After, and the next pass
// resumes where this one stopped. It returns how many subjects it judged or
// sent, and the wait muster asked for.
//
// Each answered state is acted on: live keeps it; expired ends the person's
// owner sessions only, so their open meetings keep running for the guests
// (they end by End meeting, the 24 h link lifetime or the sweep, and the
// owner reopens the tab to see them); revoked ends their sessions and open
// meetings. 🔴 Anything else, including a subject absent from an answered
// batch, is revoked: fail closed. Only what existed when the batch was sent
// is ended. A batch that fails is logged and its people kept. A 401 revokes
// the app and returns ErrRevoked.
func (a *App) checkOwners(ctx context.Context) (int, time.Duration, error) {
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	held, err := a.store.HeldSubjects(ctx, a.now())
	if err != nil || len(held) == 0 {
		return 0, 0, err
	}
	start := sort.SearchStrings(held, a.resume)
	if start < len(held) && held[start] == a.resume {
		start++
	}
	order := append(slices.Clone(held[start:]), held[:start]...)
	var revoked, expired, failures []string
	var wait time.Duration
	var first error
	sent, done := a.now(), 0
	for done < len(order) {
		batch := order[done:min(done+muster.MaxCheckSubjects, len(order))]
		states, err := a.Client.OwnersCheck(ctx, batch)
		e := muster.AsError(err)
		if e != nil && e.Kind == muster.KindAuth {
			a.checkMu.Unlock()
			a.Revoke(ctx, "owners/check answered 401")
			a.checkMu.Lock()
			return done, 0, ErrRevoked
		}
		if e != nil && e.Kind == muster.KindRateLimited {
			wait = e.RetryAfter
			a.retryAt = a.now().Add(wait)
			break
		}
		done += len(batch)
		if err != nil {
			failures = append(failures, err.Error())
			if first == nil {
				first = err
			}
			continue // its people are kept until muster answers
		}
		for _, s := range batch {
			switch states[s] {
			case muster.StateLive:
			case muster.StateExpired:
				expired = append(expired, s)
			default: // revoked, another state, or absent from the answer
				revoked = append(revoked, s)
			}
		}
	}
	if done > 0 {
		a.resume = order[done-1]
	}
	if len(expired) > 0 {
		n, err := a.store.EndSessions(ctx, expired, sent, a.now())
		if err != nil {
			return done, wait, err
		}
		a.log.Printf("owners: %d of %d people's grants expired: %d sessions deleted, their meetings kept", len(expired), len(held), n)
	}
	if len(revoked) > 0 {
		res, err := a.store.EndSubjects(ctx, revoked, sent, a.now())
		if err != nil {
			return done, wait, err
		}
		a.log.Printf("owners: muster revoked the app for %d of %d people: %d sessions deleted, %d meetings ended",
			len(revoked), len(held), res.Sessions, res.Meetings)
	}
	if wait > 0 {
		a.log.Printf("owners: owners/check: muster's limit reached after %d of %d people; resuming in %s", done, len(held), wait)
	}
	if first != nil {
		a.log.Printf("owners: POST /plugin/v1/owners/check: %s (those people are kept: only an answer ends a person)", strings.Join(failures, "; "))
		return done, wait, first
	}
	return done, wait, nil
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
	a.checkMu.Lock()
	wait := a.now().Before(a.retryAt)
	a.checkMu.Unlock()
	if wait {
		return // muster's Retry-After: not before then
	}
	_, _, _ = a.checkOwners(ctx) // it logs its own failures
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
