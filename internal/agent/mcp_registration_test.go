package agent

import (
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/mcp/oauth"
)

func registryHasTool(t *testing.T, reg *tools.Registry, name string) bool {
	t.Helper()
	for _, def := range reg.Definitions() {
		if def.Function.Name == name {
			return true
		}
	}
	return false
}

// TestMcpManagementToolRegisteredEvenWithZeroServers pins the registration
// surface that used to live only in a code comment: the `mcp` management
// tool must be registered whenever MCP OAuth is enabled — even for an
// agent with zero configured MCP servers — so the model can `mcp add`
// the first server. With OAuth disabled it must not be registered.
func TestMcpManagementToolRegisteredEvenWithZeroServers(t *testing.T) {
	rc := config.ResolvedAgent{ID: "agent-1", UserID: "owner-1"} // MCPServers empty

	ag := &Agent{registry: tools.NewRegistry(t.TempDir(), t.TempDir())}
	ag.registerMCPManagementTool(rc, &oauth.Bootstrap{})
	if !registryHasTool(t, ag.registry, "mcp") {
		t.Fatal("mcp tool not registered when OAuth is enabled with zero configured servers")
	}

	agOff := &Agent{registry: tools.NewRegistry(t.TempDir(), t.TempDir())}
	agOff.registerMCPManagementTool(rc, nil)
	if registryHasTool(t, agOff.registry, "mcp") {
		t.Fatal("mcp tool registered even though MCP OAuth is disabled")
	}
}

// TestMcpManagementToolAgentModeOnly guards the per-mode visibility half of
// the registration surface: agent mode exposes every built-in (nil filter),
// while chatbot/customize allowlists must not surface the `mcp` manager.
func TestMcpManagementToolAgentModeOnly(t *testing.T) {
	if got := builtinAllowForMode(config.PromptModeAgent); got != nil {
		t.Fatalf("agent mode allowlist = %v, want nil (all built-ins)", got)
	}
	if got := builtinAllowForMode(config.PromptModeCustomize); len(got) != 0 {
		t.Fatalf("customize mode allowlist = %v, want empty", got)
	}
	for _, name := range builtinAllowForMode(config.PromptModeChatbot) {
		if name == "mcp" {
			t.Fatal("chatbot mode allowlist must not include the mcp management tool")
		}
	}
}
