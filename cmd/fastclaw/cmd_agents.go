package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/localagents"
)

func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "Manage local FastClaw agent instances",
	}
	addAgentsSubcommand(cmd, agentsListCmd())
	addAgentsSubcommand(cmd, agentsInitCmd())
	addAgentsSubcommand(cmd, agentsStartCmd())
	addAgentsSubcommand(cmd, agentsStopCmd())
	addAgentsSubcommand(cmd, agentsRestartCmd())
	addAgentsSubcommand(cmd, agentsRemoveCmd())
	addAgentsSubcommand(cmd, agentsStatusCmd())
	addAgentsSubcommand(cmd, agentsLogCmd())
	addAgentsSubcommand(cmd, agentsConfigCmd())
	addAgentsSubcommand(cmd, agentsFilesCmd())
	return cmd
}

// addAgentsSubcommand attaches a subcommand and silences cobra's usage dump
// on every error throughout the agents tree (we keep error printing on).
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

func agentsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List local agent instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := localagents.List()
			if err != nil {
				return err
			}
			if len(agents) == 0 {
				fmt.Println("No local agents.")
				return nil
			}
			fmt.Printf("%-20s %-8s %-8s %-7s %-12s %s\n", "NAME", "STATUS", "PID", "PORT", "UPTIME", "HOME")
			for _, ag := range agents {
				status := "stopped"
				pid := "-"
				uptime := "-"
				if ag.Running {
					status = "running"
					pid = strconv.Itoa(ag.PID)
					uptime = ag.Uptime.Round(time.Second).String()
				}
				port := "-"
				if ag.Port > 0 {
					port = strconv.Itoa(ag.Port)
				}
				fmt.Printf("%-20s %-8s %-8s %-7s %-12s %s\n", ag.Name, status, pid, port, uptime, ag.Home)
			}
			return nil
		},
	}
}

func agentsInitCmd() *cobra.Command {
	var opts localagents.InitOptions
	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Configure a local agent instance without using the web UI",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := localagents.Init(args[0], opts)
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q initialized\n", res.Instance.Name)
			fmt.Printf("Agent ID: %s\n", res.Instance.AgentID)
			fmt.Printf("User ID:  %s\n", res.Instance.UserID)
			fmt.Printf("Home:     %s\n", res.Instance.Home)
			if res.Instance.Port > 0 {
				fmt.Printf("Port:     %d\n", res.Instance.Port)
			}
			if res.ProviderSaved {
				fmt.Println("Provider: saved")
			}
			if res.ModelSaved {
				fmt.Println("Model:    saved")
			}
			if !res.ModelSaved {
				if model, _ := localagents.GetConfig(args[0], "model"); model == nil || model == "" {
					fmt.Fprintln(os.Stderr, "Hint: no model is configured. Set one with:")
					fmt.Fprintf(os.Stderr, "  fastclaw agents config %s set model <provider>/<model>\n", args[0])
				}
			}
			if res.CreatedUser && res.GeneratedPassword != "" {
				fmt.Printf("Generated admin password: %s\n", res.GeneratedPassword)
			}
			warnIfAgentRunning(res.Instance.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Home, "home", "", "FASTCLAW_HOME for this agent (default: ~/.fastclaw/local-agents/<name>)")
	cmd.Flags().IntVar(&opts.Port, "port", 0, "default port to use when starting this agent")
	cmd.Flags().StringVar(&opts.AgentName, "agent-name", "", "display name for the created agent")
	cmd.Flags().StringVar(&opts.Description, "description", "", "description for the created agent")
	cmd.Flags().StringVar(&opts.Provider, "provider", "", "provider name, e.g. openai, openrouter, anthropic, ollama")
	cmd.Flags().StringVar(&opts.Model, "model", "", "default model, either <provider>/<model> or <model> with --provider")
	cmd.Flags().StringVar(&opts.APIKeyEnv, "api-key-env", "", "environment variable containing the provider API key")
	cmd.Flags().StringVar(&opts.APIBase, "api-base", "", "provider API base URL")
	cmd.Flags().StringVar(&opts.APIType, "api-type", "", "provider API type (default from provider preset)")
	cmd.Flags().StringVar(&opts.AuthType, "auth-type", "", "provider auth type (default from provider preset)")
	cmd.Flags().StringVar(&opts.Username, "username", "", "admin username to create when the local DB has no users")
	cmd.Flags().StringVar(&opts.Email, "email", "", "admin email to create when the local DB has no users")
	cmd.Flags().StringVar(&opts.Password, "password", "", "admin password to create when the local DB has no users (default: generate)")
	cmd.Flags().StringVar(&opts.DisplayName, "display-name", "", "admin display name")
	cmd.Flags().BoolVar(&opts.SandboxEnabled, "sandbox", false, "enable sandbox for the local instance")
	cmd.Flags().StringVar(&opts.SandboxBackend, "sandbox-backend", "", "sandbox backend, e.g. docker or e2b")
	cmd.Flags().StringVar(&opts.SandboxImage, "sandbox-image", "", "sandbox image/template")
	cmd.Flags().StringVar(&opts.SandboxNetwork, "sandbox-network", "", "sandbox network mode")
	return cmd
}

func agentsStartCmd() *cobra.Command {
	var port int
	var home string
	cmd := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a local agent instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := localagents.Start(args[0], localagents.StartOptions{
				Port: port,
				Home: home,
			})
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q started (PID %d)\n", inst.Name, inst.PID)
			fmt.Printf("URL:  %s\n", inst.URL)
			fmt.Printf("Home: %s\n", inst.Home)
			fmt.Printf("Logs: %s\n", inst.LogFile)
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "port for this agent gateway (default: choose a free port)")
	cmd.Flags().StringVar(&home, "home", "", "FASTCLAW_HOME for this agent (default: ~/.fastclaw/local-agents/<name>)")
	return cmd
}

func agentsStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a local agent instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := localagents.Stop(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q stopped\n", inst.Name)
			return nil
		},
	}
}

func agentsRestartCmd() *cobra.Command {
	var port int
	var home string
	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Stop and start a local agent instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			st, err := localagents.GetStatus(name)
			if err != nil {
				return err
			}
			if st.Running {
				if _, err := localagents.Stop(name); err != nil {
					return err
				}
			}
			inst, err := localagents.Start(name, localagents.StartOptions{Port: port, Home: home})
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q restarted (PID %d)\n", inst.Name, inst.PID)
			fmt.Printf("URL:  %s\n", inst.URL)
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "override port on restart (default: reuse previous)")
	cmd.Flags().StringVar(&home, "home", "", "override FASTCLAW_HOME on restart (default: reuse previous)")
	return cmd
}

func agentsRemoveCmd() *cobra.Command {
	var force, purge bool
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a local agent instance",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := localagents.Remove(args[0], localagents.RemoveOptions{
				Force: force,
				Purge: purge,
			})
			if err != nil {
				return err
			}
			fmt.Printf("Agent %q removed\n", inst.Name)
			if !purge && inst.Home != "" {
				fmt.Printf("Home preserved: %s\n", inst.Home)
				fmt.Printf("Log preserved:  %s\n", inst.LogFile)
				fmt.Println("Pass --purge to delete them too.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the agent first if it is still running")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the agent's home directory and log file")
	return cmd
}

func agentsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Show status for a single local agent instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := localagents.GetStatus(args[0])
			if err != nil {
				return err
			}
			running := "stopped"
			if st.Running {
				running = "running"
			}
			fmt.Printf("Name:    %s\n", st.Name)
			fmt.Printf("Status:  %s\n", running)
			if st.AgentID != "" {
				fmt.Printf("AgentID: %s\n", st.AgentID)
			}
			if st.UserID != "" {
				fmt.Printf("UserID:  %s\n", st.UserID)
			}
			if st.PID > 0 {
				fmt.Printf("PID:     %d\n", st.PID)
			}
			if st.Port > 0 {
				fmt.Printf("Port:    %d\n", st.Port)
			}
			if st.URL != "" {
				fmt.Printf("URL:     %s\n", st.URL)
			}
			if st.Home != "" {
				fmt.Printf("Home:    %s\n", st.Home)
			}
			if st.LogFile != "" {
				fmt.Printf("Log:     %s\n", st.LogFile)
			}
			if st.Running && !st.StartedAt.IsZero() {
				fmt.Printf("Uptime:  %s\n", st.Uptime.Round(time.Second))
			}
			return nil
		},
	}
}

func agentsConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config <name> <get|set> [key] [value]",
		Short: "Read or update a local agent instance config",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			switch args[1] {
			case "get":
				if len(args) > 3 {
					return fmt.Errorf("usage: fastclaw agents config %s get [key]", name)
				}
				key := ""
				if len(args) == 3 {
					key = args[2]
				}
				value, err := localagents.GetConfig(name, key)
				if err != nil {
					return err
				}
				return printValue(value)
			case "set":
				if len(args) != 4 {
					return fmt.Errorf("usage: fastclaw agents config %s set <key> <value>", name)
				}
				if err := localagents.SetConfig(name, args[2], args[3]); err != nil {
					return err
				}
				fmt.Printf("Set %s\n", args[2])
				warnIfAgentRunning(name)
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
		Short: "Manage local agent system files",
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "ls <name>",
		Aliases: []string{"list"},
		Short:   "List configured system files",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := localagents.ListFiles(args[0])
			if err != nil {
				return err
			}
			for _, file := range files {
				fmt.Println(file)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "put <name> <filename> <path>",
		Short: "Write a system file from a local path",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := localagents.PutFile(args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("Wrote %s\n", args[1])
			warnIfAgentRunning(args[0])
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get <name> <filename> [path]",
		Short: "Read a system file, or write it to a local path",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := localagents.GetFile(args[0], args[1])
			if err != nil {
				return err
			}
			if len(args) == 3 {
				if err := os.WriteFile(args[2], data, 0o644); err != nil {
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

func agentsLogCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:     "log <name>",
		Aliases: []string{"logs"},
		Short:   "Show local agent log output",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logFile, err := localagents.LogFile(args[0])
			if err != nil {
				return err
			}
			return tailLog(logFile, lines, follow, os.Stdout)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of lines to show")
	return cmd
}

// tailLog mirrors `tail -n LINES [-f] PATH` without depending on a system
// `tail` binary, so the command works on Windows too.
func tailLog(path string, lines int, follow bool, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no log file found at %s", path)
		}
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	start, err := lastNLineOffset(f, fi.Size(), lines)
	if err != nil {
		return err
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(out, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	pos, _ := f.Seek(0, io.SeekCurrent)
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-sigCh:
			return nil
		case <-ticker.C:
		}
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Size() < pos {
			// Log was truncated/rotated. Reopen and resume from the start.
			f.Close()
			f, err = os.Open(path)
			if err != nil {
				return err
			}
			pos = 0
		}
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					return werr
				}
				pos += int64(n)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
	}
}

// lastNLineOffset returns the byte offset where the last `n` lines of the
// file begin. Reads backward in 8KiB chunks until n newlines are found.
func lastNLineOffset(f *os.File, size int64, n int) (int64, error) {
	if n <= 0 || size == 0 {
		return size, nil
	}
	const chunk = 8192
	buf := make([]byte, chunk)
	pos := size
	count := 0
	for pos > 0 {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		if _, err := f.ReadAt(buf[:readSize], pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] != '\n' {
				continue
			}
			// The trailing newline at end-of-file does not count as a
			// line boundary that hides a preceding line.
			if pos+i+1 == size {
				continue
			}
			count++
			if count >= n {
				return pos + i + 1, nil
			}
		}
	}
	return 0, nil
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

func warnIfAgentRunning(name string) {
	st, err := localagents.GetStatus(name)
	if err == nil && st.Running {
		fmt.Fprintln(os.Stderr, "Warning: agent is running; restart it for config changes to take effect.")
	}
}
