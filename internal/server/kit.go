package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The UI kit, served from the plugin's OWN origin at /kit/v1/ (docs/ui-kit.md
// in muster: kit.css and kit.js).
//
// 🔴 No page makes the browser contact muster. A guest may not be able to
// reach muster at all (a LAN-only muster, a firewall that drops), and a
// render-blocking stylesheet or script on muster's origin would freeze the
// page until the connection timed out; it would also hand every guest's
// address to muster. So the plugin fetches the kit from muster itself
// (MUSTER_URL, which it calls anyway), keeps it in memory, revalidates it
// with its ETag once KitTTL has passed, and serves it here. The owner's page
// uses the same route: one code path, the same strict CSP ('self' only),
// and a page that cannot hang on muster either.
//
// The upstream fetch has its own timeout (KitTimeout, default 2 s). When it
// fails (muster down, slow, a 404), the last copy fetched from muster is
// served if there is one, else the kit bundled with the plugin
// (static/kit-fallback.css, static/vendor/muster-ui-kit-v1.js: muster's own
// render in the default theme); muster is asked again after KitRetry. The
// X-Kit-Source header says which: "muster", "muster-stale" or "bundled".

// KitRoute is where the pages load the kit.
const KitRoute = "/kit/v1/"

const (
	DefaultKitTTL     = time.Minute
	DefaultKitTimeout = 2 * time.Second
	// DefaultKitRetry is how soon a failed upstream is asked again.
	DefaultKitRetry = 10 * time.Second
	// kitMaxBytes caps one upstream file.
	kitMaxBytes = 1 << 20
)

// kitFiles are the kit files served, their type and the bundled copy.
var kitFiles = map[string]struct{ ctype, bundled string }{
	"kit.css": {"text/css; charset=utf-8", "web/static/kit-fallback.css"},
	"kit.js":  {"text/javascript; charset=utf-8", "web/static/vendor/muster-ui-kit-v1.js"},
}

type kitEntry struct {
	mu       sync.Mutex
	body     []byte
	upstream string // muster's ETag for body, when it came from muster
	source   string
	next     time.Time // when to ask muster again
}

type kitProxy struct {
	base    string // muster's base URL, "" for the bundled kit only
	client  *http.Client
	timeout time.Duration
	ttl     time.Duration
	retry   time.Duration
	now     func() time.Time
	files   map[string]*kitEntry
}

func newKitProxy(base string, timeout, ttl, retry time.Duration, now func() time.Time) *kitProxy {
	if retry <= 0 {
		retry = DefaultKitRetry
	}
	if timeout <= 0 {
		timeout = DefaultKitTimeout
	}
	if ttl <= 0 {
		ttl = DefaultKitTTL
	}
	// The fetch's deadline is its context's (fetch): one bound, not two.
	k := &kitProxy{base: strings.TrimRight(base, "/"), timeout: timeout, ttl: ttl, retry: retry, now: now, files: map[string]*kitEntry{},
		client: &http.Client{}}
	for name := range kitFiles {
		k.files[name] = &kitEntry{}
	}
	return k
}

func bundledKit(name string) []byte {
	b, _ := webFS.ReadFile(kitFiles[name].bundled)
	return b
}

// get is the file to serve now, and where it came from.
func (k *kitProxy) get(name string) ([]byte, string) {
	e := k.files[name]
	e.mu.Lock()
	defer e.mu.Unlock()
	now := k.now()
	if e.body != nil && now.Before(e.next) {
		return e.body, e.source
	}
	if body, etag, ok := k.fetch(name, e); ok {
		if body != nil {
			e.body, e.upstream = body, etag
		}
		e.source, e.next = "muster", now.Add(k.ttl)
		return e.body, e.source
	}
	e.next = now.Add(k.retry)
	if e.body != nil && e.upstream != "" {
		e.source = "muster-stale"
		return e.body, e.source
	}
	e.body, e.source = bundledKit(name), "bundled"
	return e.body, e.source
}

// fetch asks muster for one file, conditionally when there is a copy from
// it. ok with a nil body is a 304: the copy is current.
func (k *kitProxy) fetch(name string, e *kitEntry) ([]byte, string, bool) {
	if k.base == "" {
		return nil, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", k.base+"/ui-kit/v1/"+name, nil)
	if err != nil {
		return nil, "", false
	}
	if e.upstream != "" && e.body != nil {
		req.Header.Set("If-None-Match", e.upstream)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && e.upstream != "" {
		return nil, e.upstream, true
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, kitMaxBytes+1))
	if err != nil || len(body) == 0 || len(body) > kitMaxBytes {
		return nil, "", false
	}
	return body, resp.Header.Get("ETag"), true
}

// handleKit serves /kit/v1/{file}: no cache without revalidation, with its
// own ETag (a theme change is a new body, so a new ETag at once).
func (s *Server) handleKit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	f, ok := kitFiles[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	body, source := s.kit.get(name)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	h.Set("X-Kit-Source", source)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(body)
}
