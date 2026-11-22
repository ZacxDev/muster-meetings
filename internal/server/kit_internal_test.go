package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fixture is a vendored kit file (e2e/fixtures/ui-kit: muster's own render
// at the pinned commit) for preset.
func fixture(t *testing.T, preset, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "e2e", "fixtures", "ui-kit", preset, "v1", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeKitUpstream is muster's /ui-kit/v1/ with a theme an admin can change,
// ETags and 304s as muster sends them, and a switch to fail or hang.
type fakeKitUpstream struct {
	mu    sync.Mutex
	theme string
	fail  bool
	hang  time.Duration
	files map[string][]byte // "<theme>/<name>"
	gets  atomic.Int32
	cond  atomic.Int32 // conditional requests answered 304
	srv   *httptest.Server
}

func newFakeKitUpstream(t *testing.T) *fakeKitUpstream {
	u := &fakeKitUpstream{theme: "brass", files: map[string][]byte{}}
	for _, th := range []string{"brass", "harbor"} {
		for _, n := range []string{"kit.css", "kit.js"} {
			u.files[th+"/"+n] = fixture(t, th, n)
		}
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.gets.Add(1)
		u.mu.Lock()
		theme, fail, hang := u.theme, u.fail, u.hang
		u.mu.Unlock()
		if hang > 0 {
			select {
			case <-time.After(hang):
			case <-r.Context().Done():
				return
			}
		}
		b, ok := u.files[theme+"/"+filepath.Base(r.URL.Path)]
		if fail || !ok || filepath.Dir(r.URL.Path) != "/ui-kit/v1" {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		etag := `"` + theme + `"`
		if r.Header.Get("If-None-Match") == etag {
			u.cond.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write(b)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeKitUpstream) set(f func(*fakeKitUpstream)) { u.mu.Lock(); f(u); u.mu.Unlock() }

// The kit the pages load carries the install's theme: fetched from muster,
// served as muster sent it, revalidated with its ETag after the TTL, and a
// theme change reaching the next load after that.
func TestTheProxiedKitCarriesTheInstallsTheme(t *testing.T) {
	up := newFakeKitUpstream(t)
	clock := time.Unix(1_800_000_000, 0)
	k := newKitProxy(up.srv.URL, 0, time.Minute, 0, func() time.Time { return clock })
	for _, n := range []string{"kit.css", "kit.js"} {
		b, src := k.get(n)
		if src != "muster" || !bytes.Equal(b, up.files["brass/"+n]) {
			t.Fatalf("%s: source %q, muster's brass bytes %v", n, src, bytes.Equal(b, up.files["brass/"+n]))
		}
	}
	// Within the TTL: no request to muster.
	before := up.gets.Load()
	k.get("kit.css")
	if up.gets.Load() != before {
		t.Fatal("asked muster again within the TTL")
	}
	// After it: a conditional request, answered 304, the copy kept.
	clock = clock.Add(61 * time.Second)
	if b, src := k.get("kit.css"); src != "muster" || !bytes.Equal(b, up.files["brass/kit.css"]) || up.cond.Load() != 1 {
		t.Fatalf("revalidation: source %q, 304s %d", src, up.cond.Load())
	}
	// The admin picks harbor: the next fetch after the TTL draws it.
	up.set(func(u *fakeKitUpstream) { u.theme = "harbor" })
	clock = clock.Add(61 * time.Second)
	if b, src := k.get("kit.css"); src != "muster" || !bytes.Equal(b, up.files["harbor/kit.css"]) {
		t.Fatalf("after a theme change: source %q, harbor's bytes %v", src, bytes.Equal(b, up.files["harbor/kit.css"]))
	}
}

// 🔴 muster down or hanging: the bundled kit, at once (the upstream fetch's
// own timeout, 2 s by default), never a page held on muster. A copy fetched
// before is served stale instead of the bundled one.
func TestTheKitFallsBackQuicklyWhenMusterIsDownOrSlow(t *testing.T) {
	bundledCSS, bundledJS := bundledKit("kit.css"), bundledKit("kit.js")
	if len(bundledCSS) < 1000 || len(bundledJS) < 1000 {
		t.Fatal("the bundled kit is not embedded")
	}

	// Down: the bundled kit.
	up := newFakeKitUpstream(t)
	up.set(func(u *fakeKitUpstream) { u.fail = true })
	k := newKitProxy(up.srv.URL, 0, time.Minute, 0, time.Now)
	if b, src := k.get("kit.js"); src != "bundled" || !bytes.Equal(b, bundledJS) {
		t.Fatalf("muster answering 503: source %q", src)
	}

	// Hanging for 10 s: the bundled kit within the default timeout.
	up2 := newFakeKitUpstream(t)
	up2.set(func(u *fakeKitUpstream) { u.hang = 10 * time.Second })
	k2 := newKitProxy(up2.srv.URL, 0, time.Minute, 0, time.Now)
	start := time.Now()
	b, src := k2.get("kit.css")
	if took := time.Since(start); took > 3*time.Second || src != "bundled" || !bytes.Equal(b, bundledCSS) {
		t.Fatalf("muster hanging 10 s: source %q after %v, want the bundled kit within %v", src, took, DefaultKitTimeout)
	}

	// No upstream at all: the bundled kit.
	if _, src := newKitProxy("", 0, 0, 0, time.Now).get("kit.css"); src != "bundled" {
		t.Fatalf("no upstream: source %q", src)
	}

	// Fetched once, then muster fails: the last good copy, marked stale; and
	// muster is asked again only after the retry wait.
	up3 := newFakeKitUpstream(t)
	clock := time.Unix(1_800_000_000, 0)
	k3 := newKitProxy(up3.srv.URL, 0, time.Minute, 0, func() time.Time { return clock })
	k3.get("kit.css")
	up3.set(func(u *fakeKitUpstream) { u.fail = true })
	clock = clock.Add(61 * time.Second)
	if b, src := k3.get("kit.css"); src != "muster-stale" || !bytes.Equal(b, up3.files["brass/kit.css"]) {
		t.Fatalf("muster failing after a good fetch: source %q", src)
	}
	n := up3.gets.Load()
	clock = clock.Add(DefaultKitRetry / 2)
	k3.get("kit.css")
	if up3.gets.Load() != n {
		t.Fatal("a failed upstream was asked again before the retry wait")
	}
}
