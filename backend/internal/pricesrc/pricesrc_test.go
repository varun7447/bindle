package pricesrc

import (
	"reflect"
	"strings"
	"testing"
)

func TestAmounts(t *testing.T) {
	html := `<table><tr><td>Claude X</td><td>$5 / MTok</td><td>$25.00</td></tr>
		<tr><td>$0.50</td><td>&#36;1,250</td></tr></table><script>var p = "$999";</script>`
	if got := Amounts(html); !reflect.DeepEqual(got, []string{"0.5", "5", "25", "1250"}) {
		t.Errorf("amounts %v", got)
	}
}

func TestModelRowsAndPresence(t *testing.T) {
	html := `x,[1,[[0,&quot;gpt-5.2&quot;],[0,1.75],[0,0.175],[0,14]]],[1,[[0,&quot;gpt-5.2-pro&quot;],[0,21],[0,&quot;-&quot;],[0,168]]],<td>$5</td>` +
		`[1,[[0,{&quot;label&quot;:[0,&quot;gpt-4o-mini&quot;],&quot;deprecated&quot;:[0,true]}],[0,0.15],[0,null],[0,0.6]]]` +
		`[1,[[0,&quot;gpt-5.4-pro (&lt;272K context length)&quot;],[0,30],[0,&quot;-&quot;],[0,180]]]`
	rows := ModelRows(html)
	if !reflect.DeepEqual(rows["gpt-5.2"], [][]string{{"1.75", "0.175", "14"}}) || !reflect.DeepEqual(rows["gpt-5.2-pro"], [][]string{{"21", "-", "168"}}) ||
		!reflect.DeepEqual(rows["gpt-4o-mini"], [][]string{{"0.15", "-", "0.6"}}) || !reflect.DeepEqual(rows["gpt-5.4-pro"], [][]string{{"30", "-", "180"}}) {
		t.Fatalf("rows %v", rows)
	}
	present := Present(html)
	for _, e := range []ExpectedPrice{{Model: "gpt-5.2", Value: "14"}, {Model: "gpt-5.2-pro", Value: "168"}, {Model: "claude-x", Value: "5"}} {
		if !present(e) {
			t.Errorf("%+v should be present", e)
		}
	}
	if present(ExpectedPrice{Model: "gpt-5.2", Value: "21"}) {
		t.Error("a price must match its own model's row")
	}
	fp := Fingerprint(html)
	if !reflect.DeepEqual(fp, []string{"5", "gpt-4o-mini: 0.15 / - / 0.6", "gpt-5.2-pro: 21 / - / 168", "gpt-5.2: 1.75 / 0.175 / 14", "gpt-5.4-pro: 30 / - / 180"}) {
		t.Errorf("fingerprint %v", fp)
	}
}

func TestCompare(t *testing.T) {
	present := func(e ExpectedPrice) bool { return e.Value != "3" && e.Value != "9" }
	c := Compare("u", []string{"3", "15"}, []string{"2.5", "15"}, []ExpectedPrice{{"m", "3", false}, {"m", "15", false}, {"m", "3", false}, {"m", "9", true}}, present)
	if !reflect.DeepEqual(c.Added, []string{"2.5"}) || !reflect.DeepEqual(c.Removed, []string{"3"}) || !reflect.DeepEqual(c.Missing, []string{"m $3"}) ||
		!reflect.DeepEqual(c.Unverified, []string{"m $9"}) || !c.Changed() {
		t.Errorf("change %+v", c)
	}
	first := Compare("u", nil, []string{"3"}, []ExpectedPrice{{"m", "15", false}, {"m", "9", true}}, present)
	if first.Changed() {
		t.Errorf("first run records, reports nothing: %+v", first)
	}
}

func TestExpected(t *testing.T) {
	got, err := Expected([]byte(`{"_comment":"x","a/m":{"input_per_mtok":5,"output_per_mtok":25,"cached_input_per_mtok":0.5,"source":"https://s","as_of":"2026-11-01"},
		"b/m":{"input_per_mtok":1,"output_per_mtok":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["https://s"]) != 3 {
		t.Errorf("expected %v", got)
	}
}

const offer = `{"publicationDate":"2026-11-09T22:43:12Z",
 "products":{
  "A":{"attributes":{"feature":"On-demand Inference","model":"Claude Opus 4.5","provider":"Anthropic","inferenceType":"Input tokens","usagetype":"USE1-MP:USE1_input_tokens_global_standard-Units"}},
  "B":{"attributes":{"feature":"On-demand Inference","model":"Claude Opus 4.5","provider":"Anthropic","inferenceType":"Output tokens","usagetype":"USE1-MP:USE1_output_tokens_global_standard-Units"}},
  "C":{"attributes":{"feature":"On-demand Inference","model":"Claude Opus 4.5","provider":"Anthropic","inferenceType":"Input tokens","usagetype":"USE1-MP:USE1_input_tokens_standard-Units"}},
  "D":{"attributes":{"feature":"On-demand Inference","model":"Claude Opus 4.5","provider":"Anthropic","inferenceType":"Prompt cache read input tokens","usagetype":"USE1-MP:USE1_cache_read_global_standard-Units"}},
  "E":{"attributes":{"feature":"Provisioned Throughput Inference","model":"Claude Opus 4.5","provider":"Anthropic","inferenceType":"Input tokens"}},
  "F":{"attributes":{"feature":"On-demand Inference","model":"Nova Lite","inferenceType":"Input tokens","usagetype":"USE1-NovaLite-input-tokens"}},
  "G":{"attributes":{"feature":"On-demand Inference","model":"Odd","provider":"X","inferenceType":"Input tokens","usagetype":"u"}}
 },
 "terms":{"OnDemand":{
  "A":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"5.0000000000"}}}}},
  "B":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"25.0000000000"}}}}},
  "C":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"5.5000000000"}}}}},
  "D":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"0.5000000000"}}}}},
  "E":{"t":{"priceDimensions":{"d":{"unit":"1M tokens","pricePerUnit":{"USD":"1"}}}}},
  "F":{"t":{"priceDimensions":{"d":{"unit":"1K tokens","pricePerUnit":{"USD":"0.00006"}}}}},
  "G":{"t":{"priceDimensions":{"d":{"unit":"Hours","pricePerUnit":{"USD":"9"}}}}}
 }}}`

func TestParseBedrockOffer(t *testing.T) {
	rows, err := ParseBedrockOffer(strings.NewReader(offer), "us-east-1", "https://src")
	if err != nil {
		t.Fatal(err)
	}
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Provider+"|"+r.Model+"|"+r.Tier)
		if r.Publication != "2026-11-09" || r.Region != "us-east-1" {
			t.Errorf("row meta %+v", r)
		}
	}
	want := []string{"Amazon|Nova Lite|", "Anthropic|Claude Opus 4.5|global", "Anthropic|Claude Opus 4.5|regional"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %v", got)
	}
	g, reg, nova := rows[1], rows[2], rows[0]
	if f(g.Input) != 5 || f(g.Output) != 25 || f(g.CacheRead) != 0.5 || g.CacheWrite != nil {
		t.Errorf("global %+v", g)
	}
	if f(reg.Input) != 5.5 || reg.Output != nil {
		t.Errorf("regional: only what the list says: %+v", reg)
	}
	if v := f(nova.Input); v != 0.06 {
		t.Errorf("per-1K price converted: %v", v)
	}
	if _, err := ParseBedrockOffer(strings.NewReader("nope"), "r", "s"); err == nil {
		t.Error("bad file")
	}
	if !RegionRe.MatchString("us-east-1") || !RegionRe.MatchString("us-gov-west-1") || RegionRe.MatchString("../x") {
		t.Error("region regex")
	}
}
