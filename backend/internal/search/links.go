package search

import (
	"context"
	"sort"

	"github.com/bindle-ai/bindle/backend/internal/query"
)

var _ query.LinkStore = (*Store)(nil)

// linkDoc is the part of a step needed to follow its links.
type linkDoc struct {
	TraceID    string            `json:"trace_id"`
	StepID     string            `json:"step_id"`
	Name       string            `json:"name"`
	StartNs    int64             `json:"start_ns"`
	Links      []string          `json:"links"`
	Attributes map[string]string `json:"attributes"`
}

func (d linkDoc) kind() string { return query.LinkKind(d.Attributes["bindle.link.kind"]) }

// linkSource keeps link lookups small: attributes are needed only for
// bindle.link.kind, and payload fields never.
var linkSource = map[string]any{"includes": []string{"trace_id", "step_id", "name", "start_ns", "links", "attributes"}}

// TraceLinks implements query.LinkStore: links from steps of this run to
// other runs (outgoing), and from steps of other runs to this one
// (incoming), at most limit of each, with the other run's summary.
// Links within the run itself are ignored.
func (s *Store) TraceLinks(ctx context.Context, tenant, traceID string, limit int) ([]query.TraceLink, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []query.TraceLink

	// Outgoing: this run's steps that carry links.
	var own hits[linkDoc]
	body := map[string]any{"size": limit, "sort": []any{map[string]any{"start_ns": "asc"}}, "_source": linkSource}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(),
		[]map[string]any{term("trace_id", traceID), {"exists": map[string]any{"field": "links"}}}, body, &own); err != nil {
		return nil, err
	}
	for _, h := range own.Hits.Hits {
		d := h.Source
		if d.TraceID != traceID {
			continue
		}
		for _, l := range d.Links {
			t, sp, ok := query.ParseLink(l)
			if !ok || t == traceID || len(out) >= limit {
				continue
			}
			out = append(out, query.TraceLink{Direction: "outgoing", Kind: d.kind(), StepID: d.StepID, StepName: d.Name, TraceID: t, SpanID: sp})
		}
	}

	// Incoming: other runs' steps whose links point into this run. Links
	// are keywords "traceid:spanid", so a prefix query finds them.
	var other hits[linkDoc]
	body = map[string]any{"size": limit, "sort": []any{map[string]any{"start_ns": "asc"}}, "_source": linkSource}
	clauses := []map[string]any{
		{"prefix": map[string]any{"links": traceID + ":"}},
		{"bool": map[string]any{"must_not": []any{term("trace_id", traceID)}}},
	}
	if err := s.search(ctx, tenant, s.Names.StepsPattern(), clauses, body, &other); err != nil {
		return nil, err
	}
	nIn := 0
	for _, h := range other.Hits.Hits {
		d := h.Source
		if d.TraceID == traceID {
			continue
		}
		for _, l := range d.Links {
			t, sp, ok := query.ParseLink(l)
			if !ok || t != traceID || nIn >= limit {
				continue
			}
			out = append(out, query.TraceLink{Direction: "incoming", Kind: d.kind(), StepID: d.StepID, StepName: d.Name, TraceID: d.TraceID, SpanID: sp})
			nIn++
		}
	}
	if len(out) == 0 {
		return []query.TraceLink{}, nil
	}

	// The other runs' summaries.
	seen := map[string]bool{}
	var ids []string
	for _, l := range out {
		if !seen[l.TraceID] {
			seen[l.TraceID] = true
			ids = append(ids, l.TraceID)
		}
	}
	sort.Strings(ids)
	runs, err := s.RunsByIDs(ctx, tenant, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*query.Run, len(runs))
	for i := range runs {
		byID[runs[i].TraceID] = &runs[i]
	}
	for i := range out {
		out[i].Run = byID[out[i].TraceID]
	}
	return out, nil
}
