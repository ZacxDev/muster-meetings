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
		"RIGHT-TO-LEFT OVERRIDE": "Grace‮H",
		"LEFT-TO-RIGHT ISOLATE":  "Grace⁦H",
		"POP DIRECTIONAL":        "Grace⁩",
		"RIGHT-TO-LEFT MARK":     "Grace‏",
		"ARABIC LETTER MARK":     "Grace؜",
		"ZERO WIDTH SPACE":       "Gr​ace",
		"LINE SEPARATOR":         "Grace H",
		"invalid UTF-8":          "Grace\xff",
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
		{"10.1.2.3:4000", "", trusted, "10.1.2.3"},
		{"[2001:db8:1:2:3:4:5:6]:4000", "", nil, "2001:db8:1:2::"},
	} {
		r := httptest.NewRequest("POST", "/j/x/knock", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := lobby.ClientAddr(r, c.trusted); got.String() != c.want {
			t.Errorf("peer %s, XFF %q: %s, want %s", c.peer, c.xff, got, c.want)
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
