// Command muster-meetings is the meetings plugin for muster: a Meetings tab
// (muster's `app` capability) where the owner makes a meeting and shares its
// link. This build is phase 2a of docs/design/meetings.md in muster: the
// hand-off and owner sessions, the /self poll, the database, the meetings
// list and New meeting with its link, as one server-installed app (core PR
// 1h): an admin installs it once, and every person on the muster gets the
// tab. Phase 2b adds the call: LiveKit rooms and tokens, the guest page and
// lobby (admit, deny, remove), End meeting, LiveKit's webhook and a reconcile
// loop, screen share, the consent notice, and the knock notification. Phase
// 3a adds the live transcript, for the owner and admitted guests, and its
// archive.
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
//
// Phase 3a adds the transcript: a track Egress per microphone streams raw PCM
// to the ingest listener, and an engine turns it into lines.
//
//	MEETINGS_STT_ENGINE       off (the default: no transcript), utterance, nemotron, or fake (tests only)
//	MEETINGS_STT_URL          utterance, and nemotron's fallback: the STT service muster's dictation uses (stt-cohere), http(s)://host[:port]
//	MEETINGS_STT_TOKEN        utterance, and nemotron's fallback: its bearer (a copy of stt-cohere's one token)
//	MEETINGS_STT_MODEL        utterance: the model form field (optional; stt-cohere ignores it)
//	MEETINGS_STREAM_STT_URL   nemotron: the Nemotron server's stream endpoint, wss://host[:port]/v1/stream (ws:// inside the cluster)
//	MEETINGS_STREAM_STT_TOKEN nemotron: its bearer (a copy of stt-nemotron's STT_API_TOKEN)
//	                          nemotron needs ALL FOUR of these, or the plugin refuses to boot
//	MEETINGS_TEST_MODE        1 allows the fake engine; MEETINGS_FAKE_STT_SCRIPT its lines, separated by |
//	MEETINGS_INGEST_SECRET    32+ random bytes, base64: the HMAC key of every ingest URL's token
//	MEETINGS_INGEST_URL       the ingest listener as Egress reaches it, e.g. ws://muster-meetings:8082
//	MEETINGS_INGEST_LISTEN    the ingest listener (default :8082): internal only, Egress's alone
//
// Contacts, phase 1b (muster's docs/design/contacts.md), OPTIONAL: both or
// neither, and with neither the plugin behaves exactly as without them.
//
//	MEETINGS_CONTACTS_URL     the contacts plugin's internal API, http(s)://host[:port]
//	MEETINGS_CONTACTS_TOKEN   its bearer: a COPY of the contacts plugin's CONTACTS_API_TOKEN (32+ characters)
//
// The recording, phase 5, OPTIONAL: all four of endpoint, bucket and the
// key pair, or none; with none the plugin records nothing and behaves
// exactly as without them. Egress uploads with the same bucket and key in
// its own configuration (deploy/kubernetes/egress.yaml).
//
//	MEETINGS_S3_ENDPOINT      the store's API, http(s)://host[:port] (e.g. the homelab MinIO over the mesh)
//	MEETINGS_S3_BUCKET        the recordings bucket
//	MEETINGS_S3_REGION        its signing region (default us-east-1, MinIO's)
//	MEETINGS_S3_ACCESS_KEY    a key scoped to that bucket alone
//	MEETINGS_S3_SECRET_KEY    its secret
//	MEETINGS_FFMPEG           the ffmpeg frame sampling runs (default: ffmpeg on PATH; the image carries one)
//
// The post-call notes, OPTIONAL (README: "Post-call notes"): unset, the
// plugin makes no AI call and sends no email, exactly as without them.
//
//	OPENROUTER_API_KEY              OpenRouter's key (required unless MEETINGS_ITEMS_MODEL is off or unset)
//	MEETINGS_ITEMS_MODEL            the action items' (and summary's) model, an OpenRouter slug; unset or off: no analysis
//	MEETINGS_ITEMS_ADAPTER          extract or chat_json (default: derived from the slug)
//	MEETINGS_ITEMS_PROVIDER_PREFS   OpenRouter's provider object (data_collection "deny" always; a chat_json model needs a pin)
//	MEETINGS_ITEMS_EVERY            the live window's period (default 10s)
//	MEETINGS_VISION_MODEL           the frame analysis' model (default anthropic/claude-haiku-5.5; off: none)
//	MEETINGS_VISION_PROVIDER_PREFS  its provider object (default: Anthropic only, no fallbacks)
//	MEETINGS_SMTP_ADDR              the mail server, host:port (STARTTLS required); unset: no email
//	MEETINGS_SMTP_USER, _PASS       its login (both or neither)
//	MEETINGS_SMTP_SERVER_NAME       the name its certificate carries (default: the host)
//	MEETINGS_MAIL_FROM              the sender address, e.g. meetings@mail.example.test (required with MEETINGS_SMTP_ADDR)
//
// `muster-meetings reprocess -from YYYY-MM-DD -to YYYY-MM-DD [-items-model …]
// [-items-prefs …] [-vision-model …|off] [-vision-prefs …]` queues a post-call
// pass for every meeting that ended in the range (the running plugin's worker
// runs them; see README "Reprocessing").
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"github.com/ZacxDev/muster-meetings/internal/contacts"
	"github.com/ZacxDev/muster-meetings/internal/frames"
	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/items"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/mail"
	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/s3"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if len(os.Args) > 1 && os.Args[1] == "reprocess" {
		if err := reprocess(os.Args[2:], os.Getenv, os.Stdout); err != nil {
			logger.Printf("muster-meetings reprocess: %v", err)
			os.Exit(1)
		}
		return
	}
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

	// The transcript (phase 3a). Engine is "" when it is off.
	Engine   string
	STTURL   string
	STTToken string
	STTModel string
	// StreamURL and StreamToken are the Nemotron server's (nemotron only).
	StreamURL    string
	StreamToken  string
	FakeScript   []string
	IngestKey    []byte
	IngestURL    string
	IngestListen string

	// Contacts is the contacts plugin's API client, or nil when
	// MEETINGS_CONTACTS_URL and _TOKEN are both unset.
	Contacts *contacts.Client

	// S3 is the recordings bucket's client, or nil when every MEETINGS_S3_*
	// variable is unset (no recording). FFmpeg is MEETINGS_FFMPEG.
	S3     *s3.Client
	FFmpeg string

	// AI is the analysis (nil: off); Mail the notes' mail server (nil: off).
	AI   *server.AIConfig
	Mail *mail.Sender
}

// The frame analysis' default model and provider pin (the operator's
// decision: DeepSeek reads no images).
const (
	DefaultVisionModel = "anthropic/claude-haiku-5.5"
	DefaultVisionPrefs = `{"data_collection":"deny","order":["anthropic"],"allow_fallbacks":false}`
)

// loadAI reads the analysis: MEETINGS_ITEMS_MODEL unset or off is none,
// whatever else is set; a model needs OPENROUTER_API_KEY. 🔴 No value is
// echoed: the key is a secret.
func loadAI(getenv func(string) string) (*server.AIConfig, error) {
	model := strings.TrimSpace(getenv("MEETINGS_ITEMS_MODEL"))
	if model == "" || model == items.Off {
		return nil, nil
	}
	key := strings.TrimSpace(getenv("OPENROUTER_API_KEY"))
	if key == "" {
		return nil, errors.New("MEETINGS_ITEMS_MODEL is set but OPENROUTER_API_KEY is not (set MEETINGS_ITEMS_MODEL=off for no action items)")
	}
	ad := strings.TrimSpace(getenv("MEETINGS_ITEMS_ADAPTER"))
	switch ad {
	case "":
		ad = items.AdapterFor(model)
	case items.AdapterExtract, items.AdapterChatJSON:
	default:
		return nil, errors.New("MEETINGS_ITEMS_ADAPTER is extract or chat_json")
	}
	prefs, err := items.ParsePrefs(getenv("MEETINGS_ITEMS_PROVIDER_PREFS"), ad)
	if err != nil {
		return nil, err
	}
	c := &server.AIConfig{Items: items.Config{Model: model, Adapter: ad, Prefs: prefs, APIKey: key}, Every: items.DefaultEvery}
	if _, err := items.New(c.Items, nil); err != nil {
		return nil, err
	}
	if v := strings.TrimSpace(getenv("MEETINGS_ITEMS_EVERY")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > 5*time.Minute {
			return nil, errors.New("MEETINGS_ITEMS_EVERY is a duration from 1s to 5m, e.g. 10s")
		}
		c.Every = d
	}
	vm := strings.TrimSpace(getenv("MEETINGS_VISION_MODEL"))
	vp := getenv("MEETINGS_VISION_PROVIDER_PREFS")
	switch {
	case vm == items.Off:
		return c, nil
	case vm == "":
		vm = DefaultVisionModel
		if strings.TrimSpace(vp) == "" {
			vp = DefaultVisionPrefs
		}
	}
	vprefs, err := items.ParsePrefs(vp, items.AdapterChatJSON)
	if err != nil {
		return nil, errors.New(strings.Replace(err.Error(), "MEETINGS_ITEMS_PROVIDER_PREFS", "MEETINGS_VISION_PROVIDER_PREFS", 1))
	}
	c.Vision = items.Config{Model: vm, Adapter: items.AdapterChatJSON, Prefs: vprefs, APIKey: key}
	if _, err := items.NewVision(c.Vision, nil); err != nil {
		return nil, err
	}
	return c, nil
}

// loadMail reads the notes' mail server: MEETINGS_SMTP_ADDR unset is none.
func loadMail(getenv func(string) string, haveAI bool) (*mail.Sender, error) {
	addr := strings.TrimSpace(getenv("MEETINGS_SMTP_ADDR"))
	if addr == "" {
		return nil, nil
	}
	if !haveAI {
		return nil, errors.New("MEETINGS_SMTP_ADDR is set but MEETINGS_ITEMS_MODEL is not: the notes need the action items")
	}
	return mail.New(mail.Config{Addr: addr, User: strings.TrimSpace(getenv("MEETINGS_SMTP_USER")), Pass: getenv("MEETINGS_SMTP_PASS"),
		From: strings.TrimSpace(getenv("MEETINGS_MAIL_FROM")), ServerName: strings.TrimSpace(getenv("MEETINGS_SMTP_SERVER_NAME"))})
}

// ErrS3Half is a recording configuration with some of its variables and not
// the rest.
var ErrS3Half = errors.New("the recording needs MEETINGS_S3_ENDPOINT, MEETINGS_S3_BUCKET, MEETINGS_S3_ACCESS_KEY and MEETINGS_S3_SECRET_KEY together " +
	"(MEETINGS_S3_REGION is optional), or none of them for no recording")

// loadS3 reads the recording's bucket: the endpoint, bucket and key pair all
// unset is no recording (whatever the region says); some but not all is a
// refusal; all is a client. 🔴 No value is
// ever echoed: the keys are secrets.
func loadS3(getenv func(string) string) (*s3.Client, error) {
	cfg := s3.Config{Endpoint: strings.TrimSpace(getenv("MEETINGS_S3_ENDPOINT")), Bucket: strings.TrimSpace(getenv("MEETINGS_S3_BUCKET")),
		Region: strings.TrimSpace(getenv("MEETINGS_S3_REGION")), AccessKey: strings.TrimSpace(getenv("MEETINGS_S3_ACCESS_KEY")),
		SecretKey: strings.TrimSpace(getenv("MEETINGS_S3_SECRET_KEY"))}
	set := 0
	for _, v := range []string{cfg.Endpoint, cfg.Bucket, cfg.AccessKey, cfg.SecretKey} {
		if v != "" {
			set++
		}
	}
	switch {
	case set == 0:
		// A region alone (the Kubernetes base ships us-east-1) is no bucket.
		return nil, nil
	case set < 4:
		return nil, ErrS3Half
	}
	c, err := s3.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("MEETINGS_S3_*: %w", err)
	}
	return c, nil
}

// The engines MEETINGS_STT_ENGINE names.
const (
	EngineOff       = "off"
	EngineUtterance = "utterance"
	EngineFake      = "fake"
	EngineNemotron  = "nemotron"
)

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
	if err := c.loadTranscript(getenv); err != nil {
		return Config{}, err
	}
	if c.Contacts, err = contacts.Configure(getenv("MEETINGS_CONTACTS_URL"), getenv("MEETINGS_CONTACTS_TOKEN"), c.Token); err != nil {
		return Config{}, err
	}
	if c.S3, err = loadS3(getenv); err != nil {
		return Config{}, err
	}
	c.FFmpeg = strings.TrimSpace(getenv("MEETINGS_FFMPEG"))
	if c.AI, err = loadAI(getenv); err != nil {
		return Config{}, err
	}
	if c.Mail, err = loadMail(getenv, c.AI != nil); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadTranscript reads the transcript's settings (design: "Three engines,
// one interface"). Unset or "off" is no transcript, and needs nothing else.
// Any engine needs the ingest's secret and URL; utterance needs the STT
// service; nemotron needs the Nemotron server AND the STT service (its
// fallback): all four variables, or it refuses to boot; fake refuses to
// start unless MEETINGS_TEST_MODE=1.
func (c *Config) loadTranscript(getenv func(string) string) error {
	c.IngestListen = ":8082"
	if v := strings.TrimSpace(getenv("MEETINGS_INGEST_LISTEN")); v != "" {
		c.IngestListen = v
	}
	switch e := strings.TrimSpace(getenv("MEETINGS_STT_ENGINE")); e {
	case "", EngineOff:
		return nil
	case EngineUtterance:
		c.Engine = e
		c.STTURL, c.STTToken, c.STTModel = strings.TrimSpace(getenv("MEETINGS_STT_URL")), strings.TrimSpace(getenv("MEETINGS_STT_TOKEN")), strings.TrimSpace(getenv("MEETINGS_STT_MODEL"))
		if _, err := stt.NewClient(c.STTURL, c.STTToken, c.STTModel, 0); err != nil {
			return fmt.Errorf("MEETINGS_STT_ENGINE=utterance: %w", err)
		}
	case EngineFake:
		if strings.TrimSpace(getenv("MEETINGS_TEST_MODE")) != "1" {
			return errors.New("MEETINGS_STT_ENGINE=fake is for tests only: it refuses to start unless MEETINGS_TEST_MODE=1")
		}
		c.Engine = e
		if v := getenv("MEETINGS_FAKE_STT_SCRIPT"); v != "" {
			c.FakeScript = strings.Split(v, "|")
		}
	case EngineNemotron:
		c.Engine = e
		var err error
		if c.StreamURL, err = stt.CheckStreamURL(getenv("MEETINGS_STREAM_STT_URL")); err != nil {
			return fmt.Errorf("MEETINGS_STT_ENGINE=nemotron: %w", err)
		}
		if c.StreamToken, err = stt.CheckToken("MEETINGS_STREAM_STT_TOKEN", getenv("MEETINGS_STREAM_STT_TOKEN")); err != nil {
			return fmt.Errorf("MEETINGS_STT_ENGINE=nemotron: %w", err)
		}
		// Its per-participant fallback is the utterance engine: the STT
		// service is required too, never optional.
		c.STTURL, c.STTToken, c.STTModel = strings.TrimSpace(getenv("MEETINGS_STT_URL")), strings.TrimSpace(getenv("MEETINGS_STT_TOKEN")), strings.TrimSpace(getenv("MEETINGS_STT_MODEL"))
		if _, err := stt.NewClient(c.STTURL, c.STTToken, c.STTModel, 0); err != nil {
			return fmt.Errorf("MEETINGS_STT_ENGINE=nemotron needs its fallback, the utterance engine's STT service: %w", err)
		}
	default:
		return errors.New("MEETINGS_STT_ENGINE is off, utterance, nemotron or fake")
	}
	var err error
	if c.IngestKey, err = ingest.ParseSecret(getenv("MEETINGS_INGEST_SECRET")); err != nil {
		return err
	}
	// Egress reaches the ingest inside the cluster (or Compose's internal
	// network): plain ws, to any host.
	if c.IngestURL, err = checkURL("MEETINGS_INGEST_URL", getenv("MEETINGS_INGEST_URL"), "wss", "ws", true); err != nil {
		return err
	}
	return nil
}

// engine builds the configured engine; nil when transcription is off.
func (cfg Config) engine(logger *log.Logger) (stt.Engine, error) {
	switch cfg.Engine {
	case EngineNemotron:
		cl, err := stt.NewClient(cfg.STTURL, cfg.STTToken, cfg.STTModel, stt.CallTimeout)
		if err != nil {
			return nil, err
		}
		return stt.NewNemotron(cfg.StreamURL, cfg.StreamToken, stt.NewUtterance(cl), logger), nil
	case EngineUtterance:
		cl, err := stt.NewClient(cfg.STTURL, cfg.STTToken, cfg.STTModel, stt.CallTimeout)
		if err != nil {
			return nil, err
		}
		return stt.NewUtterance(cl), nil
	case EngineFake:
		return stt.NewFake(cfg.FakeScript), nil
	}
	return nil, nil
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

// hostPort is a URL's host: a DNS name or IPv4 address, or a bracketed IPv6
// address, with an optional port.
var hostPort = regexp.MustCompile(`^(?i:(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.?|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

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
	// 🔴 A host name or an address, nothing else: these values are written
	// into the pages' CSP (frame-ancestors), where a wildcard (*, *.x) widens
	// it and ';' ',' or a quote starts another directive or source.
	if !hostPort.MatchString(u.Host) {
		return "", fmt.Errorf("%s must name one host (letters, digits, '-' and '.', or an [IPv6] address, and a port): no wildcard and no ; , or quotes", name)
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
	eng, err := cfg.engine(logger)
	if err != nil {
		return err
	}
	sc := cfg.serverConfig(logger, lk)
	sc.Engine = eng
	srv := server.New(sc, st, app)

	go app.Run(ctx, owners.PollInterval)
	go sweep(ctx, st, logger)
	go srv.Run(ctx)

	pub := &http.Server{Addr: cfg.Listen, Handler: srv,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	internal := &http.Server{Addr: cfg.InternalListen, Handler: srv.Internal(), ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	// The ingest listener: Egress's websocket streams, and nothing else. No
	// read or write timeout: a stream lives as long as the track (and the
	// server never pings it; internal/ingest).
	ingestSrv := &http.Server{Addr: cfg.IngestListen, Handler: srv.Ingest(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 3)
	go func() { errc <- pub.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	if eng != nil {
		go func() { errc <- ingestSrv.ListenAndServe() }()
	}
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
	_ = ingestSrv.Shutdown(shut)
	return pub.Shutdown(shut)
}

// serverConfig is the server's part of cfg.
func (cfg Config) serverConfig(logger *log.Logger, lk *livekit.Client) server.Config {
	return server.Config{PublicURL: cfg.PublicURL, MusterOrigin: cfg.MusterOrigin, KitUpstream: cfg.URL, Log: logger,
		LiveKit: lk, LiveKitURL: cfg.LiveKitURL, ICE: cfg.ICE, TrustedProxies: cfg.TrustedProxies,
		ClientIPHeader: cfg.ClientIPHeader, IngestURL: cfg.IngestURL, IngestKey: cfg.IngestKey,
		Contacts: cfg.Contacts, S3: cfg.S3, Frames: cfg.frames(), AI: cfg.AI, Mail: cfg.Mail}
}

// frames is the frame sampler: off without a bucket (nothing to sample).
func (cfg Config) frames() frames.Sampler {
	if cfg.S3 == nil {
		return frames.Sampler{}
	}
	return frames.Find(cfg.FFmpeg)
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
	line := fmt.Sprintf("muster-meetings: talking to %s as server app %q (plugin %d, %s Files tasks); serving %s on %s (internal %s), framed by %s; LiveKit %s (API %s, ICE %s)",
		muster.Redact(cfg.URL), name, pluginID, files, cfg.PublicURL, cfg.Listen, cfg.InternalListen, cfg.MusterOrigin,
		muster.Redact(cfg.LiveKitURL), muster.Redact(cfg.LiveKitAPI), cfg.ICE)
	switch cfg.Engine {
	case "":
		line += "; transcript off"
	case EngineUtterance:
		line += fmt.Sprintf("; transcript: utterance engine via %s, ingest %s (Egress streams to %s)", muster.Redact(cfg.STTURL), cfg.IngestListen, muster.Redact(cfg.IngestURL))
	case EngineNemotron:
		line += fmt.Sprintf("; transcript: nemotron engine via %s, falling back to utterance via %s, ingest %s (Egress streams to %s)",
			muster.Redact(cfg.StreamURL), muster.Redact(cfg.STTURL), cfg.IngestListen, muster.Redact(cfg.IngestURL))
	default:
		line += fmt.Sprintf("; transcript: %s engine, ingest %s (Egress streams to %s)", cfg.Engine, cfg.IngestListen, muster.Redact(cfg.IngestURL))
	}
	// Nothing is added without contacts: the line is today's, byte for byte.
	if cfg.Contacts != nil {
		line += fmt.Sprintf("; contacts: %s (the guest card is offered)", muster.Redact(cfg.Contacts.Host()))
	}
	// Nothing is added without a bucket either.
	if cfg.S3 != nil {
		fr := "frame sampling off (no ffmpeg)"
		if cfg.frames().Available() {
			fr = "frames by " + cfg.frames().FFmpeg
		}
		line += fmt.Sprintf("; recording: bucket %q at %s, %s", cfg.S3.Bucket(), muster.Redact(cfg.S3.Endpoint()), fr)
	}
	// Nor without a model, nor without a mail server.
	if cfg.AI != nil {
		vision := "no frame analysis"
		if cfg.AI.Vision.Model != "" && cfg.S3 != nil {
			vision = "frames by " + cfg.AI.Vision.Model
		}
		line += fmt.Sprintf("; action items: %s (%s, live every %s), %s", cfg.AI.Items.Model, cfg.AI.Items.Adapter, cfg.AI.Every, vision)
	}
	if cfg.Mail != nil {
		line += fmt.Sprintf("; notes: mail via %s from %s", cfg.Mail.Host(), cfg.Mail.From())
	}
	return line
}

// reprocess is the admin command: a post-call pass queued for every meeting
// that ended in [-from, -to] (dates, UTC; -to inclusive), under the
// configured models or the ones given. It writes to the database the
// running plugin uses (same STATE_DIR): its worker runs them.
func reprocess(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("reprocess", flag.ContinueOnError)
	from := fs.String("from", "", "first day, YYYY-MM-DD (UTC)")
	to := fs.String("to", "", "last day, YYYY-MM-DD (UTC), inclusive")
	im := fs.String("items-model", "", "the items model (default MEETINGS_ITEMS_MODEL)")
	ip := fs.String("items-prefs", "", "its provider object (required with another model)")
	vm := fs.String("vision-model", "", "the vision model (default MEETINGS_VISION_MODEL's; off for none)")
	vp := fs.String("vision-prefs", "", "its provider object (required with another model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d0, err1 := time.Parse("2006-01-02", *from)
	d1, err2 := time.Parse("2006-01-02", *to)
	if err1 != nil || err2 != nil || d1.Before(d0) {
		return errors.New("-from and -to are dates, YYYY-MM-DD, -to not before -from")
	}
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}
	if cfg.AI == nil {
		return errors.New("MEETINGS_ITEMS_MODEL is off: there is nothing to reprocess with")
	}
	pick := func(model, prefs, defModel string, defPrefs map[string]any, adapter func(string) string, name string) (string, string, error) {
		if model == "" {
			model = defModel
			if prefs == "" {
				b, _ := json.Marshal(defPrefs)
				prefs = string(b)
			}
		} else if prefs == "" && model != defModel {
			return "", "", fmt.Errorf("-%s-model %s needs -%s-prefs (a provider pin)", name, model, name)
		} else if prefs == "" {
			b, _ := json.Marshal(defPrefs)
			prefs = string(b)
		}
		p, err := items.ParsePrefs(prefs, adapter(model))
		if err != nil {
			return "", "", err
		}
		b, _ := json.Marshal(p)
		return model, string(b), nil
	}
	itemsModel, itemsPrefs, err := pick(*im, *ip, cfg.AI.Items.Model, cfg.AI.Items.Prefs, items.AdapterFor, "items")
	if err != nil {
		return err
	}
	visionModel, visionPrefs := "", ""
	if *vm != items.Off && cfg.S3 != nil && (cfg.AI.Vision.Model != "" || *vm != "") {
		if visionModel, visionPrefs, err = pick(*vm, *vp, cfg.AI.Vision.Model, cfg.AI.Vision.Prefs, func(string) string { return items.AdapterChatJSON }, "vision"); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "meetings.db"), cfg.LinkKey)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	ids, err := st.EndedBetween(ctx, d0, d1.Add(24*time.Hour))
	if err != nil {
		return err
	}
	now := time.Now()
	for _, id := range ids {
		if _, err := st.QueuePost(ctx, id, itemsModel, itemsPrefs, visionModel, visionPrefs, "admin", now); err != nil {
			return fmt.Errorf("meeting %s: %w", id, err)
		}
	}
	fmt.Fprintf(out, "queued %d post-call runs (items %s, vision %s) for meetings that ended %s to %s; the running plugin runs them\n",
		len(ids), itemsModel, orNone(visionModel), *from, *to)
	return nil
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
