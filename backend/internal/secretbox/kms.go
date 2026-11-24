package secretbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

// KMS calls the AWS KMS JSON API (TrentService.Encrypt / Decrypt) directly,
// signed with SigV4. Standard library only.
type KMS struct {
	KeyID    string // key ID, key ARN or alias ARN
	Region   string
	Creds    awsv4.CredentialsProvider
	HTTP     *http.Client
	Endpoint string // tests only
}

// APIError is a KMS error response. It never includes plaintext.
type APIError struct {
	Status int
	Type   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kms: HTTP %d %s: %s", e.Status, e.Type, e.Msg)
}

func (k *KMS) call(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	u := k.Endpoint
	if u == "" {
		u = "https://kms." + k.Region + ".amazonaws.com/"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+op)
	creds, err := k.Creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("kms: credentials: %w", err)
	}
	awsv4.Sign(req, body, creds, k.Region, "kms", time.Now().UTC())
	hc := k.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Type: e.Type, Msg: e.Message}
	}
	return json.Unmarshal(data, out)
}

// Encrypt implements Box: "kms1:" + the KMS ciphertext blob.
func (k *KMS) Encrypt(ctx context.Context, plaintext []byte, encCtx map[string]string) ([]byte, error) {
	if len(plaintext) == 0 || len(plaintext) > 4096 {
		return nil, errors.New("secretbox: plaintext must be 1-4096 bytes")
	}
	var out struct{ CiphertextBlob []byte }
	// []byte fields marshal as base64, as the KMS JSON API expects.
	if err := k.call(ctx, "Encrypt", map[string]any{"KeyId": k.KeyID, "Plaintext": plaintext, "EncryptionContext": encCtx}, &out); err != nil {
		return nil, err
	}
	if len(out.CiphertextBlob) == 0 {
		return nil, errors.New("kms: empty ciphertext")
	}
	return append([]byte(prefixKMS), out.CiphertextBlob...), nil
}

// Decrypt implements Box. KeyId is sent so KMS refuses ciphertexts made
// under any other key.
func (k *KMS) Decrypt(ctx context.Context, ciphertext []byte, encCtx map[string]string) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte(prefixKMS)) {
		return nil, errors.New("secretbox: not a KMS ciphertext")
	}
	var out struct{ Plaintext []byte }
	if err := k.call(ctx, "Decrypt", map[string]any{"KeyId": k.KeyID, "CiphertextBlob": ciphertext[len(prefixKMS):], "EncryptionContext": encCtx}, &out); err != nil {
		return nil, err
	}
	return out.Plaintext, nil
}
