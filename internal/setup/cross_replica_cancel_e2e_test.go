package setup

// The cancel half of the two-replica pair (the admission half is in
// cross_replica_turn_e2e_test.go; the harness is shared from there).
//
// POST /api/chat/cancel may land on either replica (design X1–X7): the browser
// talks to whichever pod the load balancer picked, while the turn that must stop
// is the one holding the session's lease. The two replicas share only the
// database, so the request has to ride the lease row to the holder and be read
// by the holder at its own iteration boundary. What the user must get: the turn
// stops, says so exactly once, and the session is free again.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
)

func TestCancelOnAnotherReplicaStopsTheRunningTurnE2E(t *testing.T) {
	prov := &toolScriptProvider{toolName: "slow_probe"}
	podA, podB, agA, _, db := newReplicaPairWithLease(t, prov, 4)
	started, release := registerBlockingTool(t, agA, "slow_probe")

	const session = "chat-xcancel"
	recA := newSSERecorder()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		podA.handleChatStream(recA, chatStreamRequest(t, chatRequest{
			AgentID: "agt_e2e", SessionID: session, Message: "long job", TurnID: "turn-xcancel-a",
		}))
	}()

	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("replica A's turn never reached the tool")
	}

	// The user hits Stop in a view served by replica B. The turn is RUNNING, so the id plays no
	// part in this path: the peer has no pending entry for it and the answer comes from stamping
	// the session's live lease (that is what `wasRunning:true` reports). The string below is
	// deliberately one no server ever minted — a stop must not need the submitter's identity.
	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel",
		strings.NewReader(`{"agentId":"agt_e2e","sessionId":"`+session+`","turnId":"turn-xcancel-a"}`))
	cancelReq = cancelReq.WithContext(auth.WithIdentity(cancelReq.Context(),
		auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	podB.handleChatCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel on the peer replica: status=%d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(cancelRec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("cancel answer is not JSON: %v", err)
	}
	if answer["canceled"] != true || answer["wasRunning"] != true || answer["isRunning"] != false {
		t.Fatalf("cancel answer = %v; want canceled=true wasRunning=true isRunning=false"+
			" (the turn was running on the other replica and the stamp landed on its row)", answer)
	}

	// The tool comes back; the turn reaches its next iteration boundary, where
	// the stamp is read.
	release()
	select {
	case <-doneA:
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled turn never finished; the stamp did not reach the holder")
	}

	stream := recA.body.String()
	if got := strings.Count(stream, "stopped at your request"); got != 1 {
		t.Fatalf("the stream carries the stop σ %d times, want exactly 1; stream=%q", got, stream)
	}
	// Stopping is a σ about the user's action, not about a budget: a cancelled
	// turn must not spend another model round — counted as *every* consultation
	// (`calls`), because the fall-through this guards against asks for no tool
	// and so leaves the tool-round counter untouched — and must not tell the UI
	// that the iteration cap was reached (the class of false badge this line of
	// work removes).
	if n := prov.calls.Load(); n != 1 {
		t.Fatalf("%d model calls after the stop request, want 1 (the turn kept spending); stream=%q", n, stream)
	}
	if strings.Contains(stream, "iterationCapReached") {
		t.Fatalf("a cancelled turn reported the iteration cap as reached; stream=%q", stream)
	}

	// The possession is freed, so the next turn — on either replica — starts
	// instead of queueing behind a turn that is already over.
	sess := agA.Sessions().Get("web", "", session, "")
	if sess == nil {
		t.Fatal("session not found")
	}
	row, err := db.GetSessionLease(context.Background(), "u_1", "agt_e2e", sess.Key())
	if err != nil {
		t.Fatalf("read the lease row: %v", err)
	}
	if row != nil {
		t.Fatalf("the stopped turn left its possession behind: %+v; the next turn would queue behind a dead turn", row)
	}
}
