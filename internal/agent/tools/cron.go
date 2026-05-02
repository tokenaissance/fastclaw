package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

type createCronJobArgs struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Message  string `json:"message"`
	Type     string `json:"type"`
}

type deleteCronJobArgs struct {
	ID string `json:"id"`
}

// RegisterCronTools registers cron job management tools.
//
// Channel + chatID for the originating turn are read from the registry
// at execute time via r.MessageChannel() / r.MessageChatID() so a single
// registration at agent construction handles every chat context the
// agent runs in. The agent loop's bindSession stamps the per-turn
// values onto the registry before any tool fires.
func RegisterCronTools(r *Registry, st store.Store, userID, agentID string) {
	r.Register("create_cron_job",
		"Create a scheduled task. Use this for any user request that names a specific time, an interval, or a recurring schedule (e.g. \"5 分钟后提醒\", \"every Monday 9am\", \"each day at 8\"). When the schedule fires, the agent receives `message` as a fresh inbound prompt on the same channel the request originated from. Do NOT write timed reminders into HEARTBEAT.md — that file is only for conditional self-checks reviewed at every heartbeat tick.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Short task name (for listing / debugging).",
				},
				"schedule": map[string]interface{}{
					"type":        "string",
					"description": "When to fire. For type='cron': a 5-field cron expression like '0 9 * * *'. For type='interval': a duration like '5m' / '30m' / '2h'. For type='once': an ISO-8601 datetime in UTC like '2026-05-02T15:56:52'.",
				},
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The prompt the agent should receive when the schedule fires. Phrase it as instructions to yourself (e.g. \"提醒小m喝水\"), not as a user-facing message — the agent will compose the user reply when it processes the inbound.",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Schedule type. Use 'once' for one-shot reminders ('5 分钟后…'), 'cron' for calendar-style recurring schedules ('每天 9 点'), or 'interval' for fixed-period polling ('每 30 分钟检查一次'). Defaults to 'cron'.",
					"enum":        []string{"cron", "interval", "once"},
				},
			},
			"required": []string{"name", "schedule", "message"},
		},
		makeCreateCronJob(st, r, userID, agentID),
	)

	r.Register("list_cron_jobs",
		"List all scheduled tasks for this agent.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListCronJobs(st, userID, agentID),
	)

	r.Register("delete_cron_job",
		"Delete a scheduled task by ID.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "The cron job ID to delete",
				},
			},
			"required": []string{"id"},
		},
		makeDeleteCronJob(st, userID),
	)
}

func makeCreateCronJob(st store.Store, r *Registry, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args createCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Name == "" || args.Schedule == "" || args.Message == "" {
			return "", fmt.Errorf("name, schedule, and message are required")
		}
		jobType := args.Type
		if jobType == "" {
			jobType = "cron"
		}

		// Read the originating bus address at execute time — bindSession
		// stamps it on every turn, so this captures the channel/chatID
		// the user was on when they asked for the reminder.
		channel := r.MessageChannel()
		chatID := r.MessageChatID()

		id := generateUUID()
		now := time.Now()
		job := &store.CronJobRecord{
			ID:        id,
			AgentID:   agentID,
			Name:      args.Name,
			Type:      jobType,
			Schedule:  args.Schedule,
			Message:   args.Message,
			Channel:   channel,
			ChatID:    chatID,
			Timezone:  "UTC",
			Enabled:   true,
			NextRun:   &now,
			CreatedAt: now,
		}

		if err := st.SaveCronJob(ctx, job); err != nil {
			return "", fmt.Errorf("save cron job: %w", err)
		}

		return fmt.Sprintf("Cron job created successfully.\nID: %s\nName: %s\nSchedule: %s\nType: %s", id, args.Name, args.Schedule, jobType), nil
	}
}

func makeListCronJobs(st store.Store, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		jobs, err := st.ListCronJobsByAgent(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list cron jobs: %w", err)
		}
		filtered := jobs

		if len(filtered) == 0 {
			return "No cron jobs found for this agent.", nil
		}

		data, _ := json.MarshalIndent(filtered, "", "  ")
		return string(data), nil
	}
}

func makeDeleteCronJob(st store.Store, userID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args deleteCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if err := st.DeleteCronJob(ctx, args.ID); err != nil {
			return "", fmt.Errorf("delete cron job: %w", err)
		}
		return fmt.Sprintf("Cron job %s deleted.", args.ID), nil
	}
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
