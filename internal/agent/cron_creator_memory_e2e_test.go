package agent

import (
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// The other half of the 2026-09-27 report: a visitor on a public agent
// can schedule a job ("5 分钟后提醒我"), and the tick that fires it runs
// with the prompt the tool wrote. If that tick's per-chatter identity is
// the agent owner, the model's own instruction — "record this in
// MEMORY.md" — lands in the OWNER's memory: a scheduled write into
// someone else's mind.
//
// Same chain as TestAutonomousTurnMemoryRouting_CloudPathE2E, with the
// creator field set: bus message → Agent.chatterUserID → registry →
// file tool → agent_files row.
func TestCronTurnWritesTheCreatorsMemory_CloudPathE2E(t *testing.T) {
	db, ctx := newCronE2EStore(t)
	adapter := NewMemoryStoreAdapter(db)

	const ownerSeed = "## 事实\n- 所有者的私有记忆：`XAUT-USDT` 已剔除。\n"
	if err := adapter.SaveMemory(ctx, cronE2EAgentID, cronE2EOwnerID, ownerSeed); err != nil {
		t.Fatalf("seed owner memory: %v", err)
	}
	const visitorID = "u_visitor"
	if err := adapter.SaveMemory(ctx, cronE2EAgentID, visitorID, "## 我的记忆\n- 关注 BTC。\n"); err != nil {
		t.Fatalf("seed visitor memory: %v", err)
	}

	msg := bus.InboundMessage{
		Channel: "web", ChatID: "chat-visitor",
		UserID:      "cron",
		OwnerUserID: cronE2EOwnerID,
		// What the scheduler now carries: the job's creator.
		CreatorUserID: visitorID,
		Source:        bus.SourceCron,
	}

	reg := newCronE2ERegistry(t, db)
	a := &Agent{ownerUserID: cronE2EOwnerID}
	reg.SetChatterUserID(a.chatterUserID(msg))

	if _, err := reg.Execute(ctx, "write_file", toolArgs(t, map[string]any{
		"path":    "MEMORY.md",
		"content": "## 我的记忆\n- 关注 BTC。\n- ★ 定时任务写入：提醒我喝水。\n",
	})); err != nil {
		t.Fatalf("write_file from the scheduled turn: %v", err)
	}

	got, err := adapter.GetMemory(ctx, cronE2EAgentID, cronE2EOwnerID)
	if err != nil {
		t.Fatalf("owner memory after the tick: %v", err)
	}
	if got != ownerSeed {
		t.Errorf("the tick edited the OWNER's MEMORY.md:\n%q", got)
	}
	visitorMem, err := adapter.GetMemory(ctx, cronE2EAgentID, visitorID)
	if err != nil {
		t.Fatalf("visitor memory after the tick: %v", err)
	}
	if !strings.Contains(visitorMem, "定时任务写入") {
		t.Errorf("visitor MEMORY.md = %q, want the tick's write", visitorMem)
	}
}
