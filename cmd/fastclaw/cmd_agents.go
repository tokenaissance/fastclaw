package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agentcli"
	"github.com/fastclaw-ai/fastclaw/internal/daemon"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// agentsCmd is a thin CLI front-end for the same agent CRUD the
// dashboard performs over HTTP. Every subcommand opens the operator's
// own store via openStoreFromEnv (defined in cmd_admin.go) and writes
// into the same tables the gateway reads. There is no separate
// "instance" concept — agents created here show up in the dashboard.
func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Create and manage agents from the command line",
	}
	addAgentsSubcommand(cmd, agentsListCmd())
	addAgentsSubcommand(cmd, agentsInitCmd())
	addAgentsSubcommand(cmd, agentsRemoveCmd())
	addAgentsSubcommand(cmd, agentsConfigCmd())
	addAgentsSubcommand(cmd, agentsFilesCmd())
	return cmd
}

// addAgentsSubcommand wires a child command and silences cobra's usage
// dump on every error throughout the agents tree.
func addAgentsSubcommand(parent, child *cobra.Command) {
	silenceTree(child)
	parent.AddCommand(child)
}

func silenceTree(cmd *cobra.Command) {
	cmd.SilenceUsage = true
	for _, sub := range cmd.Commands() {
		silenceTree(sub)
	}
}

// noteGatewayRunning prints a hint when the operator's gateway is
// currently running. CLI writes do not hot-reload the gateway; the
// operator needs to restart it to pick up changes.
func noteGatewayRunning() {
	st, err := daemon.GetStatus()
	if err == nil && st != nil && st.Running {
		fmt.Fprintf(os.Stderr, "Note: gateway is running (PID %d). Restart it with `fastclaw daemon restart` for changes to take effect.\n", st.PID)
	}
}

func agentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List agents in the operator's store",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			agents, err := agentcli.List(context.Background(), st)
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Println("No agents.")
				return nil
			}
			fmt.Printf("%-30s %-22s %s\n", "NAME", "ID", "OWNER")
			for _, ag := range agents {
				fmt.Printf("%-30s %-22s %s\n", ag.Name, ag.ID, ag.UserID)
			}
			return nil
		},
	}
}

func agentsInitCmd() *cobra.Command {
	var opts agentcli.InitOptions
	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Create or update an agent in the operator's store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			res, err := agentcli.Init(ctx, st, args[0], opts)
			if err != nil {
				return err
			}
			verb := "updated"
			if res.Created {
				verb = "created"
			}
			fmt.Printf("Agent %q %s\n", res.Agent.Name, verb)
			fmt.Printf("Agent ID: %s\n", res.Agent.ID)
			fmt.Printf("Owner:    %s\n", res.Agent.UserID)
			if res.ProviderSaved {
				fmt.Println("Provider: saved")
			}
			if res.ModelSaved {
				fmt.Println("Model:    saved (agent scope)")
			}
			if !res.ModelSaved {
				model, _ := agentcli.GetConfig(ctx, st, res.Agent.ID, "model")
				if model == nil || model == "" {
					fmt.Fprintln(os.Stderr, "Hint: no model is configured for this agent. Set one with:")
					fmt.Fprintf(os.Stderr, "  fastclaw agents config %s set model <provider>/<model>\n", res.Agent.Name)
				}
			}
			if res.OwnerCreated && res.GeneratedPassword != "" {
				fmt.Printf("Generated admin password: %s\n", res.GeneratedPassword)
			}
			noteGatewayRunning()
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.AgentID, "id", "", "agent id (default: auto-generated; pass an existing agt_ id to update an agent created via the dashboard)")
	cmd.Flags().StringVar(&opts.Description, "description", "", "description for the agent")
	cmd.Flags().StringVar(&opts.Provider, "provider", "", "provider name, e.g. openai, openrouter, anthropic, ollama")
	cmd.Flags().StringVar(&opts.Model, "model", "", "default model, either <provider>/<model> or <model> with --provider")
	cmd.Flags().StringVar(&opts.APIKeyEnv, "api-key-env", "", "environment variable containing the provider API key")
	cmd.Flags().StringVar(&opts.APIBase, "api-base", "", "provider API base URL")
	cmd.Flags().StringVar(&opts.APIType, "api-type", "", "provider API type (default from provider preset)")
	cmd.Flags().StringVar(&opts.AuthType, "auth-type", "", "provider auth type (default from provider preset)")
	cmd.Flags().StringVar(&opts.Username, "username", "", "owner username (default: existing agent owner, or first super_admin)")
	cmd.Flags().StringVar(&opts.Email, "email", "", "admin email when the local DB has no users")
	cmd.Flags().StringVar(&opts.Password, "password", "", "admin password when the local DB has no users (default: generate)")
	cmd.Flags().StringVar(&opts.DisplayName, "display-name", "", "admin display name")
	return cmd
}

func agentsRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove an agent from the operator's store",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			rec, err := agentcli.Remove(context.Background(), st, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q (%s) removed\n", rec.Name, rec.ID)
			noteGatewayRunning()
			return nil
		},
	}
	return cmd
}

func agentsConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config <name> <get|set> [key] [value]",
		Short: "Read or update an agent's configuration",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			rec, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			switch args[1] {
			case "get":
				if len(args) > 3 {
					return fmt.Errorf("usage: fastclaw agents config %s get [key]", args[0])
				}
				key := ""
				if len(args) == 3 {
					key = args[2]
				}
				value, err := agentcli.GetConfig(ctx, st, rec.ID, key)
				if err != nil {
					return err
				}
				return printValue(value)
			case "set":
				if len(args) != 4 {
					return fmt.Errorf("usage: fastclaw agents config %s set <key> <value>", args[0])
				}
				if err := agentcli.SetConfig(ctx, st, rec.ID, args[2], args[3]); err != nil {
					return err
				}
				fmt.Printf("Set %s\n", args[2])
				noteGatewayRunning()
				return nil
			default:
				return fmt.Errorf("unknown config action %q; use get or set", args[1])
			}
		},
	}
}

func agentsFilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "Manage agent system files (SOUL.md, IDENTITY.md, …)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "ls <name>",
		Aliases: []string{"list"},
		Short:   "List system files saved for an agent",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			rec, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			files, err := agentcli.ListFiles(ctx, st, rec.ID, rec.UserID)
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Println(f)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "put <name> <filename> <path>",
		Short: "Write a system file from a local path",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			rec, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			if err := agentcli.PutFile(ctx, st, rec.ID, rec.UserID, args[1], data); err != nil {
				return err
			}
			fmt.Printf("Wrote %s\n", args[1])
			noteGatewayRunning()
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get <name> <filename> [path]",
		Short: "Read a system file, or write it to a local path",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			rec, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			data, err := agentcli.GetFile(ctx, st, rec.ID, rec.UserID, args[1])
			if err != nil {
				return err
			}
			if len(args) == 3 {
				if err := os.WriteFile(args[2], data, 0o600); err != nil {
					return err
				}
				fmt.Printf("Wrote %s\n", args[2])
				return nil
			}
			_, err = os.Stdout.Write(data)
			return err
		},
	})
	return cmd
}

func printValue(value interface{}) error {
	switch v := value.(type) {
	case nil:
		fmt.Println("null")
	case string:
		fmt.Println(v)
	default:
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
	}
	return nil
}

// Compile-time check that the imported store package is referenced.
// (We only use store.Store via openStoreFromEnv's return type, but the
// type appears in agentcli's signatures.)
var _ = func() store.Store { return nil }
