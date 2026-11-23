// Package contacts is the meetings side of muster-contacts (muster's
// docs/design/contacts.md, phase 1b): an OPTIONAL client of the contacts
// plugin's internal, token-gated API, the "accepting" cache the guest card
// asks, and the outbox that delivers a guest's card submission.
//
// 🔴 It is optional. With MEETINGS_CONTACTS_URL and MEETINGS_CONTACTS_TOKEN
// both unset there is no Client, and the server mounts no card route, mints
// no card, starts no outbox worker and makes no call: meetings behaves
// exactly as it did before this package existed (internal/server's
// TestNoContactsURLChangesNothing pins it byte for byte).
//
// 🔴 Nothing here is on a call's path. Every call to contacts runs in a
// goroutine of its own with CallTimeout: the accepting cache answers from
// memory and refreshes in the background, and the outbox delivers from the
// database. Contacts down, slow or wrong leaves knock, admit, token and End
// exactly as fast as before.
//
// 🔴 No PII in logs. A submission's body (a guest's name and email) is held
// in the outbox and nowhere else; log lines carry event names, statuses and
// counts only, never a body, a name, an email, a subject or a token.
//
// The API is the contacts plugin's own contract, imported at a pinned
// version (github.com/ZacxDev/muster-contacts/contactsapi in go.mod): its
// routes, wire types and field checks are what meetings sends and checks,
// and contract_test.go runs its contract table against internal/contactstest,
// the fake every other test talks to.
package contacts

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster-contacts/contactsapi"
)

// The routes phase 1b calls (contactsapi's).
const (
	// PathOwners is GET /v1/owners/{owner}: 200 accepting, 404 not.
	PathOwners = contactsapi.RouteOwner
	// PathSubmissions is POST /v1/submissions: a guest's card.
	PathSubmissions = contactsapi.RouteSubmissions
)

// CallTimeout bounds every call to contacts (design: "with a 3 s timeout").
const CallTimeout = 3 * time.Second

// MinTokenBytes is the shortest MEETINGS_CONTACTS_TOKEN accepted (the
// design's "32+ random bytes"; the contacts plugin refuses a shorter one).
const MinTokenBytes = 32

// Client calls the contacts plugin's internal API. A nil *Client is "not
// configured", and every method of the server that holds one checks for
// nil before it does anything at all.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// ErrHalfConfigured is one of the two variables set without the other.
var ErrHalfConfigured = errors.New("MEETINGS_CONTACTS_URL and MEETINGS_CONTACTS_TOKEN are both set or both unset (one alone refuses to start)")

// hostPort is one host (a DNS name, an IPv4 or a bracketed IPv6 address)
// and an optional port.
var hostPort = regexp.MustCompile(`^(?i:(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.?|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// Configure reads MEETINGS_CONTACTS_URL and MEETINGS_CONTACTS_TOKEN.
// Both empty is (nil, nil): contacts is not configured. One alone is
// ErrHalfConfigured. The URL is http(s)://host[:port] with nothing else
// (the contacts plugin's INTERNAL listener, reached inside the cluster, so
// plain http is allowed to any host, as MEETINGS_LIVEKIT_API_URL is). The
// token is at least MinTokenBytes, has no whitespace, and is not muster's
// plugin token (musterToken): the two are separate secrets. No error ever
// shows the token.
func Configure(rawURL, rawToken, musterToken string) (*Client, error) {
	u, tok := strings.TrimSpace(rawURL), strings.TrimSpace(rawToken)
	if u == "" && tok == "" {
		return nil, nil
	}
	if u == "" || tok == "" {
		return nil, ErrHalfConfigured
	}
	base, err := checkURL(u)
	if err != nil {
		return nil, err
	}
	if len(tok) < MinTokenBytes || strings.ContainsAny(tok, " \t\r\n") {
		return nil, fmt.Errorf("MEETINGS_CONTACTS_TOKEN must be at least %d characters with no whitespace (its value is not shown)", MinTokenBytes)
	}
	if musterToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(strings.TrimSpace(musterToken))) == 1 {
		return nil, errors.New("MEETINGS_CONTACTS_TOKEN equals MUSTER_PLUGIN_TOKEN: the contacts API token is a separate secret (its value is not shown)")
	}
	return New(base, tok), nil
}

func checkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !hostPort.MatchString(u.Host) {
		return "", errors.New("MEETINGS_CONTACTS_URL is not an http(s)://host[:port] URL (the contacts plugin's internal listener)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("MEETINGS_CONTACTS_URL must be http:// or https://")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || (u.Path != "" && u.Path != "/") {
		return "", errors.New("MEETINGS_CONTACTS_URL must be a scheme and host alone: no user, path, query or fragment")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// New is a client of base (scheme://host[:port]) with token. Configure is
// the checked way in; tests and the e2e harness use New directly.
func New(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{
		Timeout: CallTimeout,
		// Never follow a redirect: the bearer would go with it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Host is the configured URL with nothing but its scheme and host, for the
// startup line (the token is never part of it).
func (c *Client) Host() string { return c.base }

// Answer is what one call to contacts came back with.
type Answer struct {
	Status int
	// RetryAfter is a 429's Retry-After, when it named whole seconds.
	RetryAfter time.Duration
}

// do sends one request. A network error or a timeout is an error, with no
// Answer; every HTTP status is an Answer.
func (c *Client) do(ctx context.Context, method, path, key string, body []byte) (Answer, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set(contactsapi.IdempotencyHeader, key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Never the error's text: url.Error quotes the URL, which is fine,
		// but nothing of the request ever reaches a log through here.
		return Answer{}, errCall
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16<<10))
	a := Answer{Status: resp.StatusCode}
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		a.RetryAfter = time.Duration(s) * time.Second
	}
	return a, nil
}

// errCall is any call that got no HTTP answer: refused, reset, timed out.
var errCall = errors.New("contacts did not answer")

// Accepting asks GET /v1/owners/{owner}: true on 200, false on 404 ("not
// accepting": hidden, revoked, or the owner's own switch off). Anything
// else is an error, and the cache keeps its last answer.
func (c *Client) Accepting(ctx context.Context, owner string) (bool, Answer, error) {
	a, err := c.do(ctx, http.MethodGet, PathOwners+url.PathEscape(owner), "", nil)
	if err != nil {
		return false, a, err
	}
	switch a.Status {
	case http.StatusOK:
		return true, a, nil
	case http.StatusNotFound:
		return false, a, nil
	}
	return false, a, fmt.Errorf("contacts answered %d", a.Status)
}

// Post sends one outbox row: body to path with its Idempotency-Key.
func (c *Client) Post(ctx context.Context, path, key string, body []byte) (Answer, error) {
	return c.do(ctx, http.MethodPost, path, key, body)
}
