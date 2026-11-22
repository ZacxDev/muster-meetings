package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
)

// Asset versions. Every file a page references names a version in its URL,
// so a deploy is a new URL and no browser or CDN can keep running the old
// script: a CDN in front of the plugin may hand the browser its own
// `max-age` (four hours, measured) whatever the origin sent, and an
// unversioned /static/app.js then outlived a deploy.
//
//   - /static/<file>?v=<the file's version>: the first 8 bytes of the file's
//     SHA-256, in hex, computed once from the embedded bytes. Served
//     `public, max-age=31536000, immutable` ONLY when v is the file's
//     current version; the bare path, or any other v (a page from before a
//     deploy), is `no-cache`, so a long-cached response is always the bytes
//     its URL names.
//   - /kit/v1/<file>?v=<the build's version>: the kit is proxied from
//     muster and re-themed while the plugin runs (kit.go), so its bytes are
//     not known when a page is drawn. It carries the build's version (every
//     embedded file's, together: the bundled kit is among them), which
//     changes on a deploy, and stays `no-cache` + ETag.
//   - The pages themselves are `no-store` (they hold the owner's session).
//
// The scripts are classic <script src> files that load nothing else (no ES
// module imports, no fetch of another script), so the pages' references are
// the whole set.

// assetVersions is the version of every file under fsys: its path (slash
// separated, from fsys's root) → the first 8 bytes of its SHA-256 in hex;
// and the build's version, the same hash over every "path version\n" line in
// path order.
func assetVersions(fsys fs.FS) (map[string]string, string, error) {
	files := map[string]string{}
	var lines []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		v := shortHash(b)
		files[p] = v
		lines = append(lines, p+" "+v+"\n")
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	slices.Sort(lines)
	return files, shortHash([]byte(strings.Join(lines, ""))), nil
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// webVersions is every embedded file's version (paths from web/), and
// buildVersion the build's.
var webVersions, buildVersion = func() (map[string]string, string) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files, build, err := assetVersions(sub)
	if err != nil {
		panic(err)
	}
	return files, build
}()

// staticVersion is the version of web/static/<name>, "" when there is no
// such file.
func staticVersion(name string) string { return webVersions["static/"+name] }

// pageFuncs are the templates' asset helpers. An unknown file is an error,
// so a page naming a file that is not embedded fails to render (and its
// tests fail) instead of shipping a reference with no version.
var pageFuncs = template.FuncMap{
	"asset": func(name string) (string, error) {
		v := staticVersion(name)
		if v == "" {
			return "", fmt.Errorf("asset %q is not in web/static", name)
		}
		return "/static/" + name + "?v=" + v, nil
	},
	"kit": func(name string) (string, error) {
		if _, ok := kitFiles[name]; !ok {
			return "", fmt.Errorf("kit file %q is not served", name)
		}
		return KitRoute + name + "?v=" + buildVersion, nil
	},
}

// AssetCacheControl is what a /static/ file is served with under its
// current version, and NoCache under any other URL.
const (
	AssetCacheControl = "public, max-age=31536000, immutable"
	NoCache           = "no-cache"
)

// staticHandler serves web/static: a year for the file's current version,
// else revalidated every time.
func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cc := NoCache
		if v := r.URL.Query().Get("v"); v != "" && v == staticVersion(r.URL.Path) {
			cc = AssetCacheControl
		}
		w.Header().Set("Cache-Control", cc)
		next.ServeHTTP(w, r)
	})
}
