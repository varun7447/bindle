package api

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/query"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

type fakeMgmtKeys struct {
	keys    []control.MgmtKey
	tenant  string
	revoked string
}

func (f *fakeMgmtKeys) ListMgmtKeys(_ context.Context, tenant string) ([]control.MgmtKey, error) {
	f.tenant = tenant
	return f.keys, nil
}
func (f *fakeMgmtKeys) CreateMgmtKey(_ context.Context, tenant, _, name string, scopes []string) (control.MgmtKey, string, error) {
	f.tenant = tenant
	k := control.MgmtKey{ID: "mkey_00000000000000000001", Name: name, Scopes: scopes}
	f.keys = append(f.keys, k)
	return k, control.MgmtKeyPrefix + "secret", nil
}
func (f *fakeMgmtKeys) RevokeMgmtKey(_ context.Context, tenant, id string) error {
	f.tenant, f.revoked = tenant, id
	if id != "mkey_00000000000000000001" {
		return control.ErrNotFound
	}
	return nil
}

type fakePassports struct{ versions []control.PassportVersion }

func (f *fakePassports) ListPassports(context.Context, string) ([]control.PassportVersion, error) {
	return f.versions, nil
}
func (f *fakePassports) PassportHistory(_ context.Context, _, agent string) ([]control.PassportVersion, error) {
	var out []control.PassportVersion
	for _, v := range f.versions {
		if v.Agent == agent {
			out = append(out, v)
		}
	}
	return out, nil
}

type driftStore struct {
	*fakeStore
	calls uint64
}

func (d *driftStore) PassportDrift(context.Context, string, time.Time, time.Time) ([]query.DriftSignal, error) {
	return []query.DriftSignal{{Kind: passport.ToolNotAllowed, Agent: "refunds", Steps: 2, Runs: 1, Reasons: map[string]uint64{"tool_not_allowed:x": 2}}}, nil
}
func (d *driftStore) ToolCalls(context.Context, string, string, string, time.Time, time.Time) (uint64, string, error) {
	return d.calls, "tr1", nil
}
func (d *driftStore) CostlyRuns(context.Context, string, time.Time, time.Time, float64, int) ([]query.Run, error) {
	return nil, nil
}

type labelConfig struct{ ConfigStore }

func (labelConfig) GetSDKConfig(context.Context, string) (sdkconfig.Config, error) {
	c := sdkconfig.Default()
	c.RedactPatterns = []sdkconfig.Pattern{{Name: "ACCOUNT_ID", Pattern: "acct-[0-9]+"}}
	return c, nil
}

func TestPassportEndpoints(t *testing.T) {
	p, err := passport.Parse([]byte(`{"version":"bindle.passport/v1","agent":"refunds","owner":"payments",
		"limits":[{"tool":"refund_payment","max":3,"window":"5m"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeMgmtKeys{}
	srv := &Server{Store: &driftStore{fakeStore: &fakeStore{}, calls: 5},
		Sessions: tokenResolver{"tok-a": "tenant-a", "tok-b": "tenant-b", "member-a": "tenant-a"},
		Features: fakeFeatures{"tenant-a": {"passports"}}, MgmtKeys: keys, Config: labelConfig{},
		Passports: &fakePassports{versions: []control.PassportVersion{{Agent: "refunds", Version: 2, Passport: p}}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	h := srv.Handler()

	// Without the flag, nothing exists.
	for _, path := range []string{"/api/mgmt-keys", "/api/passports", "/api/agents/passport?agent=refunds", "/api/passport-drift"} {
		if w, _ := get(t, h, path, "tok-b"); w.Code != 404 {
			t.Errorf("%s without the flag: %d", path, w.Code)
		}
	}

	if w, body := get(t, h, "/api/agents/passport?agent=refunds", "tok-a"); w.Code != 200 || body["active"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("agent passport %d %v", w.Code, body)
	}
	if w, body := get(t, h, "/api/agents/passport?agent=nobody", "tok-a"); w.Code != 200 || body["active"] != nil {
		t.Fatalf("agent without a passport %d %v", w.Code, body)
	}
	if w, _ := get(t, h, "/api/agents/passport", "tok-a"); w.Code != 400 {
		t.Fatalf("missing agent: %d", w.Code)
	}

	w, body := get(t, h, "/api/passport-drift?window=24h", "tok-a")
	if w.Code != 200 {
		t.Fatalf("drift %d %s", w.Code, w.Body)
	}
	findings := body["findings"].([]any)
	if len(body["signals"].([]any)) != 1 || len(findings) != 1 || findings[0].(map[string]any)["kind"] != passport.RateOverLimit {
		t.Fatalf("drift body %v", body)
	}
	if body["explanations"].(map[string]any)[passport.EgressBlocked] == nil {
		t.Fatal("missing explanations")
	}

	// Management keys: members can list but not create or revoke.
	if w, _ := send(t, h, "POST", "/api/mgmt-keys", "member-a", `{"name":"ci","scopes":["passport:write"]}`); w.Code != 403 {
		t.Fatalf("member created a key: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/mgmt-keys", "tok-a", `{"name":"ci","scopes":["root"]}`); w.Code != 400 {
		t.Fatalf("bad scope: %d", w.Code)
	}
	w, body = send(t, h, "POST", "/api/mgmt-keys", "tok-a", `{"name":"ci","scopes":["passport:write"]}`)
	if w.Code != 201 || !strings.HasPrefix(body["secret"].(string), control.MgmtKeyPrefix) || keys.tenant != "tenant-a" {
		t.Fatalf("create %d %v", w.Code, body)
	}
	if w, body := get(t, h, "/api/mgmt-keys", "member-a"); w.Code != 200 || len(body["keys"].([]any)) != 1 {
		t.Fatalf("list %d %v", w.Code, body)
	}
	if w, _ := send(t, h, "POST", "/api/mgmt-keys/mkey_00000000000000000001/revoke", "member-a", ""); w.Code != 403 {
		t.Fatalf("member revoked: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/mgmt-keys/nope/revoke", "tok-a", ""); w.Code != 400 {
		t.Fatalf("bad id: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/mgmt-keys/mkey_00000000000000000002/revoke", "tok-a", ""); w.Code != 404 {
		t.Fatalf("unknown id: %d", w.Code)
	}
	if w, _ := send(t, h, "POST", "/api/mgmt-keys/mkey_00000000000000000001/revoke", "tok-a", ""); w.Code != 204 {
		t.Fatalf("revoke: %d", w.Code)
	}

	// Console validation knows the workspace's own pattern names.
	doc := `{"version":"bindle.passport/v1","agent":"refunds","owner":"o","data_classes":{"egress_block":["ACCOUNT_ID"]}}`
	if w, body := send(t, h, "POST", "/api/passports/validate", "tok-a", doc); w.Code != 200 || body["ok"] != true {
		t.Fatalf("validate %d %v", w.Code, body)
	}
	if w, body := send(t, h, "POST", "/api/passports/validate", "tok-a", strings.Replace(doc, "ACCOUNT_ID", "IBAN", 1)); w.Code != 200 || body["ok"] != false ||
		!strings.Contains(body["error"].(string), "IBAN") {
		t.Fatalf("validate unknown label %d %v", w.Code, body)
	}
}
