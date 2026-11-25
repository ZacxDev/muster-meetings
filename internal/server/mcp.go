package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/lobby"
	"github.com/ZacxDev/muster-meetings/internal/store"
)

// Meetings from the owner's own Claude Code session (README: "Using Meetings
// from Claude Code (MCP)"): an MCP server, Streamable HTTP, on this public
// listener.
//
//	POST /mcp                         JSON-RPC 2.0 (initialize, notifications/*, ping, tools/list, tools/call), for Authorization: Bearer mmc_…
//	GET  /api/settings/mcp            the owner's MCP tokens (never a token), and the endpoint
//	POST /api/settings/mcp            a new token: answered ONCE, with the ready `claude mcp add …` command
//	DELETE /api/settings/mcp/{tid}    revoke one
//
// It is stateless (no session id, no event stream): every POST is
// authenticated on its own, and every answer is one JSON body. 🔴 The
// tools (mcp_tools.go) are thin adapters over the owner app's own handlers,
// called in-process with the token owner's repository, so every rule the
// owner UI obeys (owner scoping, the AI switch, the confirmed address
// before Send, a sent recipient never redrafted) holds here by
// construction. 🔴 What a meeting holds is never logged: a tool call's log
// line is its name, its meeting id, its outcome and its duration only.

// MCPPath is the endpoint.
const MCPPath = "/mcp"

// MCPMaxBody bounds one JSON-RPC request.
const MCPMaxBody = 1 << 20

// The failed-authentication limit per client address.
const (
	MCPFailsPerAddress = 20
	MCPFailWindow      = 10 * time.Minute
)

// mcpProtocols are the MCP revisions this server speaks (newest first): it
// echoes the client's when it is one, its newest otherwise.
var mcpProtocols = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func (s *Server) mcpRoutes() {
	s.mux.HandleFunc("POST "+MCPPath, s.handleMCP)
	s.mux.HandleFunc("GET "+MCPPath, mcpNoStream)
	s.mux.HandleFunc("DELETE "+MCPPath, mcpNoStream)
	s.mux.HandleFunc("GET /api/settings/mcp", s.owner(s.handleMCPTokens))
	s.mux.HandleFunc("POST /api/settings/mcp", s.owner(s.handleMintMCPToken))
	s.mux.HandleFunc("DELETE /api/settings/mcp/{tid}", s.owner(s.handleRevokeMCPToken))
}

// mcpNoStream: no server-sent stream and no session to delete.
func mcpNoStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST")
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST JSON-RPC to this endpoint"})
}

// ---- the owner's token settings ----

// MCPCommand is the command the owner pastes into their terminal.
func (s *Server) MCPCommand(token string) string {
	return `claude mcp add --transport http meetings ` + s.cfg.PublicURL + MCPPath + ` --header "Authorization: Bearer ` + token + `"`
}

func (s *Server) handleMCPTokens(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	ts, err := repo.MCPTokens(r.Context())
	if err != nil {
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": ts, "url": s.cfg.PublicURL + MCPPath})
}

func (s *Server) handleMintMCPToken(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	tok, t, err := repo.MintMCPToken(r.Context(), s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrTooManyTokens):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "You have too many tokens. Revoke one you no longer use."})
		return
	case errors.Is(err, store.ErrNoSession):
		w.Header().Set("WWW-Authenticate", `Bearer realm="meetings"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "no live owner session: " + EnterAgain})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "id": t.ID, "created_at": t.CreatedAt, "url": s.cfg.PublicURL + MCPPath, "command": s.MCPCommand(tok)})
}

func (s *Server) handleRevokeMCPToken(w http.ResponseWriter, r *http.Request, repo *store.OwnerRepo) {
	err := repo.RevokeMCPToken(r.Context(), r.PathValue("tid"), s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such token"})
		return
	case err != nil:
		s.meetingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// ---- the door ----

// mcpRefuse is every failed authentication: the same 401 and no detail.
func mcpRefuse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="meetings-mcp"`)
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
}

// mcpAuth admits a request carrying a live MCP token (and only that: an
// owner session's mmo_ and an agent's mmr_ are refused alike), while the
// app's token is live. It answers the token owner's MCP repository.
func (s *Server) mcpAuth(w http.ResponseWriter, r *http.Request) (*store.OwnerRepo, bool) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	t, err := s.store.LookupMCPToken(r.Context(), tok, s.cfg.Now())
	if err == nil && s.app.Live() {
		return s.store.ForMCP(t), true
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the token could not be read"})
		return nil, false
	}
	// 🔴 Failed attempts are counted per client address; past the limit the
	// answer is 429, so the door cannot be used to guess at speed.
	if !s.mcpFails.Allow(s.clientAddr(r), s.cfg.Now()) {
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts"})
		return nil, false
	}
	mcpRefuse(w)
	return nil, false
}

// ---- JSON-RPC ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	rpcParse          = -32700
	rpcInvalidRequest = -32600
	rpcNoMethod       = -32601
	rpcInvalidParams  = -32602
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// 🔴 A browser page on another origin is refused (DNS rebinding, the
	// spec's MUST); Claude Code sends no Origin.
	if o := r.Header.Get("Origin"); o != "" && o != s.cfg.PublicURL {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden origin"})
		return
	}
	repo, ok := s.mcpAuth(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MCPMaxBody))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcInvalidRequest, "the request is too large"}})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(raw, &batch) != nil || len(batch) == 0 {
			writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcParse, "not JSON-RPC"}})
			return
		}
		var out []rpcResponse
		for _, m := range batch {
			if resp, ok := s.rpcOne(r.Context(), repo, m); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	resp, ok := s.rpcOne(r.Context(), repo, raw)
	if !ok {
		// A notification (or a client's response): accepted, no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// rpcOne answers one message; ok false for a notification (no answer).
func (s *Server) rpcOne(ctx context.Context, repo *store.OwnerRepo, raw json.RawMessage) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{rpcParse, "not JSON-RPC"}}, true
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return rpcResponse{}, false
	}
	out := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if req.JSONRPC != "2.0" || req.Method == "" {
		out.Error = &rpcError{rpcInvalidRequest, "not a JSON-RPC 2.0 request"}
		return out, true
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpProtocols[0]
		for _, k := range mcpProtocols {
			if k == p.ProtocolVersion {
				v = k
			}
		}
		out.Result = map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "muster-meetings", "title": "Meetings", "version": "1"},
			"instructions":    mcpInstructions,
		}
	case "ping":
		out.Result = map[string]any{}
	case "tools/list":
		list := []map[string]any{}
		for _, t := range s.mcpTools() {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		out.Result = map[string]any{"tools": list}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			out.Error = &rpcError{rpcInvalidParams, "tools/call takes {\"name\", \"arguments\"}"}
			return out, true
		}
		t, ok := s.mcpTool(p.Name)
		if !ok {
			out.Error = &rpcError{rpcInvalidParams, "no such tool"}
			return out, true
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		out.Result = s.callTool(ctx, repo, t, p.Arguments)
	default:
		out.Error = &rpcError{rpcNoMethod, "no such method"}
	}
	return out, true
}

// callTool runs one tool and logs it. 🔴 The log line is the tool's name,
// the meeting id (when the arguments name a well-formed one), the outcome
// and the duration: never an argument, a result or an error's text.
func (s *Server) callTool(ctx context.Context, repo *store.OwnerRepo, t mcpTool, args json.RawMessage) toolResult {
	start := s.cfg.Now()
	var id struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(args, &id)
	mid := "-"
	if v, ok := id.ID.(string); ok && store.ValidMeetingID(v) {
		mid = v
	}
	res := t.run(ctx, repo, args)
	outcome := "ok"
	if res.IsError {
		outcome = "error"
		if res.status != 0 {
			outcome = "error " + strconv.Itoa(res.status)
		}
	}
	if res.logNote != "" {
		outcome += " (" + res.logNote + ")"
	}
	s.cfg.Log.Printf("mcp: tool %s meeting %s: %s in %s", t.Name, mid, outcome, s.cfg.Now().Sub(start).Round(time.Millisecond))
	return res
}

// toolResult is a tools/call result.
type toolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
	// status is the inner handler's HTTP status of a failure, logNote a
	// count for the log line (a send's queued count): neither is sent.
	status  int
	logNote string
}

func textResult(text string) toolResult {
	return toolResult{Content: []map[string]any{{"type": "text", "text": text}}}
}

func jsonResult(v any) toolResult {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return toolError(0, "the answer could not be encoded")
	}
	return textResult(string(b))
}

func toolError(status int, msg string) toolResult {
	r := textResult(msg)
	r.IsError, r.status = true, status
	return r
}

// ---- in-process calls of the owner app's handlers ----

// answer is what an owner handler answered in-process.
type answer struct {
	Status int
	Header http.Header
	Body   []byte
}

type memWriter struct {
	h      http.Header
	status int
	buf    bytes.Buffer
}

func (m *memWriter) Header() http.Header { return m.h }
func (m *memWriter) WriteHeader(c int) {
	if m.status == 0 {
		m.status = c
	}
}
func (m *memWriter) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	return m.buf.Write(b)
}

// inner calls the owner handler h as if the owner app had sent method
// target (path values pv, body as JSON when not nil), with repo: the very
// code the owner UI's request runs, so the tools hold its rules.
func (s *Server) inner(ctx context.Context, h ownerHandler, repo *store.OwnerRepo, method, target string, pv map[string]string, body any) answer {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://mcp.invalid"+target, rd)
	if err != nil {
		return answer{Status: http.StatusInternalServerError, Body: []byte(`{"error":"the request could not be made"}`)}
	}
	for k, v := range pv {
		req.SetPathValue(k, v)
	}
	mw := &memWriter{h: http.Header{}}
	h(mw, req, repo)
	if mw.status == 0 {
		mw.status = http.StatusOK
	}
	return answer{Status: mw.status, Header: mw.h, Body: mw.buf.Bytes()}
}

// ok says the handler succeeded.
func (a answer) ok() bool { return a.Status >= 200 && a.Status < 300 }

// failure is a failed answer as a tool error: the sentence the owner UI
// shows. A 401 inside (the token was revoked while the call ran) says so.
func (a answer) failure() toolResult {
	if a.Status == http.StatusUnauthorized {
		return toolError(a.Status, "This token was revoked. Create a new one in Meetings settings.")
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(a.Body, &e) != nil || e.Error == "" {
		e.Error = "The request failed (" + http.StatusText(a.Status) + ")."
	}
	return toolError(a.Status, e.Error)
}

// decode reads a successful JSON answer into v.
func (a answer) decode(v any) error { return json.Unmarshal(a.Body, v) }

func query(kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// newMCPLimiter is the failed-authentication counter.
func newMCPLimiter() *lobby.Limiter { return lobby.NewLimiterOf(MCPFailsPerAddress, MCPFailWindow) }
