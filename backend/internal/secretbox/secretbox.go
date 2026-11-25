// Package secretbox encrypts the secrets Bindle keeps in the control-plane
// database: alert-channel targets (Slack and webhook URLs carry tokens) and
// signing secrets, and outcome-endpoint signing secrets.
//
// Two providers:
//
//   - kms: AWS KMS Encrypt and Decrypt on the secret itself (they are small,
//     under KMS's 4 KB limit), signed with SigV4. BINDLE_KMS_KEY_ID names the
//     key; credentials come from the environment or the ECS task role.
//   - local: AES-256-GCM with BINDLE_SECRETS_KEY (32 bytes, base64), for the
//     docker stack.
//
// Every ciphertext is bound to an encryption context naming the tenant, the
// purpose and the row (KMS encryption context, or GCM additional data), so a
// ciphertext copied to another row or tenant fails to decrypt. Ciphertexts
// carry a versioned provider prefix ("kms1:" or "loc1:").
//
// Without a provider, Encrypt fails (ErrNotConfigured): callers refuse to
// create the resource instead of storing plaintext.
package secretbox

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

// Purposes, used in the encryption context (and in IAM conditions).
const (
	PurposeAlertTarget   = "alert_target"
	PurposeAlertSecret   = "alert_secret"
	PurposeOutcomeSecret = "outcome_secret"
)

// Encryption-context keys. Plain names without ':' so they can be used in
// IAM condition keys (kms:EncryptionContext:bindle_purpose).
const (
	KeyTenant   = "bindle_tenant"
	KeyPurpose  = "bindle_purpose"
	KeyResource = "bindle_resource"
)

// ErrNotConfigured means no provider is set up.
var ErrNotConfigured = errors.New("secretbox: no secrets key configured (set BINDLE_KMS_KEY_ID or BINDLE_SECRETS_KEY)")

// Box encrypts and decrypts small secrets.
type Box interface {
	Encrypt(ctx context.Context, plaintext []byte, encCtx map[string]string) ([]byte, error)
	Decrypt(ctx context.Context, ciphertext []byte, encCtx map[string]string) ([]byte, error)
}

// Context is the encryption context for one secret.
func Context(tenant, purpose, resource string) map[string]string {
	return map[string]string{KeyTenant: tenant, KeyPurpose: purpose, KeyResource: resource}
}

const (
	prefixKMS   = "kms1:"
	prefixLocal = "loc1:"
)

// FromEnv returns the configured box: KMS when BINDLE_KMS_KEY_ID is set,
// else local AES-GCM when BINDLE_SECRETS_KEY is set, else a box that
// refuses to encrypt. Decryption results are cached briefly.
func FromEnv(getenv func(string) string) (Box, error) {
	if id := getenv("BINDLE_KMS_KEY_ID"); id != "" {
		region := getenv("AWS_REGION")
		if region == "" {
			return nil, errors.New("secretbox: AWS_REGION is required with BINDLE_KMS_KEY_ID")
		}
		return NewCache(&KMS{KeyID: id, Region: region, Creds: &awsv4.CredentialChain{Getenv: getenv}}, 5*time.Minute, 1024), nil
	}
	if k := getenv("BINDLE_SECRETS_KEY"); k != "" {
		l, err := NewLocal(k)
		if err != nil {
			return nil, err
		}
		return NewCache(l, 5*time.Minute, 1024), nil
	}
	return Disabled{}, nil
}

// Disabled refuses to encrypt or decrypt.
type Disabled struct{}

func (Disabled) Encrypt(context.Context, []byte, map[string]string) ([]byte, error) {
	return nil, ErrNotConfigured
}

func (Disabled) Decrypt(context.Context, []byte, map[string]string) ([]byte, error) {
	return nil, ErrNotConfigured
}

// EncryptOnly wraps a Box for a service that may only encrypt (the API):
// Decrypt always fails, even if the key would allow it.
type EncryptOnly struct{ Box Box }

// ErrDecryptNotAllowed is returned by EncryptOnly.Decrypt.
var ErrDecryptNotAllowed = errors.New("secretbox: this service may not decrypt")

func (e EncryptOnly) Encrypt(ctx context.Context, plaintext []byte, encCtx map[string]string) ([]byte, error) {
	if e.Box == nil {
		return nil, ErrNotConfigured
	}
	return e.Box.Encrypt(ctx, plaintext, encCtx)
}

func (EncryptOnly) Decrypt(context.Context, []byte, map[string]string) ([]byte, error) {
	return nil, ErrDecryptNotAllowed
}

// ---- local (AES-256-GCM) ---------------------------------------------------------

// Local encrypts with a key held by the process (development only).
type Local struct{ aead cipher.AEAD }

// NewLocal parses a base64 32-byte key.
func NewLocal(b64 string) (*Local, error) {
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) != 32 {
		return nil, errors.New("secretbox: BINDLE_SECRETS_KEY must be 32 bytes, base64-encoded")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Local{aead: aead}, nil
}

// Encrypt implements Box: "loc1:" + nonce + sealed, with the context as AAD.
func (l *Local) Encrypt(_ context.Context, plaintext []byte, encCtx map[string]string) ([]byte, error) {
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(prefixLocal), nonce...)
	return l.aead.Seal(out, nonce, plaintext, canonical(encCtx)), nil
}

// Decrypt implements Box.
func (l *Local) Decrypt(_ context.Context, ciphertext []byte, encCtx map[string]string) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte(prefixLocal)) {
		return nil, errors.New("secretbox: not a local ciphertext")
	}
	b := ciphertext[len(prefixLocal):]
	n := l.aead.NonceSize()
	if len(b) < n {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	pt, err := l.aead.Open(nil, b[:n], b[n:], canonical(encCtx))
	if err != nil {
		return nil, errors.New("secretbox: decryption failed")
	}
	return pt, nil
}

// canonical encodes a context deterministically: sorted "k=v" lines with
// lengths, so no two different contexts encode the same.
func canonical(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		fmt.Fprintf(&b, "%d:%s=%d:%s\n", len(k), k, len(m[k]), m[k])
	}
	return b.Bytes()
}

// ---- cache -----------------------------------------------------------------------

// Cache wraps a box and remembers decryptions for a short time, so webhook
// verification and alert delivery don't call KMS on every request.
type Cache struct {
	Box Box
	TTL time.Duration
	Max int
	Now func() time.Time

	mu      sync.Mutex
	entries map[[32]byte]cacheEntry
}

type cacheEntry struct {
	plain []byte
	at    time.Time
}

// NewCache returns a caching box.
func NewCache(b Box, ttl time.Duration, max int) *Cache {
	return &Cache{Box: b, TTL: ttl, Max: max}
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Encrypt passes through.
func (c *Cache) Encrypt(ctx context.Context, plaintext []byte, encCtx map[string]string) ([]byte, error) {
	return c.Box.Encrypt(ctx, plaintext, encCtx)
}

// Decrypt returns a cached result when the same ciphertext and context were
// decrypted within the TTL.
func (c *Cache) Decrypt(ctx context.Context, ciphertext []byte, encCtx map[string]string) ([]byte, error) {
	h := sha256.New()
	h.Write(ciphertext)
	h.Write([]byte{0})
	h.Write(canonical(encCtx))
	var key [32]byte
	copy(key[:], h.Sum(nil))
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < c.TTL {
		c.mu.Unlock()
		return append([]byte(nil), e.plain...), nil
	}
	c.mu.Unlock()
	pt, err := c.Box.Decrypt(ctx, ciphertext, encCtx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= c.Max {
		c.entries = map[[32]byte]cacheEntry{} // bounded: start over when full
	}
	c.entries[key] = cacheEntry{plain: append([]byte(nil), pt...), at: now}
	c.mu.Unlock()
	return pt, nil
}

// Len reports the number of cached entries (tests).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
