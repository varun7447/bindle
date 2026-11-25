package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/control"
	"github.com/bindle-ai/bindle/backend/internal/model"
	"github.com/bindle-ai/bindle/backend/internal/passport"
	"github.com/bindle-ai/bindle/backend/internal/sdkconfig"
)

func TestApplyPolicyRecordsDataClassesEvenWithCaptureOff(t *testing.T) {
	keep := "00000000000000000000000000000001"
	pol := policy(1, false, sdkconfig.Pattern{Name: "ACCOUNT_ID", Pattern: `acct-[0-9]{6}`})
	in := []model.Step{{TraceID: keep, Type: model.StepTool, ToolName: "send_email",
		Input:  `{"to":"[REDACTED:EMAIL]","account":"acct-123456"}`,
		Output: "card [REDACTED:CARD] on file", Attributes: map[string]string{"note": "[REDACTED:PHONE]"}}}
	out := ApplyPolicy(pol, in)
	if len(out) != 1 {
		t.Fatalf("got %d steps", len(out))
	}
	s := out[0]
	if s.Input != "" || s.Output != "" {
		t.Fatalf("capture off kept content: %q %q", s.Input, s.Output)
	}
	if want := []string{"ACCOUNT_ID", "EMAIL"}; !reflect.DeepEqual(s.InputDataClasses, want) {
		t.Fatalf("input classes %v, want %v", s.InputDataClasses, want)
	}
	if want := []string{"ACCOUNT_ID", "CARD", "EMAIL", "PHONE"}; !reflect.DeepEqual(s.DataClasses, want) {
		t.Fatalf("classes %v, want %v", s.DataClasses, want)
	}
}

func TestApplyPassports(t *testing.T) {
	p, err := passport.Parse([]byte(`{"version":"bindle.passport/v1","agent":"refunds","owner":"o",
		"tools":{"allow":["lookup_*"]},"data_classes":{"egress_block":["CARD"]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := []model.Step{
		{AgentName: "refunds", Type: model.StepTool, ToolName: "lookup_order"},
		{AgentName: "refunds", Type: model.StepTool, ToolName: "send_email", InputDataClasses: []string{"CARD"}},
		{AgentName: "other", Type: model.StepTool, ToolName: "send_email", InputDataClasses: []string{"CARD"}},
	}
	ApplyPassports(passport.Set{"refunds": p}, steps)
	if steps[0].PassportDrift != nil || steps[2].PassportDrift != nil {
		t.Fatalf("unexpected drift: %v / %v", steps[0].PassportDrift, steps[2].PassportDrift)
	}
	if want := []string{"egress_blocked:CARD", "tool_not_allowed:send_email"}; !reflect.DeepEqual(steps[1].PassportDrift, want) {
		t.Fatalf("drift %v, want %v", steps[1].PassportDrift, want)
	}
	ApplyPassports(nil, steps) // no passports: nothing changes
}

type fakeMgmt map[string]control.MgmtPrincipal

func (f fakeMgmt) Resolve(_ context.Context, key string) (control.MgmtPrincipal, bool, error) {
	if key == "boom" {
		return control.MgmtPrincipal{}, false, errors.New("db down")
	}
	p, ok := f[key]
	return p, ok, nil
}

type fakePusher struct {
	pushed []*passport.Passport
	tenant string
	by     string
}

func (f *fakePusher) Push(_ context.Context, tenant string, p *passport.Passport, by string) (control.PushResult, error) {
	f.tenant, f.by = tenant, by
	for _, old := range f.pushed {
		if reflect.DeepEqual(old, p) {
			return control.PushResult{Version: len(f.pushed), PushedAt: time.Unix(0, 0), Unchanged: true}, nil
		}
	}
	f.pushed = append(f.pushed, p)
	return control.PushResult{Version: len(f.pushed), PushedAt: time.Unix(0, 0)}, nil
}

type fakeFeatures map[string]bool

func (f fakeFeatures) Enabled(_ context.Context, tenant, feature string) bool {
	return f[tenant+"/"+feature]
}

func TestPassportHandler(t *testing.T) {
	const ingestKey = "bd_live_aaaaaaaaaaaaaaaaaaaaaaaa"
	keys := fakeMgmt{
		"bd_mgmt_writer":  {TenantID: "t1", KeyID: "mkey_1", Scopes: []string{control.ScopePassportWrite}},
		"bd_mgmt_vault":   {TenantID: "t1", KeyID: "mkey_2", Scopes: []string{control.ScopeVaultSync}},
		"bd_mgmt_nofeat":  {TenantID: "t2", KeyID: "mkey_3", Scopes: []string{control.ScopePassportWrite}},
		"bd_mgmt_pattern": {TenantID: "t1", KeyID: "mkey_4", Scopes: []string{control.ScopePassportWrite}},
	}
	store := &fakePusher{}
	h := &PassportHandler{Keys: keys, Store: store, Features: fakeFeatures{"t1/passports": true},
		Policies: &fixedPolicy{p: policy(1, true, sdkconfig.Pattern{Name: "ACCOUNT_ID", Pattern: `acct-\d+`})},
		Log:      slog.New(slog.DiscardHandler)}
	doc := `{"version":"bindle.passport/v1","agent":"refunds","owner":"payments","data_classes":{"egress_block":["CARD","ACCOUNT_ID"]}}`
	do := func(method, path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	cases := []struct {
		name, method, path, key, body string
		status                        int
	}{
		{"no key", http.MethodPut, "/v1/passports", "", doc, http.StatusUnauthorized},
		{"ingest key refused", http.MethodPut, "/v1/passports", ingestKey, doc, http.StatusUnauthorized},
		{"wrong scope", http.MethodPut, "/v1/passports", "bd_mgmt_vault", doc, http.StatusForbidden},
		{"feature off", http.MethodPut, "/v1/passports", "bd_mgmt_nofeat", doc, http.StatusNotFound},
		{"db error", http.MethodPut, "/v1/passports", "boom", doc, http.StatusServiceUnavailable},
		{"wrong method", http.MethodGet, "/v1/passports", "bd_mgmt_writer", "", http.StatusMethodNotAllowed},
		{"invalid", http.MethodPut, "/v1/passports", "bd_mgmt_writer", `{"version":"x"}`, http.StatusUnprocessableEntity},
		{"unknown label", http.MethodPut, "/v1/passports", "bd_mgmt_writer", strings.Replace(doc, "ACCOUNT_ID", "IBAN", 1), http.StatusUnprocessableEntity},
		{"validate", http.MethodPost, "/v1/passports/validate", "bd_mgmt_writer", doc, http.StatusOK},
		{"push", http.MethodPut, "/v1/passports", "bd_mgmt_writer", doc, http.StatusCreated},
		{"same again", http.MethodPut, "/v1/passports", "bd_mgmt_pattern", doc, http.StatusOK},
	}
	for _, c := range cases {
		w := do(c.method, c.path, c.key, c.body)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.status, w.Body.String())
		}
	}
	if len(store.pushed) != 1 || store.tenant != "t1" || store.by != "key:mkey_4" {
		t.Fatalf("pushes %d, tenant %q, by %q", len(store.pushed), store.tenant, store.by)
	}
	var body map[string]any
	_ = json.Unmarshal(do(http.MethodPut, "/v1/passports", "bd_mgmt_writer", doc).Body.Bytes(), &body)
	if body["unchanged"] != true || body["agent"] != "refunds" || body["version"] != float64(1) {
		t.Fatalf("response %v", body)
	}
	// The validate endpoint stores nothing.
	before := len(store.pushed)
	do(http.MethodPost, "/v1/passports/validate", "bd_mgmt_writer", strings.Replace(doc, "refunds", "x", 1))
	if len(store.pushed) != before {
		t.Fatal("validate stored a passport")
	}
}
