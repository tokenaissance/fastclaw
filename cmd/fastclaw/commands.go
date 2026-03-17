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
