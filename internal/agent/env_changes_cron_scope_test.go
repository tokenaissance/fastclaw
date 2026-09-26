package agent

import (
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// The environment-change signal lists the scheduled jobs that appeared,
// vanished or moved between two turns. That is per-agent data, but the
// signal lands in ONE chatter's context, so an unfiltered sample tells a
// visitor which reminders other people have — the read half of the same
// leak that list_cron_jobs had.
func TestCronFingerprintsAreScopedToTheChatter(t *testing.T) {
	db, ctx := newCronE2EStore(t)

	save := func(id, creator, userID string) {
		t.Helper()
		if err := db.SaveCronJob(ctx, &store.CronJobRecord{
			ID: id, AgentID: cronE2EAgentID,
			CreatorUserID: creator, UserID: userID,
			Name: id, Type: "cron", Schedule: "0 9 * * *", Message: "提醒我",
			Channel: "web", ChatID: "chat", Enabled: true, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("job-visitor", "u_visitor", "")
	save("job-owner", cronE2EOwnerID, cronE2EOwnerID)
	// Pre-column row: no creator recorded, so it belongs to the owner.
	save("job-legacy", "", cronE2EOwnerID)

	a := &Agent{name: cronE2EAgentID, dataStore: db, ownerUserID: cronE2EOwnerID}

	visitor, ok := a.cronFingerprints("u_visitor")
	if !ok {
		t.Fatal("sampler reported not-ok for a readable store")
	}
	if _, leaked := visitor["job-owner"]; leaked {
		t.Errorf("visitor's sample carries the owner's job: %v", visitor)
	}
	if _, leaked := visitor["job-legacy"]; leaked {
		t.Errorf("visitor's sample carries a legacy owner job: %v", visitor)
	}
	if _, mine := visitor["job-visitor"]; !mine {
		t.Errorf("visitor's sample dropped their own job: %v", visitor)
	}

	owner, ok := a.cronFingerprints(cronE2EOwnerID)
	if !ok {
		t.Fatal("sampler reported not-ok for the owner")
	}
	if len(owner) != 3 {
		t.Errorf("owner's sample = %v, want all three jobs", owner)
	}
}
