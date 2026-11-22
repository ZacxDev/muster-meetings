package server

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster-meetings/internal/store"
)

// 🔴 The lead log line holds marks only, whatever reaches it: the log site
// cleans the lead itself (stt.CleanLead), so a lead carrying letters (an
// engine that did not clean it; here injected straight at the log helper)
// logs as its marks, and no letter of it reaches the log.
func TestTheLeadLogCleansTheLeadItself(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{cfg: Config{Log: log.New(&buf, "", 0)}}
	s.logLead(store.Track{MeetingID: "m1", Track: "TR_1"}, "?Zanzibar x.", 100, 900, 7, store.Amended, nil)
	line := strings.TrimSpace(buf.String())
	m := regexp.MustCompile(`^transcript: lead "([^"]*)" meeting m1 track TR_1 final 100-900 ms: amended segment 7$`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the lead line %q is not the expected shape", line)
	}
	if m[1] != "?." || strings.Contains(strings.ToLower(line), "zanzibar") {
		t.Fatalf("a letter of the lead reached the log: marks %q in %q, want \"?.\"", m[1], line)
	}
}
