// Package muster is the plugin side of muster's plugin protocol
// (https://github.com/ZacxDev/muster/blob/main/docs/plugins.md). The plugin
// calls muster for everything; muster never calls the plugin.
package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// TokenPrefix starts every plugin token.
const TokenPrefix = "mpl_"

// Kind classifies a refused or failed call by its HTTP status.
type Kind string

const (
	KindAuth        Kind = "auth"         // 401: not a live plugin token (uninstalled, rotated away, unknown)
	KindCapability  Kind = "capability"   // 403: the token lacks the capability the route needs
	KindOff         Kind = "not_found"    // 404: plugins are off; on a redeem no such code; on notify and tasks no such person for this app
	KindBadRequest  Kind = "bad_request"  // 400 (and other 4xx)
	KindConflict    Kind = "conflict"     // 409
	KindRateLimited Kind = "rate_limited" // 429: see RetryAfter
	KindServer      Kind = "server"       // 5xx
	KindNetwork     Kind = "network"      // no answer at all
	KindProtocol    Kind = "protocol"     // an answer this client does not understand
)

// KindForStatus maps an HTTP status to its Kind.
func KindForStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized:
		return KindAuth
	case status == http.StatusForbidden:
		return KindCapability
	case status == http.StatusNotFound:
		return KindOff
	case status == http.StatusConflict:
		return KindConflict
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindBadRequest
	}
	return KindProtocol
}

// Error is a call muster refused, or that got no answer.
type Error struct {
	Kind       Kind
	Status     int           // 0 for KindNetwork
	Message    string        // muster's {"error"} text, when it sent one
	RetryAfter time.Duration // from Retry-After on a 429
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("muster: %s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("muster: %d %s: %s", e.Status, e.Kind, e.Message)
}

// Retryable says the same call is worth sending again. Kept, with
// RetryAfter, for 2b's knock notification: POST /plugin/v1/notify answers
// 429 with Retry-After (plugins.md, "Limits").
func (e *Error) Retryable() bool {
	return e.Kind == KindNetwork || e.Kind == KindServer || e.Kind == KindRateLimited
}

// AsError returns err as an *Error, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CheckBaseURL validates muster's address: https, except plain http to a
// loopback host or to a Kubernetes Service name (ClusterInternal); no user,
// path, query or fragment (a trailing "/" is fine). A plugin token can act
// for people, so it never crosses a network it does not stay inside in clear
// text: http is allowed only where the call cannot leave the host or the
// cluster, and a token pasted into the URL is refused rather than sent on
// every call.
func CheckBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		// Never echo the value: a mistaken one may hold the token.
		return "", errors.New("MUSTER_URL is not an absolute URL (its value is not shown, in case it holds a secret)")
	}
	h := u.Hostname()
	loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && (loopback || ClusterInternal(h))) {
		return "", fmt.Errorf("MUSTER_URL must be https:// (http:// only for localhost, 127.0.0.1, [::1] or a cluster Service name ending .svc or .svc.cluster.local)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("MUSTER_URL must carry no user, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("MUSTER_URL must be muster's address alone, with no path (e.g. https://muster.example.com)")
	}
	return u.Scheme + "://" + u.Host, nil
}

// clusterLabel is one DNS label of a Service name.
var clusterLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ClusterInternal says host is a Kubernetes Service name: one or more DNS
// labels followed by exactly ".svc" or ".svc.cluster.local" (case does not
// matter). Such a name resolves only through the cluster's own DNS, so a call
// to it stays on the cluster network. A trailing dot, an empty label, or
// ".svc" anywhere but the end (muster.svc.example.test) is not one: every
// label before the suffix must be a DNS label, so ".svc" alone and "a..svc"
// are refused.
func ClusterInternal(host string) bool {
	h := strings.ToLower(host)
	var rest string
	switch {
	case strings.HasSuffix(h, ".svc.cluster.local"):
		rest = strings.TrimSuffix(h, ".svc.cluster.local")
	case strings.HasSuffix(h, ".svc"):
		rest = strings.TrimSuffix(h, ".svc")
	default:
		return false
	}
	for _, l := range strings.Split(rest, ".") {
		if !clusterLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// Redact is a URL cut to its scheme and host, for a log line or an error: a
// path, query or user part could hold the token. A value that does not parse
// is not shown at all.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(an address not shown)"
	}
	return u.Scheme + "://" + u.Host
}

// Client calls muster's /plugin/v1 routes with one plugin token.
type Client struct {
	base  string
	token string
	// HTTP is used for every call.
	HTTP *http.Client
}

// New returns a Client. base must already pass CheckBaseURL.
func New(base, token string) (*Client, error) {
	if !strings.HasPrefix(token, TokenPrefix) || len(token) <= len(TokenPrefix) {
		return nil, fmt.Errorf("MUSTER_PLUGIN_TOKEN must start with %q", TokenPrefix)
	}
	return &Client{base: base, token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// ScopeServer is the scope of a server-installed app: an admin installed it
// once for the whole server on muster's /admin/apps, and its one token acts
// as nobody until a person's own hand-off names them (design: "Core seam:
// server-installed app plugins").
const ScopeServer = "server"

// SelfInfo is GET /plugin/v1/self's answer. For a server token it names no
// person: owner_id and owner_login are null, so this struct does not even
// have fields for them, and nothing the plugin decodes from /self can carry
// a muster user id or an email into its database.
type SelfInfo struct {
	PluginID     int64    `json:"plugin_id"`
	PluginName   string   `json:"plugin_name"`
	Scope        string   `json:"scope"`      // "server" for a server-installed app
	OwnerKind    string   `json:"owner_kind"` // "server" for a server-installed app
	Capabilities []string `json:"capabilities"`
	ActForHours  int      `json:"act_for_hours"`
}

// AppTarget is an app tab's hand-off target: {"app": {}}, or {"app": {"ref":
// "<ref>"}} when muster's page was opened with ?ref=. muster passes Ref
// through without reading it; it is a hint, never an authority.
type AppTarget struct {
	Ref string `json:"ref,omitempty"`
}

// HandoffTarget is what a hand-off code was minted for. muster sets exactly
// one of the two; Mirror is kept raw because an app plugin never accepts it.
type HandoffTarget struct {
	App    *AppTarget      `json:"app,omitempty"`
	Mirror json.RawMessage `json:"mirror,omitempty"`
}

// RedeemedOwner is the person a server app's hand-off names: a pairwise
// subject (random, 128 bits, different under every server app, stable for
// the install) and the account's display name ("" when it has none). Never
// a muster user id and never an email (design: fork 2).
type RedeemedOwner struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"display_name"`
}

// Redeemed is POST /plugin/v1/handoffs/redeem's answer to a server token.
// It has no field for a user id or an email: an answer that carried one
// anyway would be dropped by the decoder, never stored.
type Redeemed struct {
	Owner     RedeemedOwner `json:"owner"`
	Scope     string        `json:"scope"`
	Target    HandoffTarget `json:"target"`
	Purpose   string        `json:"purpose"`
	SteppedUp bool          `json:"stepped_up"`
	MintedAt  time.Time     `json:"minted_at"`
	ActUntil  time.Time     `json:"act_until"`
}

// Redeem is POST /plugin/v1/handoffs/redeem: it spends a hand-off code once.
// muster answers one 404 (KindOff) for a code that is unknown, spent,
// expired, another app's, or whose person was hidden, disabled or signed out
// everywhere since the mint. The caller must still check the purpose, the
// target and the scope.
func (c *Client) Redeem(ctx context.Context, code string) (Redeemed, error) {
	var out Redeemed
	_, err := c.do(ctx, http.MethodPost, "/plugin/v1/handoffs/redeem", nil, map[string]string{"code": code}, &out)
	return out, err
}

// Self is GET /plugin/v1/self.
func (c *Client) Self(ctx context.Context) (SelfInfo, error) {
	var out SelfInfo
	_, err := c.do(ctx, http.MethodGet, "/plugin/v1/self", nil, nil, &out)
	return out, err
}

// MaxCheckSubjects is the most subjects one POST /plugin/v1/owners/check
// takes; OwnersCheck sends more in batches of this size.
const MaxCheckSubjects = 200

var (
	subjectRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	refRE     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// ValidSubject is a subject's shape: 22 base64url characters, 128 bits.
func ValidSubject(s string) bool { return subjectRE.MatchString(s) }

// ErrBadSubject is a call for a person whose subject is not a subject's
// shape (empty, say). It is refused before anything is sent, so a request
// never goes out without its Muster-Act-For header.
var ErrBadSubject = errors.New("muster: not a subject (22 base64url characters)")

// The states owners/check answers a subject with.
const (
	// StateLive: the app may act for the person.
	StateLive = "live"
	// StateExpired: the person's 8 hours since they last opened the tab ran
	// out, and nothing else ended it. Their owner sessions end; their open
	// meetings keep running for the guests.
	StateExpired = "expired"
	// StateRevoked: the person hid the app, was disabled, or signed out
	// everywhere. Their sessions and open meetings end.
	StateRevoked = "revoked"
)

// OwnersCheck is POST /plugin/v1/owners/check: the state of each subject's
// grant, {"states": {"<subject>": "live"|"expired"|"revoked"}}, sent in
// batches of MaxCheckSubjects.
//
// 🔴 Within an answered batch it fails closed: a subject asked about but
// absent from "states", or answered with any other state, is StateRevoked,
// and a subject the answer names but that was not asked is dropped.
//
// A batch that is not answered is left out of the map entirely, and so
// decides nothing: a refusal, a 5xx, no answer, or a 2xx with no "states"
// object (the old {"live": [...]} shape, an empty body, {} or {"states":
// null}), which is KindProtocol and never "everyone revoked". The batches
// that were answered are still returned, with an error naming each failed
// one. A 401 stops at once and returns it.
func (c *Client) OwnersCheck(ctx context.Context, subjects []string) (map[string]string, error) {
	for _, s := range subjects {
		if !ValidSubject(s) {
			return nil, ErrBadSubject
		}
	}
	states := make(map[string]string, len(subjects))
	var first *Error
	var failed []string
	n := (len(subjects) + MaxCheckSubjects - 1) / MaxCheckSubjects
	for i, b := 0, 1; i < len(subjects); i, b = i+MaxCheckSubjects, b+1 {
		batch := subjects[i:min(i+MaxCheckSubjects, len(subjects))]
		var out struct {
			States *map[string]string `json:"states"`
		}
		st, err := c.do(ctx, http.MethodPost, "/plugin/v1/owners/check", nil, map[string]any{"subjects": batch}, &out)
		if err == nil && out.States == nil {
			err = &Error{Kind: KindProtocol, Status: st, Message: `owners/check answered no "states" object`}
		}
		if e := AsError(err); e != nil && e.Kind == KindAuth {
			return states, err
		}
		if err != nil {
			e := AsError(err)
			if e == nil {
				e = &Error{Kind: KindProtocol, Message: err.Error()}
			}
			if first == nil {
				first = e
			}
			failed = append(failed, fmt.Sprintf("batch %d of %d (%d subjects): %v", b, n, len(batch), e))
			continue
		}
		for _, s := range batch {
			st := (*out.States)[s]
			switch st {
			case StateLive, StateExpired:
			default:
				st = StateRevoked
			}
			states[s] = st
		}
	}
	if first != nil {
		e := *first
		e.Message = strings.Join(failed, "; ")
		return states, &e
	}
	return states, nil
}

// ActForHeader names the person a server token acts for on notify and
// tasks. The value is a subject, never a user id.
const ActForHeader = "Muster-Act-For"

func actFor(subject string) (http.Header, error) {
	if !ValidSubject(subject) {
		return nil, ErrBadSubject
	}
	return http.Header{ActForHeader: {subject}}, nil
}

// Notify is POST /plugin/v1/notify for the person subject names: a knock
// ("someone is waiting to join your meeting"; muster writes the text). It
// answers true for 202 (queued) and false for 204 (the person turned the
// toggle off or has no device: not an error). A 404 (KindOff) means muster
// cannot reach this person now (the grant ended); keep the lobby row.
func (c *Client) Notify(ctx context.Context, subject, ref string, count int) (bool, error) {
	h, err := actFor(subject)
	if err != nil {
		return false, err
	}
	if !refRE.MatchString(ref) || count < 1 || count > 99 {
		return false, errors.New("muster: a knock needs a ref of 1 to 64 base64url characters and a count of 1 to 99")
	}
	st, err := c.do(ctx, http.MethodPost, "/plugin/v1/notify", h, map[string]any{"kind": "knock", "ref": ref, "count": count}, nil)
	return st == http.StatusAccepted, err
}

// Withdraw is DELETE /plugin/v1/notify/{ref} for the person subject names.
// It answers true for 202 (a knock that was out is withdrawn) and false for
// 204 (none was out).
func (c *Client) Withdraw(ctx context.Context, subject, ref string) (bool, error) {
	h, err := actFor(subject)
	if err != nil {
		return false, err
	}
	if !refRE.MatchString(ref) {
		return false, errors.New("muster: a ref is 1 to 64 base64url characters")
	}
	st, err := c.do(ctx, http.MethodDelete, "/plugin/v1/notify/"+ref, h, nil, nil)
	return st == http.StatusAccepted, err
}

// TaskCreate is the task POST /plugin/v1/tasks files when the app has no
// open task under the key for this person.
type TaskCreate struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// TaskRequest is POST /plugin/v1/tasks's body for a server app: a key,
// and a create, a comment or both. A server app may not send route_to_chief
// or reference, so there are no fields for them.
type TaskRequest struct {
	Key     string      `json:"key"`
	Create  *TaskCreate `json:"create,omitempty"`
	Comment string      `json:"comment,omitempty"`
}

// TaskResult is POST /plugin/v1/tasks's answer. ID is nil when no task was
// found or filed.
type TaskResult struct {
	ID        *int64 `json:"id"`
	Created   bool   `json:"created"`
	Commented bool   `json:"commented"`
}

// FileTask is POST /plugin/v1/tasks for the person subject names (Make
// task). muster matches the key only among tasks this app filed for this
// person. A 404 (KindOff) means muster cannot reach this person now: the
// page asks them to open Meetings from muster again.
func (c *Client) FileTask(ctx context.Context, subject string, req TaskRequest) (TaskResult, error) {
	var out TaskResult
	h, err := actFor(subject)
	if err != nil {
		return out, err
	}
	_, err = c.do(ctx, http.MethodPost, "/plugin/v1/tasks", h, req, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, hdr http.Header, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net/http's error names the whole request URL; keep its scheme and
		// host only, so a base that slipped past CheckBaseURL with a token in
		// its path is never logged.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = Redact(ue.URL)
		}
		return 0, &Error{Kind: KindNetwork, Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.StatusCode, &Error{Kind: KindProtocol, Status: resp.StatusCode, Message: "unreadable answer: " + err.Error()}
			}
		}
		return resp.StatusCode, nil
	}
	e := &Error{Kind: KindForStatus(resp.StatusCode), Status: resp.StatusCode}
	var msg struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &msg) == nil && msg.Error != "" {
		e.Message = msg.Error
	} else {
		e.Message = strings.TrimSpace(string(raw))
	}
	if e.Kind == KindRateLimited {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		} else {
			e.RetryAfter = 60 * time.Second
		}
	}
	return resp.StatusCode, e
}
