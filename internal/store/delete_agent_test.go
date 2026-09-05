package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
)

func TestDeleteAgentRemovesScopedRows(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		ownerID    = "u_owner"
		agentID    = "agt_delete_me"
		sessionKey = "s-delete-me"
		projectID  = "prj-delete-me"
	)

	now := time.Now().UTC()
	if err := db.SaveAgent(ctx, &AgentRecord{
		ID:        agentID,
		UserID:    ownerID,
		Name:      "delete me",
		Config:    map[string]interface{}{"description": "temporary"},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	if err := db.SaveAgentFile(ctx, agentID, ownerID, "SOUL.md", []byte("soul")); err != nil {
		t.Fatalf("save agent file: %v", err)
	}
	if err := db.SaveSession(ctx, ownerID, agentID, sessionKey, &SessionRecord{
		ProjectID: projectID,
		Messages:  []SessionMessage{{Role: "user", Content: "hello"}},
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := db.AppendSessionMessage(ctx, ownerID, agentID, sessionKey, SessionMessage{
		Role:      "assistant",
		Content:   "hi",
		Timestamp: now,
	}); err != nil {
		t.Fatalf("append session message: %v", err)
	}
	if _, err := db.AppendSessionEvent(ctx, ownerID, agentID, sessionKey, "content", []byte(`{"text":"hi"}`)); err != nil {
		t.Fatalf("append session event: %v", err)
	}
	if err := db.SaveProject(ctx, &ProjectRecord{
		UserID:      ownerID,
		AgentID:     agentID,
		ID:          projectID,
		Name:        "delete project",
		Description: "temporary",
	}); err != nil {
		t.Fatalf("save project: %v", err)
	}
	if err := db.SaveProjectRuntime(ctx, &ProjectRuntimeRecord{
		UserID:      ownerID,
		AgentID:     agentID,
		ProjectID:   projectID,
		TemplateRef: "vite-react",
		Status:      "running",
	}); err != nil {
		t.Fatalf("save project runtime: %v", err)
	}
	if err := db.CreateGoal(ctx, &GoalRecord{
		ID:          "goal_delete_me",
		AgentID:     agentID,
		SessionKey:  sessionKey,
		OwnerUserID: ownerID,
		Objective:   "finish cleanup",
		Status:      "active",
	}); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	nextRun := now.Add(time.Hour)
	if err := db.SaveCronJob(ctx, &CronJobRecord{
		ID:        "cron_delete_me",
		UserID:    ownerID,
		AgentID:   agentID,
		Name:      "cleanup",
		Type:      "once",
		Schedule:  nextRun.Format(time.RFC3339),
		Message:   "cleanup",
		Channel:   "web",
		ChatID:    "chat",
		Timezone:  "UTC",
		Enabled:   true,
		NextRun:   &nextRun,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("save cron job: %v", err)
	}
	for _, cfg := range []ConfigRecord{
		{
			ID:      "cfg_agent_delete_me",
			Kind:    KindSetting,
			AgentID: agentID,
			Name:    "agents.defaults",
			Enabled: true,
			Data:    map[string]any{"model": "openai/gpt-4o-mini"},
		},
		{
			ID:      "cfg_user_agent_delete_me",
			Kind:    KindSetting,
			UserID:  ownerID,
			AgentID: agentID,
			Name:    "bindings",
			Enabled: true,
			Data:    map[string]any{"timezone": "Asia/Shanghai"},
		},
	} {
		if err := db.SaveConfig(ctx, &cfg); err != nil {
			t.Fatalf("save config %s: %v", cfg.ID, err)
		}
	}
	// Fork-specific tables that must also cascade: the dedicated IM
	// channels table (upstream keeps channels in configs kind='channel')
	// and the configs_kv mirror (agent scope + per-(user,agent) scope).
	if err := db.SaveChannel(ctx, &ChannelRecord{
		ID:        "ch_delete_me",
		UserID:    ownerID,
		AgentID:   agentID,
		Type:      "telegram",
		AccountID: "bot_delete_me",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	for _, kv := range []struct {
		scope, scopeID, name string
	}{
		{"agent", agentID, "agents.defaults.model"},
		{"user-agent", ownerID + "/" + agentID, "bindings.timezone"},
	} {
		if err := db.SetConfigValue(ctx, KindSetting, kv.scope, kv.scopeID, kv.name, "x"); err != nil {
			t.Fatalf("set config value %s/%s/%s: %v", kv.scope, kv.scopeID, kv.name, err)
		}
	}
	if err := db.AddMCPServer(ctx, agentID, "quandora", config.MCPServerConfig{
		Type: "http", URL: "https://mcp.example/quant", OAuthResource: "https://mcp.example/quant",
	}); err != nil {
		t.Fatalf("seed mcp server: %v", err)
	}

	if err := db.DeleteAgent(ctx, agentID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}

	if _, err := db.GetAgent(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAgent after delete err = %v; want ErrNotFound", err)
	}
	for _, tc := range []struct {
		table string
		where string
		arg   string
	}{
		{"agent_files", "agent_id = ?", agentID},
		{"agent_knowledge_chunks", "agent_id = ?", agentID},
		{"sessions", "agent_id = ?", agentID},
		{"session_messages", "agent_id = ?", agentID},
		{"session_events", "agent_id = ?", agentID},
		{"cron_jobs", "agent_id = ?", agentID},
		{"projects", "agent_id = ?", agentID},
		{"project_runtimes", "agent_id = ?", agentID},
		{"agent_goals", "agent_id = ?", agentID},
		// fork schema uses (user_id, agent_id), not scope_id — agent_id = ?
		// captures both the official row (user_id='') and per-user overrides.
		{"configs", "agent_id = ?", agentID},
		// fork-specific cascade additions: dedicated channels table +
		// configs_kv mirror (agent scope + per-(user,agent) scope).
		{"channels", "agent_id = ?", agentID},
		{"agent_mcp_servers", "agent_id = ?", agentID},
		{"configs_kv", "scope = 'agent' AND scope_id = ?", agentID},
		{"configs_kv", "scope = 'user-agent' AND scope_id = ?", ownerID + "/" + agentID},
	} {
		var count int
		if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table+" WHERE "+tc.where, tc.arg).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if count != 0 {
			t.Fatalf("%s rows after DeleteAgent = %d; want 0", tc.table, count)
		}
	}
}
