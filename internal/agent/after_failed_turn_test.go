package agent

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent/goal"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// errProvider fails every call with a terminal error. Context errors are not retried
// (llmRetry says so explicitly), so this models the production shape exactly: a turn whose
// deadline expired comes back as one error and goes straight to the error exit.
type errProvider struct{ err error }

func (p *errProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, p.err
}

func (p *errProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, p.err
}

// A goal's continuation is a PostTurn hook, so a turn that ended in FAILURE used to break the
// chain for good: the early return skipped PostTurn, no continuation was published, and the goal
// sat `active` with nobody scheduled to move it.
//
// Production, 2026-09-28: a goal turn cut at its 300 s budget left exactly that — 86 minutes of
// silence until the user typed "continue"; a provider 402 an hour earlier had broken the same
// chain the same way.
//
// Falsification: drop the `afterFailedTurn` calls from the error exits and this test finds nothing
// on the bus — the goal stalls in the fixture exactly as it did in production.
func TestAFailedTurnStillFiresTheGoalContinuation(t *testing.T) {
	cases := []struct {
		name string
		prov provider.Provider
	}{
		{"the provider is missing", nil},
		{"the provider failed past the turn's deadline", &errProvider{err: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newGateAgent(t)
			a.provider = tc.prov
			st := &memGoalStore{}
			a.WireGoals(st)

			msg := bus.InboundMessage{Channel: "web", UserID: "u_owner", ChatID: "chat-goal-failed", Text: "start"}
			sess := a.sessions.Get(sessionTriple(msg, msg.ProjectID))
			if err := st.CreateGoal(context.Background(), &goal.Goal{
				ID:          "g-failed-turn",
				AgentID:     a.name,
				OwnerUserID: a.ownerUserID,
				SessionKey:  sess.SessionKey(),
				Channel:     msg.Channel,
				ChatID:      msg.ChatID,
				Objective:   "keep going even when a turn dies",
				Status:      goal.StatusActive,
			}); err != nil {
				t.Fatalf("seed goal: %v", err)
			}

			done := make(chan string, 1)
			go func() { done <- a.HandleMessage(context.Background(), msg) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the failed turn never returned")
			}

			select {
			case cont := <-a.messageBus.Inbound:
				if cont.Source != bus.SourceGoalContext {
					t.Fatalf("continuation source = %q; want %q", cont.Source, bus.SourceGoalContext)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("a failed turn published no continuation — the goal chain is broken (the production symptom)")
			}
		})
	}
}
