package setup

// e2e for commit 48ee0b3 "fix runtime settings and empty chat
// responses" — the runtime-settings half.
//
// Cloud zero-impact rationale: runtime settings (timezone) are persisted
// through /api/config (GET handleGetConfig / POST handleUpdateConfig),
// which Cloud's Next.js app reaches via the /api/fastagent proxy exactly
// like every other config write. The Cloud runtime-settings page POSTs
// {prefs:{timezone}} alongside the sandbox block; the backend validates
// the IANA name, saves at user scope, and returns meta.serverTimezone so
// the page can show the deployment fallback. No endpoint / shape change —
// the prefs + meta keys are additive.
//
// This test drives the REAL handlers + a real DBStore (setupFileUploadTest
// pattern) and pins the fork's batch-table adaptation: "prefs" joined
// settingNamespaces, so loadUserConfig reads it via BatchSettings and
// saveUserConfig's sweep persists it — the upstream per-namespace
// SettingInto call became a table entry (same storage, batch-consistent).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// cfgReq builds an authenticated request against /api/config with the
// config.UserID context stamped (loadUserConfig / saveUserConfig read it).
func cfgReq(t *testing.T, method, uid, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return stampAuthAndUserID(req, uid)
}

// decodeConfigBody unmarshals the /api/config response map.
func decodeConfigBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode config response: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

func TestRuntimeSettings_PrefsTimezone_CloudPathE2E(t *testing.T) {
	s, uid, _ := setupFileUploadTest(t)
	ctx := context.Background()

	// ── 1. Initial GET: no prefs yet, but meta.serverTimezone is present
	// (the deployment fallback the runtime page displays).
	rec := httptest.NewRecorder()
	s.handleGetConfig(rec, cfgReq(t, http.MethodGet, uid, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("initial get status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cfg := decodeConfigBody(t, rec)
	// PrefsCfg is a struct with omitempty — Go does not omit empty structs,
	// so the key is present as `prefs:{}` (upstream behavior). The important
	// assertion is that no timezone is set yet.
	if prefs, _ := cfg["prefs"].(map[string]any); prefs != nil {
		if tz, _ := prefs["timezone"].(string); tz != "" {
			t.Fatalf("initial get already has a timezone: %v", cfg["prefs"])
		}
	}
	meta, _ := cfg["meta"].(map[string]any)
	if meta == nil || meta["serverTimezone"] == nil || meta["serverTimezone"] == "" {
		t.Fatalf("meta.serverTimezone missing/empty: %+v", meta)
	}
	if meta["systemDefaultModel"] == nil {
		t.Errorf("meta.systemDefaultModel dropped by the serverTimezone merge: %+v", meta)
	}

	// ── 2. POST {prefs:{timezone}} → 200, persisted at user scope.
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, cfgReq(t, http.MethodPost, uid, `{"prefs":{"timezone":"Asia/Shanghai"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update timezone status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Row landed in configs at (user, ''), name=prefs — the fork's
	// batch-table write (scope.SaveSetting from the saveUserConfig sweep).
	row, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, uid, "", scope.PrefsNamespace)
	if err != nil || row == nil {
		t.Fatalf("prefs row not found: err=%v row=%v", err, row)
	}
	if tz, _ := row.Data["timezone"].(string); tz != "Asia/Shanghai" {
		t.Fatalf("persisted prefs.timezone = %v, want Asia/Shanghai", row.Data["timezone"])
	}

	// GET reflects the saved timezone (loadUserConfig batch read).
	rec = httptest.NewRecorder()
	s.handleGetConfig(rec, cfgReq(t, http.MethodGet, uid, ""))
	cfg = decodeConfigBody(t, rec)
	prefs, _ := cfg["prefs"].(map[string]any)
	if prefs == nil || prefs["timezone"] != "Asia/Shanghai" {
		t.Fatalf("get prefs after save = %v, want timezone Asia/Shanghai", cfg["prefs"])
	}

	// ── 3. IANA validation: a bogus name → 400 with a helpful message
	// (upstream's handleUpdateConfig gate, auto-merged into the fork).
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, cfgReq(t, http.MethodPost, uid, `{"prefs":{"timezone":"Not/AZone"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-tz status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid timezone") {
		t.Fatalf("invalid-tz body = %s, want 'invalid timezone' hint", rec.Body.String())
	}

	// ── 4. PATCH semantics: a sandbox-only POST must NOT wipe prefs
	// (the namespace sweep round-trips the loaded prefs unchanged).
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, cfgReq(t, http.MethodPost, uid, `{"sandbox":{"enabled":false}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("sandbox-only update status = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleGetConfig(rec, cfgReq(t, http.MethodGet, uid, ""))
	cfg = decodeConfigBody(t, rec)
	if prefs, _ := cfg["prefs"].(map[string]any); prefs == nil || prefs["timezone"] != "Asia/Shanghai" {
		t.Fatalf("prefs wiped by sandbox-only save: %v", cfg["prefs"])
	}

	// ── 5. Clearing: {prefs:{timezone:""}} removes the row (SaveSetting
	// with empty data deletes; both the sweep and the explicit block land
	// on the same user-scope row).
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, cfgReq(t, http.MethodPost, uid, `{"prefs":{"timezone":""}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear timezone status = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleGetConfig(rec, cfgReq(t, http.MethodGet, uid, ""))
	cfg = decodeConfigBody(t, rec)
	if prefs, _ := cfg["prefs"].(map[string]any); prefs != nil {
		if tz, _ := prefs["timezone"].(string); tz != "" {
			t.Fatalf("timezone not cleared: %v", cfg["prefs"])
		}
	}
	row, _ = s.dataStore.GetConfigByName(ctx, store.KindSetting, uid, "", scope.PrefsNamespace)
	if row != nil {
		t.Fatalf("prefs row still present after clear: %+v", row)
	}
}
