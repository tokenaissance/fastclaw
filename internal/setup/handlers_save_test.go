package setup

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// TestSaveUserConfig_PreservesInfra locks down the split between
// product-domain config (written by UI) and infra-domain config (sourced
// from env / deployment). A user going through the admin UI and saving
// providers must NOT overwrite storage / objectStore / gateway.auth /
// sandbox — those belong to ops.
func TestSaveUserConfig_PreservesInfra(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	configPath := filepath.Join(tmp, ".fastclaw", "fastclaw.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed an initial file that represents what a deployer set up —
	// Postgres + OSS + admin token already configured.
	initial := config.Config{
		Storage: config.StorageCfg{Type: "postgres", DSN: "postgres://ops@db/fc"},
		ObjectStore: config.ObjectStoreCfg{
			Type:  "aliyun-oss",
			S3:    config.ObjectStoreS3Cfg{Region: "cn-hangzhou", Bucket: "prod-bucket"},
		},
		Gateway: config.GatewayCfg{Auth: config.GatewayAuth{Token: "secret-admin-token"}},
	}
	data, _ := json.MarshalIndent(&initial, "", "  ")
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a UI save: admin edits something product-ish (a tool
	// provider key) and also *tries* to clobber infra (file-backed
	// storage, their own token). Infra must be rejected.
	incoming := &config.Config{
		Storage:     config.StorageCfg{Type: "file"},                                    // hostile
		ObjectStore: config.ObjectStoreCfg{Type: "local"},                               // hostile
		Gateway:     config.GatewayCfg{Auth: config.GatewayAuth{Token: "hijack"}},       // hostile
		ToolProviders: map[string]config.ToolProviderCfg{
			"openai": {APIKey: "sk-new-product-key"},
		},
	}

	s := &Server{}
	req := httptest.NewRequest("POST", "/api/config", nil)
	if err := s.saveUserConfig(req, incoming); err != nil {
		t.Fatalf("saveUserConfig: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}

	// Product field made it through.
	if got.ToolProviders["openai"].APIKey != "sk-new-product-key" {
		t.Errorf("expected product-domain toolProviders to be saved; got %+v", got.ToolProviders)
	}

	// Infra fields are untouched — this is the invariant.
	if got.Storage.Type != "postgres" || got.Storage.DSN == "" {
		t.Errorf("storage was clobbered: %+v", got.Storage)
	}
	if got.ObjectStore.Type != "aliyun-oss" || got.ObjectStore.S3.Bucket != "prod-bucket" {
		t.Errorf("objectStore was clobbered: %+v", got.ObjectStore)
	}
	if got.Gateway.Auth.Token != "secret-admin-token" {
		t.Errorf("admin token was clobbered: %q", got.Gateway.Auth.Token)
	}
}
