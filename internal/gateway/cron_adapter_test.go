package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// The store-backed scheduler reads its work through cronStoreAdapter, and
// the per-chatter identity of the fired tick rides on CreatorUserID. A
// field dropped in this projection is silently "act for the owner" again
// — the exact bug this line of work closed — so the mapping is asserted
// against a real row rather than left to the type checker.
func TestCronStoreAdapterProjectsCreator(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file:cron_adapter_test?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SaveAgent(ctx, &store.AgentRecord{
		ID: "agt_creator_probe", UserID: "u_owner", Name: "creator-probe",
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	due := time.Now().Add(-time.Minute)
	if err := db.SaveCronJob(ctx, &store.CronJobRecord{
		ID: "job-creator-probe", AgentID: "agt_creator_probe",
		CreatorUserID: "u_visitor",
		Name:          "visitor reminder", Type: "once",
		Schedule: due.Format(time.RFC3339), Message: "提醒我",
		Channel: "web", ChatID: "chat-visitor",
		Enabled: true, NextRun: &due,
	}); err != nil {
		t.Fatalf("save cron job: %v", err)
	}

	jobs, err := (&cronStoreAdapter{st: db}).GetDueCronJobs(ctx, time.Now())
	if err != nil {
		t.Fatalf("get due jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d due jobs, want 1", len(jobs))
	}
	if jobs[0].CreatorUserID != "u_visitor" {
		t.Errorf("StoreJob.CreatorUserID = %q, want u_visitor", jobs[0].CreatorUserID)
	}
	if jobs[0].OwnerUserID != "u_owner" {
		t.Errorf("StoreJob.OwnerUserID = %q, want u_owner (routing still follows the owner)",
			jobs[0].OwnerUserID)
	}
}
