package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/plugin"
)

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
