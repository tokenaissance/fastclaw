package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
	"github.com/fastclaw-ai/fastclaw/internal/setup"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "fastclaw",
		Short: "FastClaw - Lightweight AI Agent Framework",
		// No args = default to gateway (so double-click on Windows works)
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGateway(18953)
		},
	}

	rootCmd.AddCommand(gatewayCmd())
	rootCmd.AddCommand(agentCmd())
	rootCmd.AddCommand(skillCmd())
	rootCmd.AddCommand(sessionCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(upgradeCmd())
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(backupCmd())
	rootCmd.AddCommand(resetCmd())
	rootCmd.AddCommand(pluginCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func gatewayCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Start the FastClaw gateway (loads all agents)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGateway(port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 18953, "port for setup wizard / web UI")
	return cmd
}

func runGateway(port int) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Check if config exists
	cfg, err := config.Load()
	if err != nil {
		// Config doesn't exist — run setup wizard
		slog.Info("no config found, starting setup wizard", "url", fmt.Sprintf("http://localhost:%d", port))
		return runSetupWizard(port)
	}

	slog.Info("starting gateway")

	gw, err := gateway.New(cfg)
	if err != nil {
		return fmt.Errorf("create gateway: %w", err)
	}

	// Start web UI server alongside gateway
	webSrv := setup.NewServer(port, nil)
	webSrv.SetAgentProvider(&agentProviderAdapter{mgr: gw.AgentManager()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := webSrv.Run(ctx); err != nil {
			slog.Error("web server error", "error", err)
		}
	}()

	slog.Info("web UI available", "url", fmt.Sprintf("http://localhost:%d", port))

	return gw.Run()
}

// agentProviderAdapter adapts agent.Manager to setup.AgentProvider.
type agentProviderAdapter struct {
	mgr *agent.Manager
}

func (a *agentProviderAdapter) AllAgents() []setup.AgentHandle {
	agents := a.mgr.All()
	result := make([]setup.AgentHandle, len(agents))
	for i, ag := range agents {
		result[i] = ag
	}
	return result
}

func (a *agentProviderAdapter) AgentByID(id string) setup.AgentHandle {
	ag := a.mgr.AgentByID(id)
	if ag == nil {
		return nil
	}
	return ag
}

func runSetupWizard(port int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := setup.NewServer(port, func(cfg *config.Config) {
		slog.Info("setup complete, config saved")
		cancel()
	})

	// Open browser
	url := fmt.Sprintf("http://localhost:%d", port)
	go openBrowser(url)

	return srv.Run(ctx)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	cmd.Run()
}

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents",
	}

	cmd.AddCommand(agentCreateCmd())
	cmd.AddCommand(agentListCmd())

	return cmd
}

func agentCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new agent from template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentDir := filepath.Join(homeDir, "agents", name, "agent")

			if _, err := os.Stat(agentDir); err == nil {
				return fmt.Errorf("agent %q already exists at %s", name, agentDir)
			}

			dirs := []string{
				agentDir,
				filepath.Join(agentDir, "memory"),
				filepath.Join(agentDir, "sessions"),
				filepath.Join(agentDir, "skills"),
			}
			for _, dir := range dirs {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("create directory %s: %w", dir, err)
				}
			}

			agentCfg := config.AgentFileConfig{
				Model: "gpt-4o",
			}
			data, _ := json.MarshalIndent(agentCfg, "", "  ")
			if err := os.WriteFile(filepath.Join(agentDir, "agent.json"), data, 0o644); err != nil {
				return err
			}

			bootstrapFiles := map[string]string{
				"AGENTS.md":    "# Agent Capabilities\n\nDescribe what this agent can do.",
				"IDENTITY.md":  fmt.Sprintf("# Identity\n\nYou are %s, a FastClaw AI agent.", name),
				"SOUL.md":      "# Soul\n\nYour personality and behavioral guidelines.",
				"USER.md":      "# User\n\nInformation about the user you serve.",
				"TOOLS.md":     "# Tools\n\nAdditional tool usage instructions.",
				"BOOTSTRAP.md": "# Bootstrap\n\nStartup instructions loaded on every conversation.",
				"HEARTBEAT.md": "# Heartbeat\n\nPeriodic check-in instructions.",
				"MEMORY.md":    "# Memory\n\nLong-term memory for this agent.",
			}
			for filename, content := range bootstrapFiles {
				path := filepath.Join(agentDir, filename)
				if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
					return err
				}
			}

			fmt.Printf("Agent %q created at %s\n", name, agentDir)
			return nil
		},
	}
}

func agentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentsDir := filepath.Join(homeDir, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No agents found. Create one with: fastclaw agent create <name>")
					return nil
				}
				return err
			}

			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				name := entry.Name()
				workspace := filepath.Join(agentsDir, name, "agent")

				agentJSON := filepath.Join(workspace, "agent.json")
				model := "(default)"
				if data, err := os.ReadFile(agentJSON); err == nil {
					var fc config.AgentFileConfig
					if json.Unmarshal(data, &fc) == nil && fc.Model != "" {
						model = fc.Model
					}
				}

				fmt.Printf("  %s  model=%s  workspace=%s\n", name, model, workspace)
			}

			return nil
		},
	}
}
