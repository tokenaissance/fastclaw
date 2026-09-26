package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// A scheduled job belongs to the chatter who asked for it. Before this,
// list_cron_jobs answered with every job of the agent and delete_cron_job
// deleted by id alone, so any visitor chatting with a public agent could
// read and cancel another chatter's reminders (2026-09-27 report: "两个
// session … 也可以跨 session 直接删 cron").
//
// The agent-level (dashboard) surface is deliberately not the model for
// this: handlers_cron.go gates on requireAgentOwner and the agent id, and
// the owner should keep seeing everything. The chat tool is the surface
// where one chatter's turn can name another chatter's job id.
func TestCronJobsAreScopedToTheirCreator(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file:cron_isolation_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const agentID = "agent-cron-isolation"
	reg := NewRegistry(t.TempDir(), t.TempDir())
	reg.SetOwnerUserID("u_owner")
	reg.SetAgentOwnerUserID("u_owner")
	RegisterCronTools(reg, db, "u_owner", agentID)

	jobID := func(name string) string {
		t.Helper()
		jobs, err := db.ListCronJobsByAgent(ctx, agentID)
		if err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		for _, j := range jobs {
			if j.Name == name {
				return j.ID
			}
		}
		t.Fatalf("no job named %q in %d rows", name, len(jobs))
		return ""
	}

	createAs := func(chatter, name, chatID string) {
		t.Helper()
		reg.SetChatterUserID(chatter)
		reg.SetMessageContext("web", "", chatID)
		args, _ := json.Marshal(createCronJobArgs{
			Name: name, Type: "once",
			Schedule: time.Now().Add(time.Hour).Format(time.RFC3339),
			Message:  "提醒我",
		})
		if _, err := reg.Execute(ctx, "create_cron_job", string(args)); err != nil {
			t.Fatalf("create as %s: %v", chatter, err)
		}
	}

	createAs("u_alice", "alice reminder", "chat-alice")
	createAs("u_bob", "bob reminder", "chat-bob")
	aliceJob := jobID("alice reminder")

	// Bob's own session: he sees his job, not Alice's.
	reg.SetChatterUserID("u_bob")
	out, err := reg.Execute(ctx, "list_cron_jobs", "{}")
	if err != nil {
		t.Fatalf("list as bob: %v", err)
	}
	if strings.Contains(out, "alice reminder") {
		t.Errorf("bob's list leaked alice's job:\n%s", out)
	}
	if !strings.Contains(out, "bob reminder") {
		t.Errorf("bob's list dropped his own job:\n%s", out)
	}

	// Bob naming Alice's job id must not cancel it.
	reg.SetChatterUserID("u_bob")
	args, _ := json.Marshal(deleteCronJobArgs{ID: aliceJob})
	if _, err := reg.Execute(ctx, "delete_cron_job", string(args)); err == nil {
		t.Errorf("bob deleted alice's job %s without error", aliceJob)
	}
	if _, err := db.GetCronJob(ctx, aliceJob); err != nil {
		t.Errorf("alice's job is gone after bob's delete: %v", err)
	}

	// Alice still sees and can cancel her own.
	reg.SetChatterUserID("u_alice")
	out, err = reg.Execute(ctx, "list_cron_jobs", "{}")
	if err != nil {
		t.Fatalf("list as alice: %v", err)
	}
	if !strings.Contains(out, "alice reminder") {
		t.Errorf("alice lost sight of her own job:\n%s", out)
	}
	if _, err := reg.Execute(ctx, "delete_cron_job", string(args)); err != nil {
		t.Errorf("alice could not delete her own job: %v", err)
	}

	// The owner sitting in their own turn sees every job of the agent.
	createAs("u_alice", "alice reminder 2", "chat-alice")
	reg.SetChatterUserID("u_owner")
	reg.SetCallerIsAdmin(true)
	out, err = reg.Execute(ctx, "list_cron_jobs", "{}")
	if err != nil {
		t.Fatalf("list as owner: %v", err)
	}
	for _, want := range []string{"bob reminder", "alice reminder 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("owner's list is missing %q:\n%s", want, out)
		}
	}
}
