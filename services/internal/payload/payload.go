// Package payload stores large step payloads (inputs, outputs, reasoning)
// outside the search index: in S3 in production, or a local directory for
// development. Keys are always derived from the tenant, never taken from
// stored data, so one tenant can't address another's objects.
package payload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/services/internal/awsv4"
)

// Store is a blob store.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	// Get returns at most max bytes and whether the object was longer.
	Get(ctx context.Context, key string, max int64) ([]byte, bool, error)
}

// ErrNotFound means the object does not exist.
var ErrNotFound = errors.New("payload: not found")

// Ref points to an offloaded field. Stored in the step document.
type Ref struct {
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

var segmentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Key builds an object key. Every segment is validated, so a crafted
// tenant, trace or step ID can't produce "..", slashes or encoding tricks.
func Key(tenant, day, traceID, stepID, field string) (string, error) {
	for _, s := range []string{tenant, day, traceID, stepID, field} {
		if !segmentRe.MatchString(s) || s == "." || s == ".." {
			return "", fmt.Errorf("payload: invalid key segment %q", s)
		}
	}
	return strings.Join([]string{"tenants", tenant, day, traceID, stepID, field}, "/"), nil
}

// Checksum is the hex SHA-256 of data.
func Checksum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// FromURL opens "s3://bucket/prefix" or "file:///abs/dir". Empty disables
// offload (nil, nil).
func FromURL(raw string, getenv func(string) string) (Store, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("payload: bad store URL: %w", err)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return nil, errors.New("payload: use file:///absolute/dir")
		}
		return &FileStore{Dir: u.Path}, nil
	case "s3":
		region := getenv("AWS_REGION")
		if region == "" {
			region = getenv("AWS_DEFAULT_REGION")
		}
		if u.Host == "" || region == "" {
			return nil, errors.New("payload: s3://bucket needs AWS_REGION")
		}
		return &S3Store{Bucket: u.Host, Prefix: strings.Trim(u.Path, "/"), Region: region, Creds: &awsv4.CredentialChain{Getenv: getenv}}, nil
	}
	return nil, fmt.Errorf("payload: unsupported store %q", u.Scheme)
}

// FileStore keeps objects under Dir. For local development.
type FileStore struct{ Dir string }

func (f *FileStore) path(key string) (string, error) {
	p := filepath.Join(f.Dir, filepath.FromSlash(key))
	if !strings.HasPrefix(p, filepath.Clean(f.Dir)+string(filepath.Separator)) {
		return "", errors.New("payload: key escapes store")
	}
	return p, nil
}

// Put implements Store (write to a temp file, then rename: no torn reads).
func (f *FileStore) Put(_ context.Context, key string, data []byte) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Get implements Store.
func (f *FileStore) Get(_ context.Context, key string, max int64) ([]byte, bool, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, false, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	defer fh.Close()
	return readCapped(fh, max)
}

func readCapped(r io.Reader, max int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}
