package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/plugin"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// versionCmd prints the current version info.
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print FastClaw version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("FastClaw %s\n", version)
			fmt.Printf("  commit: %s\n", commit)
			fmt.Printf("  built:  %s\n", date)
			fmt.Printf("  go:     %s\n", runtime.Version())
			fmt.Printf("  os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
		},
	}
}

// upgradeCmd downloads and installs the latest release.
func upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Upgrade FastClaw to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return doUpgrade()
		},
	}
}

func doUpgrade() error {
	const repo = "fastclaw-ai/fastclaw"
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)

	fmt.Println("⚡ Checking for updates...")

	// 1. Fetch latest release info
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("failed to parse release info: %w", err)
	}

	// 2. Check if already up to date
	if release.TagName == version {
		fmt.Printf("✅ Already up to date (%s)\n", version)
		return nil
	}
	fmt.Printf("📦 New version available: %s → %s\n", version, release.TagName)

	// 3. Find the right asset for this platform
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	var suffix string
	if goos == "windows" {
		suffix = fmt.Sprintf("fastclaw_%s_%s.zip", goos, goarch)
	} else {
		suffix = fmt.Sprintf("fastclaw_%s_%s.tar.gz", goos, goarch)
	}

	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == suffix {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s in release %s", goos, goarch, release.TagName)
	}

	fmt.Printf("⬇️  Downloading %s...\n", suffix)

	// 4. Download to temp file
	dlResp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer dlResp.Body.Close()

	tmpFile, err := os.CreateTemp("", "fastclaw-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	tmpFile.Close()

	// 5. Extract binary
	tmpDir, err := os.MkdirTemp("", "fastclaw-extract-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if goos == "windows" {
		// Unzip
		unzip := exec.Command("powershell", "-Command",
			fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", tmpPath, tmpDir))
		if out, err := unzip.CombinedOutput(); err != nil {
			return fmt.Errorf("unzip failed: %s: %w", string(out), err)
		}
	} else {
		// Untar
		tar := exec.Command("tar", "-xzf", tmpPath, "-C", tmpDir)
		if out, err := tar.CombinedOutput(); err != nil {
			return fmt.Errorf("untar failed: %s: %w", string(out), err)
		}
	}

	// 6. Find current binary path and replace
	currentBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find current binary: %w", err)
	}
	currentBin, _ = filepath.EvalSymlinks(currentBin)

	binaryName := "fastclaw"
	if goos == "windows" {
		binaryName = "fastclaw.exe"
	}
	newBin := filepath.Join(tmpDir, binaryName)

	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("extracted binary not found at %s", newBin)
	}

	// 7. Replace current binary
	fmt.Printf("📝 Installing to %s...\n", currentBin)

	// Try direct overwrite first; if permission denied, use sudo on Unix
	if err := replaceBinary(currentBin, newBin); err != nil {
		if goos != "windows" {
			fmt.Println("🔐 Need elevated permissions, trying sudo...")
			sudo := exec.Command("sudo", "cp", newBin, currentBin)
			sudo.Stdin = os.Stdin
			sudo.Stdout = os.Stdout
			sudo.Stderr = os.Stderr
			if err := sudo.Run(); err != nil {
				return fmt.Errorf("install failed (try: sudo cp %s %s): %w", newBin, currentBin, err)
			}
		} else {
			return fmt.Errorf("install failed: %w", err)
		}
	}

	fmt.Printf("✅ Upgraded to %s\n", release.TagName)
	return nil
}

func replaceBinary(dst, src string) error {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, srcData, 0o755)
}

// doctorCmd checks the config and environment for issues.
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check FastClaw configuration and environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor()
		},
	}
}

func runDoctor() error {
	fmt.Println("🩺 FastClaw Doctor")
	fmt.Println(strings.Repeat("─", 40))
	issues := 0
	warnings := 0

	// 1. Version
	fmt.Printf("\n📌 Version: %s (%s)\n", version, commit)

	// 2. Config file
	homeDir, err := config.HomeDir()
	if err != nil {
		fmt.Printf("❌ Config dir: cannot determine (%v)\n", err)
		issues++
	} else {
		fmt.Printf("📂 Config dir: %s\n", homeDir)

		configPath := filepath.Join(homeDir, "fastclaw.json")
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			fmt.Printf("❌ Config file: not found (%s)\n", configPath)
			fmt.Println("   → Run 'fastclaw' to start the setup wizard")
			issues++
		} else {
			fmt.Printf("✅ Config file: %s\n", configPath)

			// Try loading
			cfg, err := config.Load()
			if err != nil {
				fmt.Printf("❌ Config parse: %v\n", err)
				issues++
			} else {
				fmt.Printf("✅ Config parsed OK\n")

				// 3. Check providers
				fmt.Println()
				if len(cfg.Providers) == 0 {
					fmt.Println("❌ Providers: none configured")
					issues++
				} else {
					for name, prov := range cfg.Providers {
						if prov.APIKey == "" {
							fmt.Printf("⚠️  Provider '%s': no API key\n", name)
							warnings++
						} else {
							masked := prov.APIKey[:4] + "..." + prov.APIKey[len(prov.APIKey)-4:]
							base := prov.APIBase
							if base == "" {
								base = "https://api.openai.com/v1"
							}
							fmt.Printf("✅ Provider '%s': %s (key: %s)\n", name, base, masked)
						}
					}
				}

				// 4. Test LLM connection
				fmt.Println()
				fmt.Println("🔌 Testing LLM connection...")
				if testLLMConnection(cfg) {
					fmt.Println("✅ LLM connection: OK")
				} else {
					fmt.Println("❌ LLM connection: failed")
					issues++
				}

				// 5. Check agents
				fmt.Println()
				resolved := config.ResolveAgents(cfg)
				if len(resolved) == 0 {
					fmt.Println("⚠️  Agents: none configured")
					warnings++
				} else {
					for _, rc := range resolved {
						wsExists := false
						if _, err := os.Stat(rc.Workspace); err == nil {
							wsExists = true
						}
						if wsExists {
							// Check workspace files
							missing := []string{}
							for _, f := range []string{"SOUL.md", "IDENTITY.md"} {
								if _, err := os.Stat(filepath.Join(rc.Workspace, f)); err != nil {
									missing = append(missing, f)
								}
							}
							if len(missing) > 0 {
								fmt.Printf("⚠️  Agent '%s': workspace OK, missing %s\n", rc.ID, strings.Join(missing, ", "))
								warnings++
							} else {
								fmt.Printf("✅ Agent '%s': model=%s, workspace=%s\n", rc.ID, rc.Model, rc.Workspace)
							}
						} else {
							fmt.Printf("❌ Agent '%s': workspace not found (%s)\n", rc.ID, rc.Workspace)
							issues++
						}
					}
				}

				// 6. Check channels
				fmt.Println()
				if len(cfg.Channels) == 0 {
					fmt.Println("⚠️  Channels: none configured")
					warnings++
				} else {
					for name, ch := range cfg.Channels {
						if !ch.Enabled {
							fmt.Printf("⏸️  Channel '%s': disabled\n", name)
							continue
						}
						tokenCount := 0
						if ch.BotToken != "" {
							tokenCount++
						}
						for _, acct := range ch.Accounts {
							if acct.BotToken != "" {
								tokenCount++
							}
						}
						if tokenCount > 0 {
							fmt.Printf("✅ Channel '%s': enabled, %d bot(s)\n", name, tokenCount)
						} else {
							fmt.Printf("❌ Channel '%s': enabled but no bot tokens\n", name)
							issues++
						}
					}
				}

				// 7. Check bindings
				fmt.Println()
				if len(cfg.Bindings) == 0 {
					fmt.Println("⚠️  Bindings: none configured (messages won't route to agents)")
					warnings++
				} else {
					fmt.Printf("✅ Bindings: %d rule(s)\n", len(cfg.Bindings))
				}
			}
		}
	}

	// Summary
	fmt.Println()
	fmt.Println(strings.Repeat("─", 40))
	if issues == 0 && warnings == 0 {
		fmt.Println("🎉 Everything looks good!")
	} else if issues == 0 {
		fmt.Printf("✅ No errors, %d warning(s)\n", warnings)
	} else {
		fmt.Printf("🔴 %d error(s), %d warning(s)\n", issues, warnings)
	}

	return nil
}

// skillCmd handles skill management subcommands.
func skillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills",
	}
	cmd.AddCommand(skillListCmd())
	cmd.AddCommand(skillInstallCmd())
	cmd.AddCommand(skillRemoveCmd())
	return cmd
}

func skillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			skillsDir := filepath.Join(homeDir, "skills")
			entries, err := os.ReadDir(skillsDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No skills installed. Skills directory: " + skillsDir)
					return nil
				}
				return err
			}

			found := false
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				skillFile := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
				if _, err := os.Stat(skillFile); err != nil {
					continue
				}
				found = true
				fmt.Printf("  %s  %s\n", entry.Name(), skillFile)
			}
			if !found {
				fmt.Println("No skills installed.")
			}
			return nil
		},
	}
}

func skillInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <name>",
		Short: "Install a skill from the registry",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("Coming soon - skill registry not yet available.")
		},
	}
}

func skillRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an installed skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			skillDir := filepath.Join(homeDir, "skills", name)
			if _, err := os.Stat(skillDir); os.IsNotExist(err) {
				return fmt.Errorf("skill %q not found at %s", name, skillDir)
			}

			if err := os.RemoveAll(skillDir); err != nil {
				return fmt.Errorf("remove skill: %w", err)
			}

			fmt.Printf("Skill %q removed.\n", name)
			return nil
		},
	}
}

// sessionCmd handles session management subcommands.
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage sessions",
	}
	cmd.AddCommand(sessionListCmd())
	cmd.AddCommand(sessionClearCmd())
	cmd.AddCommand(sessionClearAllCmd())
	return cmd
}

func sessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all sessions across all agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentsDir := filepath.Join(homeDir, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No agents found.")
					return nil
				}
				return err
			}

			found := false
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				sessDir := filepath.Join(agentsDir, entry.Name(), "agent", "sessions")
				sessFiles, err := os.ReadDir(sessDir)
				if err != nil {
					continue
				}
				for _, sf := range sessFiles {
					if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".jsonl") {
						continue
					}
					found = true
					info, _ := sf.Info()
					size := int64(0)
					if info != nil {
						size = info.Size()
					}
					sessKey := strings.TrimSuffix(sf.Name(), ".jsonl")
					fmt.Printf("  agent=%-12s session=%-30s size=%d bytes\n", entry.Name(), sessKey, size)
				}
			}
			if !found {
				fmt.Println("No sessions found.")
			}
			return nil
		},
	}
}

func sessionClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear <session-key>",
		Short: "Clear a specific session (agent:channel_chatid format or just the filename)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentsDir := filepath.Join(homeDir, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				return err
			}

			removed := 0
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				sessFile := filepath.Join(agentsDir, entry.Name(), "agent", "sessions", key+".jsonl")
				if _, err := os.Stat(sessFile); err == nil {
					os.Remove(sessFile)
					fmt.Printf("Cleared session: %s (agent: %s)\n", key, entry.Name())
					removed++
				}
			}
			if removed == 0 {
				return fmt.Errorf("session %q not found", key)
			}
			return nil
		},
	}
}

func sessionClearAllCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-all",
		Short: "Clear all sessions across all agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentsDir := filepath.Join(homeDir, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("No agents found.")
					return nil
				}
				return err
			}

			removed := 0
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				sessDir := filepath.Join(agentsDir, entry.Name(), "agent", "sessions")
				sessFiles, err := os.ReadDir(sessDir)
				if err != nil {
					continue
				}
				for _, sf := range sessFiles {
					if sf.IsDir() || !strings.HasSuffix(sf.Name(), ".jsonl") {
						continue
					}
					os.Remove(filepath.Join(sessDir, sf.Name()))
					removed++
				}
			}
			fmt.Printf("Cleared %d session(s).\n", removed)
			return nil
		},
	}
}

// backupCmd creates a tar.gz backup of ~/.fastclaw.
func backupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Create a tar.gz backup of ~/.fastclaw",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			timestamp := time.Now().Format("20060102-150405")
			backupFile := fmt.Sprintf("fastclaw-backup-%s.tar.gz", timestamp)

			tarCmd := exec.Command("tar", "-czf", backupFile, "-C", filepath.Dir(homeDir), filepath.Base(homeDir))
			tarCmd.Stdout = os.Stdout
			tarCmd.Stderr = os.Stderr
			if err := tarCmd.Run(); err != nil {
				return fmt.Errorf("backup failed: %w", err)
			}

			cwd, _ := os.Getwd()
			fmt.Printf("Backup created: %s\n", filepath.Join(cwd, backupFile))
			return nil
		},
	}
}

// resetCmd deletes sessions and memory but keeps config.
func resetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Delete sessions and memory (keeps config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				fmt.Print("This will delete all sessions and memory. Are you sure? (use --yes to confirm): ")
				return fmt.Errorf("aborted: use --yes flag to confirm")
			}

			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			agentsDir := filepath.Join(homeDir, "agents")
			entries, err := os.ReadDir(agentsDir)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Println("Nothing to reset.")
					return nil
				}
				return err
			}

			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				agentDir := filepath.Join(agentsDir, entry.Name(), "agent")

				// Clear sessions
				sessDir := filepath.Join(agentDir, "sessions")
				os.RemoveAll(sessDir)
				os.MkdirAll(sessDir, 0o755)

				// Clear memory
				memDir := filepath.Join(agentDir, "memory")
				os.RemoveAll(memDir)
				os.MkdirAll(memDir, 0o755)

				// Clear MEMORY.md content but keep file
				memFile := filepath.Join(agentDir, "MEMORY.md")
				if _, err := os.Stat(memFile); err == nil {
					os.WriteFile(memFile, []byte("# Memory\n\n"), 0o644)
				}

				fmt.Printf("Reset agent: %s\n", entry.Name())
			}

			fmt.Println("Reset complete.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation")
	return cmd
}

func testLLMConnection(cfg *config.Config) bool {
	var provCfg config.ProviderConfig
	for _, key := range []string{"default", "openai", "openrouter"} {
		if p, ok := cfg.Providers[key]; ok {
			provCfg = p
			break
		}
	}
	if provCfg.APIKey == "" {
		for _, p := range cfg.Providers {
			provCfg = p
			break
		}
	}
	if provCfg.APIKey == "" {
		return false
	}

	apiBase := provCfg.APIBase
	if apiBase == "" {
		apiBase = "https://api.openai.com/v1"
	}
	apiBase = strings.TrimRight(apiBase, "/")

	// Quick models list check
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", apiBase+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+provCfg.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("   (error: %v)\n", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return true
	}
	fmt.Printf("   (HTTP %d)\n", resp.StatusCode)
	return resp.StatusCode < 500 // 401/403 means API works but key is bad; 5xx means endpoint down
}

// pluginCmd handles plugin management subcommands.
func pluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage plugins",
	}
	cmd.AddCommand(pluginListCmd())
	cmd.AddCommand(pluginInstallCmd())
	cmd.AddCommand(pluginRemoveCmd())
	return cmd
}

func pluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List discovered plugins and their status",
		RunE: func(cmd *cobra.Command, args []string) error {
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			paths := []string{filepath.Join(homeDir, "plugins")}

			// Also check config for extra paths
			cfg, _ := config.Load()
			if cfg != nil && cfg.Plugins.Enabled {
				for _, p := range cfg.Plugins.Paths {
					paths = append(paths, p)
				}
			}

			mgr := plugin.NewManager(nil)
			if err := mgr.Discover(paths); err != nil {
				return err
			}

			plugins := mgr.Plugins()
			if len(plugins) == 0 {
				fmt.Println("No plugins found.")
				fmt.Println("Plugin directories:", paths)
				return nil
			}

			fmt.Printf("%-15s %-20s %-10s %-10s %s\n", "ID", "NAME", "TYPE", "VERSION", "DIR")
			for _, p := range plugins {
				enabledStr := "enabled"
				if cfg != nil {
					if entry, ok := cfg.Plugins.Entries[p.Manifest.ID]; ok && !entry.Enabled {
						enabledStr = "disabled"
					}
				}
				fmt.Printf("%-15s %-20s %-10s %-10s %s [%s]\n",
					p.Manifest.ID,
					p.Manifest.Name,
					p.Manifest.Type,
					p.Manifest.Version,
					p.Manifest.Dir,
					enabledStr,
				)
			}
			return nil
		},
	}
}

func pluginInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <path>",
		Short: "Install a plugin from a local directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcDir := args[0]

			// Validate it has a plugin.json
			manifestPath := filepath.Join(srcDir, "plugin.json")
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", manifestPath, err)
			}

			var manifest struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return fmt.Errorf("invalid plugin.json: %w", err)
			}
			if manifest.ID == "" {
				return fmt.Errorf("plugin.json missing 'id' field")
			}

			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			destDir := filepath.Join(homeDir, "plugins", manifest.ID)
			if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
				return err
			}

			// Copy directory using cp -r
			cpCmd := exec.Command("cp", "-r", srcDir, destDir)
			if out, err := cpCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("copy failed: %s: %w", string(out), err)
			}

			fmt.Printf("Plugin %q installed to %s\n", manifest.ID, destDir)
			return nil
		},
	}
}

func pluginRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove an installed plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			homeDir, err := config.HomeDir()
			if err != nil {
				return err
			}

			pluginDir := filepath.Join(homeDir, "plugins", id)
			if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
				return fmt.Errorf("plugin %q not found at %s", id, pluginDir)
			}

			if err := os.RemoveAll(pluginDir); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}

			fmt.Printf("Plugin %q removed.\n", id)
			return nil
		},
	}
}
