package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/auth"
)

// A finished turn stamps its ending, and the history read hands it over: the producer
// (internal/agent/endings.go) and the reader meet here. Falsification: drop the `ending` from
// that `done` emit and both halves of this test redden — the stream stops saying it and the
// history stops carrying it.
func TestAFinishedTurnStampsItsEndingE2E(t *testing.T) {
	// A store-backed pod, because the ending is read from `session_events`: the plain chat harness
	// has no dataStore, so nothing is persisted and there would be nothing to read.
	s, _, _, _, _ := newReplicaPairWithLease(t, &e2eProvider{started: make(chan struct{}, 4), reply: "done"}, 1)
	rec := newSSERecorder()
	waitForHandler(t, postChatStream(t, s, rec, chatRequest{
		AgentID: "agt_e2e", SessionID: "chat-ending", Message: "go",
	}, "client-a"), "the POST")
	if !rec.seen(`"ending":"replied"`) {
		t.Fatalf("the closing event carried no ending: %q", rec.snapshot())
	}

	histRec := httptest.NewRecorder()
	histReq := httptest.NewRequest(http.MethodGet,
		"/api/chat/history?agentId=agt_e2e&sessionId=chat-ending", nil)
	histReq = histReq.WithContext(auth.WithIdentity(histReq.Context(),
		auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
	s.handleChatHistory(histRec, histReq)
	var body map[string]any
	if err := json.Unmarshal(histRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if body["lastEnding"] != "replied" {
		t.Fatalf("history lastEnding = %v; want %q (the reader must see what the producer stamped)",
			body["lastEnding"], "replied")
	}
}

// The de-duplication contract of docs/fastagent/design/14-turn-identity.md §3.1, exercised the
// way a client meets it: a POST to /api/chat/stream. The unit under test is the acceptance
// critical section, so every case asks the same question from a different side — did the model
// run once or twice?
//
// Falsification, run for the record when this landed: drop the `idempotencyVerdictFor` call
// from handleChatStream and `a retry does not run the model twice` goes red (startCount 2);
// widen the scope key by removing `client` and `two clients sharing a key are two
// instructions` goes red (the second client is answered from the first one's entry).

func postChatStream(t *testing.T, s *Server, rec *sseRecorder, body chatRequest, client string) chan struct{} {
	t.Helper()
	req := chatStreamRequest(t, body)
	if client != "" {
		req.Header.Set("X-Fastagent-Client", client)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleChatStream(rec, req)
	}()
	return done
}

func waitForHandler(t *testing.T, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never returned", what)
	}
}

func TestARetryOfOneInstructionDoesNotBecomeTwoE2E(t *testing.T) {
	s, ag, prov := newQueuedChatHarness(t)
	first := newSSERecorder()
	waitForHandler(t, postChatStream(t, s, first, chatRequest{
		AgentID: "agt_e2e", SessionID: "chat-idem", Message: "go", IdempotencyKey: "k-1",
	}, "client-a"), "the first POST")
	if got := prov.startCount(); got != 1 {
		t.Fatalf("model ran %d times after the first POST; want 1", got)
	}

	// The retry: same key, same content, same client.
	second := newSSERecorder()
	waitForHandler(t, postChatStream(t, s, second, chatRequest{
		AgentID: "agt_e2e", SessionID: "chat-idem", Message: "go", IdempotencyKey: "k-1",
	}, "client-a"), "the retry")

	if got := prov.startCount(); got != 1 {
		t.Fatalf("the retry reached the model: %d runs; want 1 (the first submission stands)", got)
	}
	if !second.seen(`"type":"duplicate"`) {
		t.Fatalf("the retry was not answered as a duplicate: %q", second.snapshot())
	}
	sess := ag.Sessions().Get("web", "", "chat-idem", "")
	if sess == nil {
		t.Fatal("session not found")
	}
	users := 0
	for _, m := range sess.GetMessages() {
		if m.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("the session holds %d user messages; want exactly the first instruction", users)
	}

	// Same key, different content: refused loudly, never a silent overwrite (§3.1 item 5).
	conflict := newSSERecorder()
	waitForHandler(t, postChatStream(t, s, conflict, chatRequest{
		AgentID: "agt_e2e", SessionID: "chat-idem", Message: "something else", IdempotencyKey: "k-1",
	}, "client-a"), "the conflicting POST")
	if conflict.statusCode() != http.StatusConflict {
		t.Fatalf("same key + different content answered %d (%q); want 409",
			conflict.statusCode(), conflict.snapshot())
	}
	if !strings.Contains(conflict.snapshot(), "idempotency_conflict") {
		t.Fatalf("the refusal does not name the reason: %q", conflict.snapshot())
	}
	if got := prov.startCount(); got != 1 {
		t.Fatalf("the refused POST reached the model: %d runs; want 1", got)
	}

	// A NEW instruction is a NEW key — and it runs (§3.1 item 2).
	third := newSSERecorder()
	waitForHandler(t, postChatStream(t, s, third, chatRequest{
		AgentID: "agt_e2e", SessionID: "chat-idem", Message: "go", IdempotencyKey: "k-2",
	}, "client-a"), "the new instruction")
	if got := prov.startCount(); got != 2 {
		t.Fatalf("a new key did not start a new turn: %d runs; want 2", got)
	}
}

// §3.1 item 1 / audit finding F: the domain does not cross authorization boundaries. Two clients
// sharing a key and content produce TWO instructions — anything else would silently swallow the
// second client's work.
func TestTwoClientsSharingAKeyAreTwoInstructionsE2E(t *testing.T) {
	s, _, prov := newQueuedChatHarness(t)
	for i, client := range []string{"client-a", "client-b"} {
		rec := newSSERecorder()
		waitForHandler(t, postChatStream(t, s, rec, chatRequest{
			AgentID: "agt_e2e", SessionID: "chat-idem-clients", Message: "go", IdempotencyKey: "shared",
		}, client), "POST from "+client)
		if rec.seen(`"type":"duplicate"`) {
			t.Fatalf("client %d was answered from the other client's entry: %q", i+1, rec.snapshot())
		}
	}
	if got := prov.startCount(); got != 2 {
		t.Fatalf("model ran %d times for two clients sharing a key; want 2", got)
	}
}

// A retried steer folds the sentence once (§3.1 applies to the steer verb too).
func TestARetryOfASteerFoldsOnceE2E(t *testing.T) {
	s, ag, _ := newQueuedChatHarness(t)
	sess := ag.Sessions().Get("web", "", "chat-steer-idem", "")
	if sess == nil {
		t.Fatal("session not found")
	}
	// A steer folds only into a turn that is in flight; BeginTurn is what makes one.
	sess.BeginTurn()
	defer sess.EndTurn()

	steer := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/chat/steer",
			strings.NewReader(`{"agentId":"agt_e2e","sessionId":"chat-steer-idem","message":"hurry up","idempotencyKey":"s-1"}`))
		req.Header.Set("X-Fastagent-Client", "client-a")
		req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "u_1", Role: "user", AuthMethod: "session"}))
		s.handleChatSteer(rec, req)
		return rec
	}
	first := steer()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"buffered":true`) {
		t.Fatalf("the steer did not fold: %d %s", first.Code, first.Body.String())
	}
	second := steer()
	var body map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode second steer: %v", err)
	}
	if body["duplicate"] != true {
		t.Fatalf("the retried steer was not answered as a duplicate: %s", second.Body.String())
	}
	if got := len(sess.DrainSteer()); got != 1 {
		t.Fatalf("the buffered steer holds %d entries; want 1 (the retry must not fold twice)", got)
	}
}
