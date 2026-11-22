package server_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cssNamedColours is every CSS named colour (CSS Color 4), so a value of
// `red` is caught as surely as `#f00`. transparent and currentColor are not
// hues and stay allowed.
var cssNamedColours = strings.Fields(`aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue
blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk crimson cyan darkblue darkcyan
darkgoldenrod darkgray darkgreen darkgrey darkkhaki darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon
darkseagreen darkslateblue darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey
dodgerblue firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow grey
honeydew hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral
lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen lightskyblue
lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen magenta maroon mediumaquamarine
mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue mediumspringgreen mediumturquoise
mediumvioletred midnightblue mintcream mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered
orchid palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum powderblue purple
rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown seagreen seashell sienna silver skyblue slateblue
slategray slategrey snow springgreen steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow
yellowgreen`)

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssDecl    = regexp.MustCompile(`([a-zA-Z-]+)\s*:\s*([^;{}]+)`)
	hexColour  = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	// A colour function on literal numbers: rgb(0 0 0), hsl(…), oklch(…).
	// rgb(var(--mu-…)) — a token — is what the plugin writes instead.
	fnColour = regexp.MustCompile(`\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(\s*[0-9.]`)
	word     = regexp.MustCompile(`[a-zA-Z]+`)
)

// colourLiterals lists every colour written in css that is not a kit token:
// a hex, a colour function on numbers, or a named colour, in any
// declaration's value (a custom property's own definition, --mu-x: …, is the
// token itself and is skipped).
func colourLiterals(css string) []string {
	css = cssComment.ReplaceAllString(css, "")
	named := map[string]bool{}
	for _, n := range cssNamedColours {
		named[n] = true
	}
	var out []string
	for _, m := range cssDecl.FindAllStringSubmatch(css, -1) {
		prop, val := m[1], m[2]
		if strings.HasPrefix(prop, "--") {
			continue
		}
		if hexColour.MatchString(val) || fnColour.MatchString(val) {
			out = append(out, prop+": "+strings.TrimSpace(val))
			continue
		}
		for _, w := range word.FindAllString(val, -1) {
			if named[strings.ToLower(w)] {
				out = append(out, prop+": "+strings.TrimSpace(val))
				break
			}
		}
	}
	return out
}

// 🔴 The plugin's CSS writes no colour: every colour is a kit token, so the
// install's theme (docs/ui-kit.md, "The theme") is the plugin's. The
// fallback (kit-fallback.css, muster's own render) is held to the same rule:
// its colours are token definitions only.
func TestThePluginCSSWritesNoColour(t *testing.T) {
	// The scan's own controls: it must see each kind of literal, and pass
	// the token form.
	for bad, want := range map[string]int{
		`a{color:#fff}`:                           1,
		`a { background: rgb(12 34 56 / .5); }`:   1,
		`a { box-shadow: 0 0 0 1px hsl(0 0% 0%)}`: 1,
		`a { border: 1px solid Red; }`:            1,
		`a { color: rgb(var(--mu-text)); background: rgb(var(--mu-s1) / .7); fill: currentColor; border-color: transparent }`: 0,
		`:root { --mu-bg: 17 15 28; }`:                    0,
		`/* #fff red */ a { color: rgb(var(--mu-text)) }`: 0,
	} {
		if got := colourLiterals(bad); len(got) != want {
			t.Fatalf("the scan found %d colours in %q (%v), want %d: it proves nothing", len(got), bad, got, want)
		}
	}
	files, _ := filepath.Glob(filepath.Join("web", "static", "*.css"))
	if len(files) < 2 {
		t.Fatalf("found %d stylesheets (want app.css and kit-fallback.css): this scan would prove nothing", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range colourLiterals(string(b)) {
			t.Errorf("%s writes a colour, not a kit token: %s", f, c)
		}
	}
	// No script writes one either (the pages set styles only through
	// classes and the meter's --level).
	scripts, _ := filepath.Glob(filepath.Join("web", "static", "*.js"))
	for _, f := range scripts {
		b, _ := os.ReadFile(f)
		if m := regexp.MustCompile(`['"]#[0-9a-fA-F]{3,8}['"]|\b(rgba?|hsla?)\(\s*[0-9]`).Find(b); m != nil {
			t.Errorf("%s writes a colour: %s", f, m)
		}
	}
}

// 🔴 Only prefs.js touches the browser's storage, and it writes one key with
// five device fields: the session token, a ticket, a name or a link never
// reach storage (the owner's remembered device check, static/prefs.js).
func TestOnlyPrefsTouchesStorage(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("web", "static", "*.js"))
	if len(files) < 5 {
		t.Fatalf("found %d scripts: this scan would prove nothing", len(files))
	}
	sawPrefs := false
	for _, f := range files {
		b, _ := os.ReadFile(f)
		s := string(b)
		if filepath.Base(f) == "prefs.js" {
			sawPrefs = true
			if n := strings.Count(s, "setItem("); n != 1 {
				t.Errorf("prefs.js calls setItem %d times, want once", n)
			}
			const stored = `JSON.stringify({ v: 1, mic: !!c.mic, cam: !!c.cam, micId: String(c.micId || ''), camId: String(c.camId || '') })`
			if !strings.Contains(s, "setItem(KEY, "+stored+")") {
				t.Errorf("prefs.js stores something other than the five device fields under KEY")
			}
			for _, bad := range []string{"sessionStorage", "indexedDB", "document.cookie", "token", "ticket"} {
				if strings.Contains(s, bad) {
					t.Errorf("prefs.js mentions %s", bad)
				}
			}
			continue
		}
		for _, bad := range []string{"localStorage", "sessionStorage", "indexedDB", "document.cookie", "setItem("} {
			if strings.Contains(s, bad) {
				t.Errorf("%s uses %s: only prefs.js may touch storage", f, bad)
			}
		}
	}
	if !sawPrefs {
		t.Fatal("no prefs.js: the scan saw the wrong directory")
	}
}

func mustRead(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 🔴 No page makes the browser contact muster: neither page's HTML names
// muster's origin (the owner page's frame-ancestors aside, which is a
// header, not a request), and both CSPs allow script, style and images from
// 'self' only. The kit is /kit/v1/ on this origin, and it is served (the
// bundled kit here: these tests run with no kit upstream).
func TestNoPageMakesTheBrowserContactMuster(t *testing.T) {
	e := newEnv(t)
	resp, owner := e.framed(enterPath(e.fake.MintApp(app.PluginID, ada.ID, ""), app.PluginID))
	ownerCSP := resp.Header.Get("Content-Security-Policy")
	c := e.meeting(bea)
	resp, guest := e.do("GET", "/j/"+c.Code, "", nil)
	guestCSP := resp.Header.Get("Content-Security-Policy")
	if !bytes.Contains(guest, []byte(`id="knock-form"`)) || !sessionAttr.Match(owner) {
		t.Fatal("not the two pages")
	}
	for name, p := range map[string]struct {
		body []byte
		csp  string
	}{"guest": {guest, guestCSP}, "owner": {owner, ownerCSP}} {
		if bytes.Contains(p.body, []byte(musterOrigin)) || bytes.Contains(p.body, []byte("/ui-kit/")) {
			t.Errorf("the %s page's HTML references muster's origin or its kit path", name)
		}
		for _, d := range []string{"script-src", "style-src", "img-src"} {
			if got := directive(p.csp, d); got != "'self'" {
				t.Errorf("the %s page's %s = %q, want 'self' only", name, d, got)
			}
		}
		if name == "guest" && strings.Contains(p.csp, musterOrigin) {
			t.Errorf("the guest page's CSP names muster's origin: %s", p.csp)
		}
	}
	for path, ctype := range map[string]string{"/kit/v1/kit.css": "text/css", "/kit/v1/kit.js": "text/javascript"} {
		resp, body := e.do("GET", path, "", nil)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) || len(body) < 1000 || resp.Header.Get("X-Kit-Source") != "bundled" {
			t.Errorf("%s = %d %q (%d bytes, source %q)", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body), resp.Header.Get("X-Kit-Source"))
		}
	}
	if resp, _ := e.do("GET", "/kit/v1/icons.svg", "", nil); resp.StatusCode != 404 {
		t.Errorf("/kit/v1/icons.svg = %d, want 404 (only kit.css and kit.js are served)", resp.StatusCode)
	}
}
