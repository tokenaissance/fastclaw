package cron

// e2e for commit b892d3f "fix(cron): include AccountID in fireJob
// InboundMessage" — first half: cron-scheduled proactive messages.
//
// Cloud zero-impact rationale: cron scheduling is a FastAgent-internal
// scheduler (DB-backed rows + bus.Inbound fire). Cloud (Next.js app)
// talks to FastAgent via the /api/fastagent proxy and never schedules
// cron rows itself; there is no Cloud entry point for this path. The
// fix lives in processDueJobs, the DB-backed scheduler the fork runs —
// the fired InboundMessage previously dropped AccountID, so the agent's
// reply hit "unknown outbound channel" and every cron-scheduled
// proactive message was silently lost.
//
// These tests drive processDueJobs directly with a real MessageBus +
// a recording fake StoreInterface (mirroring the store-backed
// scheduler wiring) to pin that:
//   - a due job fires an InboundMessage carrying its full (channel,
//     account, chat) triple + owner + cron source — the fix: AccountID
//     is no longer empty;
//   - when the destination channel adapter is missing, the pre-flight
//     (which also keys on AccountID) bumps the failure counter instead
//     of firing into the void.

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// fakeCronStore implements StoreInterface over in-memory state.
type fakeCronStore struct {
	due      []StoreJob
	locked   bool
	failures int
	deleted  bool
}

func (f *fakeCronStore) GetDueCronJobs(ctx context.Context, now time.Time) ([]StoreJob, error) {
	return f.due, nil
}
func (f *fakeCronStore) LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error) {
	return f.locked, nil
}
func (f *fakeCronStore) UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error {
	return nil
}
func (f *fakeCronStore) IncrementCronJobFailure(ctx context.Context, jobID string) (int, error) {
	f.failures++
	return f.failures, nil
}
func (f *fakeCronStore) DeleteCronJob(ctx context.Context, jobID string) error {
	f.deleted = true
	return nil
}
func (f *fakeCronStore) GetNextDueTime(ctx context.Context) (time.Time, error) {
	return time.Time{}, nil
}

// alwaysRegistered reports every (channel, account) as a live adapter.
type alwaysRegistered struct{}

func (alwaysRegistered) Has(channel, accountID string) bool { return true }

// neverRegistered reports no adapter — forces the pre-flight bump path.
type neverRegistered struct{}

func (neverRegistered) Has(channel, accountID string) bool { return false }

func TestCron_AccountID_CloudPathE2E(t *testing.T) {
	due := []StoreJob{{
		ID:          "job-1",
		AgentID:     "agent-1",
		OwnerUserID: "u_owner",
		Name:        "daily-ping",
		Type:        "interval",
		Schedule:    "24h",
		Message:     "ping",
		Channel:     "telegram",
		ChatID:      "chat-1",
		AccountID:   "acct-123",
	}}

	t.Run("fired-inbound-carries-accountid", func(t *testing.T) {
		mb := bus.New()
		s := NewSchedulerFromStore(&fakeCronStore{due: due, locked: true}, mb)
		s.SetChannelChecker(alwaysRegistered{})

		s.processDueJobs(context.Background())

		select {
		case msg := <-mb.Inbound:
			// The fix: AccountID was omitted from the struct literal, so
			// the outbound router got an empty account key and dropped
			// the reply as "unknown outbound channel".
			if msg.AccountID != "acct-123" {
				t.Errorf("fired InboundMessage.AccountID=%q, want acct-123", msg.AccountID)
			}
			if msg.Channel != "telegram" || msg.ChatID != "chat-1" {
				t.Errorf("fired triple = (%q,%q,%q), want (telegram,acct-123,chat-1)",
					msg.Channel, msg.AccountID, msg.ChatID)
			}
			if msg.AgentID != "agent-1" || msg.OwnerUserID != "u_owner" {
				t.Errorf("fired agent=%q owner=%q, want agent-1/u_owner", msg.AgentID, msg.OwnerUserID)
			}
			if msg.UserID != "cron" || msg.Source != bus.SourceCron {
				t.Errorf("fired user=%q source=%q, want cron/%q", msg.UserID, msg.Source, bus.SourceCron)
			}
			if msg.PeerKind != "dm" || msg.Text != "ping" {
				t.Errorf("fired peer=%q text=%q, want dm/ping", msg.PeerKind, msg.Text)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler never fired the due job into bus.Inbound")
		}
	})

	t.Run("missing-channel-bumps-failure-not-fire", func(t *testing.T) {
		mb := bus.New()
		st := &fakeCronStore{due: due, locked: true}
		s := NewSchedulerFromStore(st, mb)
		s.SetChannelChecker(neverRegistered{})

		s.processDueJobs(context.Background())

		if st.failures != 1 {
			t.Errorf("failure counter = %d, want 1 (missing adapter pre-flight)", st.failures)
		}
		select {
		case msg := <-mb.Inbound:
			t.Errorf("job fired into bus despite missing channel adapter: %+v", msg)
		default:
			// no message — correct
		}
	})
}
