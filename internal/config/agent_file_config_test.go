package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAgentFileConfigLoaderIgnoresRetiredAgentJSON locks the agent.json
// retirement: even when a legacy agent.json exists on disk, the default
// layer-3 loader must not read it — agent config is DB-only, wired by the
// composition root (gateway DB-first loader).
func TestAgentFileConfigLoaderIgnoresRetiredAgentJSON(t *testing.T) {
	home := t.TempDir()
	legacy := `{"model":"openai/gpt-4o-mini","mcpServers":{"quandora":{"type":"http","url":"https://mcp.quandora.ai/quant"}}}`
	if err := os.WriteFile(filepath.Join(home, "agent.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy agent.json: %v", err)
	}

	cfg, ok := AgentFileConfigLoader("agent-1", home)
	if ok {
		t.Fatalf("default loader read retired agent.json: cfg=%+v", cfg)
	}
	if cfg.Model != "" || len(cfg.MCPServers) != 0 {
		t.Fatalf("default loader leaked agent.json content: %+v", cfg)
	}
}
