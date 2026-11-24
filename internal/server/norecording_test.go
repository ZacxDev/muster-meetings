package server_test

// 🔴 No bucket, no change. With Config.S3 nil the plugin records nothing:
// the owner's and an admitted guest's notice and badge, the switch, admit
// and End answers, and every route the recording adds (owner-authenticated
// and not) are compared with goldens TAKEN AT THE PRE-CHANGE COMMIT
// (2e5c9c5): this file uses only what that commit had, and was run there with
// -update-norecording to write testdata/norecording.golden.json. An equality
// to the base cannot be red at the base; the mutation rows "no bucket: …"
// supply the failing case.

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
)

var updateNoRecording = flag.Bool("update-norecording", false, "rewrite testdata/norecording.golden.json (run at the pre-change commit only)")

func noRecordingSurface(t *testing.T) map[string]any {
	e := newEnv(t)
	c := e.meeting(ada)
	norm := func(a answer) answer {
		a.Body = strings.ReplaceAll(a.Body, c.ID, "{meeting}")
		return a
	}
	out := map[string]any{}
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/token", c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("owner token = %d %s", resp.StatusCode, raw)
	}
	owner := e.open("GET", "/api/meetings/"+c.ID+"/events", c.Sess, nil)
	var snap struct {
		Notice json.RawMessage `json:"notice"`
	}
	json.Unmarshal([]byte(owner.want("meeting").Data), &snap)
	out["owner_notice"] = string(snap.Notice)
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": false})
	out["ai off"] = norm(answerOf(resp, raw))
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": true})
	out["ai on"] = norm(answerOf(resp, raw))
	tk := e.knock(c.Code, grace)
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/lobby/"+e.pid(c, grace)+"/admit", c.Sess, nil)
	out["admit"] = norm(answerOf(resp, raw))
	ev := e.guestEvents(c.Code, tk)
	out["guest_meeting_event"] = ev.want("meeting").Data
	tok := strings.Repeat("A", 43)
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/settings"}, {"PUT", "/api/settings"},
		{"GET", "/api/meetings/" + c.ID + "/recording"}, {"POST", "/api/meetings/" + c.ID + "/recording-url"},
		{"POST", "/api/meetings/" + c.ID + "/share"}, {"DELETE", "/api/meetings/" + c.ID + "/share"},
		{"GET", "/api/meetings/" + c.ID + "/timeline"}, {"GET", "/api/meetings/" + c.ID + "/frame?at=1000"},
		{"GET", "/media/" + c.ID + "/1?exp=9999999999&sig=x"}, {"GET", "/r/" + tok}, {"GET", "/r/" + tok + "/1"},
	} {
		var body any
		if p.method == "PUT" {
			body = map[string]int{"recording_retention_days": 30}
		}
		resp, raw := e.do(p.method, p.path, c.Sess, body)
		out[p.method+" "+strings.ReplaceAll(p.path, c.ID, "{meeting}")+" (owner)"] = norm(answerOf(resp, raw))
		resp, raw = e.do(p.method, p.path, "", body)
		out[p.method+" "+strings.ReplaceAll(p.path, c.ID, "{meeting}")+" (no session)"] = norm(answerOf(resp, raw))
	}
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/record", c.Sess, map[string]bool{"on": false})
	out["record off"] = norm(answerOf(resp, raw))
	resp, raw = e.do("POST", "/api/meetings/"+c.ID+"/end", c.Sess, nil)
	out["end"] = norm(answerOf(resp, raw))
	return out
}

func TestNoBucketChangesNothing(t *testing.T) {
	got := noRecordingSurface(t)
	b, _ := json.MarshalIndent(got, "", "  ")
	b = append(b, '\n')
	const golden = "testdata/norecording.golden.json"
	if *updateNoRecording {
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
		t.Errorf("with no bucket the surface changed from the pre-change commit's:\ngot:\n%s\nwant (%s):\n%s", b, golden, want)
	}
}
