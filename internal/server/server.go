// Package server is the plugin's HTTP surface on its own origin
// (`https://meet.<muster host>`; design: "The plugin's HTTP API").
//
//	GET  /enter                 redeem muster's hand-off code, mint an owner session, render the app
//	GET  /api/meetings          the owner's meetings, newest first, paged
//	POST /api/meetings          New meeting: answers {"id", "join_url"}
//	GET  /api/meetings/{id}     one meeting, its participants, and its link while open
//
// Owner routes take `Authorization: Bearer mmo_…` and nothing else: no
// cookie is ever set or read (muster's frame is a third-party context, where
// Safari blocks cookies and Chrome partitions them; plugins.md, "Do not
// depend on a cookie in the frame"). A missing, unknown or expired session
// is 401, and another owner's meeting is the same 404 as one that does not
// exist.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster-meetings/internal/muster"
	"github.com/ZacxDev/muster-meetings/internal/owners"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

//go:embed web
var webFS embed.FS

var pageTmpl = template.Must(template.ParseFS(webFS, "web/*.html"))

// Config is what the server needs to know about where it runs.
type Config struct {
	// PublicURL is the plugin's own origin, e.g. https://meet.example.test:
	// the join link is built on it.
	PublicURL string
	// MusterOrigin is muster's browser-facing origin, e.g.
	// https://example.test: the only page allowed to frame the owner app
	// (CSP frame-ancestors).
	MusterOrigin string
	// Now is the clock; default time.Now.
	Now func() time.Time
	// Log receives the access log (method, path with any code redacted,
	// status, duration) and refusals. It never receives a code, a token or
	// a query string.
	Log *log.Logger
}

// Server is the plugin's public handler.
type Server struct {
	cfg   Config
	store *store.Store
	reg   *owners.Registry
	mux   *http.ServeMux
}

// New builds the handler.
func New(cfg Config, st *store.Store, reg *owners.Registry) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{cfg: cfg, store: st, reg: reg, mux: http.NewServeMux()}
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err)
	}
	s.mux.HandleFunc("GET /enter", s.handleEnter)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	s.mux.HandleFunc("GET /api/meetings", s.owner(s.handleListMeetings))
	s.mux.HandleFunc("POST /api/meetings", s.owner(s.handleCreateMeeting))
	s.mux.HandleFunc("GET /api/meetings/{id}", s.owner(s.handleGetMeeting))
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
func (s *Server) ownerPageCSP() string {
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; media-src 'self' blob:; " +
		"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + s.cfg.MusterOrigin
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
	status int
	wrote  bool
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

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// RedactPath is a request path as the access log may record it: a guest
// link's code (`/j/<code>…`) is replaced, so a log line never carries one.
func RedactPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/j/"); ok {
		if _, tail, ok := strings.Cut(rest, "/"); ok {
			return "/j/{code}/" + tail
		}
		return "/j/{code}"
	}
	return p
}

func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
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

// The /enter refusals. Each is a whole sentence the owner reads in muster's
// frame; none names a code or what muster said.
const (
	EnterAgain     = "Open Meetings from muster again."
	EnterNotSetUp  = "This Meetings server does not serve that muster tab. Ask the person who runs it to check the plugin token."
	EnterRefused   = "That link to Meetings has expired or was already used."
	EnterNoMuster  = "Meetings could not reach muster. Try again in a moment."
	EnterWrongKind = "That link was not made for the Meetings tab."
)

type enterPage struct {
	Session   string
	ExpiresAt string
	Ref       string
	Message   string
	Again     bool
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

// handleEnter is GET /enter?muster_code=…&muster_plugin=…, framed by muster's
// /apps/{id} page. It redeems the code with the token of the plugin
// muster_plugin names and admits the answer only when it is an app code for
// that token's own owner.
func (s *Server) handleEnter(w http.ResponseWriter, r *http.Request) {
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
	tok, ok := s.reg.Lookup(pluginID)
	if !ok {
		s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "plugin "+pid+" is not a live token here")
		return
	}
	red, err := tok.Client.Redeem(r.Context(), code)
	if err != nil {
		e := muster.AsError(err)
		switch {
		case e != nil && e.Kind == muster.KindOff:
			s.refuseEnter(w, http.StatusForbidden, EnterRefused, "muster: no such code for plugin "+pid)
		case e != nil && e.Kind == muster.KindAuth:
			s.reg.Revoke(r.Context(), pluginID)
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: plugin "+pid+"'s token is revoked")
		case e != nil && e.Kind == muster.KindCapability:
			s.refuseEnter(w, http.StatusForbidden, EnterNotSetUp, "muster: plugin "+pid+"'s token may not redeem")
		default:
			s.refuseEnter(w, http.StatusBadGateway, EnterNoMuster, "muster: "+kindOf(err))
		}
		return
	}
	// 🔴 The three checks that make the code this tab's: muster answers a
	// viewer code to the same route just as readily.
	if red.Purpose != "app" {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed purpose is not app")
		return
	}
	if red.Target.App == nil || len(red.Target.Mirror) > 0 {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed target is not {\"app\": {}}")
		return
	}
	if red.OwnerID != tok.OwnerID {
		s.refuseEnter(w, http.StatusForbidden, EnterWrongKind, "redeemed owner is not the token's owner")
		return
	}
	now := s.cfg.Now()
	session, exp, err := s.store.CreateSession(r.Context(), tok.OwnerID, tok.PluginID, now)
	if err != nil {
		s.refuseEnter(w, http.StatusInternalServerError, EnterNoMuster, "session: "+err.Error())
		return
	}
	// ref is a hint (a knock notification's meeting): passed on only when it
	// names one of this owner's own meetings.
	ref := ""
	if red.Target.App != nil && red.Target.App.Ref != "" {
		if _, _, err := s.store.ForOwner(tok.OwnerID).Meeting(r.Context(), red.Target.App.Ref); err == nil {
			ref = red.Target.App.Ref
		}
	}
	s.renderEnter(w, http.StatusOK, enterPage{Session: session, ExpiresAt: exp.Format(time.RFC3339), Ref: ref})
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
// the handler that owner's repository, and only it. The session must be
// unexpired, and the token it was minted through still live: a revocation
// is enforced here at once, even before its sessions are deleted.
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
		if t, live := s.reg.Lookup(sess.PluginID); !live || t.OwnerID != sess.OwnerID {
			refuse()
			return
		}
		next(w, r, s.store.ForOwner(sess.OwnerID))
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

// MaxTitle is a title's longest, in characters.
const MaxTitle = 80

// ValidTitle checks a trimmed title: at most MaxTitle characters, valid
// UTF-8, no control or bidirectional-formatting characters (a title is
// shown as text, but a bidi override still reorders what it sits beside).
func ValidTitle(t string) bool {
	if !utf8.ValidString(t) || utf8.RuneCountInString(t) > MaxTitle {
		return false
	}
	for _, c := range t {
		if unicode.IsControl(c) || unicode.Is(unicode.Bidi_Control, c) || c == ' ' || c == ' ' {
			return false
		}
	}
	return true
}

func (s *Server) handleCreateMeeting(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "the body is JSON: {\"title\"?, \"record\"?}"})
		return
	}
	var in struct {
		Title  *string `json:"title"`
		Record *bool   `json:"record"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"title\"?: string, \"record\"?: bool}"})
		return
	}
	title := ""
	if in.Title != nil {
		title = strings.Join(strings.Fields(*in.Title), " ")
		if !ValidTitle(title) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a title is at most " + strconv.Itoa(MaxTitle) + " characters, with no control characters"})
			return
		}
	}
	c, err := repo.CreateMeeting(r.Context(), store.NewMeeting{Title: title, Record: in.Record}, s.cfg.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be made"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.Meeting.ID, "join_url": s.JoinURL(c.JoinCode), "meeting": c.Meeting})
}

// JoinURL is a guest link for code.
func (s *Server) JoinURL(code string) string { return s.cfg.PublicURL + "/j/" + code }

func (s *Server) handleGetMeeting(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	m, ps, err := repo.Meeting(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such meeting"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be read"})
		return
	}
	// link: the guest link again, while the meeting is open; null once it
	// has ended or its lifetime has passed (Link deletes the sealed code then).
	var link *string
	code, err := repo.Link(r.Context(), m.ID, s.cfg.Now())
	switch {
	case err == nil:
		u := s.JoinURL(code)
		link = &u
	case errors.Is(err, store.ErrNoLink):
	case errors.Is(err, store.ErrSealed):
		if s.cfg.Log != nil {
			s.cfg.Log.Printf("meeting %s: its sealed link does not open (was MEETINGS_LINK_KEY changed?)", m.ID)
		}
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the meeting could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meeting": m, "participants": ps, "link": link})
}

// Internal is the internal port's handler: GET /healthz.
func Internal(st *store.Store, reg *owners.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil || reg.Live() == 0 {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	return mux
}
