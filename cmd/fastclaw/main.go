package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "fastclaw",
		Short: "FastClaw - Lightweight AI Agent Framework",
	}

	gatewayCmd := &cobra.Command{
		Use:   "gateway",
		Short: "Start the FastClaw gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			})))

			slog.Info("loading config")
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			slog.Info("starting gateway",
				"model", cfg.Agents.Defaults.Model,
				"workspace", cfg.Agents.Defaults.Workspace,
			)

			gw, err := gateway.New(cfg)
			if err != nil {
				return fmt.Errorf("create gateway: %w", err)
			}

			return gw.Run()
		},
	}

	rootCmd.AddCommand(gatewayCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
