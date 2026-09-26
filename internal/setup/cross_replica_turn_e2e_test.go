package setup

// Two gateway replicas, one store — the shape row 32 of
// docs/fs-formal-proof/11-change-register.md held open. What separates a
// replica from a plain server is the lease: the in-process turn gate
// (session.AcquireTurn) cannot see a peer, so without a store-backed lease the
// two servers below would each happily run the same session's history.
//
// The existing cross-replica test (chat_event_delivery_e2e_test.go) posts to a
// single replica and wires no lease, so it exercises the hub, not admission.
// This one posts to both; the cancel half — the request landing on the replica
// that is not running the turn — is in cross_replica_cancel_e2e_test.go.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// newReplicaPairWithLease is newReplicaPair plus the production wiring: one
// store, two servers, and the store-backed turn lease on both agents. Everything
// the two replicas can know about each other has to travel through the database,
// which is exactly what makes this pair a pair of replicas.
func newReplicaPairWithLease(t *testing.T, prov provider.Provider, maxToolIterations int) (podA, podB *Server, agA, agB *agent.Agent, db *store.DBStore) {
	t.Helper()
	var err error
	db, err = store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	lease := gateway.NewStoreSessionLease(db)
	podA, agA = newChatHarnessWith(t, prov, maxToolIterations, agent.WithSessionLease(lease))
	podB, agB = newChatHarnessWith(t, prov, maxToolIterations, agent.WithSessionLease(lease))
	podA.dataStore, podB.dataStore = db, db
	if podA.chatEventHub() == podB.chatEventHub() {
		t.Fatal("the two replicas must not share an event hub, or the test proves nothing")
	}
	return podA, podB, agA, agB, db
}

// registerInstantTool registers a tool that answers immediately, under a name
// another test uses for a blocking one: a replica that is only a *peer* in a
// test still has to be able to run its own turn to completion.
func registerInstantTool(t *testing.T, ag *agent.Agent, name string) {
	t.Helper()
	ag.ToolRegistry().Register(name, "test tool that answers at once", nil,
		func(context.Context, json.RawMessage) (string, error) { return "released", nil })
}

// A turn is running on replica A, parked inside a tool. A second POST for the
// same session arrives on replica B, which shares nothing with A but the
// database. B must not start a second turn: it announces that it is queued,
// names the holder it is waiting for and when that possession lapses, and does
// not reach the model. When A finishes, B's turn runs.
func TestSecondReplicaQueuesBehindTheRunningTurnE2E(t *testing.T) {
	prov := &toolScriptProvider{toolName: "slow_probe"}
	podA, podB, agA, agB, _ := newReplicaPairWithLease(t, prov, 4)
	started, release := registerBlockingTool(t, agA, "slow_probe")
	registerInstantTool(t, agB, "slow_probe")

	const session = "chat-xreplica"
	recA := newSSERecorder()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		podA.handleChatStream(recA, chatStreamRequest(t, chatRequest{
			AgentID: "agt_e2e", SessionID: session, Message: "first", TurnID: "turn-xreplica-a",
		}))
	}()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("replica A's turn never reached the tool")
	}

	// The second writer, on the other replica.
	recB := newSSERecorder()
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		podB.handleChatStream(recB, chatStreamRequest(t, chatRequest{
			AgentID: "agt_e2e", SessionID: session, Message: "second", TurnID: "turn-xreplica-b",
		}))
	}()

	waitForBody(t, recB, `"type":"queued"`, 15*time.Second)
	queued := recB.body.String()
	// "position 1" alone cannot say whose turn you are waiting for, nor how long
	// the wait is bounded by (docs/session-turn-integrity.md A4.1).
	if !strings.Contains(queued, `"holder":"`) {
		t.Fatalf("the queue σ did not name the holder it waits for: %q", queued)
	}
	if !strings.Contains(queued, `"expiresAt":"`) {
		t.Fatalf("the queue σ did not say when the possession lapses: %q", queued)
	}
	// …nor whose submission it is. The client that POSTed knows its own id; a tab that learns the
	// queue fact from a re-emission (a reload, a second tab) does not, and without it its withdraw
	// control cannot name the turn to `POST /api/chat/cancel`. Measured on the live two-tab spec
	// (cloud e2e/tests/live/queue-send-behind-a-peer.spec.ts) before this field existed.
	if !strings.Contains(queued, `"turnId":"turn-xreplica-b"`) {
		t.Fatalf("the queue σ did not name the waiting submission: %q", queued)
	}
	// The announcement is the only thing that may have happened on B: a model
	// round here would mean a second turn started on a session A is running.
	if n := prov.rounds.Load(); n != 1 {
		t.Fatalf("%d model rounds while replica A held the session, want 1"+
			" (replica B started a turn of its own); stream=%q", n, recB.body.String())
	}

	release()
	for _, d := range []struct {
		name string
		done chan struct{}
		rec  *sseRecorder
	}{{"A", doneA, recA}, {"B", doneB, recB}} {
		select {
		case <-d.done:
		case <-time.After(20 * time.Second):
			t.Fatalf("replica %s's turn never finished; stream=%q", d.name, d.rec.body.String())
		}
		if !strings.Contains(d.rec.body.String(), "done") {
			t.Fatalf("replica %s's turn delivered no answer; stream=%q", d.name, d.rec.body.String())
		}
	}
}
