// Command muster-meetings is the meetings plugin for muster: a Meetings tab
// (muster's `app` capability) where the owner makes a meeting and shares its
// link. This build is phase 2a of docs/design/meetings.md in muster: the
// hand-off and owner sessions, the /self poll, the database, the meetings
// list and New meeting with its link, as one server-installed app (core PR
// 1h): an admin installs it once, and every person on the muster gets the
// tab. Phase 2b adds the call: LiveKit rooms and tokens, the guest page and
// lobby (admit, deny, remove), End meeting, LiveKit's webhook and a reconcile
// loop, screen share, the consent notice, and the knock notification.
//
// Configuration is read from the environment only (see .env.example):
//
//	MUSTER_URL                where the plugin calls muster (https; http only to loopback, <svc>.<ns>.svc.<cluster domain>. or MEETINGS_MUSTER_HTTP_HOST)
//	MEETINGS_CLUSTER_DOMAIN   the cluster's DNS domain for that http rule (default cluster.local)
//	MEETINGS_MUSTER_HTTP_HOST one host, matched exactly, to which MUSTER_URL may be plain http (e.g. muster in Compose)
//	MUSTER_PLUGIN_TOKEN       the ONE mpl_… token an admin's install on muster's /admin/apps showed
//	MUSTER_ORIGIN             muster's origin as browsers see it: the only page that may frame the app
//	MEETINGS_PUBLIC_URL       this plugin's own origin, e.g. https://meet.example.test
//	MEETINGS_LINK_KEY         32 random bytes, base64: every meeting's link code is derived from it
//	MEETINGS_LISTEN           the public listener (default :8080)
//	MEETINGS_INTERNAL_LISTEN  the internal listener, /healthz (default :8081)
//	STATE_DIR                 where the SQLite database lives (default ./state)
//	MEETINGS_LIVEKIT_URL      where browsers connect for the call, e.g. wss://meet-rtc.example.test
//	MEETINGS_LIVEKIT_API_URL  LiveKit's server API as the plugin reaches it, e.g. http://livekit:7880
//	LIVEKIT_API_KEY           the LiveKit API key
//	LIVEKIT_API_SECRET        the LiveKit API secret: signs tokens, verifies webhooks; never sent to a browser
//	MEETINGS_LIVEKIT_ICE      relay (default: TURN over TLS is the only media path) or all (a local LiveKit)
//	MEETINGS_TRUSTED_PROXIES  comma-separated CIDRs of the ingress in front of the plugin (for the per-address knock limit)
//	MEETINGS_CLIENT_IP_HEADER one header holding the client's address (e.g. CF-Connecting-IP), believed only from a trusted proxy
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/livekit"
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
	Token          string
	MusterOrigin   string
	PublicURL      string
	LinkKey        []byte
	Listen         string
	InternalListen string
	StateDir       string
	LiveKitURL     string
	LiveKitAPI     string
	LiveKitKeys    livekit.Keys
	ICE            string
	TrustedProxies []netip.Prefix
	ClientIPHeader string
}

// LoadConfig reads and checks the environment through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	allow, err := httpAllow(getenv("MEETINGS_CLUSTER_DOMAIN"), getenv("MEETINGS_MUSTER_HTTP_HOST"))
	if err != nil {
		return Config{}, err
	}
	base, err := muster.CheckBaseURL(getenv("MUSTER_URL"), allow)
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: base, Listen: ":8080", InternalListen: ":8081", StateDir: "./state"}
	// Exactly one server token (fork 8). A comma is 2a's list of per-person
	// tokens: refused, never split, so no list is ever half-served.
	c.Token = strings.TrimSpace(getenv("MUSTER_PLUGIN_TOKEN"))
	if c.Token == "" {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN is not set: an admin installs Meetings on muster's /admin/apps, which shows the token once")
	}
	if strings.Contains(c.Token, ",") {
		// Never echo it: a mistaken value may still be a secret.
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN holds a comma: it takes exactly one server-installed app's token, not a list (its value is not shown)")
	}
	if !strings.HasPrefix(c.Token, muster.TokenPrefix) || len(c.Token) <= len(muster.TokenPrefix) {
		return Config{}, fmt.Errorf("MUSTER_PLUGIN_TOKEN does not start with %q (its value is not shown)", muster.TokenPrefix)
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
	if c.LinkKey, err = store.ParseLinkKey(getenv("MEETINGS_LINK_KEY")); err != nil {
		return Config{}, err
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
	if c.LiveKitURL, err = CheckURL("MEETINGS_LIVEKIT_URL", getenv("MEETINGS_LIVEKIT_URL"), "wss", "ws"); err != nil {
		return Config{}, err
	}
	// LiveKit's server API is reached inside the cluster, where it listens on
	// plain http (as the design's webhook URL to the plugin does): http is
	// allowed to any host. Its bearer is a one-minute token the plugin signs
	// per call, never the secret.
	if c.LiveKitAPI, err = checkURL("MEETINGS_LIVEKIT_API_URL", getenv("MEETINGS_LIVEKIT_API_URL"), "https", "http", true); err != nil {
		return Config{}, err
	}
	c.LiveKitKeys = livekit.Keys{Key: strings.TrimSpace(getenv("LIVEKIT_API_KEY")), Secret: strings.TrimSpace(getenv("LIVEKIT_API_SECRET"))}
	if !c.LiveKitKeys.Valid() {
		return Config{}, livekit.ErrKeys
	}
	switch c.ICE = strings.TrimSpace(getenv("MEETINGS_LIVEKIT_ICE")); c.ICE {
	case "":
		c.ICE = "relay"
	case "relay", "all":
	default:
		return Config{}, errors.New("MEETINGS_LIVEKIT_ICE is relay (the default: media only through TURN over TLS) or all")
	}
	for _, v := range strings.Split(getenv("MEETINGS_TRUSTED_PROXIES"), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEETINGS_TRUSTED_PROXIES: %q is not a CIDR such as 10.0.0.0/8", v)
		}
		// Trusting every address would let any client name itself in
		// X-Forwarded-For and walk past the knock and stream limits.
		if p.Bits() == 0 {
			return Config{}, fmt.Errorf("MEETINGS_TRUSTED_PROXIES: %q trusts every address; list only the ingress's own CIDRs", v)
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}
	if c.ClientIPHeader, err = checkClientIPHeader(getenv("MEETINGS_CLIENT_IP_HEADER"), len(c.TrustedProxies) > 0); err != nil {
		return Config{}, err
	}
	return c, nil
}

// httpAllow reads where plain http to muster is allowed besides loopback:
// a fully-qualified Service name in the cluster domain (default
// cluster.local), and the one host MEETINGS_MUSTER_HTTP_HOST names, matched
// exactly. A wildcard or anything but DNS labels is refused, never widened.
func httpAllow(domain, host string) (muster.HTTPAllow, error) {
	a := muster.HTTPAllow{ClusterDomain: strings.TrimSuffix(strings.TrimSpace(domain), "."), Host: strings.TrimSpace(host)}
	if a.ClusterDomain == "" {
		a.ClusterDomain = "cluster.local"
	}
	if !muster.DNSName(a.ClusterDomain) {
		return muster.HTTPAllow{}, errors.New("MEETINGS_CLUSTER_DOMAIN is not a DNS name such as cluster.local")
	}
	if a.Host != "" && !muster.DNSName(a.Host) {
		return muster.HTTPAllow{}, errors.New("MEETINGS_MUSTER_HTTP_HOST names ONE host, exactly (e.g. muster): no wildcard, port, scheme or trailing dot")
	}
	return a, nil
}

// headerName is an HTTP header name a proxy might use for the client's address.
var headerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// checkClientIPHeader validates MEETINGS_CLIENT_IP_HEADER: unset is today's
// X-Forwarded-For walk. Set, it is the one header (e.g. CF-Connecting-IP)
// believed for the client's address, and only from a trusted proxy, so it
// is refused without MEETINGS_TRUSTED_PROXIES: there would be no peer it
// could ever be believed from, and an operator who set it expects it to bind.
func checkClientIPHeader(raw string, haveTrusted bool) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if !headerName.MatchString(v) {
		return "", errors.New("MEETINGS_CLIENT_IP_HEADER is not a header name such as CF-Connecting-IP")
	}
	v = http.CanonicalHeaderKey(v)
	if v == "X-Forwarded-For" {
		return "", errors.New("MEETINGS_CLIENT_IP_HEADER: leave it unset to use X-Forwarded-For (it names a header holding ONE address, such as CF-Connecting-IP)")
	}
	if !haveTrusted {
		return "", errors.New("MEETINGS_CLIENT_IP_HEADER is set but MEETINGS_TRUSTED_PROXIES is not: the header is believed only from a trusted proxy, so set the proxy's CIDRs too")
	}
	return v, nil
}

// CheckURL validates a URL variable whose scheme is secure, or insecure only
// to a loopback host: no user, query or fragment, no path. It returns
// scheme://host.
func CheckURL(name, raw, secure, insecure string) (string, error) {
	return checkURL(name, raw, secure, insecure, false)
}

// checkURL is CheckURL; insecureAnyHost allows the insecure scheme to any
// host, not only a loopback one.
func checkURL(name, raw, secure, insecure string, insecureAnyHost bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not set to an absolute %s:// URL", name, secure)
	}
	h := u.Hostname()
	loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != secure && !(u.Scheme == insecure && (loopback || insecureAnyHost)) {
		return "", fmt.Errorf("%s must be %s:// (%s:// only for localhost, 127.0.0.1 or [::1])", name, secure, insecure)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%s must be a scheme and host alone: no user, path, query or fragment", name)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// CheckOrigin validates an origin variable: https, or http to a loopback
// host; no user, path, query or fragment. It returns scheme://host, with the
// host lower-cased.
func CheckOrigin(name, raw string) (string, error) { return CheckURL(name, raw, "https", "http") }

func run(logger *log.Logger) error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	client, err := muster.New(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "meetings.db"), cfg.LinkKey)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Boot judges every held person before serving, waiting out muster's
	// owners/check limit (60 calls of 200 a minute) when there are many.
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	app, err := owners.Boot(bootCtx, client, st, logger, time.Now)
	cancel()
	if err != nil {
		return err
	}
	logger.Print(startupLine(cfg, app.Name, app.PluginID, app.FilesTasks))

	lk, err := livekit.NewClient(cfg.LiveKitAPI, cfg.LiveKitKeys)
	if err != nil {
		return err
	}
	srv := server.New(cfg.serverConfig(logger, lk), st, app)

	go app.Run(ctx, owners.PollInterval)
	go sweep(ctx, st, logger)
	go srv.Run(ctx)

	pub := &http.Server{Addr: cfg.Listen, Handler: srv,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	internal := &http.Server{Addr: cfg.InternalListen, Handler: srv.Internal(), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
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

// serverConfig is the server's part of cfg.
func (cfg Config) serverConfig(logger *log.Logger, lk *livekit.Client) server.Config {
	return server.Config{PublicURL: cfg.PublicURL, MusterOrigin: cfg.MusterOrigin, Log: logger,
		LiveKit: lk, LiveKitURL: cfg.LiveKitURL, ICE: cfg.ICE, TrustedProxies: cfg.TrustedProxies,
		ClientIPHeader: cfg.ClientIPHeader}
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
func startupLine(cfg Config, name string, pluginID int64, tasks bool) string {
	files := "without"
	if tasks {
		files = "with"
	}
	return fmt.Sprintf("muster-meetings: talking to %s as server app %q (plugin %d, %s Files tasks); serving %s on %s (internal %s), framed by %s; LiveKit %s (API %s, ICE %s)",
		muster.Redact(cfg.URL), name, pluginID, files, cfg.PublicURL, cfg.Listen, cfg.InternalListen, cfg.MusterOrigin,
		muster.Redact(cfg.LiveKitURL), muster.Redact(cfg.LiveKitAPI), cfg.ICE)
}
