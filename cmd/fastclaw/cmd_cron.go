package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agentcli"
	"github.com/fastclaw-ai/fastclaw/internal/cron"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func cronCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cron", Short: "Manage scheduled jobs"}
	cmd.AddCommand(cronListCmd(), cronCreateCmd(), cronEnableCmd(), cronDeleteCmd())
	return cmd
}

func cronListCmd() *cobra.Command {
	var agentName string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List scheduled jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			var jobs []store.CronJobRecord
			if agentName != "" {
				ag, err := agentcli.Resolve(ctx, st, agentName)
				if err != nil {
					return err
				}
				jobs, err = st.ListCronJobsByAgent(ctx, ag.ID)
			} else {
				all, err := st.ListAllAgents(ctx)
				if err != nil {
					return err
				}
				for _, ag := range all {
					part, err := st.ListCronJobsByAgent(ctx, ag.ID)
					if err != nil {
						return err
					}
					jobs = append(jobs, part...)
				}
			}
			if err != nil {
				return err
			}
			if len(jobs) == 0 {
				fmt.Println("No cron jobs.")
				return nil
			}
			fmt.Printf("%-36s %-22s %-8s %-12s %-10s %-16s %-20s %s\n", "ID", "AGENT", "TYPE", "SCHEDULE", "ENABLED", "CHANNEL", "NEXT_RUN", "NAME")
			for _, j := range jobs {
				next := "-"
				if j.NextRun != nil {
					next = j.NextRun.Format("2006-01-02 15:04")
				}
				fmt.Printf("%-36s %-22s %-8s %-12s %-10v %-16s %-20s %s\n",
					j.ID, j.AgentID, j.Type, j.Schedule, j.Enabled, j.Channel+":"+j.AccountID, next, j.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "agent name or id")
	return cmd
}

func cronCreateCmd() *cobra.Command {
	var agentName, name, typ, schedule, message, channel, accountID, chatID, timezone string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a scheduled job",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			ag, err := agentcli.Resolve(ctx, st, agentName)
			if err != nil {
				return err
			}
			if typ == "" {
				typ = "cron"
			}
			if timezone == "" {
				timezone = time.Local.String()
			}
			next := nextCronRun(typ, schedule, timezone)
			job := &store.CronJobRecord{
				ID:        uuid.NewString(),
				UserID:    ag.UserID,
				AgentID:   ag.ID,
				Name:      name,
				Type:      typ,
				Schedule:  schedule,
				Message:   message,
				Channel:   channel,
				AccountID: accountID,
				ChatID:    chatID,
				Timezone:  timezone,
				Enabled:   true,
				NextRun:   &next,
			}
			if err := st.SaveCronJob(ctx, job); err != nil {
				return err
			}
			cron.NotifyJobCreated()
			fmt.Printf("Created cron job %s next_run=%s\n", job.ID, next.Format(time.RFC3339))
			notifyGatewayReload()
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "agent name or id (required)")
	cmd.Flags().StringVar(&name, "name", "", "job name")
	cmd.Flags().StringVar(&typ, "type", "cron", "job type: once, interval, cron")
	cmd.Flags().StringVar(&schedule, "schedule", "", "schedule: RFC3339 for once, duration for interval, 5-field cron for cron")
	cmd.Flags().StringVar(&message, "message", "", "message to send to the agent")
	cmd.Flags().StringVar(&channel, "channel", "web", "target channel")
	cmd.Flags().StringVar(&accountID, "account", "", "target channel account id")
	cmd.Flags().StringVar(&chatID, "chat", "", "target chat id")
	cmd.Flags().StringVar(&timezone, "timezone", "Asia/Shanghai", "IANA timezone")
	_ = cmd.MarkFlagRequired("agent")
	_ = cmd.MarkFlagRequired("schedule")
	_ = cmd.MarkFlagRequired("message")
	_ = cmd.MarkFlagRequired("chat")
	return cmd
}

func cronEnableCmd() *cobra.Command {
	var enabled bool
	cmd := &cobra.Command{
		Use:   "set-enabled <id>",
		Short: "Enable or disable a scheduled job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			ctx := context.Background()
			job, err := st.GetCronJob(ctx, args[0])
			if err != nil {
				return err
			}
			job.Enabled = enabled
			if err := st.SaveCronJob(ctx, job); err != nil {
				return err
			}
			fmt.Printf("Set %s enabled=%v\n", job.ID, enabled)
			cron.NotifyJobCreated()
			notifyGatewayReload()
			return nil
		},
	}
	cmd.Flags().BoolVar(&enabled, "enabled", true, "enabled value")
	return cmd
}

func cronDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete a scheduled job",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.DeleteCronJob(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted cron job %s\n", args[0])
			cron.NotifyJobCreated()
			notifyGatewayReload()
			return nil
		},
	}
}

func nextCronRun(typ, schedule, tz string) time.Time {
	now := time.Now()
	switch typ {
	case "once":
		if t, err := time.Parse(time.RFC3339, schedule); err == nil {
			return t
		}
	case "interval":
		if d, err := time.ParseDuration(schedule); err == nil {
			return now.Add(d)
		}
	case "cron":
		return cron.NextOccurrenceIn(schedule, now, cron.LocationOf(tz))
	}
	return now.Add(time.Minute)
}
