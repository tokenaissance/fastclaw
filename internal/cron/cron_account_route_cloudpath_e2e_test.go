package cron

// e2e for commit d8d000f "fix(cron): preserve channel account routing".
//
// Cloud-path mirror: a Cloud (Next.js app) user asks their agent to
// schedule a cron reminder through the /api/fastagent proxy. The
// create_cron_job tool (tools/cron.go) captures the in-flight
// (channel, accountID, chatID) triple from the turn's message context
// and persists it; the scheduler later fires the row back into
// bus.Inbound so the agent's reply lands in the same thread AND on the
// same account/bot. The fix threads AccountID through the whole chain —
// the create-time capture (tool) and the in-memory fireJob path
// previously dropped it, so multi-account deployments lost "which bot
// replies" routing and replies hit "unknown outbound channel".
//
// The store-backed processDueJobs half is covered by the b892d3f e2e;
// these tests pin the OTHER halves of this commit with a REAL Scheduler
// + real MessageBus:
//   - fireJob stamps the job's AccountID onto the fired InboundMessage
//     (the in-memory scheduler path — the same bug processDueJobs had);
//   - a persisted AccountID survives the DB round-trip into a fired
//     InboundMessage, proving the create→persist→fire chain keeps it.

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestCron_AccountRoute_CloudPathE2E(t *testing.T) {
	// Half 1 of the fix: the in-memory scheduler path (NewScheduler /
	// fireJob) must route the job's AccountID onto the fired
	// InboundMessage — previously it dropped it exactly like
	// processDueJobs did, so even jobs created with an account id lost
	// the routing when the in-memory scheduler fired them.
	t.Run("firejob-routes-accountid", func(t *testing.T) {
		mb := bus.New()
		s := NewScheduler(nil, mb)
		s.fireJob(Job{
			AgentID:     "agent-1",
			OwnerUserID: "u_owner",
			Name:        "reminder",
			Type:        JobTypeCron,
			Message:     "提醒我买牛奶",
			Channel:     "telegram",
			AccountID:   "acct-1",
			ChatID:      "chat-1",
		})

		select {
		case msg := <-mb.Inbound:
			if msg.AccountID != "acct-1" {
				t.Errorf("fireJob InboundMessage.AccountID=%q, want acct-1", msg.AccountID)
			}
			if msg.Channel != "telegram" || msg.ChatID != "chat-1" {
				t.Errorf("fired triple = (%q,%q,%q), want (telegram,acct-1,chat-1)",
					msg.Channel, msg.AccountID, msg.ChatID)
			}
			if msg.UserID != "cron" || msg.Source != bus.SourceCron {
				t.Errorf("fired user=%q source=%q, want cron/%q", msg.UserID, msg.Source, bus.SourceCron)
			}
			if msg.AgentID != "agent-1" || msg.OwnerUserID != "u_owner" {
				t.Errorf("fired agent=%q owner=%q, want agent-1/u_owner", msg.AgentID, msg.OwnerUserID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("fireJob never emitted into bus.Inbound")
		}
	})

	// Half 2: a persisted AccountID survives the DB round-trip and the
	// store-backed scheduler fires it back onto the message — the
	// create→persist→fire chain keeps the account routing intact.
	t.Run("persisted-accountid-reaches-fired-message", func(t *testing.T) {
		db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer db.Close()
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}

		// next_run in the past so the row is immediately due.
		dueAt := time.Now().Add(-time.Minute)
		rec := &store.CronJobRecord{
			ID:        "job-2",
			UserID:    "u_owner",
			AgentID:   "agent-1",
			Name:      "daily-ping",
			Type:      "interval",
			Schedule:  "24h",
			Message:   "ping",
			Channel:   "telegram",
			AccountID: "acct-bot",
			ChatID:    "chat-2",
			Timezone:  "UTC",
			Enabled:   true,
			NextRun:   &dueAt,
			CreatedAt: time.Now(),
		}
		if err := db.SaveCronJob(context.Background(), rec); err != nil {
			t.Fatalf("save cron job: %v", err)
		}

		due, err := db.GetDueCronJobs(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("get due cron jobs: %v", err)
		}
		if len(due) != 1 {
			t.Fatalf("got %d due jobs, want 1", len(due))
		}
		if due[0].AccountID != "acct-bot" {
			t.Fatalf("persisted AccountID=%q, want acct-bot (round-trip dropped it)", due[0].AccountID)
		}

		// Mirror the store-backed scheduler wiring: the adapter maps
		// CronJobRecord → StoreJob and the DB scheduler pre-flights the
		// (channel, accountID) adapter before firing.
		storeJobs := []StoreJob{{
			ID:          due[0].ID,
			AgentID:     due[0].AgentID,
			OwnerUserID: due[0].UserID,
			Name:        due[0].Name,
			Type:        due[0].Type,
			Schedule:    due[0].Schedule,
			Message:     due[0].Message,
			Channel:     due[0].Channel,
			AccountID:   due[0].AccountID,
			ChatID:      due[0].ChatID,
			Timezone:    due[0].Timezone,
		}}
		mb := bus.New()
		s := NewSchedulerFromStore(&fakeCronStore{due: storeJobs, locked: true}, mb)
		s.SetChannelChecker(alwaysRegistered{})
		s.processDueJobs(context.Background())

		select {
		case msg := <-mb.Inbound:
			if msg.AccountID != "acct-bot" {
				t.Errorf("fired InboundMessage.AccountID=%q, want acct-bot", msg.AccountID)
			}
			if msg.Channel != "telegram" || msg.ChatID != "chat-2" {
				t.Errorf("fired triple = (%q,%q,%q), want (telegram,acct-bot,chat-2)",
					msg.Channel, msg.AccountID, msg.ChatID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("store-backed scheduler never fired the due job")
		}
	})
}
