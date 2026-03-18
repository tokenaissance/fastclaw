package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

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
