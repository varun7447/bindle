// Package awsv4 signs AWS API requests with Signature Version 4 and
// resolves credentials from the environment or the ECS task role. Standard
// library only; the S3 example from AWS's documentation is a test vector.
package awsv4

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- SigV4 ---------------------------------------------------------------------

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Sign adds SigV4 headers to req. body nil means an empty payload.
func Sign(req *http.Request, body []byte, c Credentials, region, service string, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	payloadHash := emptySHA256
	if len(body) > 0 {
		h := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(h[:])
	}
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "x-amz-date" || lk == "x-amz-content-sha256" || lk == "x-amz-security-token" || lk == "range" || lk == "content-type" || lk == "x-amz-target" {
			headers[lk] = strings.Join(v, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash,
	}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is RFC 3986 encoding as SigV4 requires (unreserved kept).
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// URIEncodePath encodes each path segment for a canonical URI.
func URIEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// ---- credentials ---------------------------------------------------------------

// Credentials are AWS access keys.
type Credentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expires                                    time.Time // zero: static
}

// CredentialsProvider returns current credentials.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// CredentialChain tries environment variables, then the ECS container
// credentials endpoint (the task role), caching until shortly before expiry.
type CredentialChain struct {
	Getenv func(string) string
	HTTP   *http.Client

	mu     sync.Mutex
	cached Credentials
}

// Retrieve implements CredentialsProvider.
func (c *CredentialChain) Retrieve(ctx context.Context) (Credentials, error) {
	if id, secret := c.Getenv("AWS_ACCESS_KEY_ID"), c.Getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: c.Getenv("AWS_SESSION_TOKEN")}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached.AccessKeyID != "" && time.Until(c.cached.Expires) > 5*time.Minute {
		return c.cached, nil
	}
	u, err := containerCredentialsURL(c.Getenv)
	if err != nil {
		return Credentials{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Credentials{}, err
	}
	if tok := c.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"); tok != "" {
		req.Header.Set("Authorization", tok)
	} else if f := c.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return Credentials{}, err
		}
		req.Header.Set("Authorization", strings.TrimSpace(string(b)))
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("container credentials: HTTP %d", resp.StatusCode)
	}
	var body struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      time.Time
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return Credentials{}, err
	}
	if body.AccessKeyID == "" || body.SecretAccessKey == "" {
		return Credentials{}, errors.New("container credentials: empty")
	}
	c.cached = Credentials{AccessKeyID: body.AccessKeyID, SecretAccessKey: body.SecretAccessKey, SessionToken: body.Token, Expires: body.Expiration}
	return c.cached, nil
}

// containerCredentialsURL follows the AWS SDKs' rules: a relative URI on the
// ECS endpoint, or a full URI only to the link-local ECS/EKS endpoints or
// loopback, so a misconfiguration can't send the token to another host.
func containerCredentialsURL(getenv func(string) string) (string, error) {
	if rel := getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); rel != "" {
		if !strings.HasPrefix(rel, "/") {
			return "", errors.New("container credentials: relative URI must start with /")
		}
		return "http://169.254.170.2" + rel, nil
	}
	full := getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if full == "" {
		return "", errors.New("no AWS credentials (set AWS_ACCESS_KEY_ID or run with a task role)")
	}
	u, err := url.Parse(full)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	allowed := host == "169.254.170.2" || host == "169.254.170.23" || host == "fd00:ec2::23" || host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		allowed = true
	}
	if !allowed || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("container credentials: host %q not allowed", host)
	}
	return full, nil
}
