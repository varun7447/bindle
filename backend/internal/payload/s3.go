package payload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

// S3Store keeps objects in an S3 bucket. Requests are signed with AWS
// Signature Version 4 (standard library only); credentials come from the
// environment or the ECS task role.
type S3Store struct {
	Bucket   string
	Prefix   string // optional key prefix, no slashes at either end
	Region   string
	Creds    awsv4.CredentialsProvider
	HTTP     *http.Client
	Endpoint string // tests only: base URL, path-style addressing
	Now      func() time.Time
}

func (s *S3Store) objectURL(key string) string {
	k := key
	if s.Prefix != "" {
		k = s.Prefix + "/" + key
	}
	if s.Endpoint != "" {
		return strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + awsv4.URIEncodePath(k)
	}
	return "https://" + s.Bucket + ".s3." + s.Region + ".amazonaws.com/" + awsv4.URIEncodePath(k)
}

// Put implements Store.
func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := s.do(req, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s3Error(resp)
	}
	return nil
}

// Get implements Store.
func (s *S3Store) Get(ctx context.Context, key string, max int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.do(req, nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, s3Error(resp)
	}
	return readCapped(resp.Body, max)
}

func (s *S3Store) do(req *http.Request, body []byte) (*http.Response, error) {
	creds, err := s.Creds.Retrieve(req.Context())
	if err != nil {
		return nil, fmt.Errorf("payload: credentials: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	awsv4.Sign(req, body, creds, s.Region, "s3", now().UTC())
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return hc.Do(req)
}

func s3Error(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var code string
	if i := bytes.Index(msg, []byte("<Code>")); i >= 0 {
		if j := bytes.Index(msg[i:], []byte("</Code>")); j > 0 {
			code = string(msg[i+6 : i+j])
		}
	}
	return fmt.Errorf("payload: s3 HTTP %d %s", resp.StatusCode, code)
}
