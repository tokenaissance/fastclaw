package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/daemon"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
	"github.com/fastclaw-ai/fastclaw/internal/setup"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// apiResolver adapts *gateway.Gateway to api.UserResolver. The bridge lives
// here (in main) so the api package doesn't need to import gateway.
type apiResolver struct {
	gw *gateway.Gateway
}

func (a *apiResolver) UserSpaceFor(userID string) (*api.UserSpaceView, error) {
	sp, err := a.gw.UserSpaceFor(userID)
	if err != nil {
		return nil, err
	}
	return &api.UserSpaceView{
		UserID: sp.UserID,
		Agents: sp.Agents,
		Config: sp.Config,
	}, nil
}

func (a *apiResolver) LocalAgentManager() *agent.Manager { return a.gw.AgentManager() }
func (a *apiResolver) IsCloudMode() bool                 { return a.gw.IsCloudMode() }

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
	rootCmd.AddCommand(providerCmd())
	rootCmd.AddCommand(sandboxCmd())
	rootCmd.AddCommand(policyCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(userCmd())

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

	// Check if config exists. When FASTCLAW_* env vars are set (typical
	// K8s/container deploy), a missing fastclaw.json is fine — env
	// provides the infra fields and the UI bootstraps product config.
	// Absent both env and file, fall back to the setup wizard for
	// interactive local onboarding.
	cfg, err := config.Load()
	if err != nil {
		if hasInfraEnv() {
			slog.Info("no fastclaw.json found, bootstrapping from env")
			cfg = &config.Config{}
		} else {
			slog.Info("no config found, starting setup wizard", "url", fmt.Sprintf("http://localhost:%d", port))
			return runSetupWizard(port)
		}
	}

	// Env vars win over JSON for infra fields — see ApplyToConfig. This
	// is what makes FASTCLAW_STORAGE_DSN / FASTCLAW_OBJECT_STORE_* / etc.
	// actually take effect at runtime.
	config.LoadEnv().ApplyToConfig(cfg)

	slog.Info("starting gateway")

	// Install bundled skills (if not already present)
	agent.InstallBundledSkills()

	// Write PID file for daemon management
	if err := daemon.WritePIDFile(); err != nil {
		slog.Warn("failed to write PID file", "error", err)
	}
	defer daemon.RemovePIDFile()

	gw, err := gateway.New(cfg)
	if err != nil {
		return fmt.Errorf("create gateway: %w", err)
	}

	// Start web UI server alongside gateway
	gwCfg := &cfg.Gateway
	if gwCfg.Port > 0 {
		port = gwCfg.Port
	}

	webSrv := setup.NewServer(port, nil)
	webSrv.SetAgentProvider(&agentProviderAdapter{mgr: gw.AgentManager(), gw: gw})
	webSrv.SetTaskQueue(gw.TaskQueue())
	webSrv.SetGatewayConfig(gwCfg)
	webSrv.SetUserResolver(&apiResolver{gw: gw})
	webSrv.SetStore(gw.Store())
	webSrv.SetWorkspaceStore(gw.Workspace())
	webSrv.SetUsageMeter(gw.Usage())

	// Set up OpenAI-compatible API and WebSocket gateway.
	// In cloud mode the user registry maps bearer tokens to user IDs; in
	// local mode it's absent and everything resolves to DefaultUserID.
	gatewayToken := cfg.Gateway.Auth.Token
	var userReg *users.Registry
	if cfg.Gateway.Mode == "cloud" {
		var regErr error
		userReg, regErr = users.Load()
		if regErr != nil {
			slog.Warn("failed to load user registry", "error", regErr)
		} else {
			slog.Info("cloud mode enabled", "users", userReg.Count())
		}
	}
	apiSrv := api.NewServer(&apiResolver{gw: gw}, gatewayToken, userReg, gwCfg)
	webSrv.SetAPIServer(apiSrv)
	webSrv.SetAuth(gatewayToken, userReg)

	// Agent ↔ API key bindings. Always loaded — file is empty {} by default
	// so local mode just treats every agent as admin-owned.
	if bindings, err := users.LoadBindings(); err == nil {
		webSrv.SetAgentBindings(bindings)
	} else {
		slog.Warn("failed to load agent bindings", "error", err)
	}

	bindMode := gwCfg.Bind
	if bindMode == "" {
		bindMode = "loopback"
	}
	authMode := gwCfg.Auth.Mode
	if authMode == "" {
		authMode = "token"
	}
	slog.Info("gateway API enabled",
		"port", port,
		"bind", bindMode,
		"auth", authMode,
		"chatCompletions", gwCfg.HTTP.Endpoints.ChatCompletions.Enabled,
	)

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
	gw  *gateway.Gateway
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

func (a *agentProviderAdapter) ReloadAgents() error {
	return a.gw.ReloadAgents()
}

func runSetupWizard(port int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := setup.NewServer(port, func(cfg *config.Config) {
		slog.Info("setup complete, config saved")
		// Stop the setup wizard and restart as gateway
		go func() {
			cancel()
		}()
	})

	// Open browser
	url := fmt.Sprintf("http://localhost:%d", port)
	go openBrowser(url)

	if err := srv.Run(ctx); err != nil {
		return err
	}

	// Config was saved, now start the gateway
	slog.Info("restarting as gateway")
	return runGateway(port)
}

// hasInfraEnv reports whether the environment carries enough infra config
// to run without a fastclaw.json. Used by runGateway to skip the setup
// wizard in container/K8s deployments where JSON doesn't exist but env is
// comprehensive.
//
// The gate is loose on purpose: one of these vars being set strongly
// implies "this is a container deploy, don't prompt the user". Missing
// ones (e.g. token when mode=local) are fine because ApplyToConfig still
// populates them from defaults.
func hasInfraEnv() bool {
	for _, k := range []string{
		"FASTCLAW_MODE",
		"FASTCLAW_AUTH_TOKEN",
		"FASTCLAW_STORAGE_TYPE",
		"FASTCLAW_STORAGE_DSN",
		"FASTCLAW_OBJECT_STORE_TYPE",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
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

