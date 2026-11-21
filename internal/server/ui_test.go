package server_test

import (
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
