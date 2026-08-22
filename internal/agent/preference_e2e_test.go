package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// TestPreference_CloudPathE2E mirrors the Cloud call path for the #47
// set_preference tool. The registry is wired exactly as Manager.buildAgent
// wires it (manager.go:239 — RegisterPreferenceTool(ag.registry,
// m.opts.dataStore) behind a real store), and the per-turn chatter is
// stamped the same way bindSession does (SetChatterUserID). A chatter
// asking the agent to persist a preference lands a row at per-(user,agent)
// scope in BOTH configs (SaveSetting) and configs_kv (dual-write), with no
// leakage to sibling chatters or the agent layer.
func TestPreference_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	const (
		agentID  = "agt-e2e"
		chatter  = "chatter-e2e"
		sibling  = "chatter-other"
		prefKey  = "replicate_api_token"
		prefVal  = "r8_e2e_secret"
	)

	// Same construction Cloud uses: identity/memory store binds agentID,
	// preference tool rides the relational dataStore.
	r := tools.NewRegistry("", "")
	r.SetOwnerUserID(chatter)
	r.SetSystemFileStore(NewMemoryStoreAdapter(db), agentID)
	tools.RegisterPreferenceTool(r, db)
	// bindSession stamps the per-turn chatter for a real turn.
	r.SetChatterUserID(chatter)

	fn := r.GetFunc("set_preference")
	if fn == nil {
		t.Fatalf("set_preference not registered on agent registry")
	}
	out, err := fn(ctx, json.RawMessage(`{"key":"`+prefKey+`","value":"`+prefVal+`"}`))
	if err != nil {
		t.Fatalf("set_preference: %v", err)
	}
	if !strings.Contains(out, "Preference saved: "+prefKey) {
		t.Errorf("response = %q, want 'Preference saved' ack", out)
	}

	// configs row at exactly (chatter, agent, prefs).
	rec, err := db.GetConfigByName(ctx, store.KindSetting, chatter, agentID, scope.PrefsNamespace)
	if err != nil || rec == nil {
		t.Fatalf("GetConfigByName(chatter, agent, prefs): rec=%+v err=%v", rec, err)
	}
	if rec.Data[prefKey] != prefVal {
		t.Errorf("prefs data = %v, want %s=%s", rec.Data, prefKey, prefVal)
	}

	// configs_kv dual-write at the per-(user,agent) layer.
	if v, err := db.GetConfigValue(ctx, store.KindSetting, scope.UserAgent, chatter+"/"+agentID,
		scope.PrefsNamespace+"."+prefKey); err != nil || v != prefVal {
		t.Errorf("configs_kv %s = %q err=%v; want %s", prefKey, v, err, prefVal)
	}

	// A sibling chatter of the same agent must NOT see it — the
	// preference is per-(user,agent), not agent-shared.
	if rec, err := db.GetConfigByName(ctx, store.KindSetting, sibling, agentID, scope.PrefsNamespace); err == nil {
		t.Errorf("pref leaked to sibling chatter row: %+v", rec)
	}
	// And it must not live at the bare agent layer.
	if rec, err := db.GetConfigByName(ctx, store.KindSetting, "", agentID, scope.PrefsNamespace); err == nil {
		t.Errorf("pref leaked to agent-scope row: %+v", rec)
	}
}
