package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// billingEnv is a full HTTP server over a real sqlite store, wired the
// same way the gateway boots it: auth resolver + SQLMeter + SQLQuotaStore
// registered on the mux. Tokens are plaintext apikeys returned by
// keys.Create.
type billingEnv struct {
	ts       *httptest.Server
	aliceTok string // type=user key for alice
	aliceID  string
	bobTok   string // type=user key for bob
	bobID    string
	adminTok string // type=admin key for alice (platform admin)
	resolver *auth.Resolver
	// meter / quotaStore are the same instances wired onto the server.
	// The e2e drives them directly to simulate the agent loop's
	// meterTokens/checkQuota calls, which live in package agent and are
	// not reachable from here.
	meter      usage.Meter
	quotaStore usage.QuotaStore
}

func newBillingEnv(t *testing.T) *billingEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	keys, err := users.NewAPIKeys(st)
	if err != nil {
		t.Fatalf("apikeys: %v", err)
	}

	accA, err := accts.Create(ctx, users.CreateInput{Username: "alice", Email: "alice@x", Password: "pw"})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	accB, err := accts.Create(ctx, users.CreateInput{Username: "bob", Email: "bob@x", Password: "pw"})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	_, tokA, err := keys.Create(ctx, accA.ID, "alice-key", users.APIKeyTypeUser, nil)
	if err != nil {
		t.Fatalf("alice key: %v", err)
	}
	_, tokB, err := keys.Create(ctx, accB.ID, "bob-key", users.APIKeyTypeUser, nil)
	if err != nil {
		t.Fatalf("bob key: %v", err)
	}
	_, adminTok, err := keys.Create(ctx, accA.ID, "alice-admin", users.APIKeyTypeAdmin, nil)
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}

	authResolver, err := auth.NewResolver(st)
	if err != nil {
		t.Fatalf("auth resolver: %v", err)
	}

	srv := NewServer(nil, authResolver, nil)
	meter := usage.NewSQLMeter(st.DB(), "sqlite")
	quotaStore := usage.NewSQLQuotaStore(st.DB(), "sqlite")
	srv.SetMeter(meter)
	srv.SetQuotaStore(quotaStore)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &billingEnv{
		ts: ts, aliceTok: tokA, aliceID: accA.ID,
		bobTok: tokB, bobID: accB.ID, adminTok: adminTok,
		resolver: authResolver, meter: meter, quotaStore: quotaStore,
	}
}

func (e *billingEnv) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// doEndUser is do() plus the X-Fastagent-End-User header — the exact
// request shape Cloud sends through its proxy. The auth middleware's
// resolve() lazily mints/rebinds to the app_user for (apikey, header)
// before the handler runs.
func (e *billingEnv) doEndUser(t *testing.T, method, path, token, endUser, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set(auth.EndUserHeader, endUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s (end-user %s): %v", method, path, endUser, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// mintAppUser creates an app_user under ownerUserID via the same
// SwitchToAppUser path the auth layer uses at request time.
func (e *billingEnv) mintAppUser(t *testing.T, ownerUserID, externalID string) string {
	t.Helper()
	ident := auth.Identity{
		UserID:     ownerUserID,
		Role:       users.RoleUser,
		AuthMethod: "apikey",
		APIKeyID:   ownerUserID,
	}
	switched, err := e.resolver.SwitchToAppUser(context.Background(), ident, externalID)
	if err != nil {
		t.Fatalf("mint app_user: %v", err)
	}
	return switched.UserID
}

func TestBilling_GetUsageRequiresAuth(t *testing.T) {
	e := newBillingEnv(t)
	code, _ := e.do(t, "GET", "/v1/usage", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/usage = %d, want 401", code)
	}
}

func TestBilling_GetUsageOwnUser(t *testing.T) {
	e := newBillingEnv(t)
	code, body := e.do(t, "GET", "/v1/usage", e.aliceTok, "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/usage = %d (%v), want 200", code, body)
	}
	if body["userId"] != e.aliceID {
		t.Errorf("userId = %v, want %s", body["userId"], e.aliceID)
	}
}

// Ownership gate: alice must NOT read bob's usage via ?user_id=.
func TestBilling_GetUsageOtherUserForbidden(t *testing.T) {
	e := newBillingEnv(t)
	code, body := e.do(t, "GET", "/v1/usage?user_id="+e.bobID, e.aliceTok, "")
	if code != http.StatusForbidden {
		t.Fatalf("GET /v1/usage?user_id=bob = %d (%v), want 403", code, body)
	}
}

// Alice may read the usage of an app_user she minted.
func TestBilling_GetUsageOwnedAppUserAllowed(t *testing.T) {
	e := newBillingEnv(t)
	appUserID := e.mintAppUser(t, e.aliceID, "ext-alice-1")
	code, body := e.do(t, "GET", "/v1/usage?user_id="+appUserID, e.aliceTok, "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/usage?user_id=owned app_user = %d (%v), want 200", code, body)
	}
}

// Bob mints an app_user; alice must not read its usage.
func TestBilling_GetUsageAppUserOwnedByOtherForbidden(t *testing.T) {
	e := newBillingEnv(t)
	appUserID := e.mintAppUser(t, e.bobID, "ext-bob-1")
	code, body := e.do(t, "GET", "/v1/usage?user_id="+appUserID, e.aliceTok, "")
	if code != http.StatusForbidden {
		t.Fatalf("GET /v1/usage?user_id=bob's app_user = %d (%v), want 403", code, body)
	}
}

func TestBilling_SetQuotaOwnUser(t *testing.T) {
	e := newBillingEnv(t)
	body := fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":1000,"monthly_request_limit":10,"reset_day":1}`, e.aliceID)
	code, out := e.do(t, "PUT", "/v1/quota", e.aliceTok, body)
	if code != http.StatusOK {
		t.Fatalf("PUT /v1/quota self = %d (%v), want 200", code, out)
	}
}

// Ownership gate: alice must NOT set a quota for bob.
func TestBilling_SetQuotaOtherUserForbidden(t *testing.T) {
	e := newBillingEnv(t)
	body := fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":1000}`, e.bobID)
	code, out := e.do(t, "PUT", "/v1/quota", e.aliceTok, body)
	if code != http.StatusForbidden {
		t.Fatalf("PUT /v1/quota bob = %d (%v), want 403", code, out)
	}
}

func TestBilling_QuotaLifecycle(t *testing.T) {
	e := newBillingEnv(t)
	body := fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":5000,"reset_day":1}`, e.aliceID)
	if code, out := e.do(t, "PUT", "/v1/quota", e.aliceTok, body); code != http.StatusOK {
		t.Fatalf("PUT quota = %d (%v)", code, out)
	}
	code, out := e.do(t, "GET", "/v1/quota?user_id="+e.aliceID, e.aliceTok, "")
	if code != http.StatusOK {
		t.Fatalf("GET quota = %d (%v)", code, out)
	}
	if code, out := e.do(t, "DELETE", "/v1/quota?user_id="+e.aliceID, e.aliceTok, ""); code != http.StatusOK {
		t.Fatalf("DELETE quota = %d (%v)", code, out)
	}
	code, out = e.do(t, "GET", "/v1/quota?user_id="+e.aliceID, e.aliceTok, "")
	if code != http.StatusNotFound {
		t.Fatalf("GET quota after delete = %d (%v), want 404", code, out)
	}
}

// Admin-tier apikey may manage anyone's quota — the platform-level
// exception to the ownership gate.
func TestBilling_AdminCanManageAnyone(t *testing.T) {
	e := newBillingEnv(t)
	body := fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":9999}`, e.bobID)
	code, out := e.do(t, "PUT", "/v1/quota", e.adminTok, body)
	if code != http.StatusOK {
		t.Fatalf("admin PUT quota for bob = %d (%v), want 200", code, out)
	}
	code, out = e.do(t, "GET", "/v1/usage?user_id="+e.bobID, e.adminTok, "")
	if code != http.StatusOK {
		t.Fatalf("admin GET bob usage = %d (%v), want 200", code, out)
	}
}

// TestBilling_CloudPathE2E walks the full Cloud call chain end-to-end:
//
//	1. Header-driven lazy mint — Cloud's proxy forwards the caller's own
//	   apikey plus X-Fastagent-End-User:<ext>; the auth middleware's
//	   resolve() rebinds identity to a freshly minted app_user. A GET
//	   /v1/usage under that header must report the app_user as its own
//	   userId (proving the header→SwitchToAppUser→identity flip).
//	2. Chat records usage — the agent loop calls meterTokens = RecordTokens
//	   + RecordTokenLog on the switched user; we drive the same two calls
//	   on the same SQLMeter the server uses.
//	3. Owner readback — alice (the apikey owner) reads the app_user's
//	   consumption via GET /v1/usage?user_id=... and sees the totals.
//	4. Upstream quota PUT — the SaaS sets a low monthly token ceiling via
//	   PUT /v1/quota.
//	5. Over-limit blocks — usage.CheckQuota (the exact function agent's
//	   checkQuota calls before every LLM call) must now return
//	   Allowed=false, proving the quota the SaaS set would halt the next
//	   agent turn. Raising the ceiling flips it back to Allowed.
func TestBilling_CloudPathE2E(t *testing.T) {
	e := newBillingEnv(t)
	ctx := context.Background()

	// 1. Lazy mint via the real middleware header path.
	code, body := e.doEndUser(t, "GET", "/v1/usage", e.aliceTok, "ext-e2e-1", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/usage under end-user header = %d (%v), want 200", code, body)
	}
	appUserID, _ := body["userId"].(string)
	if appUserID == "" || appUserID == e.aliceID {
		t.Fatalf("userId = %q, want a minted app_user distinct from owner %s", appUserID, e.aliceID)
	}

	// 2. Simulate meterTokens: one Chat call burned 100 in / 50 out on the
	//    app_user, recorded into both the daily bucket and the append log.
	if err := e.meter.RecordTokens(ctx, appUserID, "agentA", "sess-e2e", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50}); err != nil {
		t.Fatalf("record tokens: %v", err)
	}
	if err := e.meter.RecordTokenLog(ctx, appUserID, "agentA", "sess-e2e", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50}, 1234); err != nil {
		t.Fatalf("record token log: %v", err)
	}

	// 3. Owner readback — alice sees the app_user's usage.
	code, body = e.do(t, "GET", "/v1/usage?user_id="+appUserID, e.aliceTok, "")
	if code != http.StatusOK {
		t.Fatalf("owner GET app_user usage = %d (%v), want 200", code, body)
	}
	tot, ok := body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals missing in %v", body)
	}
	if got := num(t, tot["inputTokens"]); got != 100 {
		t.Errorf("totals.inputTokens = %v, want 100", tot["inputTokens"])
	}
	if got := num(t, tot["outputTokens"]); got != 50 {
		t.Errorf("totals.outputTokens = %v, want 50", tot["outputTokens"])
	}
	if got := num(t, tot["requestCount"]); got != 1 {
		t.Errorf("totals.requestCount = %v, want 1", tot["requestCount"])
	}

	// 4. Upstream quota PUT — 120 token ceiling, already 150 used.
	code, body = e.do(t, "PUT", "/v1/quota", e.aliceTok,
		fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":120,"monthly_request_limit":100,"reset_day":1}`, appUserID))
	if code != http.StatusOK {
		t.Fatalf("PUT quota = %d (%v), want 200", code, body)
	}

	// 5. Over-limit blocks the next agent turn.
	status, err := usage.CheckQuota(ctx, e.quotaStore, e.meter, appUserID)
	if err != nil {
		t.Fatalf("CheckQuota: %v", err)
	}
	if status.Allowed {
		t.Fatalf("CheckQuota allowed after 150/120 tokens, want blocked: %+v", status)
	}
	// The quota status endpoint surfaces the same verdict to Cloud.
	code, body = e.do(t, "GET", "/v1/quota?user_id="+appUserID, e.aliceTok, "")
	if code != http.StatusOK {
		t.Fatalf("GET quota = %d (%v), want 200", code, body)
	}
	if st, ok := body["status"].(map[string]any); !ok || st["allowed"] != false {
		t.Errorf("GET /v1/quota status.allowed = %v, want false", body["status"])
	}

	// Raising the ceiling unblocks.
	if code, body = e.do(t, "PUT", "/v1/quota", e.aliceTok,
		fmt.Sprintf(`{"user_id":%q,"monthly_token_limit":10000,"reset_day":1}`, appUserID)); code != http.StatusOK {
		t.Fatalf("PUT raised quota = %d (%v), want 200", code, body)
	}
	status, err = usage.CheckQuota(ctx, e.quotaStore, e.meter, appUserID)
	if err != nil {
		t.Fatalf("CheckQuota (raised): %v", err)
	}
	if !status.Allowed {
		t.Fatalf("CheckQuota still blocked after ceiling raise: %+v", status)
	}
}

// num coerces a decoded JSON number to int64 for numeric assertions.
func num(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case nil:
		return 0
	}
	t.Fatalf("value %v is not a number", v)
	return 0
}
