// Command bindle-prices checks that pricing.json still matches its sources.
//
//	bindle-prices check [-pricing pricing.json] [-state pricing-sources.json] [-update] [-report report.md]
//
// It fetches every source page listed in pricing.json, compares the page's
// dollar amounts with the last recorded set, and checks that every price in
// pricing.json still appears on its page. Exit status 2 means something
// changed and a person should review the page and update pricing.json
// (prices are never edited automatically). -update records the current
// amounts as the new baseline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/pricesrc"
)

type state struct {
	Sources map[string]sourceState `json:"sources"`
}

type sourceState struct {
	Amounts   []string `json:"amounts"`
	CheckedAt string   `json:"checked_at"`
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "check" {
		fmt.Fprintln(os.Stderr, "usage: bindle-prices check [-pricing pricing.json] [-state pricing-sources.json] [-update] [-report file]")
		os.Exit(64)
	}
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	pricingPath := fs.String("pricing", "pricing.json", "price table")
	statePath := fs.String("state", "pricing-sources.json", "recorded amounts per source page")
	update := fs.Bool("update", false, "record the current amounts as the baseline")
	reportPath := fs.String("report", "", "write a Markdown report here")
	_ = fs.Parse(os.Args[2:])
	code, err := check(*pricingPath, *statePath, *reportPath, *update)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bindle-prices:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func check(pricingPath, statePath, reportPath string, update bool) (int, error) {
	pricing, err := os.ReadFile(pricingPath)
	if err != nil {
		return 0, err
	}
	expected, err := pricesrc.Expected(pricing)
	if err != nil {
		return 0, err
	}
	st := state{Sources: map[string]sourceState{}}
	if b, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(b, &st); err != nil {
			return 0, fmt.Errorf("%s: %w", statePath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	urls := make([]string, 0, len(expected))
	for u := range expected {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	client := &http.Client{Timeout: 30 * time.Second}
	now := time.Now().UTC().Format(time.RFC3339)
	var changes []pricesrc.Change
	for _, u := range urls {
		html, err := fetch(client, u)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", u, err)
		}
		cur := pricesrc.Fingerprint(html)
		if len(cur) == 0 {
			return 0, fmt.Errorf("%s: no prices found on the page (layout change?)", u)
		}
		c := pricesrc.Compare(u, st.Sources[u].Amounts, cur, expected[u], pricesrc.Present(html))
		changes = append(changes, c)
		if update || len(st.Sources[u].Amounts) == 0 {
			st.Sources[u] = sourceState{Amounts: cur, CheckedAt: now}
		}
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(statePath, append(b, '\n'), 0o644); err != nil {
		return 0, err
	}
	report := render(changes)
	fmt.Print(report)
	if reportPath != "" {
		if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
			return 0, err
		}
	}
	for _, c := range changes {
		if c.Changed() {
			return 2, nil
		}
	}
	return 0, nil
}

func fetch(client *http.Client, u string) (string, error) {
	if !strings.HasPrefix(u, "https://") {
		return "", errors.New("sources must be https")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "bindle-prices-check (+https://github.com/varun7447/bindle)")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

func render(changes []pricesrc.Change) string {
	var b strings.Builder
	changed := false
	for _, c := range changes {
		if !c.Changed() {
			fmt.Fprintf(&b, "- %s: no change\n", c.URL)
			if len(c.Unverified) > 0 {
				fmt.Fprintf(&b, "  - long-prompt prices the page shows only with JavaScript (check by hand): %s\n", strings.Join(c.Unverified, ", "))
			}
			continue
		}
		changed = true
		fmt.Fprintf(&b, "- **%s** changed\n", c.URL)
		if len(c.Missing) > 0 {
			fmt.Fprintf(&b, "  - prices in pricing.json no longer on the page: %s\n", strings.Join(c.Missing, ", "))
		}
		if len(c.Unverified) > 0 {
			fmt.Fprintf(&b, "  - long-prompt prices the page shows only with JavaScript (check by hand): %s\n", strings.Join(c.Unverified, ", "))
		}
		if len(c.Added) > 0 {
			fmt.Fprintf(&b, "  - new on the page: %s\n", strings.Join(c.Added, "; "))
		}
		if len(c.Removed) > 0 {
			fmt.Fprintf(&b, "  - gone from the page: %s\n", strings.Join(c.Removed, "; "))
		}
	}
	head := "Pricing sources match pricing.json.\n\n"
	if changed {
		head = "A pricing source changed. Review the page, update `backend/pricing.json` by hand (with the new `as_of`), then run `bindle-prices check -update` to record the new baseline.\n\n"
	}
	return head + b.String()
}
