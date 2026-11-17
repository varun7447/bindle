package control

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// KeyPrefix marks Bindle production ingest keys, so secret scanners and
// people can recognize them.
const KeyPrefix = "bd_live_"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newKey returns a new ingest key (256 random bits) and its display prefix.
func newKey() (key, display string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = KeyPrefix + strings.ToLower(b32.EncodeToString(b))
	return key, key[:len(KeyPrefix)+8], nil
}

// HashKey is how keys are stored and looked up. Keys carry 256 bits of
// entropy, so a plain SHA-256 is not guessable and needs no slow hash.
func HashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// newID returns prefix + 20 random hex characters, e.g. "key_3f9a...".
func newID(prefix string) (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
