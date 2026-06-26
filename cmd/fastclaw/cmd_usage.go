package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

func usageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "usage", Short: "Inspect token usage"}
	cmd.AddCommand(usageSummaryCmd(), usageUserCmd(), usageAgentCmd())
	return cmd
}

func usageSummaryCmd() *cobra.Command {
	var days, limit int
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Print total token usage and top users/agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			meter, closeFn, err := openSQLUsageMeter()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := context.Background()
			r := usage.LastN(days)
			totals, err := meter.Totals(ctx, r)
			if err != nil {
				return err
			}
			topAgents, err := meter.TopAgents(ctx, r, limit)
			if err != nil {
				return err
			}
			topUsers, err := meter.TopUsers(ctx, r, limit)
			if err != nil {
				return err
			}
			return printValue(map[string]any{
				"range":     usageRangeJSON(r),
				"totals":    totals,
				"topAgents": topAgents,
				"topUsers":  topUsers,
			})
		},
	}
	cmd.Flags().IntVar(&days, "days", 7, "number of recent days to include")
	cmd.Flags().IntVar(&limit, "limit", 10, "leaderboard row limit")
	return cmd
}

func usageUserCmd() *cobra.Command {
	var days int
	cmd := &cobra.Command{
		Use:   "user <user-id>",
		Short: "Print usage totals and daily rows for one user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			meter, closeFn, err := openSQLUsageMeter()
			if err != nil {
				return err
			}
			defer closeFn()
			ctx := context.Background()
			r := usage.LastN(days)
			totals, err := meter.TotalsForUser(ctx, args[0], r)
			if err != nil {
				return err
			}
			daily, err := meter.DailyForUser(ctx, args[0], r)
			if err != nil {
				return err
			}
			return printValue(map[string]any{
				"range":  usageRangeJSON(r),
				"userId": args[0],
				"totals": totals,
				"daily":  daily,
			})
		},
	}
	cmd.Flags().IntVar(&days, "days", 30, "number of recent days to include")
	return cmd
}

func usageAgentCmd() *cobra.Command {
	var days, limit int
	var userID string
	cmd := &cobra.Command{
		Use:   "agent <agent-id>",
		Short: "Print top sessions for one agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			meter, closeFn, err := openSQLUsageMeter()
			if err != nil {
				return err
			}
			defer closeFn()
			r := usage.LastN(days)
			sessions, err := meter.SessionsForAgent(context.Background(), args[0], userID, r, limit)
			if err != nil {
				return err
			}
			return printValue(map[string]any{
				"range":    usageRangeJSON(r),
				"agentId":  args[0],
				"userId":   userID,
				"sessions": sessions,
			})
		},
	}
	cmd.Flags().StringVar(&userID, "user", "", "optional user id to scope the session list")
	cmd.Flags().IntVar(&days, "days", 30, "number of recent days to include")
	cmd.Flags().IntVar(&limit, "limit", 20, "session row limit")
	return cmd
}

func openSQLUsageMeter() (*usage.SQLMeter, func(), error) {
	st, err := openStoreFromEnv()
	if err != nil {
		return nil, nil, err
	}
	dbs, ok := st.(*store.DBStore)
	if !ok {
		_ = st.Close()
		return nil, nil, fmt.Errorf("usage CLI requires SQL-backed store")
	}
	meter := usage.NewSQLMeter(dbs.DB(), dbs.Dialect())
	return meter, func() { _ = st.Close() }, nil
}

func usageRangeJSON(r usage.Range) map[string]string {
	return map[string]string{
		"since": r.Since.Format(time.DateOnly),
		"until": r.Until.Format(time.DateOnly),
	}
}
