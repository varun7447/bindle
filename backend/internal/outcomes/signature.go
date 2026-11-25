package outcomes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries the webhook signature:
//
//	X-Bindle-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, t + "." + body)>
//
// Several v1 values may be sent while a secret is being rotated.
const SignatureHeader = "X-Bindle-Signature"

// Signature errors (all HTTP 401: the request is not trusted).
var (
	ErrNoSignature  = errors.New("missing or malformed " + SignatureHeader)
	ErrStale        = errors.New("signature timestamp is too old or in the future")
	ErrBadSignature = errors.New("signature does not match")
)

// Sign returns the header value for body at t (for tests and senders).
func Sign(secret string, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

func mac(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifySignature checks header against body: the timestamp must be within
// SignatureMaxSkew of now, and a v1 value must match. Comparison is constant
// time. Replays inside the window are harmless: an event's ID is derived from
// its content, so the same request stores the same event once.
func VerifySignature(header string, body []byte, secret string, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 || secret == "" {
		return ErrNoSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > SignatureMaxSkew || d < -SignatureMaxSkew {
		return ErrStale
	}
	want := []byte(mac(secret, ts, body))
	for _, s := range sigs {
		if hmac.Equal([]byte(strings.ToLower(s)), want) {
			return nil
		}
	}
	return ErrBadSignature
}
