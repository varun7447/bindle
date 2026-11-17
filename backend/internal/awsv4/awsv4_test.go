package awsv4

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// AWS's published SigV4 example for S3 ("GET Object", Signature Version 4
// documentation, Amazon S3 API Reference).
func TestSigV4MatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	Sign(req, nil, creds, "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization:\n got  %s\n want %s", got, want)
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
