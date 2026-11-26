package server_test

// The MCP server (mcp.go, mcp_tools.go), end to end through POST /mcp: the
// door (every credential but a live mmc_ token refused alike, failures
// limited per address), the protocol, each tool's schema, every tool
// scoped to its token's owner, each tool's happy path over the same code
// the owner app runs, the owner's own action items through the model
// path's guards and redraft, the send gate, and a log that carries no word
// of a meeting.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/openroutertest"
	"github.com/ZacxDev/muster-meetings/internal/server"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// mintMCP is Create token in settings: it answers the token, checking the
// command it is shown in.
func (e *env) mintMCP(sess string) (tok, id string) {
	e.t.Helper()
	resp, raw := e.do("POST", "/api/settings/mcp", sess, nil)
	var v struct {
		Token, ID, Command, URL string
	}
	if resp.StatusCode != 201 || json.Unmarshal(raw, &v) != nil {
		e.t.Fatalf("mint = %d %s", resp.StatusCode, raw)
	}
	want := `claude mcp add --transport http meetings ` + publicURL + `/mcp --header "Authorization: Bearer ` + v.Token + `"`
	if !regexp.MustCompile(`^mmc_[A-Za-z0-9_-]{43}$`).MatchString(v.Token) || v.Command != want || v.URL != publicURL+"/mcp" {
		e.t.Fatalf("mint = %s; want the command %s", raw, want)
	}
	return v.Token, v.ID
}

type rpcAns struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc posts one JSON-RPC request to /mcp with bearer tok.
func (e *env) rpc(tok, method string, params any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 7, "method": method}
	if params != nil {
		body["params"] = params
	}
	return e.do("POST", "/mcp", tok, body, hdr...)
}

type toolAns struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (a toolAns) text() string {
	var out []string
	for _, c := range a.Content {
		if c.Type == "text" {
			out = append(out, c.Text)
		}
	}
	return strings.Join(out, "\n")
}

// tool calls a tool; it fails the test unless the call itself (not the
// tool) succeeded.
func (e *env) tool(tok, name string, args any) toolAns {
	e.t.Helper()
	resp, raw := e.rpc(tok, "tools/call", map[string]any{"name": name, "arguments": args})
	var r rpcAns
	if resp.StatusCode != 200 || json.Unmarshal(raw, &r) != nil || r.Error != nil {
		e.t.Fatalf("tools/call %s = %d %s", name, resp.StatusCode, raw)
	}
	var a toolAns
	if err := json.Unmarshal(r.Result, &a); err != nil {
		e.t.Fatalf("tools/call %s: %v: %s", name, err, r.Result)
	}
	return a
}

// ok is a tool's successful JSON answer, decoded into v.
func (e *env) ok(tok, name string, args, v any) string {
	e.t.Helper()
	a := e.tool(tok, name, args)
	if a.IsError {
		e.t.Fatalf("%s(%v) failed: %s", name, args, a.text())
	}
	if v != nil {
		if err := json.Unmarshal([]byte(a.text()), v); err != nil {
			e.t.Fatalf("%s: %v: %s", name, err, a.text())
		}
	}
	return a.text()
}

// toolNames is tools/list's names, sorted.
func (e *env) toolNames(tok string) ([]string, []map[string]any) {
	e.t.Helper()
	resp, raw := e.rpc(tok, "tools/list", nil)
	var r rpcAns
	if resp.StatusCode != 200 || json.Unmarshal(raw, &r) != nil || r.Error != nil {
		e.t.Fatalf("tools/list = %d %s", resp.StatusCode, raw)
	}
	var v struct {
		Tools []map[string]any `json:"tools"`
	}
	json.Unmarshal(r.Result, &v)
	var names []string
	for _, t := range v.Tools {
		names = append(names, t["name"].(string))
	}
	sort.Strings(names)
	return names, v.Tools
}

// 🔴 The door: no header, a token of the right shape that is no one's, an
// owner session (mmo_), an agent read token (mmr_), a revoked token, and
// any token once the app is uninstalled are all the SAME 401 with no
// detail; only a live mmc_ token opens /mcp. And an mmc_ token opens no
// owner-session route.
func TestMCPDoorRefusesEverythingButALiveMCPToken(t *testing.T) {
	e := pcEnv(t, false)
	c := e.meeting(ada)
	tok, _ := e.mintMCP(c.Sess)
	revoked, rid := e.mintMCP(c.Sess)
	if resp, raw := e.do("DELETE", "/api/settings/mcp/"+rid, c.Sess, nil); resp.StatusCode != 200 {
		t.Fatalf("revoke = %d %s", resp.StatusCode, raw)
	}
	resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/agent-token", c.Sess, nil)
	var at struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != 201 || json.Unmarshal(raw, &at) != nil || !strings.HasPrefix(at.Token, "mmr_") {
		t.Fatalf("agent token = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.rpc(tok, "tools/list", nil); resp.StatusCode != 200 {
		t.Fatalf("positive control: a live token = %d", resp.StatusCode)
	}
	var first *answer
	for name, bearer := range map[string]string{
		"no header":         "",
		"no one's token":    "mmc_" + strings.Repeat("Q", 43),
		"an owner session":  c.Sess,
		"an agent token":    at.Token,
		"a revoked token":   revoked,
		"a token's tail":    strings.TrimPrefix(tok, "mmc_"),
		"another prefix":    "mmo_" + strings.TrimPrefix(tok, "mmc_"),
		"a malformed token": tok + "x",
	} {
		resp, raw := e.rpc(bearer, "tools/list", nil)
		a := answerOf(resp, raw)
		if resp.StatusCode != 401 {
			t.Errorf("%s = %d %s, want 401", name, resp.StatusCode, raw)
			continue
		}
		if first == nil {
			first = &a
		} else if a.Body != first.Body || !sameHeaders(a.Headers, first.Headers) {
			t.Errorf("%s answered %+v; every refusal must be %+v", name, a, *first)
		}
	}
	if first != nil && strings.TrimSpace(first.Body) != `{"error":"unauthorized"}` {
		t.Errorf("the refusal says %s: want no detail", first.Body)
	}
	// An mmc_ token is no owner session.
	for _, p := range []struct{ method, path string }{{"GET", "/api/meetings"}, {"GET", "/api/meetings/" + c.ID}, {"GET", "/api/settings/mcp"}, {"POST", "/api/settings/mcp"}, {"GET", "/api/meetings/" + c.ID + "/drafts"}} {
		if resp, raw := e.do(p.method, p.path, tok, nil); resp.StatusCode != 401 {
			t.Errorf("%s %s with an MCP token = %d %s, want 401", p.method, p.path, resp.StatusCode, raw)
		}
	}
	// The app uninstalled: every token is revoked in the database, and the
	// door refuses even a token whose row survived (the app's own word that
	// it is dead decides, as for an owner session).
	e.fake.Uninstall(app.PluginID)
	e.app.PollOnce(context.Background())
	if resp, raw := e.rpc(tok, "tools/list", nil); resp.StatusCode != 401 {
		t.Errorf("a token after the uninstall = %d %s, want 401", resp.StatusCode, raw)
	}
	db, err := sql.Open("sqlite", "file:"+e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var live int
	if err := db.QueryRow(`SELECT count(*) FROM mcp_tokens WHERE revoked_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Errorf("the uninstall left %d tokens unrevoked (%v)", live, err)
	}
	if _, err := db.Exec(`UPDATE mcp_tokens SET revoked_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if resp, raw := e.rpc(tok, "tools/list", nil); resp.StatusCode != 401 {
		t.Errorf("a token whose row survived the uninstall = %d %s, want 401", resp.StatusCode, raw)
	}
}

// 🔴 Failed attempts are counted per client address: past the limit the
// door answers 429 (even a live token from that address is fine to refuse:
// it is a guesser's address); another address is untouched.
func TestMCPFailedAuthenticationIsLimitedPerAddress(t *testing.T) {
	e := newEnv(t)
	sess := e.enter(ada)
	tok, _ := e.mintMCP(sess)
	bad := "mmc_" + strings.Repeat("Z", 43)
	for i := 0; i < server.MCPFailsPerAddress; i++ {
		if resp, _ := e.rpc(bad, "ping", nil, "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 401 {
			t.Fatalf("failure %d = %d, want 401", i+1, resp.StatusCode)
		}
	}
	if resp, _ := e.rpc(bad, "ping", nil, "X-Forwarded-For", "203.0.113.9"); resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("failure %d = %d, want 429 with Retry-After", server.MCPFailsPerAddress+1, resp.StatusCode)
	}
	if resp, _ := e.rpc(bad, "ping", nil, "X-Forwarded-For", "203.0.113.10"); resp.StatusCode != 401 {
		t.Errorf("another address = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.rpc(tok, "ping", nil, "X-Forwarded-For", "203.0.113.10"); resp.StatusCode != 200 {
		t.Errorf("a live token = %d, want 200", resp.StatusCode)
	}
}

// The protocol: initialize negotiates the revision, a notification is 202
// with no body, ping answers, an unknown method and tool are JSON-RPC
// errors, GET is 405 (no stream), and a page on another origin is refused.
func TestMCPProtocol(t *testing.T) {
	e := newEnv(t)
	tok, _ := e.mintMCP(e.enter(ada))
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2024-11-05": "2024-11-05", "1999-01-01": "2025-11-25", "": "2025-11-25"} {
		resp, raw := e.rpc(tok, "initialize", map[string]any{"protocolVersion": asked, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": "2"}})
		var r rpcAns
		json.Unmarshal(raw, &r)
		var v struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct{ Name string } `json:"serverInfo"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(r.Result, &v) != nil || v.ProtocolVersion != want || v.Capabilities.Tools == nil || v.ServerInfo.Name != "muster-meetings" || string(r.ID) != "7" {
			t.Errorf("initialize %q = %d %s; want %s", asked, resp.StatusCode, raw, want)
		}
	}
	resp, raw := e.do("POST", "/mcp", tok, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if resp.StatusCode != 202 || len(raw) != 0 {
		t.Errorf("a notification = %d %q, want 202 and no body", resp.StatusCode, raw)
	}
	if resp, raw := e.rpc(tok, "ping", nil); resp.StatusCode != 200 || !strings.Contains(string(raw), `"result":{}`) {
		t.Errorf("ping = %d %s", resp.StatusCode, raw)
	}
	for _, c := range []struct {
		method string
		params any
		code   int
	}{{"resources/list", nil, -32601}, {"tools/call", map[string]any{"name": "meetings_delete_all"}, -32602}, {"tools/call", map[string]any{}, -32602}} {
		_, raw := e.rpc(tok, c.method, c.params)
		var r rpcAns
		if json.Unmarshal(raw, &r) != nil || r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s %v = %s, want error %d", c.method, c.params, raw, c.code)
		}
	}
	if resp, raw := e.do("POST", "/mcp", tok, `{not json`); resp.StatusCode != 200 || !strings.Contains(string(raw), `"code":-32700`) {
		t.Errorf("not JSON = %d %s", resp.StatusCode, raw)
	}
	if resp, _ := e.do("GET", "/mcp", tok, nil); resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET /mcp = %d, want 405", resp.StatusCode)
	}
	if resp, _ := e.rpc(tok, "ping", nil, "Origin", "https://evil.example.test"); resp.StatusCode != 403 {
		t.Errorf("another origin = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.rpc(tok, "ping", nil, "Origin", publicURL); resp.StatusCode != 200 {
		t.Errorf("the plugin's own origin = %d, want 200", resp.StatusCode)
	}
}

// The tool set is exactly what this plugin can do (a tool needing the
// analysis, mail or a bucket only with it), and every schema is a closed
// object whose required fields it defines, with a description a model can
// read.
func TestMCPToolsListAndTheirSchemas(t *testing.T) {
	full := pcEnv(t, true)
	fullTok, _ := full.mintMCP(full.enter(ada))
	names, tools := full.toolNames(fullTok)
	want := []string{"meeting_agent_join", "meeting_agent_leave", "meeting_agent_say", "meeting_agent_wait",
		"meeting_draft_retry", "meeting_draft_update", "meeting_drafts_list", "meeting_drafts_send", "meeting_frame", "meeting_get",
		"meeting_items_set", "meeting_recording_link", "meeting_reprocess", "meeting_run_set_current", "meeting_timeline", "meeting_transcript", "meetings_list"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v\nwant %v", names, want)
	}
	for _, tl := range tools {
		name := tl["name"].(string)
		if d, _ := tl["description"].(string); len(d) < 60 {
			t.Errorf("%s: description %q is too thin for a model", name, d)
		}
		s, _ := tl["inputSchema"].(map[string]any)
		props, _ := s["properties"].(map[string]any)
		if s["type"] != "object" || props == nil || s["additionalProperties"] != false {
			t.Errorf("%s: schema %v is not a closed object", name, s)
			continue
		}
		req, _ := s["required"].([]any)
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s: requires %v, which it does not define", name, r)
			}
		}
		if name != "meetings_list" && !slices.Contains(req, any("id")) {
			t.Errorf("%s: does not require the meeting id", name)
		}
		for p, v := range props {
			if m, _ := v.(map[string]any); m["type"] == nil {
				t.Errorf("%s.%s has no type", name, p)
			}
		}
	}
	// No engine, no model, no mail, no bucket: the reads only.
	bare := newEnvOpts(t, nil, func(c *server.Config) { c.Engine = nil })
	bareTok, _ := bare.mintMCP(bare.enter(ada))
	if names, _ := bare.toolNames(bareTok); !slices.Equal(names, []string{"meeting_get", "meeting_timeline", "meeting_transcript", "meetings_list"}) {
		t.Errorf("tools of a plugin with none of the features = %v", names)
	}
	// Transcribing and nothing else: the reads and the agent's tools.
	tr := newEnv(t)
	trTok, _ := tr.mintMCP(tr.enter(ada))
	if names, _ := tr.toolNames(trTok); !slices.Equal(names, []string{"meeting_agent_join", "meeting_agent_leave", "meeting_agent_say", "meeting_agent_wait",
		"meeting_get", "meeting_timeline", "meeting_transcript", "meetings_list"}) {
		t.Errorf("tools of a plugin that only transcribes = %v", names)
	}
}

// 🔴 Every tool resolves the meeting under its token's owner: another
// owner's meeting answers exactly what a meeting that does not exist
// does, and changes nothing of it.
func TestEveryMCPToolIsScopedToItsTokensOwner(t *testing.T) {
	e := pcEnv(t, true)
	c := e.fixture()
	e.postDone(c, 1)
	ds := e.drafts(c)
	a := e.analysis(c, "")
	ada, _ := e.mintMCP(c.Sess)
	beaTok, _ := e.mintMCP(e.enter(bea))
	unknown := strings.Repeat("A", 16)
	argsFor := func(name, id string) map[string]any {
		m := map[string]any{"id": id}
		switch name {
		case "meeting_frame":
			m["at_ms"] = 1000
		case "meeting_items_set":
			m["items"] = []any{map[string]any{"task": "Steal the notes"}}
		case "meeting_draft_update":
			m["draft_id"], m["removed"] = ds[0].ID, true
		case "meeting_draft_retry":
			m["draft_id"] = ds[0].ID
		case "meeting_run_set_current":
			m["run_id"] = a.Runs[0].ID
		case "meeting_transcript":
			m["format"] = "txt"
		case "meeting_agent_join":
			m["name"] = "Intruder"
		case "meeting_agent_wait", "meeting_agent_leave":
			m["agent_key"] = strings.Repeat("K", 22)
		case "meeting_agent_say":
			m["agent_key"], m["ask_id"], m["text"] = strings.Repeat("K", 22), 1, "Overheard."
		}
		return m
	}
	names, _ := e.toolNames(beaTok)
	n := 0
	for _, name := range names {
		if name == "meetings_list" {
			continue
		}
		n++
		theirs := e.tool(beaTok, name, argsFor(name, c.ID))
		none := e.tool(beaTok, name, argsFor(name, unknown))
		// The run is looked up under the meeting and its owner at once: the
		// owner app's own answer for it is "no such run".
		want := "no such meeting"
		if name == "meeting_run_set_current" {
			want = "no such run"
		}
		if !theirs.IsError || theirs.text() != want || none.text() != theirs.text() || !none.IsError {
			t.Errorf("%s on another owner's meeting = %v %q; on none = %v %q: want the same %q", name, theirs.IsError, theirs.text(), none.IsError, none.text(), want)
		}
		// Positive control: the owner's own token reaches it.
		if own := e.tool(ada, name, argsFor(name, c.ID)); own.text() == want {
			t.Errorf("%s: the owner's own call answered no such meeting", name)
		}
	}
	if n != 16 {
		t.Fatalf("%d meeting tools exercised, want 16", n)
	}
	var list struct {
		Meetings []struct{ ID string } `json:"meetings"`
	}
	e.ok(beaTok, "meetings_list", map[string]any{}, &list)
	if len(list.Meetings) != 0 {
		t.Errorf("Bea's list holds %v", list.Meetings)
	}
}

// The reads: the list, one meeting, its timeline, its transcript, a frame of
// the shared screen, and the share link (made once; replace makes another
// and turns the first off).
func TestMCPReadToolsAnswerWhatTheOwnerAppShows(t *testing.T) {
	e, c := framesMeeting(t, true)
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	var list struct {
		Meetings []struct {
			ID           string `json:"id"`
			State        string `json:"state"`
			Participants int    `json:"participants"`
			HasRecording bool   `json:"has_recording"`
			HasNotes     bool   `json:"has_notes"`
		} `json:"meetings"`
		Next *string `json:"next"`
	}
	e.ok(tok, "meetings_list", map[string]any{"limit": 5}, &list)
	if len(list.Meetings) != 1 || list.Meetings[0].ID != c.ID || list.Meetings[0].State != "ended" || list.Meetings[0].Participants != 2 ||
		!list.Meetings[0].HasRecording || !list.Meetings[0].HasNotes || list.Next != nil {
		t.Errorf("list = %+v", list)
	}
	var got struct {
		Meeting struct{ ID string } `json:"meeting"`
		People  []struct {
			Name, Role string
		} `json:"people"`
		Recording struct {
			Parts []struct{ Part int } `json:"parts"`
		} `json:"recording"`
		Runs       []struct{ ID int64 } `json:"runs"`
		CurrentRun *int64               `json:"current_run"`
		Drafts     struct{ Count int }  `json:"drafts"`
	}
	e.ok(tok, "meeting_get", map[string]any{"id": c.ID}, &got)
	var people []string
	for _, p := range got.People {
		people = append(people, p.Name+":"+p.Role)
	}
	if got.Meeting.ID != c.ID || strings.Join(people, ",") != grace+":guest" && strings.Join(people, ",") != "Ada L.:owner,"+grace+":guest" ||
		len(got.Recording.Parts) != 1 || len(got.Runs) != 1 || got.CurrentRun == nil || *got.CurrentRun != got.Runs[0].ID || got.Drafts.Count != 1 {
		t.Errorf("get = %+v", got)
	}
	var tl struct {
		Segments []struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
		} `json:"segments"`
		Frames []map[string]any `json:"frames"`
	}
	e.ok(tok, "meeting_timeline", map[string]any{"id": c.ID}, &tl)
	if len(tl.Segments) != 1 || tl.Segments[0].Text != "On this slide, I'll close the ticket." || len(tl.Frames) != 1 || tl.Frames[0]["meeting_ms"] != 6000.0 {
		t.Fatalf("timeline = %+v", tl)
	}
	if _, ok := tl.Frames[0]["href"]; ok {
		t.Errorf("a frame carries the owner app's href: %v", tl.Frames[0])
	}
	if txt := e.ok(tok, "meeting_transcript", map[string]any{"id": c.ID}, nil); !strings.Contains(txt, "[0:05] «Ada L.»: On this slide, I'll close the ticket.") {
		t.Errorf("txt = %q", txt)
	}
	if vtt := e.ok(tok, "meeting_transcript", map[string]any{"id": c.ID, "format": "vtt"}, nil); !strings.HasPrefix(vtt, "WEBVTT") || !strings.Contains(vtt, "close the ticket") {
		t.Errorf("vtt = %q", vtt)
	}
	fr := e.tool(tok, "meeting_frame", map[string]any{"id": c.ID, "at_ms": 6000})
	img, _ := base64.StdEncoding.DecodeString(fr.Content[0].Data)
	if fr.IsError || fr.Content[0].Type != "image" || fr.Content[0].MimeType != "image/jpeg" || len(img) < 4 || img[0] != 0xFF || img[1] != 0xD8 ||
		!strings.Contains(fr.text(), `"offset_ms":"5000"`) {
		t.Errorf("frame = %v %q %q %d bytes", fr.IsError, fr.Content[0].Type, fr.text(), len(img))
	}
	if fr := e.tool(tok, "meeting_frame", map[string]any{"id": c.ID, "at_ms": 500}); !fr.IsError || fr.text() != "that moment is not in the recording" {
		t.Errorf("a frame before the recording = %v %q", fr.IsError, fr.text())
	}
	var link struct {
		URL string `json:"url"`
	}
	e.ok(tok, "meeting_recording_link", map[string]any{"id": c.ID}, &link)
	if !strings.HasPrefix(link.URL, publicURL+"/r/") {
		t.Fatalf("link = %+v", link)
	}
	if resp, _ := e.do("GET", strings.TrimPrefix(link.URL, publicURL), "", nil); resp.StatusCode != 200 {
		t.Errorf("the link's page = %d", resp.StatusCode)
	}
	if again := e.tool(tok, "meeting_recording_link", map[string]any{"id": c.ID}); !again.IsError || !strings.Contains(again.text(), "already live") {
		t.Errorf("a second link without replace = %v %q", again.IsError, again.text())
	}
	var link2 struct {
		URL string `json:"url"`
	}
	e.ok(tok, "meeting_recording_link", map[string]any{"id": c.ID, "replace": true}, &link2)
	if link2.URL == link.URL || !strings.HasPrefix(link2.URL, publicURL+"/r/") {
		t.Fatalf("replace = %+v", link2)
	}
	if resp, _ := e.do("GET", strings.TrimPrefix(link.URL, publicURL), "", nil); resp.StatusCode != 404 {
		t.Errorf("the replaced link's page = %d, want 404", resp.StatusCode)
	}
	// A misspelled argument is refused, not ignored.
	if bad := e.tool(tok, "meeting_get", map[string]any{"meetingId": c.ID}); !bad.IsError || !strings.Contains(bad.text(), `unknown field "meetingId"`) {
		t.Errorf("an unknown argument = %v %q", bad.IsError, bad.text())
	}
}

type mcpDraft struct {
	DraftID string `json:"draft_id"`
	Role    string `json:"role"`
	Name    string `json:"name"`
	Address string `json:"address"`
	State   string `json:"state"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
	Items   []struct {
		ItemID int    `json:"item_id"`
		Task   string `json:"task"`
		Owner  string `json:"owner"`
	} `json:"items"`
	Yours  []struct{ Task string } `json:"yours"`
	Others []struct{ Task string } `json:"others"`
}

func (e *env) mcpDrafts(tok, id string) (ds []mcpDraft, canSend bool) {
	e.t.Helper()
	var v struct {
		Drafts  []mcpDraft `json:"drafts"`
		CanSend bool       `json:"can_send"`
	}
	e.ok(tok, "meeting_drafts_list", map[string]any{"id": id}, &v)
	return v.Drafts, v.CanSend
}

func tasks(its []struct{ Task string }) string {
	var out []string
	for _, i := range its {
		out = append(out, i.Task)
	}
	return strings.Join(out, "|")
}

// 🔴 The owner's own action items: a NEW current run recorded as "mcp" (the
// earlier run kept, no model called), and every unsent draft drawn again
// from them, each recipient's own in YOUR section and the rest in the
// others; a recipient already sent to is left exactly as sent.
func TestMCPItemsSetRedraftsWithTheSuppliedItems(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	var tl struct {
		Segments []struct {
			ID      int64  `json:"id"`
			Speaker string `json:"speaker"`
		} `json:"segments"`
	}
	e.ok(tok, "meeting_timeline", map[string]any{"id": c.ID}, &tl)
	if len(tl.Segments) != 2 {
		t.Fatalf("segments = %+v", tl.Segments)
	}
	calls := len(e.or.Requests())
	var res struct {
		Run struct {
			ID          int64  `json:"id"`
			ItemsModel  string `json:"items_model"`
			State       string `json:"state"`
			Current     bool   `json:"current"`
			RequestedBy string `json:"requested_by"`
		} `json:"run"`
		Items  int `json:"items"`
		Drafts struct {
			Made        int `json:"made"`
			AlreadySent int `json:"already_sent"`
		} `json:"drafts"`
	}
	e.ok(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{
		map[string]any{"task": "Order the flowers", "owner": "Ada L.", "segment_ids": []int64{tl.Segments[0].ID}},
		map[string]any{"task": "Print the seating chart", "owner": grace + " (guest)", "due": "Friday"},
	}}, &res)
	if res.Run.ItemsModel != "mcp" || res.Run.State != "done" || !res.Run.Current || res.Run.RequestedBy != "owner" || res.Items != 2 || res.Drafts.Made != 2 {
		t.Fatalf("items_set = %+v", res)
	}
	a := e.analysis(c, "")
	if len(a.Runs) != 2 || a.Current == nil || *a.Current != res.Run.ID || a.Output == nil || len(a.Output.Items) != 2 ||
		a.Output.Items[0].Text != "Order the flowers" || a.Output.Items[1].Assignee != grace {
		t.Errorf("analysis = %+v", a)
	}
	e.server.PostPass(context.Background())
	if n := len(e.or.Requests()); n != calls {
		t.Errorf("%d model calls after the owner's items, want still %d (the owner's run is never queued)", n, calls)
	}
	ds, _ := e.mcpDrafts(tok, c.ID)
	if len(ds) != 2 || ds[0].Role != "owner" || ds[1].Name != grace {
		t.Fatalf("drafts = %+v", ds)
	}
	if tasks(ds[0].Yours) != "Order the flowers" || tasks(ds[0].Others) != "Print the seating chart" ||
		tasks(ds[1].Yours) != "Print the seating chart" || tasks(ds[1].Others) != "Order the flowers" {
		t.Errorf("sections: owner yours %q others %q; Grace yours %q others %q", tasks(ds[0].Yours), tasks(ds[0].Others), tasks(ds[1].Yours), tasks(ds[1].Others))
	}
	if !strings.Contains(ds[1].Text, "Print the seating chart") || ds[1].Address != graceAddr {
		t.Errorf("Grace's preview: %q to %q", ds[1].Text, ds[1].Address)
	}
	// Grace's notes sent; new items leave hers exactly as sent.
	e.confirmOwner(c)
	var sent struct{ Queued int }
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID, "draft_ids": []string{ds[1].DraftID}}, &sent)
	if sent.Queued != 1 {
		t.Fatalf("queued %d", sent.Queued)
	}
	eventually(t, 10*time.Second, "Grace's notes sent", func() bool {
		d, _ := e.mcpDrafts(tok, c.ID)
		return d[1].State == "sent"
	})
	e.ok(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Call the florist", "owner": "Ada L."}}, "label": "claude-code"}, &res)
	if res.Run.ItemsModel != "claude-code" || res.Drafts.Made != 1 || res.Drafts.AlreadySent != 1 {
		t.Errorf("second items_set = %+v", res)
	}
	ds, _ = e.mcpDrafts(tok, c.ID)
	if tasks(ds[0].Yours) != "Call the florist" || ds[1].State != "sent" || len(ds[1].Items) != 2 || ds[1].Items[0].Task != "Order the flowers" {
		t.Errorf("after the second set: owner %q; Grace %s %+v (must be as sent)", tasks(ds[0].Yours), ds[1].State, ds[1].Items)
	}
}

// 🔴 The model path's guards, and the whole set refused when one item fails
// them: an owner no speaker is, a segment id that is not a line of this
// meeting (another meeting's line included), a duplicate, an item too
// long; and an open meeting. Nothing is stored by any of them.
func TestMCPItemsSetRefusesWhatTheGuardsRefuse(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	other := e.meeting(bea)
	e.started(other)
	foreign := e.say(other, store.OwnerIdentity, "Bea K.", 1_000, 2_000, "I'll pay the deposit.")
	tok, _ := e.mintMCP(c.Sess)
	runs := len(e.analysis(c, "").Runs)
	for name, it := range map[string]struct {
		items []any
		want  string
	}{
		"a stranger owns it": {[]any{map[string]any{"task": "Book the band"}, map[string]any{"task": "Wire the money", "owner": "Mallory"}},
			"item 1 its owner is not a speaker"},
		"a line not in this meeting": {[]any{map[string]any{"task": "Pay the deposit", "segment_ids": []int64{foreign}}},
			"item 0 cites a segment id that is not a line of this meeting"},
		"no such line":     {[]any{map[string]any{"task": "Pay the deposit", "segment_ids": []int64{987654}}}, "item 0 cites a segment id"},
		"a duplicate":      {[]any{map[string]any{"task": "Book the band"}, map[string]any{"task": "Book the band."}}, "item 1 says the same as an earlier item"},
		"too long":         {[]any{map[string]any{"task": strings.Repeat("x", 501)}}, "Item 0 is too long."},
		"no task":          {[]any{map[string]any{"task": "  ", "owner": "Ada L."}}, "Item 0 has no task."},
		"an unknown field": {[]any{map[string]any{"task": "Book the band", "assignee": "Ada L."}}, `unknown field "assignee"`},
	} {
		a := e.tool(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": it.items})
		if !a.IsError || !strings.Contains(a.text(), it.want) {
			t.Errorf("%s = %v %q, want %q", name, a.IsError, a.text(), it.want)
		}
	}
	if n := len(e.analysis(c, "").Runs); n != runs {
		t.Errorf("%d runs after refused sets, want %d: something was stored", n, runs)
	}
	open := e.meeting(ada)
	if a := e.tool(tok, "meeting_items_set", map[string]any{"id": open.ID, "items": []any{}}); !a.IsError || !strings.Contains(a.text(), "still open") {
		t.Errorf("an open meeting = %v %q", a.IsError, a.text())
	}
	// Positive control: the same set without the bad item is stored.
	if a := e.tool(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Book the band"}}}); a.IsError {
		t.Errorf("a good set = %q", a.text())
	}
}

// 🔴 AI off: the meeting is never analysed (reprocess refused, no model
// called at all), but the owner's own items ARE allowed, and draft their
// notes.
func TestMCPItemsSetOnAMeetingWithAIOffAndReprocessRefused(t *testing.T) {
	e := pcEnv(t, false)
	c := e.meeting(ada)
	e.started(c)
	if resp, raw := e.do("POST", "/api/meetings/"+c.ID+"/ai", c.Sess, map[string]bool{"on": false}); resp.StatusCode != 200 {
		t.Fatalf("ai off = %d %s", resp.StatusCode, raw)
	}
	e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 3_000, "I'll book the caterer.")
	e.end(c)
	e.server.PostPass(context.Background())
	tok, _ := e.mintMCP(c.Sess)
	if a := e.tool(tok, "meeting_reprocess", map[string]any{"id": c.ID}); !a.IsError || a.text() != server.ErrReprocessNoAI {
		t.Errorf("reprocess of an AI-off meeting = %v %q", a.IsError, a.text())
	}
	var res struct {
		Drafts struct{ Made int } `json:"drafts"`
	}
	e.ok(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Book the caterer", "owner": "Ada L."}}}, &res)
	ds, _ := e.mcpDrafts(tok, c.ID)
	if res.Drafts.Made != 1 || len(ds) != 1 || tasks(ds[0].Yours) != "Book the caterer" {
		t.Errorf("drafts = %+v, %+v", res, ds)
	}
	e.server.PostPass(context.Background())
	if n := len(e.or.Requests()); n != 0 {
		t.Errorf("%d model calls for a meeting with AI off, want 0", n)
	}
}

// meeting_reprocess queues a run as Run again does.
func TestMCPReprocessQueuesARunAndRevertMakesAnEarlierOneCurrent(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	first := e.postDone(c, 1).Runs[0].ID
	tok, _ := e.mintMCP(c.Sess)
	var q struct {
		Run int64 `json:"run"`
	}
	e.ok(tok, "meeting_reprocess", map[string]any{"id": c.ID}, &q)
	e.postDone(c, 2)
	if a := e.analysis(c, ""); a.Current == nil || *a.Current != q.Run {
		t.Fatalf("current after the reprocess = %v, want %d", a.Current, q.Run)
	}
	e.ok(tok, "meeting_run_set_current", map[string]any{"id": c.ID, "run_id": first}, nil)
	if a := e.analysis(c, ""); a.Current == nil || *a.Current != first {
		t.Errorf("current after the revert = %v, want %d", a.Current, first)
	}
	if a := e.tool(tok, "meeting_reprocess", map[string]any{"id": c.ID, "items_model": "other/model"}); !a.IsError || !strings.Contains(a.text(), "items_provider_prefs") {
		t.Errorf("another model with no prefs = %v %q", a.IsError, a.text())
	}
}

// 🔴 The send gate: refused, with nothing queued, until the owner's address
// is confirmed (the UI's rule); then the outbox mails each recipient
// through the fake SMTP server, and the audit says the send came via MCP.
func TestMCPSendNeedsTheConfirmedAddressThenMails(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, tid := e.mintMCP(c.Sess)
	if ds, can := e.mcpDrafts(tok, c.ID); can || len(ds) != 2 {
		t.Fatalf("can_send %v with no address; drafts %+v", can, ds)
	}
	if a := e.tool(tok, "meeting_drafts_send", map[string]any{"id": c.ID}); !a.IsError || a.text() != server.NotesNeedAddress {
		t.Fatalf("send with no address = %v %q", a.IsError, a.text())
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(e.smtp.Messages()); n != 0 {
		t.Fatalf("%d messages after a refused send", n)
	}
	e.confirmOwner(c)
	before := len(e.smtp.Messages())
	var sent struct{ Queued int }
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID}, &sent)
	if sent.Queued != 2 {
		t.Fatalf("queued %d, want 2", sent.Queued)
	}
	eventually(t, 10*time.Second, "two notes mailed", func() bool { return len(e.smtp.Messages()) == before+2 })
	var to []string
	for _, m := range e.smtp.Messages()[before:] {
		to = append(to, m.To[0])
	}
	sort.Strings(to)
	if strings.Join(to, ",") != adaAddr+","+graceAddr {
		t.Errorf("mailed to %v", to)
	}
	eventually(t, 5*time.Second, "both sent", func() bool {
		ds, _ := e.mcpDrafts(tok, c.ID)
		return ds[0].State == "sent" && ds[1].State == "sent"
	})
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var detail string
	if err := db.QueryRow(`SELECT detail FROM audit WHERE event = 'notes.approved' ORDER BY rowid DESC LIMIT 1`).Scan(&detail); err != nil || detail != `{"queued":2,"via":"mcp","token":"`+tid+`"}` {
		t.Errorf("audit = %q, %v", detail, err)
	}
	if !strings.Contains(e.log.String(), "mcp: tool meeting_drafts_send meeting "+c.ID+": ok (send via mcp: 2 queued)") {
		t.Errorf("the log does not say the send came via MCP:\n%s", e.log.String())
	}
	// Sent: nothing more to send, and a sent draft is not edited.
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID}, &sent)
	if sent.Queued != 0 {
		t.Errorf("a second send queued %d", sent.Queued)
	}
	ds, _ := e.mcpDrafts(tok, c.ID)
	if a := e.tool(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": ds[1].DraftID, "removed": true}); !a.IsError || a.text() != server.NotesLocked {
		t.Errorf("editing a sent draft = %v %q", a.IsError, a.text())
	}
}

// Add item, remove item, Don't send (and back), a guest's address; retry a
// failed send.
func TestMCPDraftUpdateAndRetry(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	ds, _ := e.mcpDrafts(tok, c.ID)
	owner, guest := ds[0], ds[1]
	var up struct {
		State string `json:"state"`
		Items []struct{ Task, Owner string }
	}
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": owner.DraftID, "remove_item_ids": []int{0}, "add_items": []any{map[string]any{"task": "Send the invoices", "owner": "Ada L."}}}, &up)
	if len(up.Items) != 2 || up.Items[0].Task != owner.Items[1].Task || up.Items[1].Task != "Send the invoices" {
		t.Errorf("edited items = %+v (from %+v)", up.Items, owner.Items)
	}
	if a := e.tool(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": owner.DraftID, "remove_item_ids": []int{9}}); !a.IsError || !strings.Contains(a.text(), "there is no item 9") {
		t.Errorf("removing an item that is not there = %v %q", a.IsError, a.text())
	}
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": guest.DraftID, "removed": true}, &up)
	if up.State != "removed" {
		t.Errorf("Don't send = %s", up.State)
	}
	e.confirmOwner(c)
	var sent struct{ Queued int }
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID}, &sent)
	if sent.Queued != 1 {
		t.Errorf("queued %d with Grace taken out, want the owner's 1", sent.Queued)
	}
	eventually(t, 10*time.Second, "the owner's notes sent", func() bool {
		d, _ := e.mcpDrafts(tok, c.ID)
		return d[0].State == "sent"
	})
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": guest.DraftID, "removed": false, "email": "grace.new@example.test"}, &up)
	if a := e.tool(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": guest.DraftID, "email": "not an address"}); !a.IsError || a.text() != server.AddressInvalid {
		t.Errorf("a bad address = %v %q", a.IsError, a.text())
	}
	// The mail server refuses Grace's for good: failed; retry sends it.
	e.smtp.Reply("RCPT", 550)
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID, "draft_ids": []string{guest.DraftID}}, &sent)
	eventually(t, 10*time.Second, "Grace's draft failed", func() bool {
		d, _ := e.mcpDrafts(tok, c.ID)
		return d[1].State == "failed"
	})
	e.smtp.Reply("RCPT", 0)
	before := len(e.smtp.Messages())
	e.ok(tok, "meeting_draft_retry", map[string]any{"id": c.ID, "draft_id": guest.DraftID}, nil)
	eventually(t, 10*time.Second, "Grace's retried notes mailed", func() bool {
		ms := e.smtp.Messages()
		return len(ms) == before+1 && ms[before].To[0] == "grace.new@example.test"
	})
	if a := e.tool(tok, "meeting_draft_retry", map[string]any{"id": c.ID, "draft_id": owner.DraftID}); !a.IsError || a.text() != server.NotesLocked {
		t.Errorf("retrying a sent draft = %v %q", a.IsError, a.text())
	}
}

// 🔴 The log carries no word of a meeting: a distinctive word said in the
// meeting, and one in the owner's items, flows through every tool (the
// positive control: each answer holds it) and is in no log line, nor is a
// guest's address; the log says each tool's name, meeting and outcome.
func TestMCPToolCallsLogNoMeetingContent(t *testing.T) {
	const said, typed = "Quixotrombe", "Vandersnatchly"
	e := pcEnv(t, false)
	c := e.meeting(ada)
	e.started(c)
	e.say(c, store.OwnerIdentity, "Ada L.", 1_000, 3_000, "I'll ship the "+said+" prototype.")
	e.cardFrom(c, grace, graceAddr, true)
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	answers := []string{
		e.ok(tok, "meeting_timeline", map[string]any{"id": c.ID}, nil),
		e.ok(tok, "meeting_transcript", map[string]any{"id": c.ID}, nil),
	}
	e.ok(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Ship the " + said + " prototype", "owner": "Ada L."}}}, nil)
	ds, _ := e.mcpDrafts(tok, c.ID)
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": ds[0].DraftID, "add_items": []any{map[string]any{"task": "Polish the " + typed}}}, nil)
	answers = append(answers, e.ok(tok, "meeting_drafts_list", map[string]any{"id": c.ID}, nil), e.ok(tok, "meeting_get", map[string]any{"id": c.ID}, nil))
	e.tool(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": typed + " again", "owner": typed}}}) // refused
	e.confirmOwner(c)
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID}, nil)
	eventually(t, 10*time.Second, "notes sent", func() bool { return len(e.smtp.Messages()) >= 3 })
	all := strings.Join(answers, "\n")
	for _, w := range []string{said, typed, graceAddr} {
		if !strings.Contains(all, w) {
			t.Fatalf("positive control: no tool answer holds %q", w)
		}
	}
	lg := e.log.String()
	for _, w := range []string{said, strings.ToLower(said), typed, strings.ToLower(typed), graceAddr, "prototype", tok} {
		if strings.Contains(lg, w) {
			t.Errorf("the log holds %q:\n%s", w, lg)
		}
	}
	for _, want := range []string{"mcp: tool meeting_timeline meeting " + c.ID + ": ok in ", "mcp: tool meeting_items_set meeting " + c.ID + ": error 400 in ",
		"mcp: tool meeting_drafts_list meeting " + c.ID + ": ok in "} {
		if !strings.Contains(lg, want) {
			t.Errorf("the log has no %q:\n%s", want, lg)
		}
	}
}

// Create token answers it once inside the command; the list shows dates,
// never a token; Revoke makes /mcp 401 at once; another owner can neither
// see nor revoke it.
func TestMCPTokenSettings(t *testing.T) {
	e := newEnv(t)
	sa, sb := e.enter(ada), e.enter(bea)
	tok, id := e.mintMCP(sa)
	e.clock.Add(2 * time.Minute)
	e.rpc(tok, "ping", nil)
	resp, raw := e.do("GET", "/api/settings/mcp", sa, nil)
	var v struct {
		Tokens []struct {
			ID         string     `json:"id"`
			CreatedAt  time.Time  `json:"created_at"`
			LastUsedAt *time.Time `json:"last_used_at"`
		} `json:"tokens"`
		URL string `json:"url"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(raw, &v) != nil || len(v.Tokens) != 1 || v.Tokens[0].ID != id || !v.Tokens[0].CreatedAt.Equal(t0) ||
		v.Tokens[0].LastUsedAt == nil || !v.Tokens[0].LastUsedAt.Equal(t0.Add(2*time.Minute)) || v.URL != publicURL+"/mcp" {
		t.Fatalf("list = %d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), tok[4:]) {
		t.Fatal("the list holds the token")
	}
	if _, raw := e.do("GET", "/api/settings/mcp", sb, nil); strings.Contains(string(raw), id) {
		t.Errorf("Bea's list holds Ada's token: %s", raw)
	}
	if resp, _ := e.do("DELETE", "/api/settings/mcp/"+id, sb, nil); resp.StatusCode != 404 {
		t.Errorf("Bea revoking Ada's token = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.rpc(tok, "ping", nil); resp.StatusCode != 200 {
		t.Fatalf("Ada's token after Bea's attempt = %d", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "/api/settings/mcp/"+id, sa, nil); resp.StatusCode != 200 {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if resp, _ := e.rpc(tok, "ping", nil); resp.StatusCode != 401 {
		t.Errorf("a revoked token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do("DELETE", "/api/settings/mcp/"+id, sa, nil); resp.StatusCode != 404 {
		t.Errorf("revoking twice = %d, want 404", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/api/settings/mcp", "", nil); resp.StatusCode != 401 {
		t.Errorf("the settings with no session = %d", resp.StatusCode)
	}
}

// 🔴 muster revoking the owner revokes their MCP token, even with no session
// open (the token alone makes them a subject the check asks about); the
// other owner's token is untouched.
func TestMusterRevokingTheOwnerRevokesTheirMCPToken(t *testing.T) {
	e := newEnv(t)
	tokA, _ := e.mintMCP(e.enter(ada))
	tokB, _ := e.mintMCP(e.enter(bea))
	e.clock.Add(9 * time.Hour) // every session expired: only the tokens hold them
	e.app.PollOnce(context.Background())
	for name, tk := range map[string]string{"Ada": tokA, "Bea": tokB} {
		if resp, _ := e.rpc(tk, "ping", nil); resp.StatusCode != 200 {
			t.Fatalf("positive control: %s's token after a live poll = %d", name, resp.StatusCode)
		}
	}
	e.fake.Hide(app.PluginID, ada.ID)
	e.app.PollOnce(context.Background())
	if resp, _ := e.rpc(tokA, "ping", nil); resp.StatusCode != 401 {
		t.Errorf("Ada's token after muster revoked her = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.rpc(tokB, "ping", nil); resp.StatusCode != 200 {
		t.Errorf("Bea's token after Ada's revocation = %d, want 200", resp.StatusCode)
	}
}

// A count of the model's calls, to read "no call" claims against.
var _ = openroutertest.KindItems

// 🔴 draft_ids present and empty is refused (it never means "all"), and the
// schema says at least one; draft_ids absent sends every unsent draft.
func TestMCPSendWithAnEmptyDraftIDsListSendsNothing(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	_, tools := e.toolNames(tok)
	for _, tl := range tools {
		if tl["name"] == "meeting_drafts_send" {
			ids := tl["inputSchema"].(map[string]any)["properties"].(map[string]any)["draft_ids"].(map[string]any)
			if ids["minItems"] != 1.0 {
				t.Errorf("draft_ids schema = %v: want minItems 1", ids)
			}
		}
	}
	e.confirmOwner(c)
	before := len(e.smtp.Messages())
	if a := e.tool(tok, "meeting_drafts_send", map[string]any{"id": c.ID, "draft_ids": []string{}}); !a.IsError || !strings.Contains(a.text(), "draft_ids is empty") {
		t.Errorf("send with draft_ids [] = %v %q, want refused", a.IsError, a.text())
	}
	time.Sleep(100 * time.Millisecond)
	if ds, _ := e.mcpDrafts(tok, c.ID); ds[0].State != "draft" || ds[1].State != "draft" || len(e.smtp.Messages()) != before {
		t.Fatalf("after draft_ids []: states %s %s, %d mailed", ds[0].State, ds[1].State, len(e.smtp.Messages())-before)
	}
	var sent struct{ Queued int }
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID}, &sent)
	if sent.Queued != 2 {
		t.Errorf("send with draft_ids absent queued %d, want every unsent (2)", sent.Queued)
	}
}

// 🔴 A guest's address corrected with meeting_draft_update stays through
// the owner's new items and through Run again, and the notes go there.
func TestMCPACorrectedGuestAddressSurvivesItemsAndReprocess(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	ds, _ := e.mcpDrafts(tok, c.ID)
	const fixed = "grace.fixed@example.test"
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": ds[1].DraftID, "email": fixed}, nil)
	e.ok(tok, "meeting_items_set", map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Print the seating chart", "owner": grace}}}, nil)
	if ds, _ = e.mcpDrafts(tok, c.ID); ds[1].Address != fixed {
		t.Errorf("Grace's address after items_set = %q, want %q", ds[1].Address, fixed)
	}
	var q struct {
		Run int64 `json:"run"`
	}
	e.ok(tok, "meeting_reprocess", map[string]any{"id": c.ID}, &q)
	e.postDone(c, 3)
	if ds, _ = e.mcpDrafts(tok, c.ID); ds[1].Address != fixed {
		t.Errorf("Grace's address after Run again = %q, want %q", ds[1].Address, fixed)
	}
	e.confirmOwner(c)
	before := len(e.smtp.Messages())
	e.ok(tok, "meeting_drafts_send", map[string]any{"id": c.ID, "draft_ids": []string{ds[1].DraftID}}, nil)
	eventually(t, 10*time.Second, "Grace's notes mailed", func() bool { return len(e.smtp.Messages()) == before+1 })
	if to := e.smtp.Messages()[before].To[0]; to != fixed {
		t.Errorf("mailed to %q, want the corrected %q", to, fixed)
	}
}

// Retries are harmless: the same item added twice is on the draft once, and
// the same set of items stored twice is one run (the current one, said
// unchanged).
func TestMCPRetriedCallsChangeNothingTwice(t *testing.T) {
	e := pcEnv(t, false)
	c := e.fixture()
	e.postDone(c, 1)
	tok, _ := e.mintMCP(c.Sess)
	ds, _ := e.mcpDrafts(tok, c.ID)
	n := len(ds[0].Items)
	for i := 0; i < 2; i++ {
		e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": ds[0].DraftID, "add_items": []any{map[string]any{"task": "Send the invoices", "owner": "Ada L."}}}, nil)
	}
	e.ok(tok, "meeting_draft_update", map[string]any{"id": c.ID, "draft_id": ds[0].DraftID, "add_items": []any{map[string]any{"task": "  send THE invoices ", "owner": "Ada L."}}}, nil)
	if ds, _ = e.mcpDrafts(tok, c.ID); len(ds[0].Items) != n+1 {
		t.Errorf("after adding one item three times: %d items, want %d", len(ds[0].Items), n+1)
	}
	set := map[string]any{"id": c.ID, "items": []any{map[string]any{"task": "Order the flowers", "owner": "Ada L.", "due": "Friday"}}}
	var a, b struct {
		Run       struct{ ID int64 } `json:"run"`
		Unchanged bool               `json:"unchanged"`
	}
	e.ok(tok, "meeting_items_set", set, &a)
	e.ok(tok, "meeting_items_set", set, &b)
	if b.Run.ID != a.Run.ID || !b.Unchanged || a.Unchanged {
		t.Errorf("the same set twice: runs %d then %d (unchanged %v, %v), want one run", a.Run.ID, b.Run.ID, a.Unchanged, b.Unchanged)
	}
	if runs := len(e.analysis(c, "").Runs); runs != 2 {
		t.Errorf("%d runs, want the post run and one owner run", runs)
	}
	// A different set is a new run.
	set["items"] = []any{map[string]any{"task": "Order the flowers", "owner": "Ada L.", "due": "Monday"}}
	var d struct {
		Run struct{ ID int64 } `json:"run"`
	}
	e.ok(tok, "meeting_items_set", set, &d)
	if d.Run.ID == a.Run.ID {
		t.Error("a changed set was not stored")
	}
}

// A JSON-RPC response (an id and a result, no method: the client answering)
// is accepted with 202 and no body, as a notification is.
func TestMCPAcceptsAJSONRPCResponseWith202(t *testing.T) {
	e := newEnv(t)
	tok, _ := e.mintMCP(e.enter(ada))
	for _, body := range []string{`{"jsonrpc":"2.0","id":5,"result":{}}`, `{"jsonrpc":"2.0","id":"x","error":{"code":-1,"message":"no"}}`} {
		if resp, raw := e.do("POST", "/mcp", tok, body); resp.StatusCode != 202 || len(raw) != 0 {
			t.Errorf("%s = %d %s, want 202 and no body", body, resp.StatusCode, raw)
		}
	}
	// A request with an id and no method is still invalid.
	if _, raw := e.do("POST", "/mcp", tok, `{"jsonrpc":"2.0","id":6}`); !strings.Contains(string(raw), `"code":-32600`) {
		t.Errorf("an id with neither method nor result = %s, want -32600", raw)
	}
}
