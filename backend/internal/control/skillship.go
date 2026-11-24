package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"

	"github.com/bindle-ai/bindle/backend/internal/skillship"
)

// ErrChanged means the skill's text changed since the caller's copy.
var ErrChanged = errors.New("control: the skill changed")

// ---- the alerts worker: cassettes for proving ----------------------------------

// CassetteWriter stores recorded runs of lessons' evidence (alerts role).
type CassetteWriter struct{ DB *sql.DB }

// LessonTrace is an evidence run without a cassette yet.
type LessonTrace struct{ Lesson, TraceID string }

// Missing lists the first n evidence runs of each lesson without a cassette.
func (w *CassetteWriter) Missing(ctx context.Context, tenant string, n int) ([]LessonTrace, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT lesson_id, trace_id FROM control.lesson_cassettes_missing($1, $2)`, tenant, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LessonTrace
	for rows.Next() {
		var lt LessonTrace
		if err := rows.Scan(&lt.Lesson, &lt.TraceID); err != nil {
			return nil, err
		}
		out = append(out, lt)
	}
	return out, rows.Err()
}

// Put stores one cassette (ignored when the lesson is gone).
func (w *CassetteWriter) Put(ctx context.Context, tenant, lesson, trace string, cassette []byte) error {
	var ok sql.NullBool
	err := w.DB.QueryRowContext(ctx, `SELECT control.lesson_cassette_put($1, $2, $3, $4)`, tenant, lesson, trace, cassette).Scan(&ok)
	return err
}

// ---- ingest: proving and shipping ------------------------------------------------

// Cassettes returns a lesson's recorded evidence runs (a JSON array).
func (l *LearningIngest) Cassettes(ctx context.Context, tenant, lesson string) (json.RawMessage, error) {
	var out []byte
	err := l.DB.QueryRowContext(ctx, `SELECT control.lesson_cassettes_for($1, $2)`, tenant, lesson).Scan(&out)
	return out, err
}

// ProofThreshold is the workspace's proof threshold (percent).
func (l *LearningIngest) ProofThreshold(ctx context.Context, tenant string) (int, error) {
	var n int
	err := l.DB.QueryRowContext(ctx, `SELECT control.proof_threshold_for($1)`, tenant).Scan(&n)
	return n, err
}

// ProofResult is what recording a proof did.
type ProofResult struct {
	Proven  bool   `json:"proven"`
	Version int    `json:"version"`
	Status  string `json:"status"`
}

// AddProof records a proof report and its verdict; a passing one moves a
// draft at that exact text to proven. ErrNotFound: no such skill.
// ErrChanged: the skill's text is no longer the proven one.
func (l *LearningIngest) AddProof(ctx context.Context, tenant string, r skillship.Report, v skillship.Verdict, by string) (ProofResult, error) {
	rep, err := json.Marshal(r)
	if err != nil {
		return ProofResult{}, err
	}
	verd, err := json.Marshal(v)
	if err != nil {
		return ProofResult{}, err
	}
	var out ProofResult
	err = l.DB.QueryRowContext(ctx, `SELECT proven, version, status FROM control.skill_proof_add($1, $2, $3, $4, $5, $6, $7, $8)`,
		tenant, r.Agent, r.Path, r.SHA256, rep, verd, v.Passed, by).Scan(&out.Proven, &out.Version, &out.Status)
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch pe.Code.Name() {
		case "no_data_found":
			return out, ErrNotFound
		case "serialization_failure":
			return out, ErrChanged
		}
	}
	return out, err
}

// Skills returns what an agent may load: the newest active release of each
// skill, only if it opted in and nothing is killed.
func (l *LearningIngest) Skills(ctx context.Context, tenant, agent string) ([]skillship.Release, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT path, release, sha256, body, released_at FROM control.skills_for($1, $2)`, tenant, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []skillship.Release{}
	for rows.Next() {
		r := skillship.Release{Agent: agent}
		if err := rows.Scan(&r.Path, &r.Release, &r.SHA256, &r.Body, &r.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- the alerts worker: measuring and rolling back ---------------------------------

// ShippingStore is the measurement job's control-plane access (alerts role).
type ShippingStore struct{ DB *sql.DB }

// ShippingRelease is a release agents are getting, with its note's state.
type ShippingRelease struct {
	Tenant string
	skillship.Release
	NoteStatus string
	NoteSHA    string
}

// Releases lists the release each opted-in agent gets, per skill, for
// workspaces with the learning loop on.
func (s *ShippingStore) Releases(ctx context.Context) ([]ShippingRelease, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT tenant_id, agent, path, release, sha256, body, released_at,
		COALESCE(note_status, ''), COALESCE(note_sha, '') FROM control.shipping_releases()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShippingRelease
	for rows.Next() {
		var r ShippingRelease
		if err := rows.Scan(&r.Tenant, &r.Agent, &r.Path, &r.Release.Release, &r.SHA256, &r.Body, &r.ReleasedAt,
			&r.NoteStatus, &r.NoteSHA); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LessonBrief is what measuring needs of a lesson.
type LessonBrief struct {
	ID, Agent, StepName, Tool, Pattern string
}

// Lessons returns a workspace's lessons by ID.
func (s *ShippingStore) Lessons(ctx context.Context, tenant string) (map[string]LessonBrief, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, agent, step_name, tool_name, error_pattern FROM control.lessons_brief($1)`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LessonBrief{}
	for rows.Next() {
		var l LessonBrief
		if err := rows.Scan(&l.ID, &l.Agent, &l.StepName, &l.Tool, &l.Pattern); err != nil {
			return nil, err
		}
		out[l.ID] = l
	}
	return out, rows.Err()
}

// PutMeasurement stores a release's latest measurement.
func (s *ShippingStore) PutMeasurement(ctx context.Context, tenant string, m skillship.Measurement) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `SELECT control.skill_measure_put($1, $2, $3, $4, $5, $6)`, tenant, m.Agent, m.Path, m.Release, b, m.Decision)
	return err
}

// Rollback marks an active release rolled back; false when it already was.
func (s *ShippingStore) Rollback(ctx context.Context, tenant, agent, path string, release int, reason string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT control.skill_rollback($1, $2, $3, $4, $5)`, tenant, agent, path, release, reason).Scan(&ok)
	return ok, err
}

// GoLive marks an approved skill live; the new note version, or 0.
func (s *ShippingStore) GoLive(ctx context.Context, tenant, agent, path, sha string) (int, error) {
	var v int
	err := s.DB.QueryRowContext(ctx, `SELECT control.skill_go_live($1, $2, $3, $4)`, tenant, agent, path, sha).Scan(&v)
	return v, err
}

// ---- the API: proofs, releases, opt-in and kill switches ---------------------------

// Proof is one stored proof report.
type Proof struct {
	ID         int64             `json:"id"`
	SHA256     string            `json:"sha256"`
	Passed     bool              `json:"passed"`
	Verdict    skillship.Verdict `json:"verdict"`
	Report     skillship.Report  `json:"report"`
	UploadedBy string            `json:"uploaded_by"`
	UploadedAt time.Time         `json:"uploaded_at"`
}

// SkillProofs returns a skill's proof reports, newest first (at most 20).
func (s *Store) SkillProofs(ctx context.Context, tenant, agent, path string) ([]Proof, error) {
	out := []Proof{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, sha256, passed, verdict, report, uploaded_by, uploaded_at FROM control.skill_proofs
			WHERE agent = $1 AND path = $2 ORDER BY uploaded_at DESC, id DESC LIMIT 20`, agent, path)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Proof
			var verd, rep []byte
			if err := rows.Scan(&p.ID, &p.SHA256, &p.Passed, &verd, &rep, &p.UploadedBy, &p.UploadedAt); err != nil {
				return err
			}
			if err := json.Unmarshal(verd, &p.Verdict); err != nil {
				return err
			}
			if err := json.Unmarshal(rep, &p.Report); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// ReleaseInfo is a shipped release with its state and latest measurement.
type ReleaseInfo struct {
	Path        string                 `json:"path"`
	Release     int                    `json:"release"`
	SHA256      string                 `json:"sha256"`
	State       string                 `json:"state"` // active, rolled_back, withdrawn
	StateReason string                 `json:"state_reason,omitempty"`
	StateAt     *time.Time             `json:"state_at,omitempty"`
	ReleasedBy  string                 `json:"released_by"`
	ReleasedAt  time.Time              `json:"released_at"`
	Serving     bool                   `json:"serving"` // the newest active release of its skill
	Measurement *skillship.Measurement `json:"measurement,omitempty"`
}

// SkillReleases returns an agent's releases (path "" for every skill),
// newest first per skill.
func (s *Store) SkillReleases(ctx context.Context, tenant, agent, path string) ([]ReleaseInfo, error) {
	out := []ReleaseInfo{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.path, r.release, r.sha256, r.state, COALESCE(r.state_reason, ''), r.state_at,
				r.released_by, r.released_at, m.measurement
			FROM control.skill_releases r
			LEFT JOIN control.skill_measurements m ON m.tenant_id = r.tenant_id AND m.agent = r.agent AND m.path = r.path AND m.release = r.release
			WHERE r.agent = $1 AND ($2 = '' OR r.path = $2)
			ORDER BY r.path, r.release DESC LIMIT 500`, agent, path)
		if err != nil {
			return err
		}
		defer rows.Close()
		served := map[string]bool{}
		for rows.Next() {
			var ri ReleaseInfo
			var at sql.NullTime
			var m []byte
			if err := rows.Scan(&ri.Path, &ri.Release, &ri.SHA256, &ri.State, &ri.StateReason, &at, &ri.ReleasedBy, &ri.ReleasedAt, &m); err != nil {
				return err
			}
			if at.Valid {
				ri.StateAt = &at.Time
			}
			if len(m) > 0 {
				var mm skillship.Measurement
				if err := json.Unmarshal(m, &mm); err != nil {
					return err
				}
				ri.Measurement = &mm
			}
			if ri.State == "active" && !served[ri.Path] {
				ri.Serving, served[ri.Path] = true, true
			}
			out = append(out, ri)
		}
		return rows.Err()
	})
	return out, err
}

// Rollback is a release Bindle rolled back, for Home.
type Rollback struct {
	Agent   string    `json:"agent"`
	Path    string    `json:"path"`
	Release int       `json:"release"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// RecentRollbacks lists releases rolled back since a time, newest first.
func (s *Store) RecentRollbacks(ctx context.Context, tenant string, since time.Time) ([]Rollback, error) {
	out := []Rollback{}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT agent, path, release, COALESCE(state_reason, ''), state_at FROM control.skill_releases
			WHERE state = 'rolled_back' AND state_at >= $1 ORDER BY state_at DESC LIMIT 50`, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Rollback
			if err := rows.Scan(&r.Agent, &r.Path, &r.Release, &r.Reason, &r.At); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// AgentSkillSettings are an agent's shipping opt-in and kill switches.
type AgentSkillSettings struct {
	Agent   string   `json:"agent"`
	Ship    bool     `json:"ship"`     // approved skills reach the agent
	KillAll bool     `json:"kill_all"` // stop every learned skill now
	Killed  []string `json:"killed"`   // skill paths switched off one by one
}

// GetAgentSkillSettings returns an agent's settings (off when never saved).
func (s *Store) GetAgentSkillSettings(ctx context.Context, tenant, agent string) (AgentSkillSettings, error) {
	out := AgentSkillSettings{Agent: agent, Killed: []string{}}
	err := s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT ship, kill_all FROM control.agent_skill_settings WHERE agent = $1`, agent).Scan(&out.Ship, &out.KillAll)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		out.Killed, err = strings1(ctx, tx, `SELECT path FROM control.skill_kills WHERE agent = $1 ORDER BY path`, agent)
		return err
	})
	return out, err
}

// PutAgentSkillSettings saves the opt-in and the kill-all switch.
func (s *Store) PutAgentSkillSettings(ctx context.Context, tenant, userID string, in AgentSkillSettings) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO control.agent_skill_settings (tenant_id, agent, ship, kill_all, updated_by) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, agent) DO UPDATE SET ship = EXCLUDED.ship, kill_all = EXCLUDED.kill_all,
				updated_by = EXCLUDED.updated_by, updated_at = now()`, tenant, in.Agent, in.Ship, in.KillAll, nullable(userID))
		return err
	})
}

// SetSkillKilled switches one skill off (killed) or back on.
func (s *Store) SetSkillKilled(ctx context.Context, tenant, userID, agent, path string, killed bool) error {
	return s.inTenant(ctx, tenant, func(tx *sql.Tx) error {
		if !killed {
			_, err := tx.ExecContext(ctx, `DELETE FROM control.skill_kills WHERE agent = $1 AND path = $2`, agent, path)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO control.skill_kills (tenant_id, agent, path, killed_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, tenant, agent, path, nullable(userID))
		return err
	})
}
