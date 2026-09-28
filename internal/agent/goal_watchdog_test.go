package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// A goal's chain is held together by PostTurn hooks and there is no timer anywhere, so anything
// that keeps a hook from firing — a killed pod, a lost event, a failed state write — leaves the row
// `active` with nobody scheduled to move it. Production, 2026-09-28: 86 minutes of silence until
// the user typed "continue" themselves.
//
// The watchdog is the timer that was missing. These two cases pin both halves of its contract:
// a stalled goal is re-fired, and a goal whose agent is busy is left alone (that turn owns the
// chain and will fire the hook itself).
//
// Falsification: make SweepStalledGoals a no-op and the first case finds nothing on the bus;
// drop the `TurnInFlight` guard and the second case fires into a session that is mid-turn.
func TestTheGoalWatchdogRefiresAStalledGoal(t *testing.T) {
	newFixture := func(t *testing.T) (*Manager, *Agent, *store.DBStore, *bus.MessageBus, string) {
		t.Helper()
		base := t.TempDir()
		t.Setenv("FASTAGENT_HOME", base)
		db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(base, "goals.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		home := filepath.Join(base, "agents", "agt_wd", "agent")
		rc := config.ResolvedAgent{
			ID: "agt_wd", UserID: "u_owner", Home: home,
			Workspace: filepath.Join(home, "workspace"), Model: "fake-model",
			MaxTokens: 128, Temperature: 0.7, MaxToolIterations: 2,
		}
		mb := bus.New()
		mgr, err := NewManager([]config.ResolvedAgent{rc}, &learnerProbeProvider{reply: "ok"}, mb,
			WithUserID("u_owner"), WithDataStore(db))
		if err != nil {
			t.Fatalf("manager: %v", err)
		}
		ag := mgr.AgentByID(rc.ID)
		if ag == nil {
			t.Fatal("manager did not build the agent")
		}
		sess := ag.sessions.Get(sessionTriple(bus.InboundMessage{Channel: "web", ChatID: "chat-watchdog"}, ""))
		return mgr, ag, db, mb, sess.SessionKey()
	}

	seedGoal := func(t *testing.T, db *store.DBStore, sessionKey string) {
		t.Helper()
		if err := db.CreateGoal(context.Background(), &store.GoalRecord{
			ID: "g-watchdog", AgentID: "agt_wd", OwnerUserID: "u_owner",
			SessionKey: sessionKey, Channel: "web", ChatID: "chat-watchdog",
			Objective: "keep going even when a turn dies", Status: "active",
		}); err != nil {
			t.Fatalf("seed goal: %v", err)
		}
	}

	t.Run("a stalled goal gets its next continuation", func(t *testing.T) {
		mgr, _, db, mb, sessionKey := newFixture(t)
		seedGoal(t, db, sessionKey)

		// A negative threshold makes every row stale, which is how this test avoids sleeping ten
		// minutes: the sweep's staleness rule is exercised by the second case's guard, not here.
		mgr.SweepStalledGoals(context.Background(), -time.Minute)

		select {
		case cont := <-mb.Inbound:
			if cont.Source != bus.SourceGoalContext {
				t.Fatalf("continuation source = %q; want %q", cont.Source, bus.SourceGoalContext)
			}
			if cont.ChatID != "chat-watchdog" {
				t.Fatalf("continuation chat = %q; want the goal's session", cont.ChatID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the watchdog left a stalled goal alone — the chain stays broken until a human talks")
		}
	})

	t.Run("a goal whose agent is mid-turn is left alone", func(t *testing.T) {
		mgr, ag, db, mb, sessionKey := newFixture(t)
		seedGoal(t, db, sessionKey)
		sess := ag.sessions.GetByKey(sessionKey)
		if sess == nil {
			t.Fatalf("session %q not found", sessionKey)
		}
		if !sess.AcquireTurn(context.Background()) {
			t.Fatal("could not take the turn slot")
		}
		defer sess.ReleaseTurn()

		mgr.SweepStalledGoals(context.Background(), -time.Minute)
		select {
		case cont := <-mb.Inbound:
			t.Fatalf("the watchdog fired into a session that is mid-turn: %+v", cont)
		case <-time.After(200 * time.Millisecond):
			// nothing published: the running turn owns the chain
		}
	})
}
