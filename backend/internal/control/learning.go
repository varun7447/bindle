package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/backend/internal/lessons"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/skills"
)

// ---- lessons (alerts worker writes, ingest and API read) ----------------------

// LearningTenant is a workspace with the learning loop on.
type LearningTenant struct {
	TenantID string
	MinRuns  int
	Capture  bool // prompts and outputs are captured; packs carry content only then
}

// LessonWriter stores recomputed lessons as the alerts role.
type LessonWriter struct{ DB *sql.DB }

// Tenants lists workspaces with the learning loop on.
func (w *LessonWriter) Tenants(ctx context.Context) ([]LearningTenant, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT tenant_id, min_runs, capture FROM control.learning_tenants()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LearningTenant
	for rows.Next() {
		var t LearningTenant
		if err := rows.Scan(&t.TenantID, &t.MinRuns, &t.Capture); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type lessonRow struct {
	ID        string       `json:"id"`
	Agent     string       `json:"agent"`
	StepName  string       `json:"step_name"`
	StepType  string       `json:"step_type"`
	Tool      string       `json:"tool_name"`
	Pattern   string       `json:"error_pattern"`
	Cause     string       `json:"cause"`
	Title     string       `json:"title"`
	Runs      int          `json:"runs"`
	Failures  int          `json:"failures"`
	FirstSeen time.Time    `json:"first_seen"`
	LastSeen  time.Time    `json:"last_seen"`
	Evidence  []string     `json:"evidence"`
	Pack      lessons.Pack `json:"pack"`
}

// Replace stores a workspace's freshly computed lessons, removing lessons
// no longer seen, and the tool names each agent called.
func (w *LessonWriter) Replace(ctx context.Context, tenant string, ls []lessons.Lesson, tools map[string][]string) (int, error) {
	rows := make([]lessonRow, 0, len(ls))
	for _, l := range ls {
		ev := l.Evidence
		if ev == nil {
			ev = []string{}
		}
		rows = append(rows, lessonRow{l.ID, l.Agent, l.StepName, l.StepType, l.Tool, l.Pattern, l.Cause, l.Title, l.Runs, l.Failures,
			l.FirstSeen, l.LastSeen, ev, l.Pack})
	}
	if tools == nil {
		tools = map[string][]string{}
	}
	rj, err := json.Marshal(rows)
	if err != nil {
		return 0, err
	}
	tj, err := json.Marshal(tools)
	if err != nil {
		return 0, err
	}
	var n int
	err = w.DB.QueryRowContext(ctx, `SELECT control.lessons_replace($1, $2, $3)`, tenant, rj, tj).Scan(&n)
	return n, err
}

// LessonSummary is a lesson without its pack.
type LessonSummary struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	StepName string    `json:"step_name"`
	Tool     string    `json:"tool,omitempty"`
	Pattern  string    `json:"error_pattern"`
	Cause    string    `json:"cause"`
	Title    string    `json:"title"`
	Runs     int       `json:"runs"`
	Failures int       `json:"failures"`
	LastSeen time.Time `json:"last_seen"`
	Evidence []string  `json:"evidence"`
}

func scanLessons(rows *sql.Rows) ([]LessonSummary, error) {
	defer rows.Close()
	out := []LessonSummary{}
	for rows.Next() {
		var l LessonSummary
		var ev pq.StringArray
		if err := rows.Scan(&l.ID, &l.Agent, &l.StepName, &l.Tool, &l.Pattern, &l.Cause, &l.Title, &l.Runs, &l.Failures, &l.LastSeen, &ev); err != nil {
			return nil, err
		}
		l.Evidence = append([]string{}, ev...)
		out = append(out, l)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LearningIngest is the learning loop's data access for ingest (the CLI's
// management-key endpoints), through the control.* functions only.
type LearningIngest struct{ DB *sql.DB }

// Lessons lists a workspace's lessons, optionally for one agent.
func (l *LearningIngest) Lessons(ctx context.Context, tenant, agent string) ([]LessonSummary, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT id, agent, step_name, tool_name, error_pattern, cause, title, runs, failures, last_seen, evidence
		FROM control.lessons_for($1, $2)`, tenant, nullable(agent))
	if err != nil {
		return nil, err
	}
	return scanLessons(rows)
}

// Pack returns a lesson's pack, or ok=false when there is no such lesson.
func (l *LearningIngest) Pack(ctx context.Context, tenant, id string) (json.RawMessage, bool, error) {
	var raw []byte
	err := l.DB.QueryRowContext(ctx, `SELECT control.lesson_pack($1, $2)`, tenant, id).Scan(&raw)
	if err != nil || raw == nil {
		return nil, false, err
	}
	return raw, true, nil
}

// AgentTools returns the tool names an agent called recently.
func (l *LearningIngest) AgentTools(ctx context.Context, tenant, agent string) ([]string, error) {
	var tools pq.StringArray
	err := l.DB.QueryRowContext(ctx, `SELECT control.agent_tools_for($1, $2)`, tenant, agent).Scan(&tools)
	return append([]string{}, tools...), err
}

// Passport returns an agent's active passport (nil: none).
func (l *LearningIngest) Passport(ctx context.Context, tenant, agent string) (*passport.Passport, error) {
	set, err := ActivePassports(ctx, l.DB, tenant)
	if err != nil {
		return nil, err
	}
	return set.For(agent), nil
}

// VaultFile is one note as the CLI syncs it.
type VaultFile struct {
	Path      string    `json:"path"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status,omitempty"`
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256"`
	Body      string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VaultList returns every note of an agent.
func (l *LearningIngest) VaultList(ctx context.Context, tenant, agent string) ([]VaultFile, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT path, kind, status, version, sha256, body, updated_at FROM control.vault_list($1, $2)`, tenant, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VaultFile{}
	for rows.Next() {
		var f VaultFile
		var st sql.NullString
		if err := rows.Scan(&f.Path, &f.Kind, &st, &f.Version, &f.SHA256, &f.Body, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.Status = st.String
		out = append(out, f)
	}
	return out, rows.Err()
}

// VaultPut saves a note (see control.vault_put).
func (l *LearningIngest) VaultPut(ctx context.Context, in VaultPutInput) (VaultPutResult, error) {
	return vaultPut(ctx, l.DB, in)
}

// ---- the vault -------------------------------------------------------------------

// VaultPutInput is a note to save. Scan and Check are stored as given.
type VaultPutInput struct {
	Tenant, Agent, Path, Kind string
	Body, SHA256, BaseSHA256  string
	Scan, Check               any
	Links                     []string
	By                        string
}

// VaultPutResult says what saving did.
type VaultPutResult struct {
	Version    int    `json:"version"`
	Conflict   bool   `json:"conflict"`
	Unchanged  bool   `json:"unchanged"`
	CurrentSHA string `json:"current_sha256"`
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func vaultPut(ctx context.Context, db queryRower, in VaultPutInput) (VaultPutResult, error) {
	scan, err := json.Marshal(in.Scan)
	if err != nil {
		return VaultPutResult{}, err
	}
	check, err := json.Marshal(in.Check)
	if err != nil {
		return VaultPutResult{}, err
	}
	links := in.Links
	if links == nil {
		links = []string{}
	}
	var r VaultPutResult
	err = db.QueryRowContext(ctx, `SELECT version, conflict, unchanged, current_sha FROM
		control.vault_put($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		in.Tenant, in.Agent, in.Path, in.Kind, in.Body, in.SHA256, in.BaseSHA256, scan, check, pq.Array(links), in.By).
		Scan(&r.Version, &r.Conflict, &r.Unchanged, &r.CurrentSHA)
	return r, err
}

// VaultPut saves a note from the console.
func (s *Store) VaultPut(ctx context.Context, in VaultPutInput) (VaultPutResult, error) {
	return vaultPut(ctx, s.DB, in)
}

// VaultSetStatus moves a skill from one status to another, at the exact
// text (sha) the caller saw. ok=false: the note changed meanwhile.
func (s *Store) VaultSetStatus(ctx context.Context, tenant, agent, path, from, to, sha, action, by string) (int, bool, error) {
	var v int
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT version, ok FROM control.vault_set_status($1, $2, $3, $4, $5, $6, $7, $8)`,
		tenant, agent, path, from, to, sha, action, by).Scan(&v, &ok)
	return v, ok, err
}

// VaultReview records that a security reviewer cleared a skill's scan for
// its current text.
func (s *Store) VaultReview(ctx context.Context, tenant, agent, path, sha, by string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT control.vault_review($1, $2, $3, $4, $5)`, tenant, agent, path, sha, by).Scan(&ok)
	return ok, err
}

// VaultNote is a note with its scan, passport check, history and backlinks.
type VaultNote struct {
	Agent       string          `json:"agent"`
	Path        string          `json:"path"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status,omitempty"`
	Version     int             `json:"version"`
	SHA256      string          `json:"sha256"`
	Body        string          `json:"content,omitempty"`
	Scan        json.RawMessage `json:"scan"`
	Check       json.RawMessage `json:"passport_check"`
	ReviewedSHA string          `json:"reviewed_sha256,omitempty"`
	ReviewedBy  string          `json:"reviewed_by,omitempty"`
	UpdatedBy   string          `json:"updated_by"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Links       []string        `json:"links,omitempty"`
	Backlinks   []string        `json:"backlinks,omitempty"`
	Versions    []VaultVersion  `json:"versions,omitempty"`
}

// VaultVersion is one entry of a note's history.
type VaultVersion struct {
	Version  int       `json:"version"`
	Status   string    `json:"status,omitempty"`
	Action   string    `json:"action"`
	SHA256   string    `json:"sha256"`
	EditedBy string    `json:"edited_by"`
	EditedAt time.Time `json:"edited_at"`
}

// VaultLink is one [[link]] between notes or to a run, tool or agent.
type VaultLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

const noteCols = `agent, path, kind, status, version, sha256, scan, passport_check, COALESCE(reviewed_sha, ''), COALESCE(reviewed_by, ''), updated_by, updated_at`

func scanNote(sc interface{ Scan(...any) error }, n *VaultNote) error {
	var st sql.NullString
	if err := sc.Scan(&n.Agent, &n.Path, &n.Kind, &st, &n.Version, &n.SHA256, &n.Scan, &n.Check, &n.ReviewedSHA, &n.ReviewedBy,
		&n.UpdatedBy, &n.UpdatedAt); err != nil {
		return err
	}
	n.Status = st.String
	return nil
}

// VaultNotes lists an agent's notes (without bodies) and every link.
func (s *Store) VaultNotes(ctx context.Context, tenant, agent string) ([]VaultNote, []VaultLink, error) {
	notes := []VaultNote{}
	links := []VaultLink{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+noteCols+` FROM control.vault_notes WHERE agent = $1 ORDER BY path`, agent)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n VaultNote
			if err := scanNote(rows, &n); err != nil {
				rows.Close()
				return err
			}
			notes = append(notes, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lr, err := tx.QueryContext(ctx, `SELECT path, target FROM control.vault_links WHERE agent = $1 ORDER BY path, target`, agent)
		if err != nil {
			return err
		}
		defer lr.Close()
		for lr.Next() {
			var l VaultLink
			if err := lr.Scan(&l.Path, &l.Target); err != nil {
				return err
			}
			links = append(links, l)
		}
		return lr.Err()
	})
	return notes, links, err
}

// GetVaultNote returns one note with body, history, links and backlinks.
func (s *Store) GetVaultNote(ctx context.Context, tenant, agent, path string) (VaultNote, error) {
	var n VaultNote
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+noteCols+`, body FROM control.vault_notes WHERE agent = $1 AND path = $2`, agent, path)
		var st sql.NullString
		err := row.Scan(&n.Agent, &n.Path, &n.Kind, &st, &n.Version, &n.SHA256, &n.Scan, &n.Check, &n.ReviewedSHA, &n.ReviewedBy,
			&n.UpdatedBy, &n.UpdatedAt, &n.Body)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		n.Status = st.String
		vr, err := tx.QueryContext(ctx, `SELECT version, COALESCE(status, ''), action, sha256, edited_by, edited_at
			FROM control.vault_versions WHERE agent = $1 AND path = $2 ORDER BY version DESC LIMIT 100`, agent, path)
		if err != nil {
			return err
		}
		for vr.Next() {
			var v VaultVersion
			if err := vr.Scan(&v.Version, &v.Status, &v.Action, &v.SHA256, &v.EditedBy, &v.EditedAt); err != nil {
				vr.Close()
				return err
			}
			n.Versions = append(n.Versions, v)
		}
		vr.Close()
		if err := vr.Err(); err != nil {
			return err
		}
		n.Links, err = strings1(ctx, tx, `SELECT target FROM control.vault_links WHERE agent = $1 AND path = $2 ORDER BY target`, agent, path)
		if err != nil {
			return err
		}
		n.Backlinks, err = strings1(ctx, tx, `SELECT path FROM control.vault_links WHERE agent = $1 AND target = $2 ORDER BY path`, agent, path)
		return err
	})
	return n, err
}

func strings1(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListLessons lists a workspace's lessons for the console.
func (s *Store) ListLessons(ctx context.Context, tenant, agent string) ([]LessonSummary, error) {
	var out []LessonSummary
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, agent, step_name, tool_name, error_pattern, cause, title, runs, failures, last_seen, evidence
			FROM control.lessons WHERE ($1 = '' OR agent = $1) ORDER BY runs DESC, id LIMIT 200`, agent)
		if err != nil {
			return err
		}
		out, err = scanLessons(rows)
		return err
	})
	return out, err
}

// AgentTools returns the tool names an agent called recently.
func (s *Store) AgentTools(ctx context.Context, tenant, agent string) ([]string, error) {
	var tools pq.StringArray
	err := s.DB.QueryRowContext(ctx, `SELECT control.agent_tools_for($1, $2)`, tenant, agent).Scan(&tools)
	return append([]string{}, tools...), err
}

// ActivePassport returns an agent's newest passport (nil: none).
func (s *Store) ActivePassport(ctx context.Context, tenant, agent string) (*passport.Passport, error) {
	var p *passport.Passport
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		var doc []byte
		err := tx.QueryRowContext(ctx, `SELECT document FROM control.passports WHERE agent = $1 ORDER BY version DESC LIMIT 1`, agent).Scan(&doc)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		p, err = passport.Parse(doc, nil)
		return err
	})
	return p, err
}

// LearningSettings are a workspace's learning-loop settings.
type LearningSettings struct {
	MinRuns int `json:"min_runs"`
}

// GetLearningSettings returns the settings (defaults when never saved).
func (s *Store) GetLearningSettings(ctx context.Context, tenant string) (LearningSettings, error) {
	out := LearningSettings{MinRuns: lessons.DefaultMinRuns}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT min_runs FROM control.learning_settings`).Scan(&out.MinRuns)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}

// PutLearningSettings saves the settings.
func (s *Store) PutLearningSettings(ctx context.Context, tenant, userID string, ls LearningSettings) (LearningSettings, error) {
	if ls.MinRuns < lessons.MinMinRuns || ls.MinRuns > lessons.MaxMinRuns {
		return LearningSettings{}, &skills.Error{Msg: "min_runs must be between 2 and 1000"}
	}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO control.learning_settings (tenant_id, min_runs, updated_by) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id) DO UPDATE SET min_runs = EXCLUDED.min_runs, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			tenant, ls.MinRuns, nullable(userID))
		return err
	})
	return ls, err
}

// ---- people ------------------------------------------------------------------------

// UserEmail returns a user's email ("" when unknown).
func (s *Store) UserEmail(ctx context.Context, tenant, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	var email string
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT u.email FROM control.users u JOIN control.memberships m ON m.user_id = u.id
			WHERE u.id = $1 AND m.tenant_id = $2`, userID, tenant).Scan(&email)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return email, err
}

// SecurityReviewer is a person who may clear a flagged skill's scan.
type SecurityReviewer struct {
	UserID  string    `json:"user_id"`
	Email   string    `json:"email"`
	AddedAt time.Time `json:"added_at"`
}

// ListSecurityReviewers lists the workspace's security reviewers.
func (s *Store) ListSecurityReviewers(ctx context.Context, tenant string) ([]SecurityReviewer, error) {
	out := []SecurityReviewer{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.user_id, u.email, r.added_at FROM control.security_reviewers r
			JOIN control.users u ON u.id = r.user_id ORDER BY u.email`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r SecurityReviewer
			if err := rows.Scan(&r.UserID, &r.Email, &r.AddedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// AddSecurityReviewer makes a member of the workspace (by email) a
// security reviewer.
func (s *Store) AddSecurityReviewer(ctx context.Context, tenant, byUser, email string) (SecurityReviewer, error) {
	var r SecurityReviewer
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT u.id, u.email FROM control.users u JOIN control.memberships m ON m.user_id = u.id
			WHERE m.tenant_id = $1 AND lower(u.email) = lower($2)`, tenant, strings.TrimSpace(email)).Scan(&r.UserID, &r.Email)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO control.security_reviewers (tenant_id, user_id, added_by) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id, user_id) DO UPDATE SET added_by = control.security_reviewers.added_by
			RETURNING added_at`, tenant, r.UserID, nullable(byUser)).Scan(&r.AddedAt)
	})
	return r, err
}

// RemoveSecurityReviewer removes a security reviewer.
func (s *Store) RemoveSecurityReviewer(ctx context.Context, tenant, userID string) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM control.security_reviewers WHERE user_id = $1`, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// IsSecurityReviewer reports whether a user is a security reviewer.
func (s *Store) IsSecurityReviewer(ctx context.Context, tenant, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var ok bool
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM control.security_reviewers WHERE user_id = $1)`, userID).Scan(&ok)
	})
	return ok, err
}
