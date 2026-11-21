package lobby_test

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/lobby"
)

// 🔴 The guest name (design: "trimmed, 1–40 characters, control and bidi
// characters refused"). Each refused case is one class: a mutant that drops
// that class's check lets exactly its rows through.
func TestCleanNameTrimsAndRefusesControlAndBidiCharacters(t *testing.T) {
	forty := strings.Repeat("é", 40) // 40 characters, 80 bytes: counted in characters
	for in, want := range map[string]string{
		"  Grace H.  ":                   "Grace H.",
		"\tLinus\n":                      "Linus",
		forty:                            forty,
		"Ada \U0001F469\u200D\U0001F4BB": "Ada \U0001F469\u200D\U0001F4BB", // the zero-width joiner inside an emoji sequence is allowed
		"<img src=x onerror=1>":          "<img src=x onerror=1>",          // markup is just text: every page renders it as text
		"李小龙":                            "李小龙",
		"e\u0301":                        "e\u0301",   // a base and its combining mark
		"\u3164Ada":                      "\u3164Ada", // one visible character is enough
	} {
		got, err := lobby.CleanName(in)
		if err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for name, in := range map[string]string{
		"empty":                  "",
		"spaces only":            "   ",
		"41 characters":          forty + "x",
		"a NUL":                  "Gra\x00ce",
		"a newline inside":       "Grace\nHopper",
		"DEL":                    "Grace\x7f",
		"a C1 control":           "Gra\u0085ce",
		"RIGHT-TO-LEFT OVERRIDE": "Grace\u202EH",
		"LEFT-TO-RIGHT ISOLATE":  "Grace\u2066H",
		"POP DIRECTIONAL":        "Grace\u2069",
		"RIGHT-TO-LEFT MARK":     "Grace\u200F",
		"ARABIC LETTER MARK":     "Grace\u061C",
		"ZERO WIDTH SPACE":       "Gr\u200Bace",
		"LINE SEPARATOR":         "Grace\u2028H",
		"invalid UTF-8":          "Grace\xff",
		// Nothing visible (the audit's cases): blank in the owner's lobby.
		"only zero-width joiners":           "\u200D\u200D\u200D",
		"HANGUL FILLER U+3164":              "\u3164\u3164",
		"HANGUL CHOSEONG FILLER U+115F":     "\u115F",
		"HANGUL JUNGSEONG FILLER U+1160":    "\u1160",
		"HALFWIDTH HANGUL FILLER U+FFA0":    "\uFFA0",
		"BRAILLE PATTERN BLANK U+2800":      "\u2800\u2800\u2800",
		"combining marks with no base":      "\u0301\u0308",
		"a space then a combining mark":     " \u0301",
		"COMBINING GRAPHEME JOINER":         "\u034F",
		"a variation selector alone":        "\uFE0F",
		"an ideographic space and a filler": "\u3000\u3164",
		"fillers around a joiner":           "\u3164\u200D\u3164",
	} {
		if got, err := lobby.CleanName(in); err == nil {
			t.Errorf("%s: CleanName(%q) = %q, want refused", name, in, got)
		}
	}
}

// The notice's words, pinned whole: what a guest reads for each switch
// state, and the badge.
func TestNoticeForEachSwitchState(t *testing.T) {
	for _, c := range []struct {
		record, ai bool
		text       string
		badge      string
	}{
		{true, true, "This meeting is recorded and transcribed. | The transcript text (not the audio) is sent to an AI service (OpenRouter) to find action items.", "Recording · Transcribing · AI"},
		{false, true, "This meeting is transcribed. | The transcript text (not the audio) is sent to an AI service (OpenRouter) to find action items.", "Transcribing · AI"},
		{true, false, "This meeting is recorded and transcribed.", "Recording · Transcribing"},
		{false, false, "This meeting is transcribed.", "Transcribing"},
	} {
		n := lobby.NoticeFor(c.record, c.ai)
		if got := strings.Join(n.Text, " | "); got != c.text || n.Badge != c.badge || n.Record != c.record || n.AI != c.ai {
			t.Errorf("NoticeFor(%v, %v) = %q / %q", c.record, c.ai, got, n.Badge)
		}
	}
}

// 🔴 "the 11th knock from one address in 10 minutes is 429; the 10th
// passes". Two addresses are counted apart, and the window slides.
func TestTheLimiterAllowsTenKnocksPerAddressPerTenMinutes(t *testing.T) {
	l := lobby.NewLimiter()
	a, b := netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("203.0.113.9")
	t0 := time.Date(2031, 3, 14, 15, 9, 26, 0, time.UTC)
	for i := 1; i <= 10; i++ {
		if !l.Allow(a, t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("knock %d from one address refused", i)
		}
	}
	if l.Allow(a, t0.Add(11*time.Second)) {
		t.Fatal("the 11th knock from one address in 10 minutes was allowed")
	}
	if !l.Allow(b, t0.Add(11*time.Second)) {
		t.Fatal("another address's first knock was refused")
	}
	if !l.Allow(a, t0.Add(10*time.Minute+2*time.Second)) {
		t.Fatal("a knock after the first left the window was refused")
	}
}

// The address a knock is counted against: the peer, unless the peer is a
// trusted proxy, then the first untrusted X-Forwarded-For hop from the
// right. An IPv6 client counts as its /64.
func TestClientAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		peer, xff string
		trusted   []netip.Prefix
		want      string
	}{
		{"198.51.100.7:4000", "", nil, "198.51.100.7"},
		{"198.51.100.7:4000", "203.0.113.9", nil, "198.51.100.7"},         // an untrusted peer's header is ignored
		{"10.1.2.3:4000", "203.0.113.9", trusted, "203.0.113.9"},          // the ingress's word
		{"10.1.2.3:4000", "6.6.6.6, 203.0.113.9", trusted, "203.0.113.9"}, // a forged left-most hop is not believed
		{"10.1.2.3:4000", "203.0.113.9, 10.9.9.9", trusted, "203.0.113.9"},
		// The right-most hop that is not a trusted proxy, never the left-most
		// past an untrusted hop: 198.51.100.1 is the client's own claim.
		{"10.1.2.3:4000", "198.51.100.1, 203.0.113.9, 10.9.9.9", trusted, "203.0.113.9"},
		{"10.1.2.3:4000", "", trusted, "10.1.2.3"},
		{"[2001:db8:1:2:3:4:5:6]:4000", "", nil, "2001:db8:1:2::"},
	} {
		r := httptest.NewRequest("POST", "/j/x/knock", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := lobby.ClientAddr(r, c.trusted, ""); got.String() != c.want {
			t.Errorf("peer %s, XFF %q: %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

// 🔴 MEETINGS_CLIENT_IP_HEADER: the header's one address, read only from a
// trusted peer; X-Forwarded-For is then not read. A header from an
// untrusted peer is ignored; a missing, repeated, listed or unparseable one
// leaves the peer.
func TestClientAddrWithAClientIPHeader(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	const hdr = "CF-Connecting-IP"
	for _, c := range []struct {
		why, peer string
		values    []string
		xff       string
		trusted   []netip.Prefix
		want      string
	}{
		{"untrusted peer spoofs the header", "198.51.100.7:4000", []string{"203.0.113.9"}, "", trusted, "198.51.100.7"},
		{"no trusted proxies at all", "198.51.100.8:4000", []string{"203.0.113.9"}, "", nil, "198.51.100.8"},
		{"trusted peer: the header, not X-Forwarded-For", "10.1.2.3:4000", []string{"203.0.113.9"}, "192.0.2.44", trusted, "203.0.113.9"},
		{"trusted peer: no header leaves the peer, X-Forwarded-For unread", "10.1.2.4:4000", nil, "192.0.2.45", trusted, "10.1.2.4"},
		{"trusted peer: a list leaves the peer", "10.1.2.5:4000", []string{"203.0.113.10, 192.0.2.46"}, "", trusted, "10.1.2.5"},
		{"trusted peer: a repeated header leaves the peer", "10.1.2.6:4000", []string{"203.0.113.50", "203.0.113.51"}, "", trusted, "10.1.2.6"},
		{"trusted peer: not an address leaves the peer", "10.1.2.7:4000", []string{"unknown"}, "", trusted, "10.1.2.7"},
		{"trusted peer: IPv6 cut to its /64", "10.1.2.8:4000", []string{"2001:db8:7:8:1:2:3:4"}, "", trusted, "2001:db8:7:8::"},
		{"trusted peer: a mapped IPv4", "10.1.2.9:4000", []string{" ::ffff:203.0.113.77 "}, "", trusted, "203.0.113.77"},
	} {
		r := httptest.NewRequest("POST", "/j/x/knock", nil)
		r.RemoteAddr = c.peer
		for _, v := range c.values {
			r.Header.Add("cf-connecting-ip", v) // the configured name is matched case-insensitively
		}
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := lobby.ClientAddr(r, c.trusted, hdr); got.String() != c.want {
			t.Errorf("%s: peer %s, %s %q: %s, want %s", c.why, c.peer, hdr, c.values, got, c.want)
		}
	}
}

// The hub counts owner streams per meeting; a guest's stream is not one.
func TestTheHubKnowsWhenAnOwnerIsWatching(t *testing.T) {
	h := lobby.NewHub()
	var opened []string
	h.OnOwnerWatching(func(id string) { opened = append(opened, id) })
	wake, stopGuest := h.Watch("m1", false)
	if h.OwnerWatching("m1") {
		t.Fatal("a guest stream counted as the owner")
	}
	_, stopOwner := h.Watch("m1", true)
	if !h.OwnerWatching("m1") || h.OwnerWatching("m2") || len(opened) != 1 || opened[0] != "m1" {
		t.Fatalf("owner watching m1 = %v, m2 = %v, opened %v", h.OwnerWatching("m1"), h.OwnerWatching("m2"), opened)
	}
	h.Changed("m1")
	select {
	case <-wake:
	default:
		t.Fatal("Changed did not wake the guest stream")
	}
	stopOwner()
	stopOwner() // twice is harmless
	if h.OwnerWatching("m1") {
		t.Fatal("the owner still counts after the stream closed")
	}
	stopGuest()
}
