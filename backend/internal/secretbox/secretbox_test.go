package secretbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

var testKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

func TestLocalRoundTripAndBinding(t *testing.T) {
	l, err := NewLocal(testKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ec := Context("ten_a", PurposeAlertSecret, "ch_1")
	ct, err := l.Encrypt(ctx, []byte("whsec_abc"), ec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ct, []byte("loc1:")) || bytes.Contains(ct, []byte("whsec_abc")) {
		t.Fatalf("ciphertext %q", ct)
	}
	pt, err := l.Decrypt(ctx, ct, ec)
	if err != nil || string(pt) != "whsec_abc" {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	for name, other := range map[string]map[string]string{
		"tenant":   Context("ten_b", PurposeAlertSecret, "ch_1"),
		"purpose":  Context("ten_a", PurposeAlertTarget, "ch_1"),
		"resource": Context("ten_a", PurposeAlertSecret, "ch_2"),
	} {
		if _, err := l.Decrypt(ctx, ct, other); err == nil {
			t.Errorf("decrypted with a different %s", name)
		}
	}
	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 1
	if _, err := l.Decrypt(ctx, bad, ec); err == nil {
		t.Error("tampered ciphertext decrypted")
	}
	if _, err := NewLocal("c2hvcnQ="); err == nil {
		t.Error("short key accepted")
	}
}

func TestDisabledFailsClosed(t *testing.T) {
	b, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Encrypt(context.Background(), []byte("x"), nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
	if _, err := FromEnv(func(k string) string {
		if k == "BINDLE_KMS_KEY_ID" {
			return "alias/bindle-secrets"
		}
		return ""
	}); err == nil {
		t.Error("KMS without AWS_REGION accepted")
	}
}

type countingBox struct {
	Box
	n atomic.Int32
}

func (c *countingBox) Decrypt(ctx context.Context, ct []byte, ec map[string]string) ([]byte, error) {
	c.n.Add(1)
	return c.Box.Decrypt(ctx, ct, ec)
}

func TestCache(t *testing.T) {
	l, _ := NewLocal(testKey)
	inner := &countingBox{Box: l}
	now := time.Unix(1000, 0)
	c := &Cache{Box: inner, TTL: time.Minute, Max: 2, Now: func() time.Time { return now }}
	ctx := context.Background()
	ec := Context("t", PurposeOutcomeSecret, "oep_1")
	ct, _ := c.Encrypt(ctx, []byte("obsec_1"), ec)
	for i := 0; i < 3; i++ {
		if pt, err := c.Decrypt(ctx, ct, ec); err != nil || string(pt) != "obsec_1" {
			t.Fatal(pt, err)
		}
	}
	if inner.n.Load() != 1 {
		t.Fatalf("decrypt calls = %d, want 1", inner.n.Load())
	}
	if _, err := c.Decrypt(ctx, ct, Context("t", PurposeOutcomeSecret, "oep_2")); err == nil {
		t.Fatal("cache served a different context")
	}
	now = now.Add(2 * time.Minute)
	_, _ = c.Decrypt(ctx, ct, ec)
	if inner.n.Load() != 3 {
		t.Fatalf("expired entry not refreshed: %d calls", inner.n.Load())
	}
	for i := 0; i < 5; i++ {
		x, _ := c.Encrypt(ctx, []byte("s"), Context("t", "p", string(rune('a'+i))))
		_, _ = c.Decrypt(ctx, x, Context("t", "p", string(rune('a'+i))))
	}
	if c.Len() > 2 {
		t.Fatalf("cache grew to %d entries, max 2", c.Len())
	}
}

type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (awsv4.Credentials, error) {
	return awsv4.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "test-secret-not-real", SessionToken: "tok"}, nil
}

// fakeKMS checks request shape and signing, and "encrypts" by reversing the
// plaintext and storing the context alongside.
func fakeKMS(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/us-west-2/kms/aws4_request") ||
			r.Header.Get("X-Amz-Date") == "" || r.Header.Get("X-Amz-Security-Token") != "tok" {
			t.Errorf("unsigned request: %q", auth)
		}
		if r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		var in struct {
			KeyId             string
			Plaintext         []byte
			CiphertextBlob    []byte
			EncryptionContext map[string]string
		}
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatal(err)
		}
		if in.KeyId != "arn:aws:kms:us-west-2:111122223333:alias/bindle-secrets" || in.EncryptionContext[KeyTenant] == "" {
			t.Errorf("request %s", body)
		}
		ecJSON, _ := json.Marshal(in.EncryptionContext)
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.Encrypt":
			blob := append(append([]byte(nil), ecJSON...), '|')
			blob = append(blob, in.Plaintext...)
			_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": blob, "KeyId": in.KeyId})
		case "TrentService.Decrypt":
			parts := bytes.SplitN(in.CiphertextBlob, []byte("|"), 2)
			if len(parts) != 2 || !bytes.Equal(parts[0], ecJSON) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"__type":"InvalidCiphertextException","message":"bad"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": parts[1]})
		default:
			t.Errorf("target %q", r.Header.Get("X-Amz-Target"))
		}
	}))
}

func TestKMSProvider(t *testing.T) {
	srv := fakeKMS(t)
	defer srv.Close()
	k := &KMS{KeyID: "arn:aws:kms:us-west-2:111122223333:alias/bindle-secrets", Region: "us-west-2", Creds: staticCreds{}, Endpoint: srv.URL}
	ctx := context.Background()
	ec := Context("ten_a", PurposeAlertTarget, "ch_9")
	ct, err := k.Encrypt(ctx, []byte("https://hooks.slack.com/services/T/B/X"), ec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(ct, []byte("kms1:")) {
		t.Fatalf("prefix: %q", ct[:5])
	}
	pt, err := k.Decrypt(ctx, ct, ec)
	if err != nil || string(pt) != "https://hooks.slack.com/services/T/B/X" {
		t.Fatalf("round trip %q %v", pt, err)
	}
	var apiErr *APIError
	if _, err := k.Decrypt(ctx, ct, Context("ten_b", PurposeAlertTarget, "ch_9")); !errors.As(err, &apiErr) || apiErr.Type != "InvalidCiphertextException" {
		t.Fatalf("context mismatch: %v", err)
	}
	if _, err := k.Decrypt(ctx, []byte("loc1:x"), ec); err == nil {
		t.Fatal("accepted a local ciphertext")
	}
}
