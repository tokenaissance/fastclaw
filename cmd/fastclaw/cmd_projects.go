package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agentcli"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func projectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "Manage agent projects and runtime records"}
	cmd.AddCommand(projectsListCmd(), projectsCreateCmd(), projectsDeleteCmd(), projectsRuntimeCmd())
	return cmd
}

func projectsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <agent>",
		Short: "List projects for an agent owner",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			rows, err := st.ListProjects(ctx, ag.UserID, ag.ID)
			if err != nil {
				return err
			}
			return printValue(rows)
		},
	}
	return cmd
}

func projectsCreateCmd() *cobra.Command {
	var id, name, desc string
	cmd := &cobra.Command{
		Use:   "create <agent>",
		Short: "Create or update a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			if id == "" {
				id = "prj_" + uuid.NewString()
			}
			if name == "" {
				name = id
			}
			rec := &store.ProjectRecord{UserID: ag.UserID, AgentID: ag.ID, ID: id, Name: name, Description: desc}
			if err := st.SaveProject(ctx, rec); err != nil {
				return err
			}
			fmt.Printf("Saved project %s\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "project id; generated when omitted")
	cmd.Flags().StringVar(&name, "name", "", "project name")
	cmd.Flags().StringVar(&desc, "description", "", "project description")
	return cmd
}

func projectsDeleteCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "delete <agent> <project-id>",
		Aliases: []string{"rm"},
		Short:   "Delete a project",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			if !force {
				n, err := st.CountProjectSessions(ctx, ag.UserID, ag.ID, args[1])
				if err != nil {
					return err
				}
				if n > 0 {
					return fmt.Errorf("project has %d sessions; pass --force to delete the project row anyway", n)
				}
			}
			if err := st.DeleteProjectRuntime(ctx, ag.UserID, ag.ID, args[1]); err != nil {
				return err
			}
			if err := st.DeleteProject(ctx, ag.UserID, ag.ID, args[1]); err != nil {
				return err
			}
			fmt.Printf("Deleted project %s\n", args[1])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "delete even if sessions still reference this project")
	return cmd
}

func projectsRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "runtime", Short: "Manage persisted project runtime records"}
	cmd.AddCommand(projectsRuntimeGetCmd(), projectsRuntimeSetCmd(), projectsRuntimeDeleteCmd())
	return cmd
}

func projectsRuntimeGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <agent> <project-id>",
		Short: "Print a runtime record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			rec, err := st.GetProjectRuntime(ctx, ag.UserID, ag.ID, args[1])
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					fmt.Println("null")
					return nil
				}
				return err
			}
			return printValue(rec)
		},
	}
}

func projectsRuntimeSetCmd() *cobra.Command {
	var templateRef, status, previewURL, containerID, gitRef, lastError string
	var devPort, hostPort int
	cmd := &cobra.Command{
		Use:   "set <agent> <project-id>",
		Short: "Create or update a runtime record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			rec := &store.ProjectRuntimeRecord{UserID: ag.UserID, AgentID: ag.ID, ProjectID: args[1]}
			if existing, err := st.GetProjectRuntime(ctx, ag.UserID, ag.ID, args[1]); err == nil {
				rec = existing
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if templateRef != "" {
				rec.TemplateRef = templateRef
			}
			if status != "" {
				rec.Status = status
			}
			if previewURL != "" {
				rec.PreviewURL = previewURL
			}
			if containerID != "" {
				rec.ContainerID = containerID
			}
			if gitRef != "" {
				rec.GitRef = gitRef
			}
			if lastError != "" {
				rec.LastError = lastError
			}
			if devPort != 0 {
				rec.DevPort = devPort
			}
			if hostPort != 0 {
				rec.HostPort = hostPort
			}
			if rec.Status == "" {
				rec.Status = "none"
			}
			if err := st.SaveProjectRuntime(ctx, rec); err != nil {
				return err
			}
			fmt.Printf("Saved runtime for project %s\n", args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&templateRef, "template-ref", "", "template reference")
	cmd.Flags().StringVar(&status, "status", "", "runtime status")
	cmd.Flags().StringVar(&previewURL, "preview-url", "", "preview URL")
	cmd.Flags().StringVar(&containerID, "container-id", "", "sandbox container id")
	cmd.Flags().StringVar(&gitRef, "git-ref", "", "snapshot git ref")
	cmd.Flags().StringVar(&lastError, "last-error", "", "last runtime error")
	cmd.Flags().IntVar(&devPort, "dev-port", 0, "container dev-server port")
	cmd.Flags().IntVar(&hostPort, "host-port", 0, "host preview port")
	return cmd
}

func projectsRuntimeDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <agent> <project-id>",
		Aliases: []string{"rm"},
		Short:   "Delete a runtime record",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, args[0])
			if err != nil {
				return err
			}
			if err := st.DeleteProjectRuntime(ctx, ag.UserID, ag.ID, args[1]); err != nil {
				return err
			}
			fmt.Printf("Deleted runtime for project %s\n", args[1])
			return nil
		},
	}
}
