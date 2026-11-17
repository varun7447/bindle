package alerts

import (
	"bytes"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/awsv4"
)

// Deliverer sends alerts. HTTP goes through a client that refuses private,
// loopback, link-local and metadata addresses at connect time (so DNS
// tricks don't help) and never follows redirects.
type Deliverer struct {
	HTTP       *http.Client // default: SafeClient()
	SES        *SES         // nil disables email
	ConsoleURL string       // for links in messages
}

// SafeClient is an HTTP client for customer-supplied URLs.
func SafeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !publicIP(ip) {
				return fmt.Errorf("alerts: refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			Proxy:                 nil, // a proxy would bypass the address check
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("alerts: redirects are not followed") },
	}
}

func parseHostIP(h string) net.IP { return net.ParseIP(strings.Trim(h, "[]")) }

// publicIP reports whether ip is a globally routable unicast address.
func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0, v4[0] == 100 && v4[1]&0xc0 == 64, // 0/8, 100.64/10 (CGNAT)
			v4[0] == 192 && v4[1] == 0 && v4[2] == 0,     // 192.0.0/24
			v4[0] == 198 && (v4[1] == 18 || v4[1] == 19), // benchmarking
			v4[0] >= 240: // reserved, broadcast
			return false
		}
		return true
	}
	return !(ip[0] == 0x00 || (ip[0] == 0xfd && ip[1] == 0x00 && ip[2] == 0x0e && ip[3] == 0xc2)) // ::/8, fd00:ec2::/32
}

func (d *Deliverer) link(ev Event) string {
	base := strings.TrimRight(d.ConsoleURL, "/")
	if base == "" {
		return ""
	}
	if ev.TraceID != "" {
		return base + "/traces/" + ev.TraceID
	}
	return base + "/"
}

// Send delivers one event to one channel.
func (d *Deliverer) Send(ctx context.Context, ch Channel, ev Event, r Rule) error {
	if err := ValidateChannel(ch); err != nil {
		return err
	}
	text := "Bindle alert: " + ev.Summary
	if l := d.link(ev); l != "" {
		text += "\n" + l
	}
	switch ch.Kind {
	case "slack":
		body, _ := json.Marshal(map[string]string{"text": text})
		return d.post(ctx, ch.Target, body, nil)
	case "webhook":
		body, _ := json.Marshal(map[string]any{
			"id": ev.ID, "kind": ev.Kind, "rule_id": ev.RuleID, "summary": ev.Summary, "trace_id": ev.TraceID,
			"details": ev.Details, "fired_at": ev.FiredAt, "url": d.link(ev),
		})
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(ch.Secret))
		mac.Write([]byte(ts + "." + string(body)))
		return d.post(ctx, ch.Target, body, map[string]string{
			"X-Bindle-Timestamp": ts,
			"X-Bindle-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		})
	case "email":
		if d.SES == nil {
			return errors.New("email delivery is not configured")
		}
		return d.SES.Send(ctx, ch.Target, "Bindle alert: "+truncate(ev.Summary, 120), text+"\n\nYou receive this because this address is an alert channel in your Bindle workspace.")
	}
	return fmt.Errorf("unknown channel kind %q", ch.Kind)
}

func (d *Deliverer) post(ctx context.Context, url string, body []byte, headers map[string]string) error {
	hc := d.HTTP
	if hc == nil {
		hc = SafeClient()
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Bindle-Alerts/1")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				return last // the receiver rejected it; retrying won't help
			}
		} else {
			last = err
			if strings.Contains(err.Error(), "non-public address") || strings.Contains(err.Error(), "redirects") {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return last
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SES sends email through Amazon SES (v2 API), signed with SigV4.
type SES struct {
	Region   string
	From     string // a verified SES identity
	Creds    awsv4.CredentialsProvider
	HTTP     *http.Client
	Endpoint string // tests only
}

// Send sends a plain-text email.
func (s *SES) Send(ctx context.Context, to, subject, text string) error {
	body, _ := json.Marshal(map[string]any{
		"FromEmailAddress": s.From,
		"Destination":      map[string]any{"ToAddresses": []string{to}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]string{"Data": subject, "Charset": "UTF-8"},
			"Body":    map[string]any{"Text": map[string]string{"Data": text, "Charset": "UTF-8"}},
		}},
	})
	u := s.Endpoint
	if u == "" {
		u = "https://email." + s.Region + ".amazonaws.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u+"/v2/email/outbound-emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	creds, err := s.Creds.Retrieve(ctx)
	if err != nil {
		return err
	}
	awsv4.Sign(req, body, creds, s.Region, "ses", time.Now().UTC())
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ses: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
