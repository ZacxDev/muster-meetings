// Package server is the plugin's HTTP surface on its own origin
// (`https://meet.<muster host>`; design: "The plugin's HTTP API").
//
//	GET  /enter                 redeem muster's hand-off code, mint an owner session, render the app
//	GET  /api/meetings          the owner's meetings, newest first, paged
//	POST /api/meetings          New meeting: answers {"id", "join_url", "meeting"}
//	GET  /api/meetings/{id}     one meeting, and its link while it is open
//
// Phase 2b adds the call (calls.go), the guest page and lobby (guest.go), and
// on the internal listener LiveKit's webhook (machine.go).
//
// The plugin runs as one server-installed app (core PR 1h): one token serves
// every person on the muster, each known by the pairwise subject muster's
// redeem gives.
//
// Owner routes take `Authorization: Bearer mmo_…` and nothing else: no
// cookie is ever set or read (muster's frame is a third-party context, where
// Safari blocks cookies and Chrome partitions them; plugins.md, "Do not
// depend on a cookie in the frame"). A missing, unknown or expired session
// is 401, and another person's meeting is the same 404 as one that does not
// exist.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/ingest"
	"github.com/ZacxDev/muster-meetings/internal/livekit"
	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/store"
	"github.com/ZacxDev/muster-meetings/internal/stt"
)

//go:embed web
var webFS embed.FS

// pageTmpl is the pages; every asset they reference is named through
// pageFuncs (assets.go), so it carries its version.
var pageTmpl = template.Must(template.New("").Funcs(pageFuncs).ParseFS(webFS, "web/*.html"))

// Config is what the server needs to know about where it runs.
type Config struct {
	// PublicURL is the plugin's own origin, e.g. https://meet.example.test:
	// the join link is built on it.
	PublicURL string
	// MusterOrigin is muster's browser-facing origin, e.g.
	// https://example.test: the only page allowed to frame the owner app
	// (CSP frame-ancestors).
	MusterOrigin string
	// KitUpstream is muster's base URL as the plugin calls it (MUSTER_URL):
	// the UI kit is fetched from <KitUpstream>/ui-kit/v1/ server-side and
	// served at KitRoute (kit.go). "" serves the bundled kit only.
	KitUpstream string
	// KitTimeout bounds each upstream kit fetch (default DefaultKitTimeout);
	// KitTTL is how long a fetched kit is served before muster is asked again
	// (default DefaultKitTTL).
	// KitRetry is how soon a failed fetch is tried again (default
	// DefaultKitRetry).
	KitTimeout time.Duration
	KitTTL     time.Duration
	KitRetry   time.Duration
	// Now is the clock; default time.Now.
	Now func() time.Time
	// Log receives the access log (method, path with any code redacted,
	// status, duration) and refusals. It never receives a code, a token, a
	// ticket, a guest's name or a query string.
	Log *log.Logger

	// LiveKit is the server API client (rooms, participants) and the keys
	// tokens are minted with.
	LiveKit *livekit.Client
	// LiveKitURL is where browsers connect for media signalling, e.g.
	// wss://meet-rtc.example.test. It and its https twin are the only
	// origins the pages may connect to besides their own.
	LiveKitURL string
	// ICE is the LiveKit client's ICE transport policy: "relay" (the
	// deployment's: TURN over TLS on 443 is the only media path, decision
	// 17), or "all" (a local LiveKit with no TURN).
	ICE string
	// TrustedProxies are the addresses of the ingress in front of the
	// plugin: a knock from one of them is counted against the
	// X-Forwarded-For client instead (lobby.ClientAddr).
	TrustedProxies []netip.Prefix
	// ClientIPHeader, when set, names the one header (e.g. CF-Connecting-IP)
	// that carries the client's address in place of X-Forwarded-For. It is
	// read only from a peer inside TrustedProxies (lobby.ClientAddr).
	ClientIPHeader string
	// StreamRecheck is how often an event stream re-reads the database
	// even with no change signalled (an end made elsewhere: the 24 h sweep,
	// a revocation); default 5 s. ReconcileEvery is the reconcile loop's
	// period; default 15 s (design).
	StreamRecheck  time.Duration
	ReconcileEvery time.Duration
	// Heartbeat is how often an idle event stream sends a comment, so
	// proxies keep it open; default 15 s.
	Heartbeat time.Duration
	// StreamsPerAddress and MaxStreams cap the open event streams per client
	// address and in all (streams.go); defaults DefaultStreamsPerAddress and
	// DefaultMaxStreams.
	StreamsPerAddress int
	MaxStreams        int

	// Engine transcribes each microphone track (stt.UtteranceEngine,
	// stt.FakeEngine); nil is transcription off: no egress is started and
	// the ingest refuses every stream (transcribe.go).
	Engine stt.Engine
	// IngestURL is the ingest listener as Egress reaches it, e.g.
	// ws://muster-meetings:8082: each track egress streams to
	// <IngestURL>/stt/ingest/<track>/<token>.
	IngestURL string
	// IngestKey is MEETINGS_INGEST_SECRET: the HMAC key of every ingest
	// token.
	IngestKey []byte
	// EgressBackoff is the wait before each retry of a refused track egress
	// start; default DefaultEgressBackoff.
	EgressBackoff []time.Duration
	// After is the timer the egress start and restart waits use; default
	// time.After (a test records the waits).
	After func(time.Duration) <-chan time.Time
}

// Server is the plugin's public handler.
type Server struct {
	kit     *kitProxy
	cfg     Config
	store   *store.Store
	app     *owners.App
	mux     *http.ServeMux
	hub     *lobby.Hub
	knocker *lobby.Knocker
	limiter *lobby.Limiter
	// rtcOrigins is the LiveKit signalling origin and its https twin, for
	// connect-src.
	rtcOrigins string
	streams    *streamReg
	// xffUntrusted: a request carried X-Forwarded-For with no trusted
	// proxies configured (clientAddr).
	xffUntrusted atomic.Bool
	// tr is the transcript's in-memory side (transcribe.go); bg the context
	// its goroutines run under, cancelled when Run returns.
	tr     *transcriber
	bg     context.Context
	bgStop context.CancelFunc
}

// New builds the handler.
func New(cfg Config, st *store.Store, app *owners.App) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StreamRecheck <= 0 {
		cfg.StreamRecheck = 5 * time.Second
	}
	if cfg.ReconcileEvery <= 0 {
		cfg.ReconcileEvery = 15 * time.Second
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.StreamsPerAddress <= 0 {
		cfg.StreamsPerAddress = DefaultStreamsPerAddress
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultMaxStreams
	}
	if cfg.ICE == "" {
		cfg.ICE = "relay"
	}
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	if len(cfg.EgressBackoff) == 0 {
		cfg.EgressBackoff = DefaultEgressBackoff
	}
	s := &Server{cfg: cfg, store: st, app: app, mux: http.NewServeMux(), hub: lobby.NewHub(), limiter: lobby.NewLimiter(),
		rtcOrigins: RTCOrigins(cfg.LiveKitURL), streams: newStreamReg(), tr: newTranscriber()}
	s.bg, s.bgStop = context.WithCancel(context.Background())
	s.kit = newKitProxy(cfg.KitUpstream, cfg.KitTimeout, cfg.KitTTL, cfg.KitRetry, cfg.Now)
	s.knocker = lobby.NewKnocker(app.Client, s.hub, st.LobbyCount, cfg.Log, cfg.Now)
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err)
	}
	s.mux.HandleFunc("GET /enter", s.handleEnter)
	s.mux.HandleFunc("GET "+KitRoute+"{file}", s.handleKit)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	s.mux.HandleFunc("GET /api/meetings", s.owner(s.handleListMeetings))
	s.mux.HandleFunc("POST /api/meetings", s.owner(s.handleCreateMeeting))
	s.mux.HandleFunc("GET /api/meetings/{id}", s.owner(s.handleGetMeeting))
	s.mux.HandleFunc("POST /api/meetings/{id}/token", s.owner(s.handleOwnerToken))
	s.mux.HandleFunc("POST /api/meetings/{id}/record", s.owner(s.handleSwitch(store.SwitchRecord)))
	s.mux.HandleFunc("POST /api/meetings/{id}/ai", s.owner(s.handleSwitch(store.SwitchAI)))
	s.mux.HandleFunc("GET /api/meetings/{id}/events", s.owner(s.handleOwnerEvents))
	s.mux.HandleFunc("POST /api/meetings/{id}/lobby/{pid}/admit", s.owner(s.handleMove(store.EvAdmit)))
	s.mux.HandleFunc("POST /api/meetings/{id}/lobby/{pid}/deny", s.owner(s.handleMove(store.EvDeny)))
	s.mux.HandleFunc("DELETE /api/meetings/{id}/participants/{pid}", s.owner(s.handleMove(store.EvRemove)))
	s.mux.HandleFunc("POST /api/meetings/{id}/end", s.owner(s.handleEnd))
	s.mux.HandleFunc("GET /api/meetings/{id}/transcript", s.owner(s.handleTranscript))
	s.mux.HandleFunc("GET /j/{code}", s.handleGuestPage)
	s.mux.HandleFunc("POST /j/{code}/knock", s.handleKnock)
	s.mux.HandleFunc("POST /j/{code}/wait", s.handleWait)
	s.mux.HandleFunc("POST /j/{code}/events", s.handleGuestEvents)
	s.mux.HandleFunc("POST /j/{code}/leave", s.handleLeave)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	})
	return s
}

// The headers every response carries; ownerPageCSP replaces the CSP on the
// pages muster frames.
const (
	baseCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	// PermissionsPolicy is the design's, for every page.
	PermissionsPolicy = "camera=(self), microphone=(self), display-capture=(self)"
)

// ownerPageCSP is the owner app's policy: its own script and style only, its
// own API only, and framed by muster's exact origin alone.
func (s *Server) ownerPageCSP() string { return s.pageCSP(s.cfg.MusterOrigin) }

// guestPageCSP is the guest page's: the same, but framed by nobody.
func (s *Server) guestPageCSP() string { return s.pageCSP("'none'") }

// pageCSP is a page's policy (design: "Every page sets its own strict CSP"):
// its own script, style and images ONLY (muster's UI kit is served from this
// origin, kit.go: no page makes the browser contact muster); its own API and
// LiveKit's signalling origin (wss, and https for the client's /rtc/validate
// check) and nothing else; media from itself and blob:; framed by
// frameAncestors.
func (s *Server) pageCSP(frameAncestors string) string {
	connect := "'self'"
	if s.rtcOrigins != "" {
		connect += " " + s.rtcOrigins
	}
	src := "'self'"
	return "default-src 'self'; script-src " + src + "; style-src " + src + "; img-src " + src + "; connect-src " + connect + "; media-src 'self' blob:; " +
		"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + frameAncestors
}

// RTCOrigins is connect-src's LiveKit part for a signalling URL: its origin
// and that origin's http(s) twin, "wss://h https://h" (or "ws://h http://h"
// for a local LiveKit). "" for a URL that is not ws or wss.
func RTCOrigins(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	switch u.Scheme {
	case "wss":
		return "wss://" + u.Host + " https://" + u.Host
	case "ws":
		return "ws://" + u.Host + " http://" + u.Host
	}
	return ""
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.cfg.Now()
	h := w.Header()
	h.Set("Content-Security-Policy", baseCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Permissions-Policy", PermissionsPolicy)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(sw, r)
	if s.cfg.Log != nil {
		s.cfg.Log.Printf("http %s %s %d %s", r.Method, RedactPath(r.URL.Path), sw.status, s.cfg.Now().Sub(start).Round(time.Millisecond))
	}
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	wrote    bool
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (the ingest's
// websocket upgrade hijacks it).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RedactPath is a request path as the access log may record it: a guest
// link's code never reaches a log line. A path whose cleaned form is
// `/j/<code>…` (so `//j/…`, `/./j/…` and `/x/../j/…` too, which the router
// would redirect there) logs as `/j/{code}…`, and any other path holding a
// `/j/` segment anywhere logs as `/{redacted}`. An ingest path
// (`/stt/ingest/<track>/<token>`, which the public listener does not serve)
// logs its track only.
func RedactPath(p string) string {
	c := path.Clean("/" + p)
	// An ingest URL's last segment is its token: on the public listener it
	// is a 404, and its token still never reaches the log.
	if strings.HasPrefix(c, "/stt/ingest/") || strings.Contains(p, "/stt/ingest/") {
		return ingest.LogPath(c)
	}
	if rest, ok := strings.CutPrefix(c, "/j/"); ok {
		if _, tail, ok := strings.Cut(rest, "/"); ok {
			return "/j/{code}/" + tail
		}
		return "/j/{code}"
	}
	if strings.Contains(p, "/j/") || strings.Contains(c+"/", "/j/") {
		return "/{redacted}"
	}
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- /enter: the hand-off ----

// handoffCodeRE is muster's code shape: 32 bytes, base64url, 43 characters.
// Anything else is refused before it is sent to muster.
var handoffCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// The /enter refusals. Each is a whole sentence the person reads (in muster's
// frame, or in a tab of their own when /enter was not framed); none names a
// code or what muster said.
const (
	EnterAgain = "Open Meetings from muster again."
	// EnterNotFramed is /enter loaded anywhere but in a frame: a top-level
	// tab, a fetch, a link preview (design: "/enter refuses unless
	// Sec-Fetch-Dest: iframe").
	EnterNotFramed = "Open Meetings from muster."
	// EnterOldBrowser is /enter from a browser that sends no Sec-Fetch-*
	// headers at all (Safari and iOS before 16.4): a dead end, not a hole.
	EnterOldBrowser = "This browser is too old for Meetings. Update it, or open muster in a current browser."
	EnterNotSetUp   = "This Meetings server does not serve that muster tab. Ask the person who runs it to check the plugin token."
	EnterRefused    = "That link to Meetings has expired or was already used."
	EnterNoMuster   = "Meetings could not reach muster. Try again in a moment."
	EnterWrongKind  = "That link was not made for the Meetings tab."
)

type enterPage struct {
	Session string
	Ref     string
	Message string
	Again   bool
}

func (s *Server) renderEnter(w http.ResponseWriter, code int, p enterPage) {
	h := w.Header()
	h.Set("Content-Security-Policy", s.ownerPageCSP())
	h.Del("X-Frame-Options") // framed by muster: frame-ancestors decides
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = pageTmpl.ExecuteTemplate(w, "app.html", p)
}

func (s *Server) refuseEnter(w http.ResponseWriter, code int, msg, why string) {
	if s.cfg.Log != nil {
		s.cfg.Log.Printf("enter: refused %d: %s", code, why)
	}
	s.renderEnter(w, code, enterPage{Message: msg, Again: msg != EnterNotSetUp})
}

// framed says the browser is loading /enter into a frame: exactly one
// Sec-Fetch-Dest header, and it says iframe. When it is not, why is the
// sentence to show: EnterOldBrowser when the request carries no Sec-Fetch-*
// header at all, EnterNotFramed otherwise.
func framed(r *http.Request) (ok bool, why string) {
	if d := r.Header.Values("Sec-Fetch-Dest"); len(d) == 1 && d[0] == "iframe" {
		return true, ""
	}
	for k := range r.Header {
		if strings.HasPrefix(k, "Sec-Fetch-") {
			return false, EnterNotFramed
		}
	}
	return false, EnterOldBrowser
}

// handleEnter is GET /enter?muster_code=…&muster_plugin=…, framed by muster's
// /apps/{id} page. It redeems the code with the server token and admits the
// answer only when it is an app code of this server app that names a person
// by subject.
//
// 🔴 It refuses unless the browser says it is loading a frame
// (Sec-Fetch-Dest: iframe), before anything is redeemed. With one server
// token, a link to /enter carrying someone else's unspent code would sign
// whoever opens it in as that someone (sign-in CSRF, any person to any
// person). Refused unframed, the code is not spent and stays usable by the
// frame it was minted for; framed by anyone but muster, frame-ancestors
// stops the browser rendering what /enter answers.
func (s *Server) handleEnter(w http.ResponseWriter, r *http.Request) {
	if ok, why := framed(r); !ok {
		s.refuseEnter(w, http.StatusForbidden, why, "not loaded in a frame (Sec-Fetch-Dest)")
		return
	}
	q := r.URL.Query()
	code, pid := q.Get("muster_code"), q.Get("muster_plugin")
	if code == "" && pid == "" {
		// A reload inside the frame, after history.replaceState took the
		// code away: the session lived only in the page's memory.
		s.refuseEnter(w, http.StatusBadRequest, EnterAgain, "no code (a reload)")
		return
	}
	pluginID, err := strconv.ParseInt(pid, 10, 64)
	if err != nil || pluginID <= 0 || !handoffCodeRE.MatchString(code) {
		s.refuseEnter(w, http.StatusBadRequest, EnterAgain, "a malformed code or plugin id")
		return
	}
	// muster_plugin no longer picks a token: there is one, and the frame
	// must be this app's.
	if pluginID != s.app.PluginID || !s.app.Live() {
		s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "plugin "+pid+" is not this server app, or its token is dead")
		return
	}
	red, err := s.app.Client.Redeem(r.Context(), code)
	if err != nil {
		e := muster.AsError(err)
		switch {
		case e != nil && e.Kind == muster.KindOff:
			s.refuseEnter(w, http.StatusForbidden, EnterRefused, "muster: no such code")
			return
		case e != nil && e.Kind == muster.KindAuth:
			// The revocation must finish even if the browser goes away.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			s.app.Revoke(ctx, "the redeem answered 401: the app was uninstalled or its token rotated")
			cancel()
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: the token is revoked")
			return
		case e != nil && e.Kind == muster.KindCapability:
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: the token may not redeem")
			return
		default:
			// Fail closed: muster down, a 5xx, an answer this client cannot
			// read. Nothing is admitted on an answer we did not get.
			s.refuseEnter(w, http.StatusBadGateway, EnterNoMuster, "muster: "+kindOf(err))
			return
		}
	}
	// 🔴 The checks that make the code this tab's: muster answers a viewer
	// code to the same route just as readily, and only a server app's answer
	// names a person by subject.
	if red.Purpose != "app" {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed purpose is not app")
		return
	}
	if red.Target.App == nil || len(red.Target.Mirror) > 0 {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed target is not {\"app\": {}}")
		return
	}
	if red.Scope != muster.ScopeServer {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed scope is not server")
		return
	}
	if !store.ValidSubject(red.Owner.Subject) {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed owner has no subject")
		return
	}
	subject := red.Owner.Subject
	now := s.cfg.Now()
	session, _, err := s.store.CreateSession(r.Context(), subject, red.Owner.DisplayName, now)
	if err != nil {
		s.refuseEnter(w, http.StatusInternalServerError, EnterNoMuster, "session: "+err.Error())
		return
	}
	// ref is a hint (a knock notification's meeting): passed on only when it
	// names one of this person's own meetings.
	ref := ""
	if red.Target.App != nil && red.Target.App.Ref != "" {
		if _, err := s.store.ForSubject(subject).Meeting(r.Context(), red.Target.App.Ref); err == nil {
			ref = red.Target.App.Ref
		}
	}
	s.renderEnter(w, http.StatusOK, enterPage{Session: session, Ref: ref})
}

// kindOf names an error by its kind only: a network error's text can carry
// an address, which is fine, but never anything of the request.
func kindOf(err error) string {
	if e := muster.AsError(err); e != nil {
		return string(e.Kind) + " " + strconv.Itoa(e.Status)
	}
	return "error"
}

// ---- owner sessions ----

type ownerHandler func(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo)

// owner admits a request carrying a live owner session's bearer and hands
// the handler that person's repository, and only it. The session must be
// unexpired, and the app's token still live: an uninstall is enforced here
// at once, even before its sessions are deleted.
func (s *Server) owner(next ownerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		refuse := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			refuse()
			return
		}
		sess, err := s.store.LookupSession(r.Context(), tok, s.cfg.Now())
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the session could not be read"})
			return
		}
		if !s.app.Live() {
			refuse()
			return
		}
		next(w, r, s.store.ForSession(sess))
	}
}

// ---- /api/meetings ----

// ListLimit is a page's default size; MaxListLimit its largest.
const (
	ListLimit    = 50
	MaxListLimit = 100
)

func (s *Server) handleListMeetings(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	q := r.URL.Query()
	limit := ListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxListLimit {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit is 1 to " + strconv.Itoa(MaxListLimit)})
			return
		}
		limit = n
	}
	var after *store.Cursor
	if v := q.Get("cursor"); v != "" {
		c, ok := store.ParseCursor(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cursor is the next value of an earlier page"})
			return
		}
		after = &c
	}
	ms, next, err := repo.ListMeetings(r.Context(), after, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meetings could not be read"})
		return
	}
	var nextS *string
	if next != nil {
		v := next.String()
		nextS = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{"meetings": ms, "next": nextS})
}

// testHookBeforeCreate, when set (by tests only), runs after the owner's
// session was admitted and before the meeting is inserted: the window a
// revocation can race.
var testHookBeforeCreate func()

// handleCreateMeeting is New meeting. It takes no options: no body, or `{}`.
func (s *Server) handleCreateMeeting(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if b := strings.TrimSpace(string(raw)); err != nil || (b != "" && b != "{}") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "New meeting takes no options: send no body, or {}"})
		return
	}
	if testHookBeforeCreate != nil {
		testHookBeforeCreate()
	}
	c, err := repo.CreateMeetingWith(r.Context(), s.cfg.Now(), func(room string) error {
		if _, err := s.cfg.LiveKit.CreateRoom(r.Context(), room); err != nil {
			return liveKitErr{err}
		}
		return nil
	})
	if lk := (liveKitErr{}); errors.As(err, &lk) {
		s.cfg.Log.Printf("new meeting: LiveKit: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": CallsUnavailable})
		return
	}
	if errors.Is(err, store.ErrNoSession) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be made"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.Meeting.ID, "join_url": s.JoinURL(c.JoinCode), "meeting": c.Meeting})
}

// JoinURL is a guest link for code.
func (s *Server) JoinURL(code string) string { return s.cfg.PublicURL + "/j/" + code }

// handleGetMeeting is one meeting: {"meeting", "link"}. link is the guest
// link while the meeting is open and under LinkLifetime old, null
// otherwise; it is the owner's way to copy the link again.
func (s *Server) handleGetMeeting(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	m, err := repo.Meeting(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such meeting"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be read"})
		return
	}
	var link *string
	code, err := repo.Link(r.Context(), m.ID, s.cfg.Now())
	switch {
	case err == nil:
		u := s.JoinURL(code)
		link = &u
	case errors.Is(err, store.ErrNoLink):
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meeting": m, "link": link})
}

// Internal is the internal port's handler: GET /healthz, and POST
// /livekit/webhook (machine.go). It is never exposed by an ingress: LiveKit
// reaches it in the cluster.
func (s *Server) Internal() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /livekit/webhook", s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil || !s.app.Live() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
		if s.xffUntrusted.Load() {
			io.WriteString(w, "warning: "+XFFWarning+"\n")
		}
		if n := s.SilentStartFlags(); n > 0 {
			fmt.Fprintf(w, "warning: %d silent-start flag(s): a track had speech energy, or one constant level, and no words for %s (see the log)\n", n, stt.SilentStart)
		}
	})
	return mux
}
