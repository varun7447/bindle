// Package pricesrc reads model prices from their published sources, never
// guessing: it detects when a provider's pricing page changes (so a person
// updates pricing.json from the page), and parses AWS's public price list
// for Amazon Bedrock (so a workspace can copy the exact list price it pays
// into its own model prices).
package pricesrc

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---- pricing page change detection ------------------------------------------

var (
	dollarRe = regexp.MustCompile(`\$\s?([0-9]{1,5}(?:,[0-9]{3})*(?:\.[0-9]{1,6})?)`)
	tagRe    = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
)

// rowRe matches a model's row in a page's embedded table data, as the
// OpenAI pricing page serializes it: [0,"gpt-5.2"],[0,1.75],[0,0.175],[0,14]
// (quotes HTML-escaped). Cells are numbers or "-".
// Deprecated models are labelled [0,{"label":[0,"gpt-4o-mini"],"deprecated":[0,true]}],
// and tiered ones carry a note: "gpt-5.4-pro (<272K context length)".
var rowRe = regexp.MustCompile(`\[0,(?:(?:&quot;|")([a-z0-9][a-z0-9.\-]{1,60})(?: \((?:[^)&"]|&lt;|&gt;|&amp;)*\))?(?:&quot;|")|\{(?:&quot;|")label(?:&quot;|"):\[0,(?:&quot;|")([a-z0-9][a-z0-9.\-]{1,60})(?: \((?:[^)&"]|&lt;|&gt;|&amp;)*\))?(?:&quot;|")\][^}]{0,80}\})\]((?:,\[0,(?:[0-9]+(?:\.[0-9]+)?|null|(?:&quot;|")-(?:&quot;|"))\]){1,6})`)
var cellRe = regexp.MustCompile(`\[0,([0-9]+(?:\.[0-9]+)?|null|(?:&quot;|")-(?:&quot;|"))\]`)

// ModelRows returns, per model ID found in embedded table data, the price
// cells that follow it ("-" for none). A model listed in several tables
// (standard, batch, flex) keeps every row.
func ModelRows(html string) map[string][][]string {
	out := map[string][][]string{}
	for _, m := range rowRe.FindAllStringSubmatch(html, -1) {
		model := m[1]
		if model == "" {
			model = m[2]
		}
		var row []string
		for _, c := range cellRe.FindAllStringSubmatch(m[3], -1) {
			if n := norm(c[1]); n != "" {
				row = append(row, n)
			} else {
				row = append(row, "-")
			}
		}
		out[model] = append(out[model], row)
	}
	return out
}

// Fingerprint is what change detection compares for a page: its dollar
// amounts plus one "model: cells" line per embedded table row.
func Fingerprint(html string) []string {
	fp := Amounts(html)
	rows := ModelRows(html)
	for model, rs := range rows {
		seen := map[string]bool{}
		for _, r := range rs {
			line := model + ": " + strings.Join(r, " / ")
			if !seen[line] {
				seen[line] = true
				fp = append(fp, line)
			}
		}
	}
	sort.Strings(fp)
	return fp
}

// Amounts returns the distinct dollar amounts in a page, normalized ("$5.00"
// and "$5" are the same amount), sorted.
func Amounts(html string) []string {
	text := tagRe.ReplaceAllString(html, " ")
	text = strings.NewReplacer("&#36;", "$", "&dollar;", "$", "&nbsp;", " ").Replace(text)
	seen := map[string]bool{}
	for _, m := range dollarRe.FindAllStringSubmatch(text, -1) {
		if n := norm(strings.ReplaceAll(m[1], ",", "")); n != "" {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return num(out[i]) < num(out[j]) })
	return out
}

func norm(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func num(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// Change compares a page's amounts with the last recorded ones.
type Change struct {
	URL     string   `json:"url"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Missing are prices in pricing.json for this source ("model $price")
	// that no longer appear on the page: the strongest sign an entry is out
	// of date.
	Missing []string `json:"missing"`
	// Unverified are long-prompt prices the page's HTML doesn't contain
	// (rendered by script); a person checks them on the page.
	Unverified []string `json:"unverified"`
}

// Changed reports whether there is anything for a person to review.
func (c Change) Changed() bool { return len(c.Added)+len(c.Removed)+len(c.Missing) > 0 }

// ExpectedPrice is one price from pricing.json and the model it belongs to.
type ExpectedPrice struct {
	Model string // model ID without the provider prefix
	Value string
	// Long marks long-prompt tier prices. Some pages fill these in with
	// JavaScript, so a missing one is reported as unverified, not missing.
	Long bool
}

// Present reports whether a price is on the page: in the embedded row for
// its model, else anywhere as a dollar amount.
func Present(html string) func(ExpectedPrice) bool {
	amounts := map[string]bool{}
	for _, a := range Amounts(html) {
		amounts[a] = true
	}
	rows := ModelRows(html)
	return func(e ExpectedPrice) bool {
		for _, r := range rows[e.Model] {
			for _, c := range r {
				if c == e.Value {
					return true
				}
			}
		}
		return amounts[e.Value]
	}
}

// Compare diffs the current fingerprint against the recorded one and lists
// expected prices (from pricing.json) that are no longer on the page.
func Compare(url string, recorded, current []string, expected []ExpectedPrice, present func(ExpectedPrice) bool) Change {
	in := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, v := range list {
			m[v] = true
		}
		return m
	}
	rec, cur := in(recorded), in(current)
	c := Change{URL: url, Added: []string{}, Removed: []string{}, Missing: []string{}, Unverified: []string{}}
	for _, v := range current {
		if !rec[v] && len(recorded) > 0 {
			c.Added = append(c.Added, v)
		}
	}
	for _, v := range recorded {
		if !cur[v] {
			c.Removed = append(c.Removed, v)
		}
	}
	seen := map[ExpectedPrice]bool{}
	for _, e := range expected {
		if !seen[e] && !present(e) {
			if e.Long {
				c.Unverified = append(c.Unverified, e.Model+" $"+e.Value)
			} else {
				c.Missing = append(c.Missing, e.Model+" $"+e.Value)
			}
		}
		seen[e] = true
	}
	sort.Strings(c.Missing)
	sort.Strings(c.Unverified)
	return c
}

// Expected collects, per source URL, every price in a pricing.json table.
func Expected(pricingJSON []byte) (map[string][]ExpectedPrice, error) {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(pricingJSON, &raw); err != nil {
		return nil, err
	}
	out := map[string][]ExpectedPrice{}
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(v, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		src, _ := p["source"].(string)
		if src == "" {
			continue
		}
		model := k
		if i := strings.Index(k, "/"); i >= 0 {
			model = k[i+1:]
		}
		for field, val := range p {
			if f, ok := val.(float64); ok && strings.HasSuffix(field, "_per_mtok") {
				out[src] = append(out[src], ExpectedPrice{Model: model, Value: strconv.FormatFloat(f, 'f', -1, 64), Long: strings.HasPrefix(field, "long_")})
			}
		}
	}
	return out, nil
}

// ---- AWS price list (Amazon Bedrock) ------------------------------------------

// OfferURL is the public AWS price list file for a Bedrock offer in a
// region. No AWS account or credentials are needed.
func OfferURL(offer, region string) string {
	return "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/" + offer + "/current/" + region + "/index.json"
}

// BedrockOffers are the price list offers that hold model token prices:
// Amazon's own models, and models sold through AWS Marketplace (Anthropic,
// Meta, Mistral and others).
var BedrockOffers = []string{"AmazonBedrock", "AmazonBedrockFoundationModels"}

// RegionRe validates AWS region codes.
var RegionRe = regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-[0-9]$`)

// BedrockPrice is one model's on-demand token prices for one usage type.
// Prices are USD per million tokens; nil means the price list has none.
type BedrockPrice struct {
	Model       string   `json:"model"`    // as the price list names it, e.g. "Claude Opus 4.5"
	Provider    string   `json:"provider"` // e.g. "Anthropic"; "Amazon" for Amazon's models
	Tier        string   `json:"tier"`     // "global", "regional", "latency optimized", or ""
	Input       *float64 `json:"input_per_mtok"`
	Output      *float64 `json:"output_per_mtok"`
	CacheRead   *float64 `json:"cache_read_per_mtok"`
	CacheWrite  *float64 `json:"cache_write_per_mtok"`
	Region      string   `json:"region"`
	Source      string   `json:"source"`
	Publication string   `json:"as_of"`
}

type offerFile struct {
	PublicationDate string `json:"publicationDate"`
	Products        map[string]struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"products"`
	Terms struct {
		OnDemand map[string]map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string            `json:"unit"`
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// tier reads the price list's usage type: global cross-region inference,
// in-region ("regional") inference, or latency-optimized inference.
func tier(usage string) string {
	u := strings.ToLower(usage)
	switch {
	case strings.Contains(u, "latencyoptimized") || strings.Contains(u, "latency_optimized"):
		return "latency optimized"
	case strings.Contains(u, "global"):
		return "global"
	case strings.Contains(u, "_standard") || strings.Contains(u, "regional"):
		return "regional"
	}
	return ""
}

// perMTok converts a price per unit to USD per million tokens, or false
// for units it doesn't recognize (it never guesses a unit).
func perMTok(unit, usd string) (float64, bool) {
	v, err := strconv.ParseFloat(usd, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "1m tokens", "million tokens", "1m units":
		return v, true
	case "1k tokens", "thousand tokens":
		return round(v * 1000), true
	case "tokens", "token":
		return round(v * 1e6), true
	}
	return 0, false
}

// ParseBedrockOffer reads one price list file and returns on-demand token
// prices, one row per model, provider and tier. When the file lists more
// than one price for the same row, all are kept as separate rows: a person
// picks, Bindle doesn't.
func ParseBedrockOffer(r io.Reader, region, source string) ([]BedrockPrice, error) {
	var f offerFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return nil, fmt.Errorf("price list: %w", err)
	}
	type key struct{ model, provider, tier string }
	type slot struct{ in, out, cr, cw []float64 }
	rows := map[key]*slot{}
	for sku, p := range f.Products {
		a := p.Attributes
		if a["feature"] != "On-demand Inference" || a["model"] == "" {
			continue
		}
		var field *[]float64
		k := key{a["model"], a["provider"], tier(a["usagetype"])}
		if k.provider == "" {
			k.provider = "Amazon"
		}
		s := rows[k]
		if s == nil {
			s = &slot{}
			rows[k] = s
		}
		switch strings.ToLower(a["inferenceType"]) {
		case "input tokens":
			field = &s.in
		case "output tokens":
			field = &s.out
		case "prompt cache read input tokens", "cache read input tokens":
			field = &s.cr
		case "prompt cache write input tokens", "cache write input tokens":
			field = &s.cw
		default:
			continue
		}
		for _, t := range f.Terms.OnDemand[sku] {
			for _, d := range t.PriceDimensions {
				if v, ok := perMTok(d.Unit, d.PricePerUnit["USD"]); ok {
					*field = append(*field, v)
				}
			}
		}
	}
	day := f.PublicationDate
	if len(day) >= 10 {
		day = day[:10]
	}
	var out []BedrockPrice
	for k, s := range rows {
		if len(s.in) == 0 && len(s.out) == 0 {
			continue
		}
		n := max(len(s.in), len(s.out), 1)
		for i := 0; i < n; i++ {
			bp := BedrockPrice{Model: k.model, Provider: k.provider, Tier: k.tier, Region: region, Source: source, Publication: day,
				Input: pick(s.in, i), Output: pick(s.out, i), CacheRead: pick(s.cr, i), CacheWrite: pick(s.cw, i)}
			out = append(out, bp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		return val(a.Input) < val(b.Input)
	})
	return out, nil
}

// pick returns the i-th distinct price, sorted, so paired rows line up
// cheapest with cheapest; one price applies to every row.
// round drops float noise from unit conversion (0.00006 * 1000 is
// 0.060000000000000005); prices have far fewer than 9 decimals.
func round(v float64) float64 { return math.Round(v*1e9) / 1e9 }

func pick(vs []float64, i int) *float64 {
	vs = distinct(vs)
	switch {
	case len(vs) == 0:
		return nil
	case len(vs) == 1:
		v := vs[0]
		return &v
	case i < len(vs):
		v := vs[i]
		return &v
	}
	return nil
}

func distinct(vs []float64) []float64 {
	seen := map[float64]bool{}
	var out []float64
	for _, v := range vs {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}

func val(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}
