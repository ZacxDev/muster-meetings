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
	"strconv"
	"strings"
	"time"
)

// TokenPrefix starts every plugin token.
const TokenPrefix = "mpl_"

// Kind classifies a refused or failed call by its HTTP status.
type Kind string

const (
	KindAuth        Kind = "auth"         // 401: not a live plugin token (revoked, unknown, owner disabled)
	KindCapability  Kind = "capability"   // 403: the token lacks the capability the route needs
	KindOff         Kind = "not_found"    // 404: plugins are off on this server, or (on a redeem) no such code
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
// loopback host; no user, path, query or fragment (a trailing "/" is fine). A
// plugin token acts as its owner, so it never crosses a network in clear text,
// and a token pasted into the URL is refused rather than sent on every call.
func CheckBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		// Never echo the value: a mistaken one may hold the token.
		return "", errors.New("MUSTER_URL is not an absolute URL (its value is not shown, in case it holds a secret)")
	}
	h := u.Hostname()
	loopback := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("MUSTER_URL must be https:// (http:// only for localhost, 127.0.0.1 or [::1])")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("MUSTER_URL must carry no user, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("MUSTER_URL must be muster's address alone, with no path (e.g. https://muster.example.com)")
	}
	return u.Scheme + "://" + u.Host, nil
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

// SelfInfo is GET /plugin/v1/self's answer.
type SelfInfo struct {
	PluginID     int64    `json:"plugin_id"`
	PluginName   string   `json:"plugin_name"`
	OwnerID      int64    `json:"owner_id"`
	OwnerLogin   string   `json:"owner_login"`
	OwnerKind    string   `json:"owner_kind"`
	Capabilities []string `json:"capabilities"`
	DeliverTo    *string  `json:"deliver_to"`
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

// Redeemed is POST /plugin/v1/handoffs/redeem's answer.
type Redeemed struct {
	OwnerID   int64         `json:"owner_id"`
	Target    HandoffTarget `json:"target"`
	Purpose   string        `json:"purpose"`
	SteppedUp bool          `json:"stepped_up"`
	MintedAt  time.Time     `json:"minted_at"`
}

// Redeem is POST /plugin/v1/handoffs/redeem: it spends a hand-off code once.
// muster answers one 404 (KindOff) for a code that is unknown, spent,
// expired, or minted for another plugin. The caller must still check the
// purpose and target: muster answers a viewer code just as readily.
func (c *Client) Redeem(ctx context.Context, code string) (Redeemed, error) {
	var out Redeemed
	_, err := c.do(ctx, http.MethodPost, "/plugin/v1/handoffs/redeem", map[string]string{"code": code}, &out)
	return out, err
}

// Self is GET /plugin/v1/self.
func (c *Client) Self(ctx context.Context) (SelfInfo, error) {
	var out SelfInfo
	_, err := c.do(ctx, http.MethodGet, "/plugin/v1/self", nil, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
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
