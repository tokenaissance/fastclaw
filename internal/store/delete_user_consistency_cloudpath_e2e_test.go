package store

// e2e for commit 33877b3 "fix agent tool handling and deletion" — the
// deletion half.
//
// Upstream's #15 expanded DeleteAgent's cascade to also remove
// projects / project_runtimes / agent_goals. The fork goes further:
// DeleteUser's per-agent cleanup loop matches DeleteAgent's table set
// (the three new tables included), closing the same orphan gap on the
// user-deletion path that upstream left open. This test exercises the
// REAL DBStore.DeleteUser against a full agent-scoped dataset and
// asserts every row is cleaned, including the fork's configs semantics
// (official agent rows + per-user agent overrides keyed by
// (user_id, agent_id), scope_id dropped).
//
// Cloud zero-impact rationale: Cloud deletes whole users via the
// /api/fastagent proxy's DELETE /users path, which lands here. This is
// a server-side data-integrity fix — no endpoint / auth / shape change.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeleteUser_CleansAgentScopedRows_CloudPathE2E(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const (
		ownerID    = "u_owner_delete"
		agentID    = "agt_user_delete"
		sessionKey = "s-user-delete"
		projectID  = "prj-user-delete"
	)

	now := time.Now().UTC()
	if err := db.CreateUser(ctx, &UserRecord{
		ID:       ownerID,
		Username: "delete-me",
		Email:    "delete@example.com",
		Role:     "user",
		Status:   "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
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
	if err := db.SaveCronJob(ctx, &CronJobRecord{
		ID:        "cron_user_delete",
		UserID:    ownerID,
		AgentID:   agentID,
		Name:      "cleanup",
		Type:      "once",
		Schedule:  now.Add(time.Hour).Format(time.RFC3339),
		Message:   "cleanup",
		Channel:   "web",
		ChatID:    "chat",
		Timezone:  "UTC",
		Enabled:   true,
		NextRun:   func() *time.Time { n := now.Add(time.Hour); return &n }(),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("save cron job: %v", err)
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
		ID:          "goal_user_delete",
		AgentID:     agentID,
		SessionKey:  sessionKey,
		OwnerUserID: ownerID,
		Objective:   "finish cleanup",
		Status:      "active",
	}); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	for _, cfg := range []ConfigRecord{
		{
			ID:      "cfg_agent_user_delete",
			Kind:    KindSetting,
			AgentID: agentID,
			Name:    "agents.defaults",
			Enabled: true,
			Data:    map[string]any{"model": "openai/gpt-4o-mini"},
		},
		{
			ID:      "cfg_user_agent_delete",
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
	// Fork-specific tables that must also cascade on user delete: the
	// dedicated IM channels table (upstream keeps channels in configs)
	// and the configs_kv mirror (agent / per-(user,agent) / user scopes).
	if err := db.SaveChannel(ctx, &ChannelRecord{
		ID:        "ch_user_delete",
		UserID:    ownerID,
		AgentID:   agentID,
		Type:      "telegram",
		AccountID: "bot_user_delete",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("save channel: %v", err)
	}
	for _, kv := range []struct {
		scope, scopeID, name string
	}{
		{"agent", agentID, "agents.defaults.model"},
		{"user-agent", ownerID + "/" + agentID, "bindings.timezone"},
		{"user", ownerID, "general.locale"},
	} {
		if err := db.SetConfigValue(ctx, KindSetting, kv.scope, kv.scopeID, kv.name, "x"); err != nil {
			t.Fatalf("set config value %s/%s/%s: %v", kv.scope, kv.scopeID, kv.name, err)
		}
	}

	if err := db.DeleteUser(ctx, ownerID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if _, err := db.GetUser(ctx, ownerID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser after delete err = %v; want ErrNotFound", err)
	}
	if _, err := db.GetAgent(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAgent after delete err = %v; want ErrNotFound", err)
	}

	// DeleteUser cleans the agent's rows in the same tables DeleteAgent
	// does (the fork's fork-level adaptation extends this to
	// projects/project_runtimes/agent_goals), plus the user-scoped rows
	// (sessions, configs) that aren't agent-keyed.
	for _, tc := range []struct {
		table string
		where string
		arg   string
	}{
		{"agent_files", "agent_id = ?", agentID},
		{"sessions", "agent_id = ?", agentID},
		{"session_messages", "agent_id = ?", agentID},
		{"session_events", "agent_id = ?", agentID},
		{"cron_jobs", "agent_id = ?", agentID},
		{"projects", "agent_id = ?", agentID},
		{"project_runtimes", "agent_id = ?", agentID},
		{"agent_goals", "agent_id = ?", agentID},
		// fork schema uses (user_id, agent_id) — both the official agent
		// rows (user_id='', agent_id=X) and the owner's per-agent
		// overrides (user_id=owner, agent_id=X) must be gone.
		{"configs", "agent_id = ?", agentID},
		{"configs", "user_id = ?", ownerID},
		// fork-specific cascade additions: dedicated channels table +
		// configs_kv mirror across all three scopes this user/agent
		// appears in.
		{"channels", "agent_id = ?", agentID},
		{"configs_kv", "scope = 'agent' AND scope_id = ?", agentID},
		{"configs_kv", "scope = 'user-agent' AND scope_id = ?", ownerID + "/" + agentID},
		{"configs_kv", "scope = 'user' AND scope_id = ?", ownerID},
	} {
		var count int
		if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table+" WHERE "+tc.where, tc.arg).Scan(&count); err != nil {
			t.Fatalf("count %s (%s): %v", tc.table, tc.where, err)
		}
		if count != 0 {
			t.Fatalf("%s rows (%s) after DeleteUser = %d; want 0", tc.table, tc.where, count)
		}
	}
}
