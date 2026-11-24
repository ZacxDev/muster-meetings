// Package s3test is a fake S3-compatible store for tests and the e2e
// harness: one bucket, objects in memory, path-style requests, and every
// request's Signature Version 4 checked as the real store does (a request
// signed with another secret, for another region, or not at all is 403).
// GET answers Range as a store does (http.ServeContent: 206, 416); DELETE of
// a missing object is 204, as S3's is.
package s3test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/s3"
)

// The fake's bucket and key (New uses them).
const (
	Bucket    = "meetings-test"
	Region    = "us-east-1"
	AccessKey = "AKIAFAKEMEETINGS0001"
	SecretKey = "fake-s3-secret-for-tests-7d2a9c41"
)

// Fake is the store.
type Fake struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string][]byte
	calls   []string
	down    bool
}

// New starts a fake; Close it when done.
func New() *Fake {
	f := &Fake{objects: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Config is a client configuration for the fake.
func (f *Fake) Config() s3.Config {
	return s3.Config{Endpoint: f.URL, Bucket: Bucket, Region: Region, AccessKey: AccessKey, SecretKey: SecretKey}
}

// Put stores an object directly (as Egress's upload would).
func (f *Fake) Put(key string, body []byte) {
	f.mu.Lock()
	f.objects[key] = append([]byte(nil), body...)
	f.mu.Unlock()
}

// Has says key is stored.
func (f *Fake) Has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

// Keys is every stored key, sorted.
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetDown makes every request answer 503 (the store unreachable), or stop.
func (f *Fake) SetDown(on bool) { f.mu.Lock(); f.down = on; f.mu.Unlock() }

// Calls is every request, as "<METHOD> <key>[ <range>]", in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func s3err(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>fake</Message></Error>`, code)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(path, "/"+Bucket+"/")
	key := r.URL.Path
	if ok {
		key = strings.TrimPrefix(r.URL.Path, "/"+Bucket+"/")
	}
	f.mu.Lock()
	c := r.Method + " " + key
	if rg := r.Header.Get("Range"); rg != "" {
		c += " " + rg
	}
	f.calls = append(f.calls, c)
	down := f.down
	f.mu.Unlock()
	if down {
		s3err(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	if !ok || rest == "" {
		s3err(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if !f.verify(r, path) {
		s3err(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	f.mu.Lock()
	body, exists := f.objects[key]
	f.mu.Unlock()
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if !exists {
			s3err(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, len(body)))
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.Put(key, b)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3err(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// verify recomputes the request's signature with the fake's own secret, over
// the headers it says it signed, at the date it says, and compares.
func (f *Fake) verify(r *http.Request, path string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+AccessKey+"/") {
		return false
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || time.Since(at).Abs() > 15*time.Minute {
		return false
	}
	i := strings.Index(auth, "SignedHeaders=")
	if i < 0 {
		return false
	}
	signed := strings.SplitN(auth[i+len("SignedHeaders="):], ",", 2)[0]
	re, _ := http.NewRequest(r.Method, "http://"+r.Host+path, nil)
	for _, h := range strings.Split(signed, ";") {
		if h == "host" || h == "x-amz-date" || h == "x-amz-content-sha256" {
			continue
		}
		re.Header.Set(h, r.Header.Get(h))
	}
	s3.Sign(re, path, Region, AccessKey, SecretKey, at)
	return re.Header.Get("Authorization") == auth
}
