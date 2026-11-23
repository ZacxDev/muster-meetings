package server_test

// 🔴 No contacts URL, no change (muster's docs/design/contacts.md, "The
// guarantee: no URL, no change"). With Config.Contacts nil, what a guest
// sees at the end of a meeting, the Leave answer, and the routes the card
// would add are compared with goldens TAKEN AT THE PRE-CHANGE COMMIT
// (5e4655b): this file uses only what that commit had, and was run there
// with -update-nourl to write testdata/nourl.golden.json. An equality to the
// base cannot be red at the base; the mutation rows "no URL: …" supply the
// failing case.

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

var updateNoURL = flag.Bool("update-nourl", false, "rewrite testdata/nourl.golden.json (run at the pre-change commit only)")

type answer struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func answerOf(resp *http.Response, body []byte) answer {
	h := map[string]string{}
	var ks []string
	for k := range resp.Header {
		if k != "Date" && k != "Content-Length" {
			ks = append(ks, k)
		}
	}
	sort.Strings(ks)
	for _, k := range ks {
		h[k] = strings.Join(resp.Header.Values(k), ", ")
	}
	return answer{Status: resp.StatusCode, Headers: h, Body: string(body)}
}

// noURLSurface is everything the guarantee covers, from a server built
// exactly as newEnv builds it (no contacts).
func noURLSurface(t *testing.T) map[string]any {
	e := newEnv(t)
	c := e.meeting(ada)
	// An admitted guest who leaves; an admitted guest at End; a guest
	// still waiting at End.
	tl := e.knock(c.Code, linus)
	e.admit(c, linus)
	ta := e.knock(c.Code, grace)
	e.admit(c, grace)
	tw := e.knock(c.Code, "Wanda W.")
	ev := e.guestEvents(c.Code, ta)
	ev.want("meeting")
	wt := e.wait(c.Code, tw)
	wt.want("waiting")
	resp, raw := e.do("POST", "/j/"+c.Code+"/leave", "", map[string]string{"ticket": tl})
	leave := answerOf(resp, raw)
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("end = %d %s", resp.StatusCode, raw)
	}
	ended := ev.want("ended").Data
	waitEnded := wt.want("ended").Data
	out := map[string]any{"leave": leave, "guest_ended_event": ended, "waiting_ended_event": waitEnded}
	for _, p := range []string{"/card", "/card/skip", "/card?x=1"} {
		out["POST "+p] = try(e, "POST", p, `{"id":"`+strings.Repeat("A", 43)+`","name":"`+grace+`","email":"grace.h@example.test"}`)
	}
	out["GET /card"] = try(e, "GET", "/card", "")
	return out
}

// try is one request whose failure to answer at all is recorded as the
// answer, so a change that breaks the route shows as a golden difference.
func try(e *env, method, path, body string) answer {
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{Body: "no answer: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return answerOf(resp, raw)
}

func TestNoContactsURLChangesNothing(t *testing.T) {
	got := noURLSurface(t)
	b, _ := json.MarshalIndent(got, "", "  ")
	b = append(b, '\n')
	const golden = "testdata/nourl.golden.json"
	if *updateNoURL {
		if err := os.WriteFile(golden, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(want) {
		t.Errorf("with no contacts URL the guest's surface changed from the pre-change commit's:\ngot:\n%s\nwant (%s):\n%s", b, golden, want)
	}
}
