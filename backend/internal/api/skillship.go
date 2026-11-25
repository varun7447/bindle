package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/ledger"
	"github.com/bindle-ai/bindle/backend/internal/session"
	"github.com/bindle-ai/bindle/backend/internal/skillship"
	"github.com/bindle-ai/bindle/backend/internal/vault"
)

// ShipStore is phase 2 of the learning loop (implemented by *control.Store).
type ShipStore interface {
	SkillProofs(ctx context.Context, tenant, agent, path string) ([]control.Proof, error)
	SkillReleases(ctx context.Context, tenant, agent, path string) ([]control.ReleaseInfo, error)
	RecentRollbacks(ctx context.Context, tenant string, since time.Time) ([]control.Rollback, error)
	GetAgentSkillSettings(ctx context.Context, tenant, agent string) (control.AgentSkillSettings, error)
	PutAgentSkillSettings(ctx context.Context, tenant, userID string, in control.AgentSkillSettings) error
	SetSkillKilled(ctx context.Context, tenant, userID, agent, path string, killed bool) error
}

func (s *Server) shipRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/skills/proofs", s.listProofs)
	api.HandleFunc("GET /api/skills/releases", s.listReleases)
	api.HandleFunc("GET /api/skills/rollbacks", s.listRollbacks)
	api.HandleFunc("GET /api/skills/settings", s.getAgentSkillSettings)
	api.HandleFunc("PUT /api/skills/settings", s.putAgentSkillSettings)
	api.HandleFunc("POST /api/skills/kill", s.killSkill)
}

func (s *Server) shipOn(w http.ResponseWriter, r *http.Request) bool {
	if s.Ship == nil || s.Learning == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return false
	}
	return s.learningOn(w, r)
}

// mayControlSkills: workspace owners and admins, and the agent's passport
// owner, can switch shipping on or off and use the kill switches.
func (s *Server) mayControlSkills(ctx context.Context, w http.ResponseWriter, r *http.Request, agent string) bool {
	p := principalFrom(ctx)
	if p.Role == control.RoleOwner || p.Role == control.RoleAdmin {
		return true
	}
	pp, err := s.Learning.ActivePassport(ctx, p.TenantID, agent)
	if err != nil {
		s.storeError(w, r, err)
		return false
	}
	email, err := s.callerEmail(ctx, p)
	if err != nil {
		s.storeError(w, r, err)
		return false
	}
	if pp != nil && email != "" && strings.EqualFold(strings.TrimSpace(pp.Owner), email) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden", "only the agent's owner or a workspace owner or admin can change how skills ship")
	return false
}

func agentAndPath(w http.ResponseWriter, r *http.Request, needPath bool) (string, string, bool) {
	agent, ok := agentParam(w, r)
	if !ok {
		return "", "", false
	}
	path := r.URL.Query().Get("path")
	if (needPath || path != "") && !skillship.IsSkillPath(path) {
		badRequest(w, "path must be a skill path such as skills/<name>/SKILL.md")
		return "", "", false
	}
	return agent, path, true
}

func (s *Server) listProofs(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	agent, path, ok := agentAndPath(w, r, true)
	if !ok {
		return
	}
	ps, err := s.Ship.SkillProofs(r.Context(), tenantFrom(r.Context()), agent, path)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proofs": ps})
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	agent, path, ok := agentAndPath(w, r, false)
	if !ok {
		return
	}
	rs, err := s.Ship.SkillReleases(r.Context(), tenantFrom(r.Context()), agent, path)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	set, err := s.Ship.GetAgentSkillSettings(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": rs, "settings": set})
}

// listRollbacks: releases Bindle rolled back in the last ?days= (default 7,
// at most 30), for Home.
func (s *Server) listRollbacks(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			badRequest(w, "days must be between 1 and 30")
			return
		}
		days = n
	}
	rs, err := s.Ship.RecentRollbacks(r.Context(), tenantFrom(r.Context()), s.now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollbacks": rs})
}

func (s *Server) getAgentSkillSettings(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	set, err := s.Ship.GetAgentSkillSettings(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

type shipSettingsIn struct {
	Agent   string `json:"agent"`
	Ship    bool   `json:"ship"`
	KillAll bool   `json:"kill_all"`
}

// putAgentSkillSettings switches shipping for an agent on or off (opt-in;
// off by default) and the kill switch for all of its learned skills. The
// SDKs see a kill within one config poll.
func (s *Server) putAgentSkillSettings(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	var in shipSettingsIn
	if !decodeBody(w, r, &in) {
		return
	}
	if _, err := vault.CleanAgent(in.Agent); err != nil {
		badRequest(w, err.Error())
		return
	}
	ctx := r.Context()
	if !s.mayControlSkills(ctx, w, r, in.Agent) {
		return
	}
	p := principalFrom(ctx)
	before, err := s.Ship.GetAgentSkillSettings(ctx, p.TenantID, in.Agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if err := s.Ship.PutAgentSkillSettings(ctx, p.TenantID, p.UserID, control.AgentSkillSettings{Agent: in.Agent, Ship: in.Ship, KillAll: in.KillAll}); err != nil {
		s.storeError(w, r, err)
		return
	}
	who := byUser(p.UserID)
	if before.Ship != in.Ship {
		s.recordShip(ctx, p.TenantID, in.Agent, "*", map[bool]string{true: "ship_on", false: "ship_off"}[in.Ship], who)
	}
	if before.KillAll != in.KillAll {
		s.recordShip(ctx, p.TenantID, in.Agent, "*", map[bool]string{true: "kill", false: "unkill"}[in.KillAll], who)
	}
	set, err := s.Ship.GetAgentSkillSettings(ctx, p.TenantID, in.Agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

type killIn struct {
	Agent  string `json:"agent"`
	Path   string `json:"path"`
	Killed bool   `json:"killed"`
}

// killSkill is the kill switch for one skill.
func (s *Server) killSkill(w http.ResponseWriter, r *http.Request) {
	if !s.shipOn(w, r) {
		return
	}
	var in killIn
	if !decodeBody(w, r, &in) {
		return
	}
	if _, err := vault.CleanAgent(in.Agent); err != nil || !skillship.IsSkillPath(in.Path) {
		badRequest(w, "agent and a skill path are required")
		return
	}
	ctx := r.Context()
	if !s.mayControlSkills(ctx, w, r, in.Agent) {
		return
	}
	p := principalFrom(ctx)
	if err := s.Ship.SetSkillKilled(ctx, p.TenantID, p.UserID, in.Agent, in.Path, in.Killed); err != nil {
		s.storeError(w, r, err)
		return
	}
	s.recordShip(ctx, p.TenantID, in.Agent, in.Path, map[bool]string{true: "kill", false: "unkill"}[in.Killed], byUser(p.UserID))
	set, err := s.Ship.GetAgentSkillSettings(ctx, p.TenantID, in.Agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// recordShip writes a skill_change ledger entry for a shipping switch (when
// the ledger is on). A failure is logged: the switch itself took effect.
func (s *Server) recordShip(ctx context.Context, tenant, agent, path, action, by string) {
	if s.LearningLedger == nil || !s.hasFeature(ctx, tenant, ledger.Feature) {
		return
	}
	e := ledger.SkillChange(agent, path, 0, "", action, "by "+by, s.now())
	e.EntryKey += ":" + strconv.FormatInt(s.now().UnixNano(), 10)
	if _, err := s.LearningLedger.Append(ctx, tenant, agent, []ledger.Entry{e}); err != nil && s.Log != nil {
		s.Log.Warn("skills: ledger", "tenant", tenant, "err", err)
	}
}

// callerEmail is the caller's email for owner checks: the dev bypass's
// configured email when there is no signed-in user, otherwise the user's
// email from the control plane.
func (s *Server) callerEmail(ctx context.Context, p session.Principal) (string, error) {
	if p.UserID == "" {
		return p.Email, nil
	}
	return s.Learning.UserEmail(ctx, p.TenantID, p.UserID)
}
