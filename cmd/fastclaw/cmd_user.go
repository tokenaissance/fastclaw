package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// userCmd manages the cloud-mode user registry.
func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users for cloud/multi-user mode",
		Long: `Manage the user registry at ~/.fastclaw/users.json.

In local mode FastClaw runs as a single user ("local") and the registry is
unused. In cloud mode (gateway.mode = "cloud"), the registry maps bearer
tokens to user IDs so the HTTP API routes each request to the right user's
agents. Each user gets their own workspace at ~/.fastclaw/users/{id}/.`,
	}
	cmd.AddCommand(userAddCmd())
	cmd.AddCommand(userListCmd())
	cmd.AddCommand(userRemoveCmd())
	cmd.AddCommand(userTokenCmd())
	return cmd
}

func userAddCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Create a new user and issue an access token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if id == config.DefaultUserID {
				return fmt.Errorf("%q is reserved for local mode", id)
			}

			reg, err := users.Load()
			if err != nil {
				return fmt.Errorf("load registry: %w", err)
			}

			u, token, err := reg.Add(id, name)
			if err != nil {
				return err
			}
			if err := reg.Save(); err != nil {
				return fmt.Errorf("save registry: %w", err)
			}

			if err := users.ProvisionWorkspace(id); err != nil {
				return fmt.Errorf("provision workspace: %w", err)
			}

			fmt.Printf("Created user %q (name=%q)\n", u.ID, u.Name)
			fmt.Printf("Workspace: ~/.fastclaw/users/%s/\n", u.ID)
			fmt.Printf("Token: %s\n", token)
			fmt.Println()
			fmt.Println("Store this token — it won't be shown again.")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name for the user")
	return cmd
}

func userListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all users in the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := users.Load()
			if err != nil {
				return err
			}
			list := reg.List()
			if len(list) == 0 {
				fmt.Println("No users registered.")
				fmt.Println("Local mode uses the implicit \"local\" user; add cloud users with 'fastclaw user add'.")
				return nil
			}
			for _, u := range list {
				name := u.Name
				if name == "" {
					name = "-"
				}
				fmt.Printf("  %-20s name=%-20s tokens=%d  created=%s\n",
					u.ID, name, len(u.Tokens), u.CreatedAt.Format("2006-01-02"))
			}
			return nil
		},
	}
}

func userRemoveCmd() *cobra.Command {
	var keepWorkspace bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a user from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			reg, err := users.Load()
			if err != nil {
				return err
			}
			if err := reg.Remove(id); err != nil {
				return err
			}
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Printf("Removed user %q from registry.\n", id)

			if !keepWorkspace {
				userDir, err := config.UserDir(id)
				if err == nil {
					if _, err := os.Stat(userDir); err == nil {
						fmt.Printf("Workspace at %s was left in place.\n", userDir)
						fmt.Println("Delete it manually if you want to reclaim the data.")
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepWorkspace, "keep-workspace", true, "(no-op) workspaces are always kept; delete manually")
	return cmd
}

func userTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token <id>",
		Short: "Issue an additional access token for a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			reg, err := users.Load()
			if err != nil {
				return err
			}
			token, err := reg.IssueToken(id)
			if err != nil {
				return err
			}
			if err := reg.Save(); err != nil {
				return err
			}
			fmt.Println(token)
			return nil
		},
	}
}

// provisionUserWorkspace is now in internal/users/provision.go as
// users.ProvisionWorkspace, shared between CLI and Admin API.
