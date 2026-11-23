// Package ledger is the tamper-evident, per-agent journal of what an agent
// saw, decided, called and cost. Each entry summarizes one already-redacted
// step (never prompts or outputs) and is chained to the previous one:
//
//	hash = SHA-256(prev_hash || canonical(entry))
//
// The canonical encoding is versioned (field "v"), so it can change later
// without breaking verification of older entries.
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/model"
)

// Entry kinds.
const (
	KindStep        = "step"
	KindDrift       = "drift"
	KindSkillChange = "skill_change"
	KindVaultChange = "vault_change"
)

// Version is the canonical encoding version written into every new entry.
const Version = 1

// Genesis is the prev_hash of an agent's first entry.
var Genesis = make([]byte, sha256.Size)

// Entry is one ledger record. TenantID, Agent, Seq, Day and PrevHash are set
// when it is appended.
type Entry struct {
	TenantID      string    `json:"-"`
	Agent         string    `json:"agent"`
	Seq           int64     `json:"seq"`
	Day           string    `json:"day"` // UTC day it was appended, YYYY-MM-DD
	At            time.Time `json:"at"`  // when the step happened (microsecond precision)
	Kind          string    `json:"kind"`
	TraceID       string    `json:"trace_id,omitempty"`
	StepID        string    `json:"step_id,omitempty"`
	EntryKey      string    `json:"entry_key,omitempty"`
	StepType      string    `json:"step_type,omitempty"`
	StepName      string    `json:"step_name,omitempty"`
	ToolName      string    `json:"tool_name,omitempty"`
	Status        string    `json:"status,omitempty"`
	ErrorClass    string    `json:"error_class,omitempty"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CostUSD       string    `json:"cost_usd"` // decimal string, exactly as hashed
	CostKnown     bool      `json:"cost_known"`
	SecurityFlags []string  `json:"security_flags,omitempty"`
	Detail        string    `json:"detail"` // canonical JSON object
	PrevHash      []byte    `json:"-"`
	Hash          []byte    `json:"-"`
}

// Canonical is the entry's hashed form: a JSON object with sorted keys, no
// insignificant whitespace and no HTML escaping. Every field is a string,
// integer, boolean or array of strings, so there are no float formatting
// choices left to make.
func Canonical(e Entry) []byte {
	flags := append([]string{}, e.SecurityFlags...)
	sort.Strings(flags)
	m := map[string]any{
		"v":              Version,
		"tenant":         e.TenantID,
		"agent":          e.Agent,
		"seq":            e.Seq,
		"day":            e.Day,
		"at":             e.At.UTC().Format("2006-01-02T15:04:05.000000Z"),
		"kind":           e.Kind,
		"trace_id":       e.TraceID,
		"step_id":        e.StepID,
		"entry_key":      e.EntryKey,
		"step_type":      e.StepType,
		"step_name":      e.StepName,
		"tool_name":      e.ToolName,
		"status":         e.Status,
		"error_class":    e.ErrorClass,
		"input_tokens":   e.InputTokens,
		"output_tokens":  e.OutputTokens,
		"cost_usd":       e.CostUSD,
		"cost_known":     e.CostKnown,
		"security_flags": flags,
		"detail":         e.Detail,
		"prev_hash":      hex.EncodeToString(e.PrevHash),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m) // map keys are sorted by encoding/json
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// Hash computes an entry's hash from its prev_hash and canonical form.
func Hash(e Entry) []byte {
	h := sha256.New()
	h.Write(e.PrevHash)
	h.Write(Canonical(e))
	return h.Sum(nil)
}

// FormatCost renders a USD amount as the decimal string stored and hashed.
func FormatCost(usd float64) string {
	if usd <= 0 {
		return "0"
	}
	return strconv.FormatFloat(usd, 'f', -1, 64)
}

// canonicalDetail encodes a small detail object the same way as entries.
func canonicalDetail(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m)
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// FromStep summarizes a normalized, already-redacted step. Inputs, outputs,
// reasoning and raw error messages are never included: the error is its
// normalized pattern, which has IDs, numbers and quoted values removed.
func FromStep(s model.Step) Entry {
	at := time.Unix(0, int64(s.StartUnixNano)).UTC().Truncate(time.Microsecond)
	detail := map[string]string{}
	if s.AgentVersion != "" {
		detail["agent_version"] = s.AgentVersion
	}
	if s.Model != "" {
		detail["model"] = s.Model
	}
	if s.Provider != "" {
		detail["provider"] = s.Provider
	}
	if s.DurationMs > 0 {
		detail["duration_ms"] = strconv.FormatFloat(s.DurationMs, 'f', 3, 64)
	}
	if s.SessionID != "" {
		detail["session_id"] = s.SessionID
	}
	flags := append([]string{}, s.SecurityFlags...)
	sort.Strings(flags)
	return Entry{
		TenantID:      s.TenantID,
		Agent:         s.AgentName,
		At:            at,
		Kind:          KindStep,
		TraceID:       s.TraceID,
		StepID:        s.StepID,
		StepType:      s.Type,
		StepName:      s.Name,
		ToolName:      s.ToolName,
		Status:        s.Status,
		ErrorClass:    s.ErrorPattern,
		InputTokens:   s.InputTokens,
		OutputTokens:  s.OutputTokens,
		CostUSD:       FormatCost(s.CostUSD),
		CostKnown:     s.CostKnown,
		SecurityFlags: flags,
		Detail:        canonicalDetail(detail),
	}
}

// key identifies an entry for idempotency: a retried batch has the same keys.
type key struct{ kind, trace, step, ekey string }

func (e Entry) key() key { return key{e.Kind, e.TraceID, e.StepID, e.EntryKey} }
