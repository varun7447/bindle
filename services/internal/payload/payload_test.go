package payload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bindle-ai/bindle/services/internal/awsv4"
)

func TestKeyValidation(t *testing.T) {
	k, err := Key("tn_1", "2026.11.17", "abc", "def", "input")
	if err != nil || k != "tenants/tn_1/2026.11.17/abc/def/input" {
		t.Fatalf("%q %v", k, err)
	}
	for _, bad := range [][5]string{
		{"..", "d", "t", "s", "input"}, {"a/b", "d", "t", "s", "input"}, {"t", "d", "t", "s", ""},
		{"t", "d", "%2e%2e", "s", "input"}, {"t", "d", "t", "s\x00", "input"}, {"t", ".", "t", "s", "input"},
	} {
		if _, err := Key(bad[0], bad[1], bad[2], bad[3], bad[4]); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	s := &FileStore{Dir: t.TempDir()}
	if err := s.Put(ctx, "tenants/t/d/x/y/input", []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	b, trunc, err := s.Get(ctx, "tenants/t/d/x/y/input", 5)
	if err != nil || string(b) != "hello" || !trunc {
		t.Fatalf("%q %v %v", b, trunc, err)
	}
	b, trunc, _ = s.Get(ctx, "tenants/t/d/x/y/input", 100)
	if string(b) != "hello world" || trunc {
		t.Fatal("full read")
	}
	if _, _, err := s.Get(ctx, "tenants/t/d/x/y/missing", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Put(ctx, "../escape", []byte("x")); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestFromURL(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if s, err := FromURL("", env(nil)); s != nil || err != nil {
		t.Fatal("empty should disable")
	}
	if s, err := FromURL("file:///data/payloads", env(nil)); err != nil || s.(*FileStore).Dir != "/data/payloads" {
		t.Fatal(err)
	}
	s, err := FromURL("s3://my-bucket/payloads/", env(map[string]string{"AWS_REGION": "us-west-2"}))
	if err != nil || s.(*S3Store).Bucket != "my-bucket" || s.(*S3Store).Prefix != "payloads" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{"s3://bucket", "file://relative/dir", "file:relative", "ftp://x"} {
		if _, err := FromURL(bad, env(nil)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeS3 checks that requests are signed and stores objects in memory.
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
	auth []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") || r.Header.Get("X-Amz-Security-Token") != "tok" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
		return
	}
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[r.URL.Path] = b
	case http.MethodGet:
		b, ok := f.objs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		_, _ = w.Write(b)
	}
}

type staticCreds awsv4.Credentials

func (s staticCreds) Retrieve(context.Context) (awsv4.Credentials, error) {
	return awsv4.Credentials(s), nil
}

func TestS3StoreAgainstFake(t *testing.T) {
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	s := &S3Store{Bucket: "b", Prefix: "payloads", Region: "us-west-2", Endpoint: srv.URL,
		Creds: staticCreds{AccessKeyID: "AKID", SecretAccessKey: "secret", SessionToken: "tok"}}
	if err := s.Put(ctx, "tenants/t/d/tr/st/input", []byte("big payload")); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objs["/b/payloads/tenants/t/d/tr/st/input"]; !ok {
		t.Fatalf("stored at %v", f.objs)
	}
	b, trunc, err := s.Get(ctx, "tenants/t/d/tr/st/input", 3)
	if err != nil || string(b) != "big" || !trunc {
		t.Fatalf("%q %v %v", b, trunc, err)
	}
	if _, _, err := s.Get(ctx, "tenants/t/d/tr/st/nope", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	bad := &S3Store{Bucket: "b", Region: "us-west-2", Endpoint: srv.URL, Creds: staticCreds{AccessKeyID: "OTHER", SecretAccessKey: "x"}}
	if err := bad.Put(ctx, "k", []byte("x")); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("denied: %v", err)
	}
}
