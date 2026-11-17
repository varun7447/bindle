package session

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims are the verified claims of a Cognito access token that Bindle uses.
type Claims struct {
	Subject  string `json:"sub"`
	Username string `json:"username"`
	ClientID string `json:"client_id"`
	TokenUse string `json:"token_use"`
	Issuer   string `json:"iss"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
}

// Verifier checks Cognito access tokens: RS256 signatures against the user
// pool's published keys (JWKS), plus issuer, token_use, client_id and
// expiry. Standard library only.
type Verifier struct {
	Issuer   string // https://cognito-idp.<region>.amazonaws.com/<user pool id>
	ClientID string // the console's app client ID
	JWKSURL  string // default Issuer + "/.well-known/jwks.json"
	HTTP     *http.Client
	Now      func() time.Time
	Leeway   time.Duration // clock skew allowance; default 60s

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// ErrInvalidToken covers every verification failure; callers return 401.
var ErrInvalidToken = errors.New("session: invalid token")

const maxTokenBytes = 8 << 10

// Verify returns the token's claims if, and only if, it is valid.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	var c Claims
	if v.Issuer == "" || v.ClientID == "" {
		return c, fmt.Errorf("%w: verifier not configured", ErrInvalidToken)
	}
	if len(token) > maxTokenBytes {
		return c, fmt.Errorf("%w: too long", ErrInvalidToken)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, fmt.Errorf("%w: malformed", ErrInvalidToken)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return c, err
	}
	// Only RS256: refuses "none", HMAC-with-public-key confusion, etc.
	if hdr.Alg != "RS256" || hdr.Kid == "" {
		return c, fmt.Errorf("%w: unsupported alg %q", ErrInvalidToken, hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return c, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, fmt.Errorf("%w: signature encoding", ErrInvalidToken)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return c, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	if err := decodeSegment(parts[1], &c); err != nil {
		return c, err
	}
	now := v.now()
	leeway := v.Leeway
	if leeway <= 0 {
		leeway = 60 * time.Second
	}
	switch {
	case c.Issuer != v.Issuer:
		return Claims{}, fmt.Errorf("%w: issuer", ErrInvalidToken)
	case c.TokenUse != "access":
		return Claims{}, fmt.Errorf("%w: token_use %q", ErrInvalidToken, c.TokenUse)
	case c.ClientID != v.ClientID:
		return Claims{}, fmt.Errorf("%w: client_id", ErrInvalidToken)
	case c.Subject == "":
		return Claims{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	case c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(leeway)):
		return Claims{}, fmt.Errorf("%w: expired", ErrInvalidToken)
	case c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(leeway)):
		return Claims{}, fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	return c, nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return fmt.Errorf("%w: segment encoding", ErrInvalidToken)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%w: segment json", ErrInvalidToken)
	}
	return nil
}

const (
	jwksMaxAge     = time.Hour
	jwksMinRefetch = time.Minute // unknown kids can't make us hammer the endpoint
)

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && now.Sub(v.fetchedAt) < jwksMaxAge {
		return k, nil
	}
	if v.keys == nil || now.Sub(v.fetchedAt) >= jwksMinRefetch {
		keys, err := v.fetchJWKS(ctx)
		if err != nil {
			if k, ok := v.keys[kid]; ok { // keep serving known keys if the endpoint is down
				return k, nil
			}
			return nil, fmt.Errorf("%w: fetching keys: %v", ErrInvalidToken, err)
		}
		v.keys, v.fetchedAt = keys, now
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown key id", ErrInvalidToken)
}

func (v *Verifier) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	u := v.JWKSURL
	if u == "" {
		u = strings.TrimRight(v.Issuer, "/") + "/.well-known/jwks.json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty, Kid, N, E, Use, Alg string
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable RSA keys")
	}
	return out, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
