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
	"time"
)

// AWS's published SigV4 example for S3 ("GET Object", Signature Version 4
// documentation, Amazon S3 API Reference).
func TestSigV4MatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	sign(req, nil, creds, "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization:\n got  %s\n want %s", got, want)
	}
}

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

type staticCreds Credentials

func (s staticCreds) Retrieve(context.Context) (Credentials, error) { return Credentials(s), nil }

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

func TestContainerCredentials(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "secret-token" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"AccessKeyId":"ASIA1","SecretAccessKey":"s","Token":"t","Expiration":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()
	env := map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL + "/creds", "AWS_CONTAINER_AUTHORIZATION_TOKEN": "secret-token"}
	c := &CredentialChain{Getenv: func(k string) string { return env[k] }}
	for i := 0; i < 3; i++ {
		got, err := c.Retrieve(context.Background())
		if err != nil || got.AccessKeyID != "ASIA1" || got.SessionToken != "t" {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("credentials not cached: %d calls", calls)
	}
	for _, full := range []string{"http://169.254.169.254/latest", "http://evil.example/creds", "file:///etc/passwd"} {
		if _, err := containerCredentialsURL(func(k string) string {
			return map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": full}[k]
		}); err == nil {
			t.Errorf("allowed %s", full)
		}
	}
	envCreds := &CredentialChain{Getenv: func(k string) string {
		return map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}[k]
	}}
	if got, _ := envCreds.Retrieve(context.Background()); got.AccessKeyID != "AK" {
		t.Fatal("env credentials")
	}
}
