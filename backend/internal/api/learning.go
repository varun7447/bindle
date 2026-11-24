package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/ledger"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/redact"
	"github.com/bindle-ai/bindle/backend/internal/skills"
	"github.com/bindle-ai/bindle/backend/internal/vault"
)

// LearningStore is the learning loop's control-plane data. Implemented by
// *control.Store.
type LearningStore interface {
	ListLessons(ctx context.Context, tenant, agent string) ([]control.LessonSummary, error)
	GetLearningSettings(ctx context.Context, tenant string) (control.LearningSettings, error)
	PutLearningSettings(ctx context.Context, tenant, userID string, ls control.LearningSettings) (control.LearningSettings, error)
	VaultNotes(ctx context.Context, tenant, agent string) ([]control.VaultNote, []control.VaultLink, error)
	GetVaultNote(ctx context.Context, tenant, agent, path string) (control.VaultNote, error)
	VaultPut(ctx context.Context, in control.VaultPutInput) (control.VaultPutResult, error)
	VaultSetStatus(ctx context.Context, tenant, agent, path, from, to, sha, action, by string) (int, bool, error)
	VaultReview(ctx context.Context, tenant, agent, path, sha, by string) (bool, error)
	ActivePassport(ctx context.Context, tenant, agent string) (*passport.Passport, error)
	AgentTools(ctx context.Context, tenant, agent string) ([]string, error)
	UserEmail(ctx context.Context, tenant, userID string) (string, error)
	ListSecurityReviewers(ctx context.Context, tenant string) ([]control.SecurityReviewer, error)
	AddSecurityReviewer(ctx context.Context, tenant, byUser, email string) (control.SecurityReviewer, error)
	RemoveSecurityReviewer(ctx context.Context, tenant, userID string) error
	IsSecurityReviewer(ctx context.Context, tenant, userID string) (bool, error)
}

var userIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) learningRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/lessons", s.listLessons)
	api.HandleFunc("GET /api/learning/settings", s.getLearningSettings)
	api.HandleFunc("PUT /api/learning/settings", s.putLearningSettings)
	api.HandleFunc("GET /api/vault", s.listVault)
	api.HandleFunc("GET /api/vault/note", s.getVaultNote)
	api.HandleFunc("PUT /api/vault/note", s.putVaultNote)
	api.HandleFunc("POST /api/vault/note/action", s.vaultAction)
	api.HandleFunc("POST /api/vault/note/review", s.vaultReview)
	api.HandleFunc("GET /api/vault/export", s.exportVault)
	api.HandleFunc("GET /api/security-reviewers", s.listReviewers)
	api.HandleFunc("POST /api/security-reviewers", s.addReviewer)
	api.HandleFunc("DELETE /api/security-reviewers/{id}", s.removeReviewer)
}

// learningOn is featureOn for the learning loop, plus its store.
func (s *Server) learningOn(w http.ResponseWriter, r *http.Request) bool {
	if s.Learning == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return false
	}
	return s.featureOn(w, r, skills.Feature)
}

func (s *Server) hasFeature(ctx context.Context, tenant, feature string) bool {
	if s.Features == nil {
		return false
	}
	got, err := s.Features.List(ctx, tenant)
	if err != nil {
		return false
	}
	for _, f := range got {
		if f == feature {
			return true
		}
	}
	return false
}

// byUser names who changed a note: "user:<id>" ("user:dev" for the local
// dev bypass, which has no user).
func byUser(id string) string {
	if id == "" {
		return "user:dev"
	}
	return "user:" + id
}

func (s *Server) saver() *vault.Saver {
	return &vault.Saver{
		Store:    s.Learning,
		Passport: s.Learning.ActivePassport,
		Tools:    s.Learning.AgentTools,
		Labels:   s.customLabels,
		Ledger:   s.LearningLedger,
		LedgerOn: func(ctx context.Context, t string) bool {
			return s.LearningLedger != nil && s.hasFeature(ctx, t, ledger.Feature)
		},
		Now: s.now,
	}
}

// customLabels are the workspace's redaction pattern names (data classes).
func (s *Server) customLabels(ctx context.Context, tenant string) (map[string]bool, error) {
	out := map[string]bool{}
	if s.Config == nil {
		return out, nil
	}
	c, err := s.Config.GetSDKConfig(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, p := range c.RedactPatterns {
		out[p.Name] = true
	}
	return out, nil
}

func agentParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	a, err := vault.CleanAgent(r.URL.Query().Get("agent"))
	if err != nil {
		badRequest(w, err.Error())
		return "", false
	}
	return a, true
}

func (s *Server) listLessons(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	agent := r.URL.Query().Get("agent")
	if len(agent) > vault.MaxAgent {
		badRequest(w, "agent is too long")
		return
	}
	ls, err := s.Learning.ListLessons(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	set, err := s.Learning.GetLearningSettings(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lessons": ls, "min_runs": set.MinRuns})
}

func (s *Server) getLearningSettings(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	set, err := s.Learning.GetLearningSettings(r.Context(), tenantFrom(r.Context()))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) putLearningSettings(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	if !p.CanManageKeys() {
		writeError(w, http.StatusForbidden, "forbidden", "only workspace owners and admins can change learning settings")
		return
	}
	var in control.LearningSettings
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.Learning.PutLearningSettings(r.Context(), p.TenantID, p.UserID, in)
	if skills.IsInvalid(err) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listVault(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	notes, links, err := s.Learning.VaultNotes(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	p, err := s.Learning.ActivePassport(r.Context(), tenantFrom(r.Context()), agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	owner := ""
	if p != nil {
		owner = p.Owner
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "notes": notes, "links": links, "passport": p != nil, "owner": owner})
}

func (s *Server) getVaultNote(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if !skills.ValidPath(path) {
		badRequest(w, "invalid path")
		return
	}
	n, err := s.Learning.GetVaultNote(r.Context(), tenantFrom(r.Context()), agent, path)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such note")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

type noteIn struct {
	Agent   string `json:"agent"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Base    string `json:"base_sha256"`
}

func (s *Server) putVaultNote(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	var in noteIn
	if !decodeBodyLimit(w, r, &in, skills.MaxBytes+4<<10) {
		return
	}
	p := principalFrom(r.Context())
	res, err := s.saver().Save(r.Context(), p.TenantID, in.Agent, in.Path, in.Content, in.Base, byUser(p.UserID))
	if pe, ok := vault.IsPassportError(err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{"code": "passport", "message": pe.Error()},
			"passport_check": pe.Check})
		return
	}
	if skills.IsInvalid(err) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if res.Conflict {
		writeError(w, http.StatusConflict, "conflict", "the note changed since you opened it; reload and apply your edit again")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type actionIn struct {
	Agent  string `json:"agent"`
	Path   string `json:"path"`
	Action string `json:"action"`
	SHA256 string `json:"sha256"`
}

func (s *Server) loadForAction(w http.ResponseWriter, r *http.Request, in actionIn) (control.VaultNote, bool) {
	if _, err := vault.CleanAgent(in.Agent); err != nil || !skills.ValidPath(in.Path) {
		badRequest(w, "agent and path are required")
		return control.VaultNote{}, false
	}
	n, err := s.Learning.GetVaultNote(r.Context(), tenantFrom(r.Context()), in.Agent, in.Path)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such note")
		return n, false
	}
	if err != nil {
		s.storeError(w, r, err)
		return n, false
	}
	if n.SHA256 != in.SHA256 {
		writeError(w, http.StatusConflict, "conflict", "the note changed since you opened it; reload it first")
		return n, false
	}
	return n, true
}

// vaultAction approves, retires or restores a skill. The passport check is
// recomputed against the agent's current passport, so a passport that
// narrowed since the draft was pushed can't be bypassed.
func (s *Server) vaultAction(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	var in actionIn
	if !decodeBody(w, r, &in) {
		return
	}
	n, ok := s.loadForAction(w, r, in)
	if !ok {
		return
	}
	ctx := r.Context()
	p := principalFrom(ctx)
	pp, err := s.Learning.ActivePassport(ctx, p.TenantID, in.Agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	check := skills.Check{Passport: pp != nil}
	if sk, perr := skills.Parse(n.Body); perr == nil {
		tools, err := s.Learning.AgentTools(ctx, p.TenantID, in.Agent)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		labels, err := s.customLabels(ctx, p.TenantID)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		for _, l := range redact.Labels {
			labels[l] = true
		}
		t, c := skills.References(sk, n.Body, tools, labels)
		check = skills.CheckPassport(pp, t, c)
	}
	var scan skills.Scan
	_ = json.Unmarshal(n.Scan, &scan)
	email, err := s.Learning.UserEmail(ctx, p.TenantID, p.UserID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	actor := vault.Actor{UserID: p.UserID, Email: email, Role: p.Role}
	to, err := vault.Decide(vault.Note{Kind: n.Kind, Status: n.Status, SHA256: n.SHA256, ReviewedSHA: n.ReviewedSHA, Flagged: scan.Flagged},
		in.Action, actor, pp, check)
	var denied *vault.DeniedError
	switch {
	case errors.As(err, &denied):
		writeError(w, http.StatusForbidden, "forbidden", denied.Msg)
		return
	case skills.IsInvalid(err):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
		return
	case err != nil:
		s.storeError(w, r, err)
		return
	}
	who := byUser(p.UserID)
	version, done, err := s.Learning.VaultSetStatus(ctx, p.TenantID, in.Agent, in.Path, n.Status, to, n.SHA256, in.Action, who)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if !done {
		writeError(w, http.StatusConflict, "conflict", "the note changed meanwhile; reload it first")
		return
	}
	if err := s.saver().Record(ctx, p.TenantID, in.Agent, in.Path, version, n.SHA256, in.Action, who); err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": in.Path, "status": to, "version": version})
}

// vaultReview lets a security reviewer clear a flagged skill's scan for
// its current text. An edit makes the skill a draft and voids the review.
func (s *Server) vaultReview(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	var in actionIn
	if !decodeBody(w, r, &in) {
		return
	}
	n, ok := s.loadForAction(w, r, in)
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	reviewer, err := s.Learning.IsSecurityReviewer(r.Context(), p.TenantID, p.UserID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if !reviewer {
		writeError(w, http.StatusForbidden, "forbidden", "only a security reviewer can clear a flagged skill")
		return
	}
	who := byUser(p.UserID)
	done, err := s.Learning.VaultReview(r.Context(), p.TenantID, in.Agent, in.Path, n.SHA256, who)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if !done {
		writeError(w, http.StatusConflict, "conflict", "the note changed meanwhile; reload it first")
		return
	}
	if err := s.saver().Record(r.Context(), p.TenantID, in.Agent, in.Path, n.Version, n.SHA256, "review", who); err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": in.Path, "reviewed_sha256": n.SHA256})
}

// exportVault returns an agent's notes as a zip of plain Markdown files.
func (s *Server) exportVault(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	agent, ok := agentParam(w, r)
	if !ok {
		return
	}
	tenant := tenantFrom(r.Context())
	notes, _, err := s.Learning.VaultNotes(r.Context(), tenant, agent)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="vault.zip"`)
	zw := zip.NewWriter(w)
	for _, n := range notes {
		full, err := s.Learning.GetVaultNote(r.Context(), tenant, agent, n.Path)
		if err != nil {
			s.logger().Error("vault export", "err", err)
			break
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: n.Path, Method: zip.Deflate, Modified: n.UpdatedAt})
		if err != nil {
			break
		}
		_, _ = f.Write([]byte(full.Body))
	}
	_ = zw.Close()
}

func (s *Server) listReviewers(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	rs, err := s.Learning.ListSecurityReviewers(r.Context(), p.TenantID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	me, err := s.Learning.IsSecurityReviewer(r.Context(), p.TenantID, p.UserID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviewers": rs, "me": me, "can_manage": p.Role == control.RoleOwner})
}

func (s *Server) addReviewer(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	if p.Role != control.RoleOwner {
		writeError(w, http.StatusForbidden, "forbidden", "only workspace owners can add security reviewers")
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if !strings.Contains(in.Email, "@") || len(in.Email) > 320 {
		badRequest(w, "email must be a member's email address")
		return
	}
	rv, err := s.Learning.AddSecurityReviewer(r.Context(), p.TenantID, p.UserID, in.Email)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no member of this workspace has that email")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rv)
}

func (s *Server) removeReviewer(w http.ResponseWriter, r *http.Request) {
	if !s.learningOn(w, r) {
		return
	}
	p := principalFrom(r.Context())
	if p.Role != control.RoleOwner {
		writeError(w, http.StatusForbidden, "forbidden", "only workspace owners can remove security reviewers")
		return
	}
	id := r.PathValue("id")
	if !userIDRe.MatchString(id) {
		badRequest(w, "invalid user ID")
		return
	}
	err := s.Learning.RemoveSecurityReviewer(r.Context(), p.TenantID, id)
	if errors.Is(err, control.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "not a security reviewer")
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
