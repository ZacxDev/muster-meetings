// Package owners holds the plugin's muster tokens, one per owner it serves
// (design: "The owner: the hand-off": "The plugin holds one token per owner it
// serves (muster_plugin picks it)"), and learns from muster when one dies.
//
// muster never calls the plugin, so a revoked token is learned only by
// asking: every PollInterval the registry calls GET /plugin/v1/self with each
// live token, and a 401 deletes every owner session minted through it and
// ends the owner's open meetings (design: "Security and privacy", "Owner
// sessions").
package owners

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// PollInterval is how often each token's /self is asked (design: 60 s).
const PollInterval = 60 * time.Second

// Capability is the muster capability every token must hold.
const Capability = "app"

// Token is one live plugin token and whom it serves.
type Token struct {
	PluginID   int64
	OwnerID    int64
	OwnerLogin string
	Client     *muster.Client
}

// Registry is the plugin's tokens. It is safe for concurrent use.
type Registry struct {
	store *store.Store
	log   *log.Logger
	now   func() time.Time

	mu   sync.Mutex
	live map[int64]Token // plugin id → token; a revoked token is removed
}

// Boot asks /self of every client and returns the registry of live tokens.
//
// A token muster answers 401 (revoked while the plugin was down) is left
// out and logged. Any other refusal stops the boot: a token without `app`,
// a team's token (an app tab is a person's), two tokens for one plugin, a
// muster that does not answer. Then the database is reconciled with what is
// live: every owner session minted through a token that is not live is
// deleted, and every open meeting of an owner no live token serves is ended,
// so a revocation the plugin slept through is applied before it serves
// anyone.
func Boot(ctx context.Context, clients []*muster.Client, st *store.Store, logger *log.Logger, now func() time.Time) (*Registry, error) {
	r := &Registry{store: st, log: logger, now: now, live: map[int64]Token{}}
	for i, c := range clients {
		s, err := c.Self(ctx)
		if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
			logger.Printf("owners: token %d of %d: muster refused it (revoked, or its owner disabled); not serving it", i+1, len(clients))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("token %d of %d: GET /plugin/v1/self: %w", i+1, len(clients), err)
		}
		if !slices.Contains(s.Capabilities, Capability) {
			return nil, fmt.Errorf("token %d of %d (plugin %d) lacks the %q capability: mint it on muster's /plugins with App tab checked", i+1, len(clients), s.PluginID, Capability)
		}
		if s.OwnerKind != "person" {
			return nil, fmt.Errorf("token %d of %d (plugin %d) is a %s's: an app tab is a person's", i+1, len(clients), s.PluginID, s.OwnerKind)
		}
		if _, dup := r.live[s.PluginID]; dup {
			return nil, fmt.Errorf("token %d of %d: plugin %d is listed twice", i+1, len(clients), s.PluginID)
		}
		r.live[s.PluginID] = Token{PluginID: s.PluginID, OwnerID: s.OwnerID, OwnerLogin: s.OwnerLogin, Client: c}
		if err := st.UpsertOwner(ctx, s.OwnerID, s.OwnerLogin, s.PluginID, now()); err != nil {
			return nil, err
		}
		logger.Printf("owners: serving owner %d through plugin %d", s.OwnerID, s.PluginID)
	}
	if len(r.live) == 0 {
		return nil, fmt.Errorf("no live plugin token: muster refused every one in MUSTER_PLUGIN_TOKEN")
	}
	plugins, owners := []int64{}, []int64{}
	for _, t := range r.live {
		plugins = append(plugins, t.PluginID)
		owners = append(owners, t.OwnerID)
	}
	rec, err := st.ReconcileLive(ctx, plugins, owners, now())
	if err != nil {
		return nil, err
	}
	if rec.Sessions > 0 || rec.Meetings > 0 {
		logger.Printf("owners: boot: %d sessions of tokens no longer live deleted, %d meetings ended", rec.Sessions, rec.Meetings)
	}
	return r, nil
}

// Lookup returns the live token of pluginID.
func (r *Registry) Lookup(pluginID int64) (Token, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.live[pluginID]
	return t, ok
}

// PollOnce asks /self of every live token once. A 401 revokes it (Revoke);
// any other failure is logged and the token kept, since only muster's 401
// says it is dead.
func (r *Registry) PollOnce(ctx context.Context) {
	r.mu.Lock()
	toks := make([]Token, 0, len(r.live))
	for _, t := range r.live {
		toks = append(toks, t)
	}
	r.mu.Unlock()
	for _, t := range toks {
		_, err := t.Client.Self(ctx)
		if err == nil {
			continue
		}
		if e := muster.AsError(err); e != nil && e.Kind == muster.KindAuth {
			r.Revoke(ctx, t.PluginID)
			continue
		}
		r.log.Printf("owners: plugin %d: GET /plugin/v1/self: %v (keeping the token: only a 401 ends it)", t.PluginID, err)
	}
}

// Revoke stops serving pluginID's token: it leaves the registry, every owner
// session minted through it is deleted, and, unless another live token still
// serves the same owner, the owner's open meetings are ended.
func (r *Registry) Revoke(ctx context.Context, pluginID int64) {
	r.mu.Lock()
	t, ok := r.live[pluginID]
	delete(r.live, pluginID)
	other := false
	for _, o := range r.live {
		if o.OwnerID == t.OwnerID {
			other = true
		}
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	res, err := r.store.RevokePlugin(ctx, pluginID, t.OwnerID, !other, r.now())
	if err != nil {
		// The token is already out of the registry, so /enter refuses it; the
		// sessions are retried at the next boot's reconcile.
		r.log.Printf("owners: plugin %d revoked by muster, but its sessions could not be deleted: %v", pluginID, err)
		return
	}
	r.log.Printf("owners: plugin %d revoked by muster: %d sessions deleted, %d meetings ended", pluginID, res.Sessions, res.Meetings)
}

// Live is the number of live tokens.
func (r *Registry) Live() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// Run polls every interval until ctx ends.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.PollOnce(ctx)
		}
	}
}
