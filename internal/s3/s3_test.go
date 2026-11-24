package s3_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ZacxDev/muster-meetings/internal/s3"
	"github.com/ZacxDev/muster-meetings/internal/s3test"
)

// The signer against AWS's own published example ("Signature Calculations
// for the Authorization Header", Example: GET Object): the literal signature
// AWS documents, not one this package computed.
func TestSignMatchesAWSDocumentedGetObjectExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	s3.Sign(req, "/test.txt", "us-east-1", "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	const want = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization:\n got %s\nwant %s", got, want)
	}
}

func client(t *testing.T, f *s3test.Fake) *s3.Client {
	t.Helper()
	c, err := s3.New(f.Config())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGetRangeSizeAndDeleteAgainstTheFake(t *testing.T) {
	f := s3test.New()
	defer f.Close()
	c := client(t, f)
	ctx := context.Background()
	const key = "meetings/SUBJ/MEET/recording-1.mp4"
	f.Put(key, []byte("0123456789abcdefghij"))

	resp, err := c.Get(ctx, key, "bytes=5-9")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != "56789" || resp.Header.Get("Content-Range") != "bytes 5-9/20" {
		t.Fatalf("range: %d %q %q", resp.StatusCode, b, resp.Header.Get("Content-Range"))
	}
	if n, err := c.Size(ctx, key); err != nil || n != 20 {
		t.Fatalf("size = %d, %v", n, err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if f.Has(key) {
		t.Fatal("deleted object still stored")
	}
	// A second delete of the same key is not an error (the sweeper repeats).
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
	if _, err := c.Get(ctx, key, ""); !s3.NotFound(err) {
		t.Fatalf("get of a missing object: %v", err)
	}
}

func TestTheFakeRefusesAWrongSecret(t *testing.T) {
	f := s3test.New()
	defer f.Close()
	cfg := f.Config()
	cfg.SecretKey = "not-the-fake-secret"
	c, _ := s3.New(cfg)
	f.Put("k", []byte("x"))
	_, err := c.Get(context.Background(), "k", "")
	var e *s3.Error
	if err == nil || !errorsAs(err, &e) || e.Status != 403 || e.Code != "SignatureDoesNotMatch" {
		t.Fatalf("a wrong secret: %v", err)
	}
}

func TestDeleteWithTheStoreDownIsAnError(t *testing.T) {
	f := s3test.New()
	defer f.Close()
	c := client(t, f)
	f.Put("k", []byte("x"))
	f.SetDown(true)
	if err := c.Delete(context.Background(), "k"); err == nil {
		t.Fatal("a delete with the store down said it worked")
	}
	if !f.Has("k") {
		t.Fatal("the object went")
	}
}

func TestValidRange(t *testing.T) {
	for in, want := range map[string]bool{"bytes=0-99": true, "bytes=100-": true, "bytes=-500": true, "bytes=-": false,
		"bytes=0-1,5-6": false, "items=0-1": false, "": false, "bytes=a-b": false} {
		if got := s3.ValidRange(in); got != want {
			t.Errorf("ValidRange(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewRefusesHalfAConfig(t *testing.T) {
	good := s3.Config{Endpoint: "http://minio.example.test:9000", Bucket: "meetings", AccessKey: "a", SecretKey: "b"}
	if _, err := s3.New(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*s3.Config){
		"no endpoint":  func(c *s3.Config) { c.Endpoint = "" },
		"no bucket":    func(c *s3.Config) { c.Bucket = "" },
		"no key":       func(c *s3.Config) { c.AccessKey = "" },
		"no secret":    func(c *s3.Config) { c.SecretKey = "" },
		"a path":       func(c *s3.Config) { c.Endpoint = "http://minio.example.test/x" },
		"ftp":          func(c *s3.Config) { c.Endpoint = "ftp://minio.example.test" },
		"upper bucket": func(c *s3.Config) { c.Bucket = "Meetings" },
	} {
		c := good
		mut(&c)
		if _, err := s3.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func errorsAs(err error, target **s3.Error) bool {
	e, ok := err.(*s3.Error)
	if ok {
		*target = e
	}
	return ok
}
