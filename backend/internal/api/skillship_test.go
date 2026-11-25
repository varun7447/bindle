package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/passport"
)

type fakeShipAPI struct {
	settings map[string]control.AgentSkillSettings // tenant/agent
	kills    map[string]bool                       // tenant/agent/path
}

func (f *fakeShipAPI) SkillProofs(context.Context, string, string, string) ([]control.Proof, error) {
	return []control.Proof{{ID: 1, Passed: true}}, nil
}
func (f *fakeShipAPI) SkillReleases(_ context.Context, t, a, _ string) ([]control.ReleaseInfo, error) {
	if t != "tenant-a" {
		return []control.ReleaseInfo{}, nil
	}
	return []control.ReleaseInfo{{Path: "skills/retry-lookup/SKILL.md", Release: 2, State: "active", Serving: true}}, nil
}
func (f *fakeShipAPI) RecentRollbacks(_ context.Context, t string, _ time.Time) ([]control.Rollback, error) {
	return []control.Rollback{{Agent: "refunds", Path: "skills/retry-lookup/SKILL.md", Release: 3, Reason: "success fell"}}, nil
}
func (f *fakeShipAPI) GetAgentSkillSettings(_ context.Context, t, a string) (control.AgentSkillSettings, error) {
	s := f.settings[t+"/"+a]
	s.Agent, s.Killed = a, []string{}
	for k := range f.kills {
		if len(k) > len(t+"/"+a+"/") && k[:len(t+"/"+a+"/")] == t+"/"+a+"/" {
			s.Killed = append(s.Killed, k[len(t+"/"+a+"/"):])
		}
	}
	return s, nil
}
func (f *fakeShipAPI) PutAgentSkillSettings(_ context.Context, t, _ string, in control.AgentSkillSettings) error {
	f.settings[t+"/"+in.Agent] = in
	return nil
}
func (f *fakeShipAPI) SetSkillKilled(_ context.Context, t, _, a, p string, killed bool) error {
	if killed {
		f.kills[t+"/"+a+"/"+p] = true
	} else {
		delete(f.kills, t+"/"+a+"/"+p)
	}
	return nil
}

func TestShipEndpoints(t *testing.T) {
	pp := &passport.Passport{Agent: "refunds", Owner: "owner@example.com"}
	learning := &fakeLearning{notes: map[string]*control.VaultNote{}, reviewers: map[string]bool{},
		passports: map[string]*passport.Passport{"tenant-a/refunds": pp},
		emails:    map[string]string{"usr_member-owner": "owner@example.com", "usr_member-a": "member@example.com"}}
	ship := &fakeShipAPI{settings: map[string]control.AgentSkillSettings{}, kills: map[string]bool{}}
	led := &fakeAppender{}
	srv := &Server{Store: &fakeStore{}, Sessions: tokenResolver{"tok-a": "tenant-a", "tok-b": "tenant-b", "member-a": "tenant-a", "member-owner": "tenant-a"},
		Features: fakeFeatures{"tenant-a": {"learning_loop", "ledger"}}, Learning: learning, Ship: ship, LearningLedger: led,
		Config: labelConfig{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	h := srv.Handler()

	for _, path := range []string{"/api/skills/releases?agent=refunds", "/api/skills/rollbacks", "/api/skills/settings?agent=refunds",
		"/api/skills/proofs?agent=refunds&path=skills/retry-lookup/SKILL.md"} {
		if w, _ := get(t, h, path, "tok-b"); w.Code != 404 {
			t.Errorf("%s without the flag: %d", path, w.Code)
		}
		if w, _ := get(t, h, path, "tok-a"); w.Code != 200 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w, _ := get(t, h, "/api/skills/proofs?agent=refunds&path=notes/x.md", "tok-a"); w.Code != 400 {
		t.Fatalf("non-skill path: %d", w.Code)
	}

	// Opt-in: a member who isn't the agent's owner can't; the passport
	// owner and workspace owners can.
	body := `{"agent":"refunds","ship":true,"kill_all":false}`
	if w, _ := send(t, h, "PUT", "/api/skills/settings", "member-a", body); w.Code != 403 {
		t.Fatalf("member opt-in: %d", w.Code)
	}
	if w, got := send(t, h, "PUT", "/api/skills/settings", "member-owner", body); w.Code != 200 || got["ship"] != true {
		t.Fatalf("owner opt-in: %d %v", w.Code, got)
	}
	if w, _ := send(t, h, "PUT", "/api/skills/settings", "tok-a", `{"agent":"refunds","ship":true,"kill_all":true}`); w.Code != 200 {
		t.Fatalf("kill all: %d", w.Code)
	}
	kill := `{"agent":"refunds","path":"skills/retry-lookup/SKILL.md","killed":true}`
	if w, _ := send(t, h, "POST", "/api/skills/kill", "member-a", kill); w.Code != 403 {
		t.Fatalf("member kill: %d", w.Code)
	}
	if w, got := send(t, h, "POST", "/api/skills/kill", "tok-a", kill); w.Code != 200 || len(got["killed"].([]any)) != 1 {
		t.Fatalf("kill one: %d %v", w.Code, got)
	}
	if w, _ := send(t, h, "POST", "/api/skills/kill", "tok-a", `{"agent":"refunds","path":"notes/x.md","killed":true}`); w.Code != 400 {
		t.Fatalf("kill a note: %d", w.Code)
	}
	// Each switch is in the ledger: ship on, kill all, kill one.
	if len(led.entries) != 3 || led.entries[0].Kind != "skill_change" {
		t.Fatalf("ledger entries %d", len(led.entries))
	}
}
