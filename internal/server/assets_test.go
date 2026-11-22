package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every asset a page references carries a version (internal/server
// assets.go). Behind a CDN that hands the browser `max-age=14400` whatever
// the origin said, an unversioned /static/app.js kept an owner on the old
// script for up to four hours after a deploy. So: every /static/ URL in every
// page is `?v=` the first 8 bytes of its file's SHA-256, in hex (pinned here
// from the file on disk, not from the server's own table); every /static/
// response is `no-cache`, versioned or not; the kit (/kit/v1/, proxied from
// muster and re-themed at run time) is `?v=` the build's version and is
// `no-cache` too; and the pages themselves are never cached.

// fileVersion is a static file's version as the pages must name it.
func fileVersion(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("web", "static", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("a page references /static/%s, which is not in web/static: %v", rel, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// buildVersion is the version of every file under web/ together: each
// file's path (from web/, slash-separated) and its own version, one per line,
// in path order, hashed the same way.
func buildVersion(t *testing.T) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir("web", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("web", p)
		sum := sha256.Sum256(b)
		lines = append(lines, filepath.ToSlash(rel)+" "+hex.EncodeToString(sum[:8])+"\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:8])
}

var (
	// attrURL is every src= and href= in a page.
	attrURL      = regexp.MustCompile(`\s(?:src|href)="([^"]*)"`)
	versionedURL = regexp.MustCompile(`^/(static|kit/v1)/([A-Za-z0-9._/-]+)\?v=([0-9a-f]{16})$`)
)

// servedPages is every page the server renders, with the template it comes
// from: the owner's app (from muster's frame), /enter's refusal (the same
// template's other branch), the guest's join page, and its refusal.
func servedPages(t *testing.T, e *env) map[string]struct {
	tmpl string
	path string
	body []byte
	cc   string
} {
	t.Helper()
	out := map[string]struct {
		tmpl string
		path string
		body []byte
		cc   string
	}{}
	add := func(name, tmpl, path string, status int, framed bool, want string) {
		var hdr []string
		if framed {
			hdr = inFrame
		}
		resp, body := e.do("GET", path, "", nil, hdr...)
		if resp.StatusCode != status || !strings.Contains(string(body), want) {
			t.Fatalf("%s (%s) = %d, want %d holding %q: %s", name, path, resp.StatusCode, status, want, body)
		}
		out[name] = struct {
			tmpl string
			path string
			body []byte
			cc   string
		}{tmpl, path, body, resp.Header.Get("Cache-Control")}
	}
	add("the owner's app", "app.html", enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID), 200, true, `data-session="mmo_`)
	add("/enter's refusal", "app.html", "/enter", 400, true, `id="message"`)
	c := e.meeting(bea)
	add("the guest's join page", "guest.html", "/j/"+c.Code, 200, false, `id="knock-form"`)
	add("the guest's refusal", "guest.html", "/j/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 404, false, `id="message"`)
	return out
}

func TestEveryAssetAPageReferencesCarriesItsVersion(t *testing.T) {
	e := newEnv(t)
	pages := servedPages(t, e)

	// The ledger: every template under web/ is rendered above (a new page
	// must be added here, or this fails).
	tmpls, _ := filepath.Glob(filepath.Join("web", "*.html"))
	var want, got []string
	for _, p := range tmpls {
		want = append(want, filepath.Base(p))
	}
	for _, p := range pages {
		if !slices.Contains(got, p.tmpl) {
			got = append(got, p.tmpl)
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the pages rendered come from templates %q; web/ holds %q", got, want)
	}

	build := buildVersion(t)
	for name, p := range pages {
		refs := attrURL.FindAllSubmatch(p.body, -1)
		n := 0
		for _, m := range refs {
			u := string(m[1])
			if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") {
				continue // not a file on this origin (none is, today)
			}
			n++
			v := versionedURL.FindStringSubmatch(u)
			if v == nil {
				t.Errorf("%s: %s is not versioned", name, u)
				continue
			}
			switch v[1] {
			case "static":
				if wantV := fileVersion(t, v[2]); v[3] != wantV {
					t.Errorf("%s: %s, want version %s (the file's bytes)", name, u, wantV)
				}
			case "kit/v1":
				if v[3] != build {
					t.Errorf("%s: %s, want the build's version %s", name, u, build)
				}
			}
		}
		// Each page links kit.css and app.css; the two working pages load
		// six scripts as well (TestTheAppPageCarriesNoInlineScript).
		min := 2
		if !strings.Contains(name, "refusal") {
			min = 8
		}
		if n < min {
			t.Errorf("%s: %d references to files on this origin, want at least %d (the scan sees nothing?)", name, n, min)
		}
	}
}

// The scan's own control: an unversioned reference, a wrong version and a
// version on the wrong file are each seen.
func TestTheAssetScanSeesAnUnversionedReference(t *testing.T) {
	appJS := fileVersion(t, "app.js")
	for _, c := range []struct {
		html string
		ok   bool
	}{
		{`<script src="/static/app.js?v=` + appJS + `"></script>`, true},
		{`<script src="/static/app.js"></script>`, false},
		{`<link rel="stylesheet" href="/static/app.css?v=">`, false},
		{`<script src="/static/call.js?v=` + appJS + `"></script>`, false},
	} {
		m := attrURL.FindStringSubmatch(c.html)
		if m == nil {
			t.Fatalf("no reference seen in %s", c.html)
		}
		v := versionedURL.FindStringSubmatch(m[1])
		ok := v != nil && v[3] == fileVersion(t, v[2])
		if ok != c.ok {
			t.Errorf("%s: accepted %v, want %v", c.html, ok, c.ok)
		}
	}
}

func TestPagesAreNeverCachedAndAssetsAreRevalidated(t *testing.T) {
	e := newEnv(t)
	for name, p := range servedPages(t, e) {
		if !(p.cc == "no-store" || p.cc == "no-cache") || strings.Contains(p.cc, "max-age") {
			t.Errorf("%s: Cache-Control %q, want no-store or no-cache (a page is never reused unchecked)", name, p.cc)
		}
	}
	pages := servedPages(t, e)
	seen := 0
	for _, m := range attrURL.FindAllSubmatch(pages["the guest's join page"].body, -1) {
		u := string(m[1])
		bare, _, _ := strings.Cut(u, "?")
		urls := []string{u}
		if strings.HasPrefix(u, "/static/") {
			seen++
			// The same file unversioned, or under another version.
			urls = append(urls, bare, bare+"?v=0000000000000000")
		}
		for _, x := range urls {
			resp, body := e.do("GET", x, "", nil)
			if cc := resp.Header.Get("Cache-Control"); resp.StatusCode != 200 || len(body) == 0 || cc != "no-cache" {
				t.Errorf("%s = %d (%d bytes), Cache-Control %q, want 200 no-cache", x, resp.StatusCode, len(body), cc)
			}
		}
	}
	if seen < 6 {
		t.Fatalf("%d versioned /static/ files fetched from the guest page, want at least 6", seen)
	}
}
