// Package vault saves notes in an agent's knowledge vault: the shared path
// for the CLI's vault sync and skill pushes (ingest, management key) and
// for edits in the console (API).
//
// Every save redacts the text (secrets never reach storage), scans it with
// the security detectors, checks a skill against the agent's passport,
// stores a new version and, when the ledger is on, writes a vault_change
// entry to the agent's ledger.
package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/ledger"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/redact"
	"github.com/bindle-ai/bindle/backend/internal/skills"
)

// Store saves notes (control.vault_put).
type Store interface {
	VaultPut(ctx context.Context, in control.VaultPutInput) (control.VaultPutResult, error)
}

// Appender writes ledger entries (ledger.Writer).
type Appender interface {
	Append(ctx context.Context, tenant, agent string, entries []ledger.Entry) (int, error)
}

// Saver holds what saving a note needs.
type Saver struct {
	Store    Store
	Passport func(ctx context.Context, tenant, agent string) (*passport.Passport, error)
	Tools    func(ctx context.Context, tenant, agent string) ([]string, error) // tools the agent called
	Labels   func(ctx context.Context, tenant string) (map[string]bool, error) // known data classes
	Ledger   Appender                                                          // nil: no ledger entries
	LedgerOn func(ctx context.Context, tenant string) bool
	Now      func() time.Time
}

// PassportError rejects a skill that references tools or data classes
// outside the agent's passport. Nothing is stored.
type PassportError struct{ Check skills.Check }

func (e *PassportError) Error() string { return "skill rejected: " + e.Check.Reason }

// Result is what a save did.
type Result struct {
	Agent      string        `json:"agent"`
	Path       string        `json:"path"`
	Kind       string        `json:"kind"`
	Status     string        `json:"status,omitempty"`
	Version    int           `json:"version"`
	SHA256     string        `json:"sha256"`
	Unchanged  bool          `json:"unchanged"`
	Conflict   bool          `json:"conflict"`
	CurrentSHA string        `json:"current_sha256,omitempty"`
	Scan       skills.Scan   `json:"scan"`
	Check      *skills.Check `json:"passport_check,omitempty"`
}

// MaxAgent bounds an agent name.
const MaxAgent = 128

// CleanAgent validates an agent name.
func CleanAgent(a string) (string, error) {
	if a == "" || len(a) > MaxAgent || strings.TrimSpace(a) != a || strings.ContainsAny(a, "\n\r\t") {
		return "", &skills.Error{Msg: fmt.Sprintf("agent must be 1-%d characters on one line", MaxAgent)}
	}
	return a, nil
}

// Save validates and stores a note. base is the sha256 of the text the
// writer started from ("" for a new note); a different current text is a
// conflict, reported in the result with nothing stored.
func (s *Saver) Save(ctx context.Context, tenant, agent, path, content, base, by string) (Result, error) {
	agent, err := CleanAgent(agent)
	if err != nil {
		return Result{}, err
	}
	if !skills.ValidPath(path) {
		return Result{}, &skills.Error{Msg: "path must be a relative .md path of lowercase folders, without .. (at most 200 characters)"}
	}
	if len(content) > skills.MaxBytes || !utf8.ValidString(content) {
		return Result{}, &skills.Error{Msg: fmt.Sprintf("a note must be UTF-8 text of at most %d KiB", skills.MaxBytes>>10)}
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	kind := skills.KindForPath(path)
	res := Result{Agent: agent, Path: path, Kind: kind}

	var sk *skills.Skill
	if kind == skills.KindSkill {
		name, ok := skills.NameFromPath(path)
		if !ok {
			return Result{}, &skills.Error{Msg: "a skill lives at skills/<name>/SKILL.md"}
		}
		if sk, err = skills.Parse(content); err != nil {
			return Result{}, err
		}
		if sk.Name != name {
			return Result{}, &skills.Error{Msg: fmt.Sprintf("the skill's name (%s) must match its folder (%s)", sk.Name, name)}
		}
		if a := sk.Metadata[skills.MetaAgent]; a != "" && a != agent {
			return Result{}, &skills.Error{Msg: fmt.Sprintf("metadata %s is %s, but this is %s's vault", skills.MetaAgent, a, agent)}
		}
	}

	stored, scan := skills.Clean(content)
	res.Scan = scan
	if sk != nil {
		tools, err := s.tools(ctx, tenant, agent)
		if err != nil {
			return Result{}, err
		}
		labels, err := s.labels(ctx, tenant)
		if err != nil {
			return Result{}, err
		}
		// Parse the stored (redacted) text, so the check sees what is kept.
		parsed, perr := skills.Parse(stored)
		if perr != nil {
			parsed = sk
		}
		refTools, refClasses := skills.References(parsed, stored, tools, labels)
		var p *passport.Passport
		if s.Passport != nil {
			if p, err = s.Passport(ctx, tenant, agent); err != nil {
				return Result{}, err
			}
		}
		check := skills.CheckPassport(p, refTools, refClasses)
		if p != nil && !check.OK {
			return Result{}, &PassportError{Check: check}
		}
		res.Check = &check
	}

	res.SHA256 = skills.Digest(stored)
	var check any = map[string]any{}
	if res.Check != nil {
		check = res.Check
	}
	put, err := s.Store.VaultPut(ctx, control.VaultPutInput{Tenant: tenant, Agent: agent, Path: path, Kind: kind, Body: stored,
		SHA256: res.SHA256, BaseSHA256: base, Scan: scan, Check: check, Links: skills.Links(stored), By: by})
	if err != nil {
		return Result{}, err
	}
	res.Version, res.Unchanged, res.Conflict, res.CurrentSHA = put.Version, put.Unchanged, put.Conflict, put.CurrentSHA
	if kind == skills.KindSkill && !res.Conflict {
		res.Status = skills.StatusDraft
	}
	if !res.Conflict && !res.Unchanged {
		action := "edit"
		if put.Version == 1 {
			action = "create"
		}
		if err := s.Record(ctx, tenant, agent, path, put.Version, res.SHA256, action, by); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Record writes a vault_change entry when the ledger is on for the tenant.
// Retrying is safe: the entry is keyed by path and version.
func (s *Saver) Record(ctx context.Context, tenant, agent, path string, version int, sha, action, by string) error {
	if s.Ledger == nil || s.LedgerOn == nil || !s.LedgerOn(ctx, tenant) {
		return nil
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	_, err := s.Ledger.Append(ctx, tenant, agent, []ledger.Entry{ledger.VaultChange(agent, path, version, sha, action, by, now)})
	if err != nil {
		return fmt.Errorf("vault: ledger: %w", err)
	}
	return nil
}

func (s *Saver) tools(ctx context.Context, tenant, agent string) ([]string, error) {
	if s.Tools == nil {
		return nil, nil
	}
	return s.Tools(ctx, tenant, agent)
}

func (s *Saver) labels(ctx context.Context, tenant string) (map[string]bool, error) {
	known := map[string]bool{}
	for _, l := range redact.Labels {
		known[l] = true
	}
	if s.Labels == nil {
		return known, nil
	}
	extra, err := s.Labels(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for l := range extra {
		known[l] = true
	}
	return known, nil
}

// IsPassportError reports whether err rejects a skill for its passport.
func IsPassportError(err error) (*PassportError, bool) {
	var pe *PassportError
	ok := errors.As(err, &pe)
	return pe, ok
}

// ---- console actions ----------------------------------------------------------

// Actor is the person acting in the console.
type Actor struct {
	UserID   string
	Email    string
	Role     string // workspace role: owner, admin, member
	Reviewer bool   // a security reviewer
}

// Note is the state an action is decided on.
type Note struct {
	Kind        string
	Status      string
	SHA256      string
	ReviewedSHA string
	Flagged     bool
}

// Decide checks whether actor may take action on a skill, and returns the
// new status. Approving needs the agent's named passport owner (matched by
// email), a passport check that passes now, and either a clean security
// scan or a security reviewer's clearance for this exact text. Retiring and
// restoring need the owner or a workspace owner or admin.
func Decide(n Note, action string, actor Actor, p *passport.Passport, check skills.Check) (string, error) {
	if n.Kind != skills.KindSkill {
		return "", &skills.Error{Msg: "only skills have a status"}
	}
	to, err := skills.Transition(n.Status, action)
	if err != nil {
		return "", err
	}
	owner := p != nil && actor.Email != "" && strings.EqualFold(strings.TrimSpace(p.Owner), actor.Email)
	switch action {
	case skills.ActionApprove:
		if p == nil {
			return "", &DeniedError{Msg: "the agent has no passport; approving needs its named owner"}
		}
		if !owner {
			return "", &DeniedError{Msg: fmt.Sprintf("only the agent's owner (%s, from its passport) can approve skills", p.Owner)}
		}
		if !check.OK {
			return "", &DeniedError{Msg: "the passport check fails: " + check.Reason}
		}
		if n.Flagged && n.ReviewedSHA != n.SHA256 {
			return "", &DeniedError{Msg: "the security scan flagged this skill; a security reviewer must clear it first"}
		}
	default:
		if !owner && actor.Role != control.RoleOwner && actor.Role != control.RoleAdmin {
			return "", &DeniedError{Msg: "only the agent's owner or a workspace owner or admin can do that"}
		}
	}
	return to, nil
}

// DeniedError means the actor may not take the action.
type DeniedError struct{ Msg string }

func (e *DeniedError) Error() string { return e.Msg }
