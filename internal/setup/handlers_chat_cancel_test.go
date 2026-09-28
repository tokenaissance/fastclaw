package setup

import (
	"context"
	"strings"
	"testing"
)

// A queued turn can be withdrawn; once it started it cannot — that is the
// whole contract behind the dashboard's "Edit"/"Cancel" on a queued message
// (Codex's edit-last-queued-message binding makes the same distinction).
func TestPendingTurnRegistryWithdrawContract(t *testing.T) {
	s := &Server{}
	var canceled bool
	cancel := context.CancelFunc(func() { canceled = true })

	key := chatTurnKey("u_1", "agt_1", "s_1", "turn_1")
	s.registerPendingTurn(key, cancel)

	// Still queued: cancel reaches the request context and the turn never
	// starts.
	if turn := lookupForTest(s, key); turn == nil || turn.started {
		t.Fatal("freshly registered turn should be withdrawable")
	}
	turn := lookupForTest(s, key)
	turn.cancel()
	if !canceled {
		t.Fatal("withdrawing a queued turn must cancel its request context")
	}

	// Once the agent holds the slot the turn is past the point of no return.
	s.markPendingTurnStarted(key)
	if turn := lookupForTest(s, key); turn == nil || !turn.started {
		t.Fatal("started turn should not be withdrawable")
	}

	s.unregisterPendingTurn(key)
	if lookupForTest(s, key) != nil {
		t.Fatal("unregistered turn should be gone")
	}
}

// Another tab (or another session's turn) must not be able to withdraw this
// one: the key includes the user, the agent, the session and the client's
// turn id.
func TestPendingTurnKeyIsolatesTabsAndSessions(t *testing.T) {
	s := &Server{}
	key := chatTurnKey("u_1", "agt_1", "s_1", "turn_1")
	s.registerPendingTurn(key, func() {})

	for _, other := range []string{
		chatTurnKey("u_1", "agt_1", "s_1", "turn_2"),
		chatTurnKey("u_1", "agt_1", "s_2", "turn_1"),
		chatTurnKey("u_1", "agt_2", "s_1", "turn_1"),
		chatTurnKey("u_2", "agt_1", "s_1", "turn_1"),
	} {
		if lookupForTest(s, other) != nil {
			t.Fatalf("key %q collided with %q", other, key)
		}
	}
}

func lookupForTest(s *Server, key string) *pendingWebTurn {
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	return s.pendingTurns[key]
}

// pendingTurnIDsForTest returns the identities currently registered for one session, oldest
// registration order not implied. A caller can no longer name a submission in advance — the
// identity is minted at acceptance — so a test that wants "my queued turn" waits for the
// count and then reads the id from the σ (`queuedTurnID`), exactly like a real client.
func pendingTurnIDsForTest(s *Server, uid, agentID, sessionID string) []string {
	prefix := uid + "|" + agentID + "|" + sessionID + "|"
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	var ids []string
	for key := range s.pendingTurns {
		if strings.HasPrefix(key, prefix) {
			ids = append(ids, strings.TrimPrefix(key, prefix))
		}
	}
	return ids
}
