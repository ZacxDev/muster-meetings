// Command muster-meetings is the meetings plugin for muster: a Meetings tab
// (muster's `app` capability) where the owner makes a meeting and shares its
// link. This build is phase 2a of docs/design/meetings.md in muster: the
// hand-off and owner sessions, the /self poll, the database, the meetings
// list and New meeting with its link. Calls (LiveKit), the guest page and the
// lobby come next.
//
// Configuration is read from the environment only (see .env.example):
//
//	MUSTER_URL                where the plugin calls muster (https, or http to localhost)
//	MUSTER_PLUGIN_TOKEN       the mpl_… token(s) minted on muster's /plugins with App tab checked, comma-separated
//	MUSTER_ORIGIN             muster's origin as browsers see it: the only page that may frame the app
//	MEETINGS_PUBLIC_URL       this plugin's own origin, e.g. https://meet.example.test
//	MEETINGS_LISTEN           the public listener (default :8080)
//	MEETINGS_INTERNAL_LISTEN  the internal listener, /healthz (default :8081)
//	STATE_DIR                 where the SQLite database lives (default ./state)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("muster-meetings: %v", err)
		os.Exit(1)
	}
}

// Config is everything the plugin reads from its environment.
type Config struct {
	URL            string
	Tokens         []string
	MusterOrigin   string
	PublicURL      string
	Listen         string
	InternalListen string
	StateDir       string
}

// LoadConfig reads and checks the environment through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	base, err := muster.CheckBaseURL(getenv("MUSTER_URL"))
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: base, Listen: ":8080", InternalListen: ":8081", StateDir: "./state"}
	for _, t := range strings.Split(getenv("MUSTER_PLUGIN_TOKEN"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.Tokens = append(c.Tokens, t)
		}
	}
	if len(c.Tokens) == 0 {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN is not set (mint one on muster's /plugins page with App tab checked)")
	}
	for i, t := range c.Tokens {
		if !strings.HasPrefix(t, muster.TokenPrefix) || len(t) <= len(muster.TokenPrefix) {
			// Never echo it: a mistaken value may still be a secret.
			return Config{}, fmt.Errorf("MUSTER_PLUGIN_TOKEN: token %d of %d does not start with %q (its value is not shown)", i+1, len(c.Tokens), muster.TokenPrefix)
		}
	}
	if c.MusterOrigin, err = CheckOrigin("MUSTER_ORIGIN", getenv("MUSTER_ORIGIN")); err != nil {
		return Config{}, err
	}
	if c.PublicURL, err = CheckOrigin("MEETINGS_PUBLIC_URL", getenv("MEETINGS_PUBLIC_URL")); err != nil {
		return Config{}, err
	}
	// The plugin may never be served from muster's own origin: framed there
	// with allow-same-origin, its script would BE muster (design: "Why the
	// plugin may not be served from muster's exact origin").
	if c.PublicURL == c.MusterOrigin {
		return Config{}, errors.New("MEETINGS_PUBLIC_URL must be its own origin (e.g. https://meet.<muster host>), never MUSTER_ORIGIN")
	}
	if v := getenv("MEETINGS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("MEETINGS_INTERNAL_LISTEN"); v != "" {
		c.InternalListen = v
	}
	if v := getenv("STATE_DIR"); v != "" {
		c.StateDir = v
	}
	return c, nil
}

// CheckOrigin validates an origin variable: https, or http to a loopback
// host; no user, path, query or fragment. It returns scheme://host, with the
// host lower-cased.
func CheckOrigin(name, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not an absolute URL such as https://meet.example.test", name)
	}
	h := u.Hostname()
	loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("%s must be https:// (http:// only for localhost, 127.0.0.1 or [::1])", name)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s must be an origin alone: no user, path, query or fragment", name)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func run(logger *log.Logger) error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	clients := make([]*muster.Client, len(cfg.Tokens))
	for i, t := range cfg.Tokens {
		if clients[i], err = muster.New(cfg.URL, t); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "meetings.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	reg, err := owners.Boot(bootCtx, clients, st, logger, time.Now)
	cancel()
	if err != nil {
		return err
	}
	logger.Print(startupLine(cfg, reg.Live()))

	go reg.Run(ctx, owners.PollInterval)
	go sweep(ctx, st, logger)

	pub := &http.Server{Addr: cfg.Listen, Handler: server.New(server.Config{PublicURL: cfg.PublicURL, MusterOrigin: cfg.MusterOrigin, Log: logger}, st, reg),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	internal := &http.Server{Addr: cfg.InternalListen, Handler: server.Internal(st, reg), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- pub.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		stop()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = internal.Shutdown(shut)
	return pub.Shutdown(shut)
}

// sweep ends meetings past the 24 h cap and drops expired sessions, every
// minute.
func sweep(ctx context.Context, st *store.Store, logger *log.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := st.Sweep(ctx, time.Now()); err != nil {
				logger.Printf("sweep: %v", err)
			} else if n > 0 {
				logger.Printf("sweep: %d meetings reached the 24 h cap and ended", n)
			}
		}
	}
}

// startupLine is the first line the plugin logs. It names muster by scheme and
// host only (muster.Redact): whatever else the address held could be the token.
func startupLine(cfg Config, live int) string {
	return fmt.Sprintf("muster-meetings: talking to %s for %d owner token(s); serving %s on %s (internal %s), framed by %s",
		muster.Redact(cfg.URL), live, cfg.PublicURL, cfg.Listen, cfg.InternalListen, cfg.MusterOrigin)
}
