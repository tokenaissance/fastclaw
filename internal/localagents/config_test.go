package localagents

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

func setupTestHome(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "fastclaw")
	t.Setenv("FASTCLAW_HOME", root)
	return root
}

func TestInitPreservesAgentConfigAndSettings(t *testing.T) {
	setupTestHome(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	if _, err := Init("scratch", InitOptions{
		Description: "initial",
		Provider:    "openai",
		Model:       "openai/gpt-4.1",
		APIKeyEnv:   "OPENAI_API_KEY",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := SetConfig("scratch", "temperature", "0.2"); err != nil {
		t.Fatalf("set temperature: %v", err)
	}

	st, inst, err := storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName: %v", err)
	}
	rec, err := st.GetAgent(context.Background(), inst.AgentID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	rec.Config["workspace"] = "/tmp/workspace"
	if err := st.SaveAgent(context.Background(), rec); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	st.Close()

	if _, err := Init("scratch", InitOptions{
		Provider:  "openai",
		Model:     "openai/gpt-4.1-mini",
		APIKeyEnv: "OPENAI_API_KEY",
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	st, inst, err = storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName after re-init: %v", err)
	}
	defer st.Close()
	rec, err = st.GetAgent(context.Background(), inst.AgentID)
	if err != nil {
		t.Fatalf("get agent after re-init: %v", err)
	}
	if got := rec.Config["description"]; got != "initial" {
		t.Fatalf("description not preserved: got %#v", got)
	}
	if got := rec.Config["workspace"]; got != "/tmp/workspace" {
		t.Fatalf("workspace not preserved: got %#v", got)
	}

	temp, err := GetConfig("scratch", "temperature")
	if err != nil {
		t.Fatalf("get temperature: %v", err)
	}
	if got, ok := temp.(float64); !ok || got != 0.2 {
		t.Fatalf("temperature not preserved: got %#v", temp)
	}
	model, err := GetConfig("scratch", "model")
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	if model != "openai/gpt-4.1-mini" {
		t.Fatalf("model not updated: got %#v", model)
	}
}

func TestSetProviderFieldUsesPresetForNewProvider(t *testing.T) {
	setupTestHome(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	if _, err := Init("scratch", InitOptions{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := SetConfig("scratch", "provider.openai.apiKeyEnv", "OPENAI_API_KEY"); err != nil {
		t.Fatalf("set provider apiKeyEnv: %v", err)
	}

	base, err := GetConfig("scratch", "provider.openai.apiBase")
	if err != nil {
		t.Fatalf("get provider apiBase: %v", err)
	}
	if base != "https://api.openai.com/v1" {
		t.Fatalf("apiBase preset missing: got %#v", base)
	}
	apiType, err := GetConfig("scratch", "provider.openai.apiType")
	if err != nil {
		t.Fatalf("get provider apiType: %v", err)
	}
	if apiType != "openai-chat" {
		t.Fatalf("apiType preset missing: got %#v", apiType)
	}
}

func TestInitRejectsMismatchedProviderModel(t *testing.T) {
	setupTestHome(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	_, err := Init("scratch", InitOptions{
		Provider:  "openai",
		Model:     "anthropic/claude-3-5-sonnet",
		APIKeyEnv: "OPENAI_API_KEY",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected provider/model mismatch error, got %v", err)
	}
}

func TestPutFileRejectsUnsupportedSystemFilename(t *testing.T) {
	setupTestHome(t)
	if _, err := Init("scratch", InitOptions{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	path := filepath.Join(t.TempDir(), "NOTES.md")
	if err := os.WriteFile(path, []byte("notes"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	err := PutFile("scratch", "NOTES.md", path)
	if err == nil {
		t.Fatal("expected unsupported filename error")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected validation before store lookup, got %v", err)
	}
}

func TestInitRejectsRunningPortChange(t *testing.T) {
	root := setupTestHome(t)
	p, err := instancePaths("scratch")
	if err != nil {
		t.Fatalf("instance paths: %v", err)
	}
	if err := saveInstance(p.metaFile, &Instance{
		Name: "scratch",
		PID:  os.Getpid(),
		Port: 34567,
		Home: filepath.Join(root, "local-agents", "scratch"),
	}); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	_, err = Init("scratch", InitOptions{Port: 34568})
	if err == nil || !strings.Contains(err.Error(), "stop it before changing --port") {
		t.Fatalf("expected running port-change error, got %v", err)
	}
}

func TestStartRejectsUnavailablePort(t *testing.T) {
	setupTestHome(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	_, err = Start("scratch", StartOptions{Port: port})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected unavailable port error, got %v", err)
	}
}

func TestInitPreservesExistingModelEntryFields(t *testing.T) {
	setupTestHome(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	if _, err := Init("scratch", InitOptions{
		Provider:  "openai",
		Model:     "openai/gpt-4.1",
		APIKeyEnv: "OPENAI_API_KEY",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Simulate the dashboard enriching the model entry with cost/context.
	st, _, err := storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName: %v", err)
	}
	rec, err := st.GetConfigByName(context.Background(), store.KindProvider, scope.System, "", "openai")
	if err != nil {
		t.Fatalf("get provider: %v", err)
	}
	models, _ := rec.Data["models"].([]interface{})
	if len(models) == 0 {
		st.Close()
		t.Fatal("expected at least one model entry")
	}
	first, _ := models[0].(map[string]interface{})
	first["contextWindow"] = float64(128000)
	first["maxTokens"] = float64(4096)
	first["cost"] = map[string]interface{}{"input": 0.01, "output": 0.03}
	rec.Data["models"] = models
	if err := scope.SaveProvider(context.Background(), st, scope.System, "", "openai", providerConfigFromMap(rec.Data)); err != nil {
		st.Close()
		t.Fatalf("save provider: %v", err)
	}
	st.Close()

	// Re-init with the same model — metadata must survive.
	if _, err := Init("scratch", InitOptions{
		Provider:  "openai",
		Model:     "openai/gpt-4.1",
		APIKeyEnv: "OPENAI_API_KEY",
	}); err != nil {
		t.Fatalf("re-init: %v", err)
	}

	st, _, err = storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName after re-init: %v", err)
	}
	defer st.Close()
	rec, err = st.GetConfigByName(context.Background(), store.KindProvider, scope.System, "", "openai")
	if err != nil {
		t.Fatalf("get provider after re-init: %v", err)
	}
	models, _ = rec.Data["models"].([]interface{})
	if len(models) == 0 {
		t.Fatal("models cleared after re-init")
	}
	first, _ = models[0].(map[string]interface{})
	if got := first["contextWindow"]; got != float64(128000) {
		t.Fatalf("contextWindow not preserved: got %#v", got)
	}
	if got := first["maxTokens"]; got != float64(4096) {
		t.Fatalf("maxTokens not preserved: got %#v", got)
	}
	cost, _ := first["cost"].(map[string]interface{})
	if cost["input"] != 0.01 || cost["output"] != 0.03 {
		t.Fatalf("cost not preserved: got %#v", cost)
	}
}

// providerConfigFromMap is a test helper that bridges the loose map shape
// the store returns back into a typed ProviderConfig for SaveProvider.
func providerConfigFromMap(data map[string]interface{}) config.ProviderConfig {
	pc := config.ProviderConfig{
		APIKey:   stringField(data, "apiKey"),
		APIBase:  stringField(data, "apiBase"),
		APIType:  stringField(data, "apiType"),
		AuthType: stringField(data, "authType"),
	}
	models, _ := data["models"].([]interface{})
	for _, m := range models {
		entry, _ := m.(map[string]interface{})
		me := config.ModelEntry{
			ID:            stringField(entry, "id"),
			Name:          stringField(entry, "name"),
			ContextWindow: int(numField(entry, "contextWindow")),
			MaxTokens:     int(numField(entry, "maxTokens")),
		}
		if c, ok := entry["cost"].(map[string]interface{}); ok {
			me.Cost.Input = numField(c, "input")
			me.Cost.Output = numField(c, "output")
			me.Cost.CacheRead = numField(c, "cacheRead")
			me.Cost.CacheWrite = numField(c, "cacheWrite")
		}
		pc.Models = append(pc.Models, me)
	}
	return pc
}

func stringField(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func numField(m map[string]interface{}, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

func TestEnsureAccountRejectsMissingExplicitUser(t *testing.T) {
	setupTestHome(t)

	// Seed the store with a real super-admin user so the count > 0.
	if _, err := Init("scratch", InitOptions{}); err != nil {
		t.Fatalf("init seed: %v", err)
	}

	_, err := Init("scratch", InitOptions{Username: "ghost"})
	if err == nil {
		t.Fatal("expected error when --username does not exist")
	}
	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("expected error to name the missing user, got %v", err)
	}
}

func TestEnsureAccountResolvesExistingExplicitUser(t *testing.T) {
	setupTestHome(t)

	res1, err := Init("scratch", InitOptions{Username: "alice"})
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if !res1.CreatedUser {
		t.Fatal("first init should create alice")
	}

	res2, err := Init("scratch", InitOptions{Username: "alice"})
	if err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if res2.CreatedUser {
		t.Fatal("alice already existed; expected reuse, not re-create")
	}
	if res1.Instance.UserID != res2.Instance.UserID {
		t.Fatalf("UserID changed across re-init: %q -> %q", res1.Instance.UserID, res2.Instance.UserID)
	}

	st, _, err := storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName: %v", err)
	}
	defer st.Close()
	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	acct, err := accts.Get(context.Background(), res2.Instance.UserID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acct.Username != "alice" {
		t.Fatalf("expected alice, got %q", acct.Username)
	}
}

func TestRemoveAgentDefaultPreservesHomeAndLog(t *testing.T) {
	setupTestHome(t)

	if _, err := Init("scratch", InitOptions{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	p, err := instancePaths("scratch")
	if err != nil {
		t.Fatalf("instance paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(p.logFile, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if _, err := Remove("scratch", RemoveOptions{}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(p.metaFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata still present: %v", err)
	}
	if _, err := os.Stat(p.homeDir); err != nil {
		t.Fatalf("home should be preserved: %v", err)
	}
	if _, err := os.Stat(p.logFile); err != nil {
		t.Fatalf("log should be preserved: %v", err)
	}

	// Removing a missing agent must error rather than silently succeed.
	if _, err := Remove("scratch", RemoveOptions{}); err == nil {
		t.Fatal("expected error removing unknown agent")
	}
}

func TestRemoveAgentPurgeWipesHomeAndLog(t *testing.T) {
	setupTestHome(t)

	if _, err := Init("scratch", InitOptions{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	p, err := instancePaths("scratch")
	if err != nil {
		t.Fatalf("instance paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.logFile), 0o755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.WriteFile(p.logFile, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if _, err := Remove("scratch", RemoveOptions{Purge: true}); err != nil {
		t.Fatalf("remove --purge: %v", err)
	}
	if _, err := os.Stat(p.homeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("home not removed: %v", err)
	}
	if _, err := os.Stat(p.logFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log not removed: %v", err)
	}
}

func TestRemoveAgentRefusesRunningWithoutForce(t *testing.T) {
	root := setupTestHome(t)
	p, err := instancePaths("scratch")
	if err != nil {
		t.Fatalf("instance paths: %v", err)
	}
	if err := saveInstance(p.metaFile, &Instance{
		Name: "scratch",
		PID:  os.Getpid(),
		Home: filepath.Join(root, "local-agents", "scratch"),
	}); err != nil {
		t.Fatalf("save instance: %v", err)
	}

	_, err = Remove("scratch", RemoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("expected refusal for running agent, got %v", err)
	}
	// Metadata must remain so the user can still stop the agent.
	if _, err := os.Stat(p.metaFile); err != nil {
		t.Fatalf("metadata vanished on refused remove: %v", err)
	}
}

func TestInitRefusesUserSwitchOnExistingAgent(t *testing.T) {
	setupTestHome(t)

	if _, err := Init("scratch", InitOptions{Username: "alice"}); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	// Manually create a second user. Init resolves --username to that user
	// and would re-save the agent record under a new owner if we let it.
	st, _, err := storeForName("scratch")
	if err != nil {
		t.Fatalf("storeForName: %v", err)
	}
	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	bob, err := accts.Create(context.Background(), "bob", "bob@local", "secret-bob", "Bob", users.RoleUser)
	if err != nil {
		st.Close()
		t.Fatalf("create bob: %v", err)
	}
	st.Close()

	_, err = Init("scratch", InitOptions{Username: "bob"})
	if err == nil || !strings.Contains(err.Error(), "is owned by user") {
		t.Fatalf("expected refusal to rebind, got %v", err)
	}

	// Without --username the existing owner must be preserved (not
	// silently rebound to whichever super-admin we land on).
	res, err := Init("scratch", InitOptions{})
	if err != nil {
		t.Fatalf("re-init without username: %v", err)
	}
	if res.Instance.UserID == bob.ID {
		t.Fatalf("re-init silently rebound owner to bob")
	}
}

func TestSetProviderModelRejectsEmptyValue(t *testing.T) {
	setupTestHome(t)
	t.Setenv("OPENAI_API_KEY", "test-key")

	if _, err := Init("scratch", InitOptions{
		Provider:  "openai",
		Model:     "openai/gpt-4.1",
		APIKeyEnv: "OPENAI_API_KEY",
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	if err := SetConfig("scratch", "provider.openai.model", ""); err == nil {
		t.Fatal("empty model id must be rejected, not silently wipe the list")
	}
	if got := loadProviderModelCount(t, "scratch", "openai"); got == 0 {
		t.Fatal("models list was wiped despite the rejection")
	}

	// Explicit clear is allowed via the plural key with [].
	if err := SetConfig("scratch", "provider.openai.models", "[]"); err != nil {
		t.Fatalf("explicit clear: %v", err)
	}
	if got := loadProviderModelCount(t, "scratch", "openai"); got != 0 {
		t.Fatalf("expected models cleared, still have %d", got)
	}
}

func loadProviderModelCount(t *testing.T, name, provider string) int {
	t.Helper()
	st, _, err := storeForName(name)
	if err != nil {
		t.Fatalf("storeForName: %v", err)
	}
	defer st.Close()
	rec, err := st.GetConfigByName(context.Background(), store.KindProvider, scope.System, "", provider)
	if err != nil {
		t.Fatalf("get provider %q: %v", provider, err)
	}
	models, _ := rec.Data["models"].([]interface{})
	return len(models)
}

func TestCorruptMetadataIsNotOverwritten(t *testing.T) {
	setupTestHome(t)
	p, err := instancePaths("scratch")
	if err != nil {
		t.Fatalf("instance paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.metaFile), 0o755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.WriteFile(p.metaFile, []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	if _, err := Init("scratch", InitOptions{}); err == nil {
		t.Fatal("expected init to reject corrupt metadata")
	}
	if _, err := Start("scratch", StartOptions{}); err == nil {
		t.Fatal("expected start to reject corrupt metadata")
	}
}
