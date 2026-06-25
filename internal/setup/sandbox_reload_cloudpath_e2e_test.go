package setup

// e2e for commit 92a3cbb "hot reload sandbox and improve web fetch" —
// the sandbox hot-reload half.
//
// Cloud call path: the FastAgent admin dashboard (proxied through
// /api/fastagent/api/config) POSTs the sandbox block at SYSTEM scope —
// Cloud reaches the backend as super_admin (FASTAGENT_ADMIN_API_KEY), so
// scopeForSave resolves to scope.System and handleUpdateConfig fires
// reloadSystemSandbox() → Gateway.ReloadSandbox(), rebuilding the
// gateway-wide sandbox executor pool without a process restart. The
// settings page then gets a 200 and the next chat's exec tools pick up
// the new backend/API key immediately.
//
// This test drives the REAL handleUpdateConfig + a real DBStore and a
// fake api.UserResolver implementing ReloadSandbox()/ReloadAgents()
// (mirroring the real gateway resolver the Cloud admin path hits),
// pinning: (1) system-scope sandbox save fires exactly one
// ReloadSandbox; (2) a user-scope save does NOT (pool is gateway-wide,
// only system rows own it); (3) a resolver that doesn't implement the
// sandboxReloader interface degrades to a silent no-op, not a panic.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// reloadSandboxResolver implements api.UserResolver plus the two system-
// scope hooks the Cloud admin path fires: ReloadSandbox (sandbox pool
// rebuild, called by reloadSystemSandbox when sc == scope.System) and
// ReloadAgents (cache invalidation, called by invalidateScope(System)).
type reloadSandboxResolver struct {
	sandboxReloads int
	agentReloads   int
}

func (r *reloadSandboxResolver) UserSpaceFor(string) (*api.UserSpaceView, error) {
	return nil, nil
}
func (r *reloadSandboxResolver) LocalAgentManager() *agent.Manager { return nil }
func (r *reloadSandboxResolver) IsCloudMode() bool                 { return true }
func (r *reloadSandboxResolver) ReloadSandbox() error {
	r.sandboxReloads++
	return nil
}
func (r *reloadSandboxResolver) ReloadAgents() error {
	r.agentReloads++
	return nil
}

// stampSystemAdmin stamps a super_admin non-acting identity (+ system
// config.UserID) so scopeForSave resolves to scope.System — the exact
// shape Cloud's admin API key produces.
func stampSystemAdmin(r *http.Request) *http.Request {
	ctx := auth.WithIdentity(r.Context(), auth.Identity{
		UserID:     "sys_admin",
		Role:       "super_admin",
		AuthMethod: "session",
	})
	ctx = config.WithUserID(ctx, "")
	return r.WithContext(ctx)
}

func TestSandbox_HotReloadCloudPathE2E(t *testing.T) {
	s, uid, _ := setupFileUploadTest(t)
	resolver := &reloadSandboxResolver{}
	s.userResolver = resolver
	ctx := context.Background()

	// ── 1. System-scope sandbox save → ReloadSandbox fires exactly once,
	// the pool rebuild the Cloud admin dashboard depends on. The system
	// invalidate (ReloadAgents) also fires because scopeForSave returned
	// System, mirroring the real gateway resolver.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"sandbox":{"enabled":true,"backend":"e2b","apiKey":"test-key"}}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleUpdateConfig(rec, stampSystemAdmin(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("system sandbox save status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if resolver.sandboxReloads != 1 {
		t.Fatalf("ReloadSandbox called %d time(s); want 1", resolver.sandboxReloads)
	}
	if resolver.agentReloads != 1 {
		t.Fatalf("ReloadAgents called %d time(s); want 1 (system-scope invalidate)", resolver.agentReloads)
	}
	// The sandbox row landed at SYSTEM scope (user_id='', agent_id='') —
	// the row ReloadSandbox's readSystemSandboxCfg reads back from.
	row, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, "", "", "sandbox")
	if err != nil || row == nil {
		t.Fatalf("system sandbox row missing: err=%v row=%v", err, row)
	}
	if b, _ := row.Data["backend"].(string); b != "e2b" {
		t.Fatalf("persisted sandbox.backend = %v, want e2b", row.Data["backend"])
	}

	// ── 2. A user-scope sandbox save must NOT trigger ReloadSandbox —
	// the executor pool is gateway-wide; only system rows own it.
	rec = httptest.NewRecorder()
	s.handleUpdateConfig(rec, cfgReq(t, http.MethodPost, uid, `{"sandbox":{"enabled":false}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("user sandbox save status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if resolver.sandboxReloads != 1 {
		t.Fatalf("user-scope save fired ReloadSandbox %d time(s); want still 1",
			resolver.sandboxReloads)
	}

	// ── 3. Resolver without the sandboxReloader interface: reloadSystemSandbox
	// degrades to a silent no-op — no panic, still 200 (older/remote
	// resolvers that can't rebuild the local pool keep working).
	s.userResolver = &recordingChannelResolver{}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"sandbox":{"enabled":false}}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleUpdateConfig(rec, stampSystemAdmin(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-reloader system save status = %d, body=%s", rec.Code, rec.Body.String())
	}
}
