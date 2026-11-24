package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"

	"github.com/ZacxDev/muster-meetings/internal/lobby"
)

// Asset versions. Every file a page references names a version in its URL,
// so a deploy is a new URL and no browser or CDN can keep running the old
// script: a CDN in front of the plugin may hand the browser its own
// `max-age` (four hours, measured) whatever the origin sent, and an
// unversioned /static/app.js then outlived a deploy.
//
//   - /static/<file>?v=<the file's version>: the first 8 bytes of the file's
//     SHA-256, in hex, computed once from the embedded bytes. Every
//     /static/ response is `no-cache`, whatever v says: the version is what
//     makes a deploy a new URL (a cache that ignores `no-cache` still keys
//     on the URL); the server does not promise more.
//   - /kit/v1/<file>?v=<the build's version>: the kit is proxied from
//     muster and re-themed while the plugin runs (kit.go), so its bytes are
//     not known when a page is drawn. It carries the build's version (every
//     embedded file's, together: the bundled kit is among them), which
//     changes on a deploy of THIS plugin only, and stays `no-cache` + ETag.
//     🔴 So behind a CDN that overrides the origin's `no-cache` with its own
//     browser cache TTL (measured: `max-age=14400` on /kit/v1/kit.css and
//     kit.js), a muster re-theme or a new muster kit.js can reach a browser
//     up to that TTL late: the URL does not move for it. The fix is at the
//     CDN, a rule for /kit/v1/* that respects the origin's headers, not
//     here.
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

// staticHandler serves web/static, always revalidated (`no-cache`); the
// pages' ?v= is what keeps a browser off an old file after a deploy. The
// file's version is its strong ETag (the embedded files carry no
// modification time, so without it every revalidation was a full download).
// The file server (http.ServeContent) answers an If-None-Match naming it
// with a 304 and no body, and any other with the file.
func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		if v := staticVersion(r.URL.Path); v != "" {
			h.Set("ETag", `"`+v+`"`)
		}
		next.ServeHTTP(w, r)
	})
}

// pageSamples is what each page template is drawn with at startup
// (checkPages): every branch that names an asset. A template under web/
// with no samples here fails the check, so a new page cannot skip it.
var pageSamples = map[string][]any{
	"app.html":   {enterPage{Session: "mmo_sample", Ref: "sample"}, enterPage{Message: "sample", Again: true}},
	"guest.html": {guestPage{OwnerName: "sample", Notice: lobby.Notice{Record: true, AI: true, Transcribed: true, Text: []string{"sample"}, Badge: "sample"}}, guestPage{Message: "sample"}},
	"share.html": {sharePage{Missing: true}, sharePage{Title: "sample", When: "sample", Until: "until sample", Many: true,
		Parts: []sharePart{{Label: "sample", Src: "/r/sample/1"}}, Gaps: []string{"sample"}}},
}

// checkPages draws every page of t with its samples and answers the first
// error: a page that cannot render (a file that is not embedded, a field
// that does not exist) fails the plugin's start, not a guest's request,
// where the error would come after the 200 and truncate the page.
func checkPages(t *template.Template) error {
	n := 0
	for _, p := range t.Templates() {
		name := p.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		samples, ok := pageSamples[name]
		if !ok {
			return fmt.Errorf("page %s has no samples in pageSamples", name)
		}
		for _, d := range samples {
			if err := t.ExecuteTemplate(io.Discard, name, d); err != nil {
				return fmt.Errorf("page %s: %w", name, err)
			}
		}
		n++
	}
	if n == 0 {
		return errors.New("no page templates")
	}
	return nil
}

// The pages are checked when the plugin starts: it does not start with one
// that cannot render.
var _ = func() bool {
	if err := checkPages(pageTmpl); err != nil {
		panic("meetings: " + err.Error())
	}
	return true
}()
