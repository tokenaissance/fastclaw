package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// nopSystemFileStore satisfies the registry's SystemFileStore slice; the
// preference tests only use it to bind an agentID to the registry (the
// same role manager.go:205's SetSystemFileStore plays for real agents).
type nopSystemFileStore struct{}

func (nopSystemFileStore) GetWorkspaceFile(context.Context, string, string, string) ([]byte, error) {
	return nil, store.ErrNotFound
}
func (nopSystemFileStore) GetWorkspaceFileExact(context.Context, string, string, string) ([]byte, error) {
	return nil, store.ErrNotFound
}
func (nopSystemFileStore) SaveWorkspaceFile(context.Context, string, string, string, []byte) error {
	return nil
}

// prefFixture builds a Registry bound to agent "agent-A" against a real
// in-memory sqlite store, with set_preference registered exactly as
// manager.go:239 wires it. The per-turn chatter is set per-test via
// setChatter (mirroring bindSession).
func prefFixture(t *testing.T) (*Registry, store.Store) {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := NewRegistry("", "")
	r.SetSystemFileStore(nopSystemFileStore{}, "agent-A")
	RegisterPreferenceTool(r, db)
	return r, db
}

// setChatter simulates the agent loop stamping the per-turn chatter via
// SetChatterUserID (bindSession's identity resolution).
func setChatter(r *Registry, uid string) { r.SetChatterUserID(uid) }

// TestSetPreferenceWritesPerUserAgentScope: happy path persists a
// preference at the (chatter, agent) layer — NOT the agent layer or the
// user layer — and dual-writes it to configs_kv under the same
// per-(user,agent) scope.
func TestSetPreferenceWritesPerUserAgentScope(t *testing.T) {
	r, db := prefFixture(t)
	setChatter(r, "chatter-1")
	ctx := context.Background()

	out, err := callTool(t, r, "set_preference", `{"key":"timezone","value":"Asia/Shanghai"}`)
	if err != nil {
		t.Fatalf("set_preference: %v", err)
	}
	if !strings.Contains(out, "Preference saved: timezone = Asia/Shanghai") {
		t.Errorf("response = %q, want 'Preference saved' ack", out)
	}

	// configs row at exactly (chatter-1, agent-A, prefs).
	rec, err := db.GetConfigByName(ctx, store.KindSetting, "chatter-1", "agent-A", scope.PrefsNamespace)
	if err != nil || rec == nil {
		t.Fatalf("GetConfigByName(chatter-1, agent-A, prefs): rec=%+v err=%v", rec, err)
	}
	if rec.Data["timezone"] != "Asia/Shanghai" {
		t.Errorf("prefs data = %v, want timezone=Asia/Shanghai", rec.Data)
	}

	// configs_kv dual-write at the per-(user,agent) layer.
	v, err := db.GetConfigValue(ctx, store.KindSetting, scope.UserAgent, "chatter-1/agent-A", "prefs.timezone")
	if err != nil || v != "Asia/Shanghai" {
		t.Errorf("configs_kv prefs.timezone = %q err=%v; want Asia/Shanghai", v, err)
	}

	// Nothing leaked to the agent layer or the user layer.
	if rec, err := db.GetConfigByName(ctx, store.KindSetting, "", "agent-A", scope.PrefsNamespace); err == nil {
		t.Errorf("pref leaked to agent-scope row: %+v", rec)
	}
	if rec, err := db.GetConfigByName(ctx, store.KindSetting, "chatter-1", "", scope.PrefsNamespace); err == nil {
		t.Errorf("pref leaked to user-scope row: %+v", rec)
	}
}

// TestSetPreferenceMergePreservesExistingPrefs: a second set_preference
// call merges into the existing prefs row instead of clobbering it.
func TestSetPreferenceMergePreservesExistingPrefs(t *testing.T) {
	r, db := prefFixture(t)
	setChatter(r, "chatter-1")
	ctx := context.Background()

	if err := scope.SaveSetting(ctx, db, "chatter-1", "agent-A", scope.PrefsNamespace,
		map[string]interface{}{"language": "zh-CN"}); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}

	if _, err := callTool(t, r, "set_preference", `{"key":"drawing_style","value":"line-art"}`); err != nil {
		t.Fatalf("set_preference: %v", err)
	}

	rec, err := db.GetConfigByName(ctx, store.KindSetting, "chatter-1", "agent-A", scope.PrefsNamespace)
	if err != nil || rec == nil {
		t.Fatalf("read back prefs: rec=%+v err=%v", rec, err)
	}
	if rec.Data["language"] != "zh-CN" {
		t.Errorf("existing 'language' pref clobbered: %v", rec.Data)
	}
	if rec.Data["drawing_style"] != "line-art" {
		t.Errorf("new 'drawing_style' pref not saved: %v", rec.Data)
	}
}

// TestSetPreferenceRejectsEmptyKey / EmptyValue: the args are required.
func TestSetPreferenceRejectsEmptyKey(t *testing.T) {
	r, _ := prefFixture(t)
	setChatter(r, "chatter-1")
	_, err := callTool(t, r, "set_preference", `{"key":"","value":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "key is required") {
		t.Fatalf("empty key: want 'key is required', got %v", err)
	}
}

func TestSetPreferenceRejectsEmptyValue(t *testing.T) {
	r, _ := prefFixture(t)
	setChatter(r, "chatter-1")
	_, err := callTool(t, r, "set_preference", `{"key":"timezone","value":""}`)
	if err == nil || !strings.Contains(err.Error(), "value is required") {
		t.Fatalf("empty value: want 'value is required', got %v", err)
	}
}

// TestSetPreferenceNoChatterIdentity: firing outside a chat turn (no
// chatter resolved) must surface a recoverable error, never silently
// write an empty-keyed row.
func TestSetPreferenceNoChatterIdentity(t *testing.T) {
	r, _ := prefFixture(t)
	// Deliberately no SetChatterUserID / SetOwnerUserID.
	_, err := callTool(t, r, "set_preference", `{"key":"timezone","value":"UTC"}`)
	if err == nil || !strings.Contains(err.Error(), "no chatter identity") {
		t.Fatalf("no chatter: want 'no chatter identity', got %v", err)
	}
}

// TestSetPreferenceNoAgentIdentity: the registry must carry an agentID
// (bound at build time); without it the preference has nowhere to live.
func TestSetPreferenceNoAgentIdentity(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	r := NewRegistry("", "")
	r.SetChatterUserID("chatter-1") // agentID never bound
	RegisterPreferenceTool(r, db)

	_, err = callTool(t, r, "set_preference", `{"key":"timezone","value":"UTC"}`)
	if err == nil || !strings.Contains(err.Error(), "no agent identity") {
		t.Fatalf("no agent: want 'no agent identity', got %v", err)
	}
}

// TestSetPreferenceMalformedArgs: invalid JSON / wrong-typed args must
// come back as a Go error, not a panic and not a silent success.
func TestSetPreferenceMalformedArgs(t *testing.T) {
	r, _ := prefFixture(t)
	setChatter(r, "chatter-1")
	for _, bad := range []string{
		`{`,                    // truncated JSON
		`{"key":"a"}`,          // missing value
		`{"value":"b"}`,        // missing key
		`{"key":1,"value":"b"}`, // wrong type
	} {
		if _, err := callTool(t, r, "set_preference", bad); err == nil {
			t.Errorf("set_preference accepted malformed args %q", bad)
		}
	}
}

// TestSetPreferenceRegistered: guard that the tool ships under its
// expected name.
func TestSetPreferenceRegistered(t *testing.T) {
	r, _ := prefFixture(t)
	if r.GetFunc("set_preference") == nil {
		t.Error("set_preference not registered")
	}
}
