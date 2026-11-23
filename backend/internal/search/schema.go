package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Names derives index, template, policy and security object names from a
// prefix, so tests can use an isolated namespace in a shared cluster.
type Names struct{ Prefix string }

func (n Names) p() string {
	if n.Prefix == "" {
		return "bindle"
	}
	return n.Prefix
}

// StepsPattern matches all daily step indices.
func (n Names) StepsPattern() string { return n.p() + "-steps-*" }

// StepsIndex is the daily step index for a UTC day ("2006.01.02").
func (n Names) StepsIndex(day string) string { return n.p() + "-steps-" + day }

// Traces is the per-trace rollup index.
func (n Names) Traces() string          { return n.p() + "-traces" }
func (n Names) template() string        { return n.p() + "-steps" }
func (n Names) retentionPolicy() string { return n.p() + "-steps-retention" }

// Users names the app's least-privilege users and their passwords.
type Users struct {
	Writer, Reader                 string
	WriterPassword, ReaderPassword string
}

// DefaultUsers are the app user names when none are configured.
var DefaultUsers = Users{Writer: "bindle_writer", Reader: "bindle_reader"}

// SetupOptions configures Setup.
type SetupOptions struct {
	Names         Names
	Users         Users
	RetentionDays int // default 30
	Replicas      int // replicas per shard: 0 for single-node dev, 1+ on AWS
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// keyword fields: ignore_above keeps one oversized customer value from
// failing the document (it stays in _source, just isn't indexed).
func kw() map[string]any { return map[string]any{"type": "keyword", "ignore_above": 1024} }

func stepsMapping() map[string]any {
	p := map[string]any{}
	for _, f := range []string{"tenant_id", "trace_id", "step_id", "parent_step_id", "name", "type", "status",
		"framework", "service_name", "agent_name", "agent_version", "session_id", "environment",
		"provider", "model", "tool_name", "input_hash", "redacted_kinds", "links", "error_pattern", "score_name", "score_pass", "score_source", "security_flags", "security_reasons", "data_classes", "correlations", "passport_drift"} {
		p[f] = kw()
	}
	for _, f := range []string{"attempt", "input_tokens", "output_tokens", "cached_tokens", "start_ns",
		"input_bytes", "output_bytes", "reasoning_bytes", "user_input_bytes"} {
		p[f] = map[string]any{"type": "long"}
	}
	p["cost_usd"] = map[string]any{"type": "double"}
	p["duration_ms"] = map[string]any{"type": "double"}
	p["score_value"] = map[string]any{"type": "double"}
	p["cost_known"] = map[string]any{"type": "boolean"}
	p["redacted"] = map[string]any{"type": "boolean"}
	p["start_time"] = map[string]any{"type": "date_nanos"}
	p["ingested_at"] = map[string]any{"type": "date"}
	// Full-text searchable payloads.
	for _, f := range []string{"error_message", "input", "output", "reasoning", "user_input"} {
		p[f] = map[string]any{"type": "text"}
	}
	p["attributes"] = map[string]any{"type": "flat_object"}
	p["payload_refs"] = payloadRefsMapping()
	return map[string]any{"dynamic": "strict", "properties": p}
}

// payloadRefsMapping: kept in _source, not indexed.
func payloadRefsMapping() map[string]any { return map[string]any{"type": "object", "enabled": false} }

func tracesMapping() map[string]any {
	p := map[string]any{}
	for _, f := range []string{"tenant_id", "trace_id", "agent_name", "agent_version", "framework",
		"environment", "session_id", "root_status"} {
		p[f] = kw()
	}
	for _, f := range []string{"first_start_ns", "steps", "failed_steps", "cost_unknown_steps",
		"input_tokens", "output_tokens", "cached_tokens", "llm_calls", "tool_calls", "peak_input_tokens"} {
		p[f] = map[string]any{"type": "long"}
	}
	p["cost_usd"] = map[string]any{"type": "double"}
	p["root_duration_ms"] = map[string]any{"type": "double"}
	p["first_start"] = map[string]any{"type": "date_nanos"}
	p["updated_at"] = map[string]any{"type": "date"}
	return map[string]any{"dynamic": "strict", "properties": p}
}

// Setup creates or updates the retention policy, index template, rollup
// index and least-privilege users/roles. Idempotent. c must authenticate
// as an admin (security plugin admin, or the AWS FGAC master user).
func Setup(ctx context.Context, c *Client, o SetupOptions) error {
	n := o.Names
	for _, s := range []string{n.p(), o.Users.Writer, o.Users.Reader} {
		if !nameRe.MatchString(s) {
			return fmt.Errorf("search: invalid name %q", s)
		}
	}
	if o.Users.Writer == o.Users.Reader {
		return fmt.Errorf("search: writer and reader users must differ")
	}
	days := o.RetentionDays
	if days <= 0 {
		days = 30
	}
	settings := map[string]any{
		"number_of_shards":   1,
		"number_of_replicas": o.Replicas,
		"refresh_interval":   "1s",
	}

	// Retention: ISM deletes each daily step index once it is old enough.
	policy := map[string]any{"policy": map[string]any{
		"description":   "Delete Bindle step indices after the retention period",
		"default_state": "hot",
		"states": []any{
			map[string]any{"name": "hot", "actions": []any{},
				"transitions": []any{map[string]any{"state_name": "delete",
					"conditions": map[string]any{"min_index_age": strconv.Itoa(days) + "d"}}}},
			map[string]any{"name": "delete", "actions": []any{map[string]any{"delete": map[string]any{}}}, "transitions": []any{}},
		},
		"ism_template": []any{map[string]any{"index_patterns": []string{n.StepsPattern()}, "priority": 100}},
	}}
	if err := putISMPolicy(ctx, c, n.retentionPolicy(), policy); err != nil {
		return err
	}

	if err := c.Do(ctx, http.MethodPut, "/_index_template/"+n.template(), nil, map[string]any{
		"index_patterns": []string{n.StepsPattern()},
		"priority":       100,
		"template":       map[string]any{"settings": settings, "mappings": stepsMapping()},
	}, nil); err != nil {
		return fmt.Errorf("search: index template: %w", err)
	}

	// Additive mapping changes for step indices that already exist.
	if err := c.Do(ctx, http.MethodPut, "/"+n.StepsPattern()+"/_mapping",
		url.Values{"allow_no_indices": {"true"}, "ignore_unavailable": {"true"}},
		map[string]any{"properties": map[string]any{"payload_refs": payloadRefsMapping(), "error_pattern": kw(),
			"score_name": kw(), "score_pass": kw(), "score_source": kw(), "score_value": map[string]any{"type": "double"},
			"security_flags": kw(), "security_reasons": kw(), "data_classes": kw(), "passport_drift": kw(), "correlations": kw(), "user_input": map[string]any{"type": "text"}, "user_input_bytes": map[string]any{"type": "long"}}}, nil); err != nil {
		return fmt.Errorf("search: steps mapping: %w", err)
	}

	err := c.Do(ctx, http.MethodPut, "/"+n.Traces(), nil, map[string]any{"settings": settings, "mappings": tracesMapping()}, nil)
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("search: traces index: %w", err)
	}
	if err != nil { // exists: apply additive mapping changes
		if err := c.Do(ctx, http.MethodPut, "/"+n.Traces()+"/_mapping", nil, tracesMapping(), nil); err != nil {
			return fmt.Errorf("search: traces mapping: %w", err)
		}
	}

	// Least privilege. Writer: create daily indices and add documents,
	// read steps to recompute rollups, delete expired rollups. Reader:
	// read only. Neither can delete indices, change mappings or settings,
	// or touch security configuration.
	roles := map[string]any{
		o.Users.Writer: map[string]any{
			"cluster_permissions": []string{"cluster_composite_ops"},
			"index_permissions": []any{
				map[string]any{"index_patterns": []string{n.StepsPattern()}, "allowed_actions": []string{"create_index", "index", "read"}},
				map[string]any{"index_patterns": []string{n.Traces()}, "allowed_actions": []string{"index", "read",
					"indices:data/write/delete", "indices:data/write/delete/byquery"}},
			},
		},
		o.Users.Reader: map[string]any{
			"cluster_permissions": []string{"cluster_composite_ops_ro"},
			"index_permissions": []any{
				map[string]any{"index_patterns": []string{n.StepsPattern(), n.Traces()}, "allowed_actions": []string{"read"}},
			},
		},
	}
	passwords := map[string]string{o.Users.Writer: o.Users.WriterPassword, o.Users.Reader: o.Users.ReaderPassword}
	for name, role := range roles {
		if err := c.Do(ctx, http.MethodPut, "/_plugins/_security/api/roles/"+name, nil, role, nil); err != nil {
			return fmt.Errorf("search: role %s: %w", name, err)
		}
		if pw := passwords[name]; pw != "" {
			if err := c.Do(ctx, http.MethodPut, "/_plugins/_security/api/internalusers/"+name, nil,
				map[string]any{"password": pw, "backend_roles": []string{}}, nil); err != nil {
				return fmt.Errorf("search: user %s: %w", name, err)
			}
		}
		if err := c.Do(ctx, http.MethodPut, "/_plugins/_security/api/rolesmapping/"+name, nil,
			map[string]any{"users": []string{name}}, nil); err != nil {
			return fmt.Errorf("search: role mapping %s: %w", name, err)
		}
	}
	return nil
}

func isAlreadyExists(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusBadRequest && e.Type == "resource_already_exists_exception"
}

// putISMPolicy creates the policy, or updates it using optimistic
// concurrency when it already exists.
func putISMPolicy(ctx context.Context, c *Client, id string, policy map[string]any) error {
	var cur struct {
		SeqNo       json.Number `json:"_seq_no"`
		PrimaryTerm json.Number `json:"_primary_term"`
	}
	err := c.Do(ctx, http.MethodGet, "/_plugins/_ism/policies/"+id, nil, nil, &cur)
	switch {
	case IsStatus(err, http.StatusNotFound):
		err = c.Do(ctx, http.MethodPut, "/_plugins/_ism/policies/"+id, nil, policy, nil)
	case err == nil:
		q := url.Values{"if_seq_no": {cur.SeqNo.String()}, "if_primary_term": {cur.PrimaryTerm.String()}}
		err = c.Do(ctx, http.MethodPut, "/_plugins/_ism/policies/"+id, q, policy, nil)
	}
	if err != nil {
		return fmt.Errorf("search: ISM policy: %w", err)
	}
	return nil
}
