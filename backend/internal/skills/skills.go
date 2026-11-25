// Package skills validates learned skills and checks them before a person
// can approve them.
//
// A skill is a SKILL.md file in the open Agent Skills format
// (agentskills.io/specification): YAML frontmatter with name and description
// (required) and license, compatibility, metadata and allowed-tools
// (optional), then a Markdown body. Bindle drafts skills from lessons in the
// customer's environment (the CLI calls the customer's own model), so this
// package only parses, scans and checks; it never calls a model.
//
// The frontmatter parser accepts the subset the specification uses: one
// "key: value" per line, values optionally quoted, and metadata as an
// indented map of strings. Anything else (block scalars, lists, anchors) is
// rejected with a message, rather than guessed at.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/redact"
	"github.com/bindle-ai/bindle/backend/internal/security"
)

// Feature is the per-workspace flag for the learning loop.
const Feature = "learning_loop"

// Limits from the specification, plus a size cap for storage.
const (
	MaxName          = 64
	MaxDescription   = 1024
	MaxCompatibility = 500
	MaxLines         = 500 // the specification recommends a SKILL.md under 500 lines
	MaxBytes         = 64 << 10
)

// Bindle's keys in the metadata map.
const (
	MetaLesson  = "bindle-lesson"
	MetaAgent   = "bindle-agent"
	MetaStatus  = "bindle-status"
	MetaVersion = "bindle-version"
)

// Skill is a parsed SKILL.md.
type Skill struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  []string          `json:"allowed_tools,omitempty"`
	Body          string            `json:"-"`
}

// Error is a SKILL.md that doesn't follow the specification.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// IsInvalid reports whether err is a validation Error.
func IsInvalid(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

func invalid(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	keyRe     = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):(?:\s(.*))?$`)
	metaKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Parse reads and validates a SKILL.md against the specification.
func Parse(text string) (*Skill, error) {
	if len(text) > MaxBytes {
		return nil, invalid("a SKILL.md must be at most %d KiB", MaxBytes>>10)
	}
	if !utf8.ValidString(text) {
		return nil, invalid("a SKILL.md must be UTF-8 text")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, invalid("a SKILL.md must start with YAML frontmatter between --- lines")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return nil, invalid("the frontmatter has no closing --- line")
	}
	front := text[4 : 4+end]
	rest := text[4+end+4:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 && strings.TrimSpace(rest[:nl]) == "" {
		rest = rest[nl+1:]
	} else if strings.TrimSpace(rest) == "" {
		rest = ""
	} else if nl < 0 || strings.TrimSpace(rest[:nl]) != "" {
		return nil, invalid("the closing --- line must be on its own")
	}
	s := &Skill{Body: strings.TrimLeft(rest, "\n")}
	if err := s.parseFront(front); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	if n := strings.Count(text, "\n") + 1; n > MaxLines {
		return nil, invalid("a SKILL.md should stay under %d lines (this one has %d); move detail to referenced files", MaxLines, n)
	}
	return s, nil
}

func (s *Skill) parseFront(front string) error {
	seen := map[string]bool{}
	lines := strings.Split(front, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return invalid("frontmatter line %d: unexpected indentation", i+1)
		}
		m := keyRe.FindStringSubmatch(line)
		if m == nil {
			return invalid("frontmatter line %d: expected \"key: value\"", i+1)
		}
		key, raw := m[1], strings.TrimSpace(m[2])
		if seen[key] {
			return invalid("frontmatter: %s is set twice", key)
		}
		seen[key] = true
		if key == "metadata" {
			if raw != "" {
				return invalid("frontmatter: metadata must be an indented map of strings")
			}
			s.Metadata = map[string]string{}
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				ml := strings.TrimSpace(lines[i])
				if ml == "" {
					continue
				}
				mm := keyRe.FindStringSubmatch(ml)
				if mm == nil {
					return invalid("frontmatter line %d: expected \"key: value\" under metadata", i+1)
				}
				if !metaKeyRe.MatchString(mm[1]) {
					return invalid("frontmatter: metadata key %q is not valid", mm[1])
				}
				v, err := scalar(strings.TrimSpace(mm[2]))
				if err != nil {
					return invalid("frontmatter: metadata %s: %s", mm[1], err.Error())
				}
				if _, dup := s.Metadata[mm[1]]; dup {
					return invalid("frontmatter: metadata %s is set twice", mm[1])
				}
				s.Metadata[mm[1]] = v
			}
			continue
		}
		v, err := scalar(raw)
		if err != nil {
			return invalid("frontmatter: %s: %s", key, err.Error())
		}
		switch key {
		case "name":
			s.Name = v
		case "description":
			s.Description = v
		case "license":
			s.License = v
		case "compatibility":
			s.Compatibility = v
		case "allowed-tools":
			s.AllowedTools = strings.Fields(v)
		default:
			return invalid("frontmatter: unknown field %q (allowed: name, description, license, compatibility, metadata, allowed-tools)", key)
		}
	}
	return nil
}

// scalar reads a single-line YAML scalar: plain, "double" or 'single' quoted.
func scalar(raw string) (string, error) {
	switch {
	case raw == "":
		return "", nil
	case raw == "|" || raw == ">" || strings.HasPrefix(raw, "|") || strings.HasPrefix(raw, ">"):
		return "", errors.New("use a single-line value (block scalars aren't supported)")
	case strings.HasPrefix(raw, `"`):
		v, err := strconv.Unquote(raw)
		if err != nil {
			return "", errors.New("unterminated or invalid double-quoted value")
		}
		return v, nil
	case strings.HasPrefix(raw, "'"):
		if len(raw) < 2 || !strings.HasSuffix(raw, "'") {
			return "", errors.New("unterminated single-quoted value")
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	case strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "&") || strings.HasPrefix(raw, "*"):
		return "", errors.New("use a plain string value")
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	return raw, nil
}

func (s *Skill) validate() error {
	switch {
	case s.Name == "":
		return invalid("name is required")
	case len(s.Name) > MaxName || !nameRe.MatchString(s.Name):
		return invalid("name must be 1-%d lowercase letters, digits and single hyphens, not starting or ending with a hyphen", MaxName)
	case strings.TrimSpace(s.Description) == "":
		return invalid("description is required")
	case utf8.RuneCountInString(s.Description) > MaxDescription:
		return invalid("description must be at most %d characters", MaxDescription)
	case s.Compatibility != "" && utf8.RuneCountInString(s.Compatibility) > MaxCompatibility:
		return invalid("compatibility must be at most %d characters", MaxCompatibility)
	}
	return nil
}

// Path is where a skill lives in an agent's vault.
func Path(name string) string { return "skills/" + name + "/SKILL.md" }

// NameFromPath returns the skill name of a vault path "skills/<name>/SKILL.md".
func NameFromPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "skills/") || !strings.HasSuffix(p, "/SKILL.md") {
		return "", false
	}
	n := strings.TrimSuffix(strings.TrimPrefix(p, "skills/"), "/SKILL.md")
	return n, nameRe.MatchString(n) && len(n) <= MaxName
}

// Digest is the SHA-256 of a note's text, hex encoded.
func Digest(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// ---- security scan ----------------------------------------------------------

// Scan is the security scan of a skill or note. A flagged draft needs a
// security reviewer before it can be approved.
type Scan struct {
	Flagged bool     `json:"flagged"`
	Reasons []string `json:"reasons"` // "flag:reason", sorted
	// Redacted is true when the text contained values the redactor
	// removes; the stored text has markers in their place.
	Redacted bool `json:"redacted"`
}

// Clean redacts text (secrets and personal data never reach storage) and
// scans it with the same detectors Bindle runs on tool results: injected
// instructions, hidden Unicode-tag text, and credentials found by the
// redactor. It returns the text to store and the scan.
func Clean(text string) (string, Scan) {
	stored := redact.Default().Apply(text)
	res := security.Detect("retrieval", "", "", stored)
	sc := Scan{Reasons: append([]string{}, res.Reasons...), Redacted: stored != text}
	if strings.Contains(stored, "[REDACTED:") {
		for _, l := range redact.Found(stored) {
			if l == "SECRET" || l == "PRIVATE_KEY" {
				sc.Reasons = appendUnique(sc.Reasons, security.SecretExposed+":credential_in_skill")
			}
		}
	}
	sort.Strings(sc.Reasons)
	sc.Flagged = len(sc.Reasons) > 0
	return stored, sc
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// ---- passport check ---------------------------------------------------------

// Check is the passport check of a skill. A skill that references a tool or
// data class outside the agent's passport is rejected; with no passport,
// approval is blocked until there is one.
type Check struct {
	Passport bool     `json:"passport"`              // the agent has a passport
	OK       bool     `json:"ok"`                    // nothing outside the passport
	Tools    []string `json:"tools,omitempty"`       // tools the skill references
	Classes  []string `json:"classes,omitempty"`     // data classes it references
	BadTools []string `json:"bad_tools,omitempty"`   // referenced, not in tools.allow
	BadClass []string `json:"bad_classes,omitempty"` // referenced, not in data_classes.allow
	Reason   string   `json:"reason,omitempty"`
}

var wordRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_.:/-]*`)

// References finds the tools and data classes a skill mentions: every
// allowed-tools entry, every known tool name (tools the agent has called)
// that appears as a word in the text, and every data-class label that
// appears as a word or a redaction marker.
func References(s *Skill, stored string, knownTools []string, knownClasses map[string]bool) (tools, classes []string) {
	words := map[string]bool{}
	for _, w := range wordRe.FindAllString(stored, -1) {
		words[strings.TrimRight(w, ".:/-")] = true
	}
	t := map[string]bool{}
	for _, a := range s.AllowedTools {
		// Agent Skills tool entries look like Bash(git:*) or Read; the tool
		// is the part before any parenthesis.
		if i := strings.IndexByte(a, '('); i > 0 {
			a = a[:i]
		}
		if a != "" {
			t[a] = true
		}
	}
	for _, k := range knownTools {
		if k != "" && words[k] {
			t[k] = true
		}
	}
	c := map[string]bool{}
	for _, l := range redact.Found(stored) {
		c[l] = true
	}
	for l := range knownClasses {
		if words[l] {
			c[l] = true
		}
	}
	return sortedKeys(t), sortedKeys(c)
}

// CheckPassport compares a skill's references with the agent's passport.
// p is nil when the agent has none.
func CheckPassport(p *passport.Passport, tools, classes []string) Check {
	c := Check{Passport: p != nil, Tools: tools, Classes: classes}
	if p == nil {
		c.Reason = "the agent has no passport; push one before approving skills"
		return c
	}
	var allowTools, allowClasses []string
	if p.Tools != nil {
		allowTools = p.Tools.Allow
	}
	if p.DataClasses != nil {
		allowClasses = p.DataClasses.Allow
	}
	for _, t := range tools {
		if !passport.MatchAny(allowTools, t) {
			c.BadTools = append(c.BadTools, t)
		}
	}
	for _, l := range classes {
		if !passport.MatchAny(allowClasses, l) {
			c.BadClass = append(c.BadClass, l)
		}
	}
	c.OK = len(c.BadTools) == 0 && len(c.BadClass) == 0
	if !c.OK {
		var parts []string
		if len(c.BadTools) > 0 {
			parts = append(parts, "tools not in the passport: "+strings.Join(c.BadTools, ", "))
		}
		if len(c.BadClass) > 0 {
			parts = append(parts, "data classes not in the passport: "+strings.Join(c.BadClass, ", "))
		}
		c.Reason = strings.Join(parts, "; ")
	}
	return c
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- statuses ---------------------------------------------------------------

// Skill statuses. Notes other than skills have no status.
const (
	StatusDraft    = "draft"
	StatusProven   = "proven"
	StatusApproved = "approved"
	StatusLive     = "live"
	StatusRetired  = "retired"
)

// Note kinds in the vault.
const (
	KindSkill    = "skill"
	KindLesson   = "lesson"
	KindIncident = "incident"
	KindNote     = "note"
)

// Actions a person takes on a skill in the console.
const (
	ActionApprove = "approve" // proven -> approved: the agent's passport owner
	ActionRetire  = "retire"  // any -> retired: the owner, or a workspace owner or admin
	ActionRestore = "restore" // retired -> draft: the same
)

// Transition returns the status an action moves a skill to, or an error
// naming why it isn't allowed. Proving (draft -> proven) is recorded by a
// passing proof report and going live by shipping; neither is a console
// action.
func Transition(from, action string) (string, error) {
	switch action {
	case ActionApprove:
		if from != StatusProven {
			return "", invalid("only a proven skill can be approved (this one is %s); a passing proof report marks it proven", from)
		}
		return StatusApproved, nil
	case ActionRetire:
		if from == StatusRetired {
			return "", invalid("the skill is already retired")
		}
		return StatusRetired, nil
	case ActionRestore:
		if from != StatusRetired {
			return "", invalid("only a retired skill can be restored")
		}
		return StatusDraft, nil
	}
	return "", invalid("unknown action %q", action)
}

// ---- vault paths and links ----------------------------------------------------

var (
	pathRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*\.md$`)
	linkRe = regexp.MustCompile(`\[\[([^\[\]\n|]{1,200})(?:\|[^\[\]\n]{0,200})?\]\]`)
	runRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// MaxPath bounds a vault note's path.
const MaxPath = 200

// ValidPath reports whether p is a safe relative vault path ending in .md,
// without "..", empty segments or a leading slash.
func ValidPath(p string) bool {
	return len(p) <= MaxPath && pathRe.MatchString(p) && !strings.Contains(p, "..")
}

// KindForPath is the note kind a path implies.
func KindForPath(p string) string {
	switch {
	case strings.HasPrefix(p, "skills/"):
		return KindSkill
	case strings.HasPrefix(p, "lessons/"):
		return KindLesson
	case strings.HasPrefix(p, "incidents/"):
		return KindIncident
	}
	return KindNote
}

// Links returns a note's [[link]] targets, normalized and de-duplicated:
// "run:<trace>", "tool:<name>", "agent:<name>", or a vault path (".md" is
// added when missing). Malformed targets are dropped.
func Links(text string) []string {
	seen := map[string]bool{}
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		t := strings.TrimSpace(m[1])
		switch {
		case strings.HasPrefix(t, "run:"):
			id := strings.TrimPrefix(t, "run:")
			if !runRe.MatchString(id) {
				continue
			}
		case strings.HasPrefix(t, "tool:"), strings.HasPrefix(t, "agent:"):
			if len(t) > 140 || strings.ContainsAny(t, "\t") {
				continue
			}
		default:
			if !strings.HasSuffix(t, ".md") {
				t += ".md"
			}
			if !ValidPath(t) {
				continue
			}
		}
		seen[t] = true
	}
	return sortedKeys(seen)
}
