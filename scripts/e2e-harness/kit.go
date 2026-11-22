package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kitFiles are the kit's files under /ui-kit/v1/ and their Content-Type
// (docs/ui-kit.md, "What the responses carry").
var kitFiles = map[string]string{
	"kit.css":   "text/css; charset=utf-8",
	"theme.css": "text/css; charset=utf-8",
	"kit.js":    "text/javascript; charset=utf-8",
	"icons.svg": "image/svg+xml",
}

// kitThemes are the presets vendored (scripts/vendor-ui-kit.sh).
var kitThemes = []string{"brass", "harbor"}

// fakeKit serves the vendored kit as muster serves it.
type fakeKit struct {
	files map[string][]byte // "<theme>/<file>" -> bytes

	mu      sync.Mutex
	theme   string
	missing map[string]bool // file -> answer 404
	slow    time.Duration
}

// loadKit reads every fixture and checks it against SHA256SUMS: a fixture
// edited by hand (or a partial vendor run) refuses to start the harness.
func loadKit(dir string) (*fakeKit, error) {
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, fmt.Errorf("the UI kit fixtures: %w (run scripts/vendor-ui-kit.sh)", err)
	}
	want := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			want[strings.TrimPrefix(f[1], "./")] = f[0]
		}
	}
	k := &fakeKit{files: map[string][]byte{}, theme: "brass", missing: map[string]bool{}}
	for _, th := range kitThemes {
		for name := range kitFiles {
			rel := th + "/v1/" + name
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); want[rel] == "" || got != want[rel] {
				return nil, fmt.Errorf("the UI kit fixture %s does not match SHA256SUMS (%s, want %q): re-run scripts/vendor-ui-kit.sh", rel, got, want[rel])
			}
			k.files[th+"/"+name] = b
		}
	}
	return k, nil
}

func (k *fakeKit) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui-kit/v1/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.PathValue("file"))
		ctype, ok := kitFiles[name]
		k.mu.Lock()
		theme, gone, slow := k.theme, k.missing[strings.TrimPrefix(path.Ext(name), ".")], k.slow
		k.mu.Unlock()
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-r.Context().Done():
				return
			}
		}
		if !ok || gone {
			http.NotFound(w, r)
			return
		}
		b := k.files[theme+"/"+name]
		sum := sha256.Sum256(b)
		h := w.Header()
		etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", ctype)
		h.Set("ETag", etag)
		h.Set("Cache-Control", "no-cache")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	})
	mux.HandleFunc("GET /kit-mode", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		k.mu.Lock()
		defer k.mu.Unlock()
		if th := q.Get("theme"); th != "" {
			if _, ok := k.files[th+"/kit.css"]; !ok {
				http.Error(w, "no such theme", http.StatusBadRequest)
				return
			}
			k.theme = th
		}
		k.missing = map[string]bool{}
		ms, _ := strconv.Atoi(q.Get("slow"))
		k.slow = time.Duration(ms) * time.Millisecond
		for _, m := range strings.Split(q.Get("missing"), ",") {
			if m != "" {
				k.missing[m] = true
			}
		}
		fmt.Fprintf(w, "theme=%s missing=%v slow=%v\n", k.theme, q.Get("missing"), k.slow)
	})
}
