// Package s3 is the plugin's client for the recordings bucket: an
// S3-compatible store (the homelab MinIO, or any other) reached with ONE
// bucket-scoped access key. It does only what the plugin needs (design:
// "The object store"): read an object (with Range, for seeking and frame
// sampling), read its size, and delete it (the retention sweeper). Egress
// writes the objects; the plugin never uploads.
//
// Requests are signed with AWS Signature Version 4 (header form) and
// addressed path-style (<endpoint>/<bucket>/<key>), which MinIO and every
// S3-compatible store answer. The payload of every request is empty, so its
// hash is the empty string's.
//
// 🔴 The secret key is never logged and never leaves the process except as
// a signature. Errors name a status and S3's error code only, never a key or
// a URL with a query.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is the bucket and its key (MEETINGS_S3_*).
type Config struct {
	// Endpoint is the store's base URL, scheme and host only, e.g.
	// http://minio.example.test:9000.
	Endpoint string
	Bucket   string
	// Region is the signing region; MinIO's default is us-east-1.
	Region    string
	AccessKey string
	SecretKey string
}

// ErrConfig is a configuration that cannot work.
var ErrConfig = errors.New("s3: the endpoint, bucket, access key and secret key are all needed")

// bucketRE is an S3 bucket name (3–63 lowercase letters, digits, '.', '-').
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Client signs and sends the plugin's requests.
type Client struct {
	cfg  Config
	base *url.URL
	// HTTP is used for every call; Now is the clock (default time.Now).
	HTTP *http.Client
	Now  func() time.Time
	// CallTimeout bounds a whole Size or Delete (default DefaultCallTimeout).
	CallTimeout time.Duration
}

// bounded is ctx cut to CallTimeout, for a call that is not a stream.
func (c *Client) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.CallTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.CallTimeout)
}

// New checks cfg and returns a client. The endpoint must be http(s) with a
// host and no path, query or user.
func New(cfg Config) (*Client, error) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Region = strings.TrimSpace(cfg.Region)
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, ErrConfig
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("s3: the endpoint is an http(s)://host[:port] URL with no path, query or user")
	}
	if !bucketRE.MatchString(cfg.Bucket) {
		return nil, errors.New("s3: the bucket is not an S3 bucket name (3 to 63 lowercase letters, digits, '.' and '-')")
	}
	u.Path = ""
	// No overall timeout on the client: a recording streamed to a viewer can
	// take as long as they watch. Instead: connecting, TLS and the answer's
	// headers are bounded (a store that never answers), and every call that
	// is not a stream (Size, Delete) is bounded as a whole by CallTimeout.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: DefaultHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	}
	return &Client{cfg: cfg, base: u, HTTP: &http.Client{Transport: tr}, Now: time.Now, CallTimeout: DefaultCallTimeout}, nil
}

// The store's timeouts: an answer's headers (every call, the streams
// included), and a whole call that is not a stream.
const (
	DefaultHeaderTimeout = 30 * time.Second
	DefaultCallTimeout   = 60 * time.Second
)

// Bucket is the bucket's name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// Endpoint is the store's base URL (scheme and host).
func (c *Client) Endpoint() string { return c.base.String() }

// Region is the signing region.
func (c *Client) Region() string { return c.cfg.Region }

// Error is a store's refusal: its HTTP status and S3's error code (NoSuchKey,
// AccessDenied, …). Never the request's URL or the key.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string { return fmt.Sprintf("s3: %d %s", e.Status, e.Code) }

// NotFound says the object does not exist.
func NotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Code == "NoSuchKey")
}

// ObjectPath is key's path in the bucket, path-style: /<bucket>/<key>, each
// segment URI-encoded as SigV4 wants it.
func (c *Client) ObjectPath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return "/" + c.cfg.Bucket + "/" + strings.Join(segs, "/")
}

// emptySHA256 is the SHA-256 of an empty payload.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// newRequest builds a signed request for method on key, with extra headers
// (Range) included in the signature.
func (c *Client) newRequest(ctx context.Context, method, key string, extra http.Header) (*http.Request, error) {
	u := *c.base
	u.Path = "" // set Opaque-free: RawPath carries the encoded form
	p := c.ObjectPath(key)
	full := u.Scheme + "://" + u.Host + p
	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	Sign(req, p, c.cfg.Region, c.cfg.AccessKey, c.cfg.SecretKey, c.Now().UTC())
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL carries no secret (header signing), but say only what
		// kind of failure it was.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("s3: %s: %w", req.Method, ue.Err)
		}
		return nil, fmt.Errorf("s3: %s: %w", req.Method, err)
	}
	return resp, nil
}

// errorOf reads a refusal's S3 error code and closes the body.
func errorOf(resp *http.Response) error {
	defer resp.Body.Close()
	e := &Error{Status: resp.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var v struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(raw, &v) == nil {
		e.Code = v.Code
	}
	if e.Code == "" && resp.StatusCode == http.StatusNotFound {
		e.Code = "NoSuchKey"
	}
	return e
}

// rangeRE is the one Range form passed on: a single byte range.
var rangeRE = regexp.MustCompile(`^bytes=(\d{1,19})?-(\d{1,19})?$`)

// ValidRange says rng is one byte range ("bytes=0-99", "bytes=100-",
// "bytes=-500"). Anything else (several ranges, another unit) is not passed
// to the store; the caller answers the whole object instead.
func ValidRange(rng string) bool {
	m := rangeRE.FindStringSubmatch(rng)
	return m != nil && (m[1] != "" || m[2] != "")
}

// Get reads key, the byte range rng when it is one (ValidRange; "" for the
// whole object). It answers the store's response as is for 200, 206 and 416
// (the caller copies status, headers and body, and closes it); any other
// status is an *Error.
func (c *Client) Get(ctx context.Context, key, rng string) (*http.Response, error) {
	h := http.Header{}
	if rng != "" && ValidRange(rng) {
		h.Set("Range", rng)
	}
	req, err := c.newRequest(ctx, http.MethodGet, key, h)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
		return resp, nil
	}
	return nil, errorOf(resp)
}

// Size is key's size in bytes.
func (c *Client) Size(ctx context.Context, key string) (int64, error) {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodHead, key, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return 0, &Error{Status: resp.StatusCode, Code: map[bool]string{true: "NoSuchKey"}[resp.StatusCode == http.StatusNotFound]}
	}
	resp.Body.Close()
	return resp.ContentLength, nil
}

// Delete deletes key. 🔴 An object that is already gone is not an error: the
// outcome is the same, so the sweeper can repeat a delete it is unsure of.
func (c *Client) Delete(ctx context.Context, key string) error {
	ctx, cancel := c.bounded(ctx)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		resp.Body.Close()
		return nil
	}
	return errorOf(resp)
}

// ---- Signature Version 4 ----

// Sign signs req in place (header form) for the S3 service: it sets Host,
// X-Amz-Date and X-Amz-Content-Sha256 (the empty payload's hash), and
// Authorization over every header req carries (Host, the x-amz-* ones, and
// any the caller set, e.g. Range). path is the request's path, already
// encoded as it is sent.
func Sign(req *http.Request, path, region, accessKey, secretKey string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	host := req.URL.Host
	req.Host = host

	names := []string{"host"}
	vals := map[string]string{"host": host}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" {
			continue
		}
		names = append(names, lk)
		vals[lk] = strings.Join(vs, ",")
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + strings.TrimSpace(vals[n]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), ch.String(), signed, emptySHA256}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	var ks []string
	for k := range q {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's URI encoding: every byte but A–Z a–z 0–9 - _ . ~ is
// %XX (upper-case hex).
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
