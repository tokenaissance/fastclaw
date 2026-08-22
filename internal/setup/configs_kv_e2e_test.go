package setup

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

// TestProviders_CloudPathE2E drives the configs_kv lifecycle through the
// real scoped provider handlers (the Cloud proxy path: POST/GET/DELETE
// /api/providers?scope=...) and asserts the dual-write contract holds:
//
//   - handleCreateProvider dual-writes each provider to configs_kv
//     (single-value rows at the right scope layer) AND the legacy
//     configs JSON blob;
//   - handleListProviders reads through scope.Providers, which prefers
//     configs_kv once populated;
//   - handleDeleteProvider dual-deletes, draining the configs_kv row
//     together with the legacy configs row.
func TestProviders_CloudPathE2E(t *testing.T) {
	s, uid, aid := setupFileUploadTest(t)
	ctx := context.Background()

	// 1. Create a user-scope provider → 200 + configs_kv row at the
	//    (kind, scope, scope_id) layer, not the agent layer.
	create := func(sc, scopeID, name, apiKey, apiBase string) int {
		t.Helper()
		body := strings.NewReader(`{"name":"` + name + `","apiKey":"` + apiKey +
			`","apiBase":"` + apiBase + `"}`)
		req := httptest.NewRequest(http.MethodPost,
			"/api/providers?scope="+sc+"&scopeId="+scopeID, body)
		req.Header.Set("Content-Type", "application/json")
		req = stampAuthAndUserID(req, uid)
		rec := httptest.NewRecorder()
		s.handleCreateProvider(rec, req)
		return rec.Code
	}
	if code := create(scope.User, uid, "openai", "sk-e2e-user", "https://api.openai.com"); code != http.StatusOK {
		t.Fatalf("create user-scope provider status = %d", code)
	}
	v, err := s.dataStore.GetConfigValue(ctx, store.KindProvider, scope.User, uid, "openai.api_key")
	if err != nil || v != "sk-e2e-user" {
		t.Fatalf("configs_kv user openai.api_key = %q err=%v; want sk-e2e-user", v, err)
	}
	// Nothing leaked into the agent layer.
	if v, err := s.dataStore.GetConfigValue(ctx, store.KindProvider, scope.Agent, aid, "openai.api_key"); err == nil {
		t.Fatalf("user-scope key leaked into agent layer: %q", v)
	}

	// 2. Create an agent-scope provider → 200 + configs_kv row at the
	//    agent layer.
	if code := create(scope.Agent, aid, "anthropic", "sk-e2e-agent", "https://api.anthropic.com"); code != http.StatusOK {
		t.Fatalf("create agent-scope provider status = %d", code)
	}
	v, err = s.dataStore.GetConfigValue(ctx, store.KindProvider, scope.Agent, aid, "anthropic.api_key")
	if err != nil || v != "sk-e2e-agent" {
		t.Fatalf("configs_kv agent anthropic.api_key = %q err=%v; want sk-e2e-agent", v, err)
	}

	// 3. Read-through the merged provider view for (uid, aid): both the
	//    user-scope and agent-scope keys resolve, and configs_kv is the
	//    preferred source (the values written to KV come back, not a
	//    legacy-configs fallback).
	provs, err := scope.Providers(ctx, s.dataStore, uid, aid)
	if err != nil {
		t.Fatalf("scope.Providers: %v", err)
	}
	if got := provs["openai"].APIKey; got != "sk-e2e-user" {
		t.Errorf("openai.APIKey = %q, want sk-e2e-user", got)
	}
	if got := provs["anthropic"].APIKey; got != "sk-e2e-agent" {
		t.Errorf("anthropic.APIKey = %q, want sk-e2e-agent", got)
	}
	// A sibling agent of the same user must not inherit the agent-scope
	// key, and must not see the user key unless it's genuinely the same
	// ownership the read path was asked for.
	sibProvs, err := scope.Providers(ctx, s.dataStore, uid, "agt_other")
	if err != nil {
		t.Fatalf("scope.Providers(sibling): %v", err)
	}
	if _, ok := sibProvs["anthropic"]; ok {
		t.Errorf("sibling agent leaked agent-scope provider: %+v", sibProvs)
	}

	// 4. List through the HTTP handler → masked key, KV-backed value.
	list := httptest.NewRequest(http.MethodGet,
		"/api/providers?scope="+scope.User+"&scopeId="+uid, nil)
	list = stampAuthAndUserID(list, uid)
	rec := httptest.NewRecorder()
	s.handleListProviders(rec, list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Providers) != 1 {
		t.Fatalf("list providers = %d; want 1, got %+v", len(listResp.Providers), listResp.Providers)
	}
	if p := listResp.Providers[0]; p["name"] != "openai" || p["apiKey"] == "sk-e2e-user" {
		t.Errorf("listed provider = %+v; want name=openai with masked key", p)
	}

	// 5. Delete → dual-delete drains configs_kv together with the legacy
	//    configs row.
	rec2, err := s.dataStore.GetConfigByName(ctx, store.KindProvider, uid, "", "openai")
	if err != nil || rec2 == nil {
		t.Fatalf("GetConfigByName(openai): rec=%+v err=%v", rec2, err)
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/providers/"+rec2.ID, nil)
	del.SetPathValue("id", rec2.ID)
	del = stampAuthAndUserID(del, uid)
	rec = httptest.NewRecorder()
	s.handleDeleteProvider(rec, del)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if v, err := s.dataStore.GetConfigValue(ctx, store.KindProvider, scope.User, uid, "openai.api_key"); err == nil {
		t.Errorf("configs_kv openai.api_key still present after delete: %q", v)
	}
	if _, err := s.dataStore.GetConfigByName(ctx, store.KindProvider, uid, "", "openai"); err == nil {
		t.Errorf("legacy configs openai row still present after delete")
	}
}
