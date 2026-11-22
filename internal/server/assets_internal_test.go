package server

import (
	"testing"
	"testing/fstest"
)

// A file's version is its bytes' (the first 8 bytes of their SHA-256, in
// hex: the literals below are `printf one | sha256sum` and `printf two |
// sha256sum`), so a deploy that changes a file changes its URL, and one that
// does not leaves it alone. The build's version follows any file.
func TestAssetVersionsFollowTheBytes(t *testing.T) {
	before := fstest.MapFS{"static/app.js": {Data: []byte("one")}, "static/app.css": {Data: []byte("two")}}
	after := fstest.MapFS{"static/app.js": {Data: []byte("two")}, "static/app.css": {Data: []byte("two")}}
	vb, bb, err := assetVersions(before)
	if err != nil {
		t.Fatal(err)
	}
	va, ba, err := assetVersions(after)
	if err != nil {
		t.Fatal(err)
	}
	if vb["static/app.js"] != "7692c3ad3540bb80" || va["static/app.js"] != "3fc4ccfe745870e2" {
		t.Errorf("app.js versions %q before, %q after; want 7692c3ad3540bb80 (\"one\"), 3fc4ccfe745870e2 (\"two\")", vb["static/app.js"], va["static/app.js"])
	}
	if vb["static/app.css"] != "3fc4ccfe745870e2" || va["static/app.css"] != vb["static/app.css"] {
		t.Errorf("app.css (unchanged) versions %q, %q; want 3fc4ccfe745870e2 both times", vb["static/app.css"], va["static/app.css"])
	}
	if bb == ba || len(bb) != 16 {
		t.Errorf("the build's version %q before, %q after a file changed: want two different 16-hex versions", bb, ba)
	}
	// The same bytes under another path is another build.
	moved := fstest.MapFS{"static/app2.js": {Data: []byte("one")}, "static/app.css": {Data: []byte("two")}}
	if _, bm, _ := assetVersions(moved); bm == bb {
		t.Errorf("a renamed file left the build's version %q", bm)
	}
}

// A page naming a file that is not embedded does not render a bare URL: the
// helper refuses.
func TestAPageCannotNameAnAssetThatIsNotEmbedded(t *testing.T) {
	asset := pageFuncs["asset"].(func(string) (string, error))
	if u, err := asset("app.js"); err != nil || u != "/static/app.js?v="+webVersions["static/app.js"] {
		t.Fatalf("asset(app.js) = %q, %v", u, err)
	}
	if u, err := asset("nope.js"); err == nil {
		t.Fatalf("asset(nope.js) = %q, want an error", u)
	}
	kit := pageFuncs["kit"].(func(string) (string, error))
	if u, err := kit("icons.svg"); err == nil {
		t.Fatalf("kit(icons.svg) = %q, want an error", u)
	}
}
