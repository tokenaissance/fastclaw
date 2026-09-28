package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// pendingWebTurn is a dashboard chat POST that has been accepted but whose turn
// has not started yet (it is queued behind another turn on the same session).
//
// The web UI offers "Edit"/"Cancel" on a queued message — Codex's queue
// preview offers the same via its edit-queued-message binding and interrupt.
// Withdrawing the message has to cancel this request's turn, so the cancel
// func lives here and the "started" bit (flipped by the agent's admission
// signal, not by an event from the shared session hub, which also carries
// other turns' events) decides whether withdrawal is still honest.
type pendingWebTurn struct {
	cancel  context.CancelFunc
	started bool
	// seq is the order this submission was ACCEPTED in. The map has no order of its own, and "the
	// earliest queued instruction" is a contract (§14.2's withdraw_task), so the registration assigns
	// one. It is a per-process counter on purpose: this queue's lifetime is the process (path 1 gave the
	// entry the wait's lifetime; a pod restart or a hand-off still loses it, and that boundary is
	// declared rather than papered over — see docs/mcp-task-submission.md §14.6).
	seq int64
}

// chatTurnKey identifies one client's chat POST. turnID is chosen by the
// client so a second tab cannot cancel this tab's queued message by accident.
func chatTurnKey(uid, agentID, sessionID, turnID string) string {
	return uid + "|" + agentID + "|" + sessionID + "|" + turnID
}

func (s *Server) registerPendingTurn(key string, cancel context.CancelFunc) *pendingWebTurn {
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	if s.pendingTurns == nil {
		s.pendingTurns = make(map[string]*pendingWebTurn)
	}
	s.pendingSeq++
	turn := &pendingWebTurn{cancel: cancel, seq: s.pendingSeq}
	s.pendingTurns[key] = turn
	return turn
}

// withdrawEarliestPendingTurn cancels the OLDEST queued submission for this (user, agent, session) and
// names it. One map, one lock: the scan is over that session's entries only, and the seq assigned at
// registration is what makes "earliest" decidable — a map iteration order would make it arbitrarily
// whichever entry the runtime happened to visit first.
//
// What it deliberately does NOT do: report the withdrawn instruction's text or its position. Neither
// lives in the pending entry (the text never reached this layer, and the position is a live count the
// emitter owns), so the answer names the submission and nothing else rather than inventing the rest.
func (s *Server) withdrawEarliestPendingTurn(uid, agentID, sessionID string) (string, bool) {
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	prefix := uid + "|" + agentID + "|" + sessionID + "|"
	var best *pendingWebTurn
	var bestTurnID string
	for key, turn := range s.pendingTurns {
		if turn.started || !strings.HasPrefix(key, prefix) {
			continue
		}
		if best == nil || turn.seq < best.seq {
			best, bestTurnID = turn, strings.TrimPrefix(key, prefix)
		}
	}
	if best == nil {
		return "", false
	}
	best.cancel()
	return bestTurnID, true
}

func (s *Server) unregisterPendingTurn(key string) {
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	delete(s.pendingTurns, key)
}

// markPendingTurnStarted flips the turn past the point where it can be
// withdrawn.
func (s *Server) markPendingTurnStarted(key string) {
	s.pendingTurnsMu.Lock()
	defer s.pendingTurnsMu.Unlock()
	if turn, ok := s.pendingTurns[key]; ok {
		turn.started = true
	}
}

type chatCancelRequest struct {
	AgentID   string `json:"agentId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	// WithdrawEarliest asks for the OTHER operation this endpoint answers (§14.2's withdraw_task):
	// cancel the oldest still-queued submission of the session, not the running turn. It is an explicit
	// field rather than "an empty turnId means withdraw" because an empty turnId already means "stop
	// whatever is running" to the dashboard's Stop path — measured, and load-bearing.
	WithdrawEarliest bool `json:"withdrawEarliest,omitempty"`
}

// handleChatCancel stops a turn the user no longer wants.
//
// Two shapes, one endpoint (design X1–X7, docs/session-turn-integrity.md A4):
//
//   - still queued  → withdraw it here by canceling its request context; the
//     turn never starts and nothing reaches the session. 200 {"canceled": true}
//   - already running → stamp a cancel request on the session's LIVE lease row.
//     The holder may be another replica; it reads the request at its next
//     iteration boundary (next to the fence check) and stops with a σ. 200
//     {"canceled": true, "wasRunning": true, "isRunning": false}
//   - nothing at all → 200 {"canceled": false, "wasRunning": false}. Deleting
//     the old 409 "already_started" is the point: that answer told the caller
//     to "use the normal stop", which only detached the client's stream while
//     the server kept working — a stop button that did not stop anything.
//
// Idempotent by construction: withdrawing twice is one withdrawal, and stamping
// the cancel twice is one request on the same possession.
func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request) {
	var req chatCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	if req.WithdrawEarliest {
		turnID, withdrawn := s.withdrawEarliestPendingTurn(uid, ag.Name(), req.SessionID)
		jsonResponse(w, http.StatusOK, map[string]any{
			"withdrawn": withdrawn,
			"turnId":    turnID,
		})
		return
	}

	key := chatTurnKey(uid, ag.Name(), req.SessionID, req.TurnID)
	s.pendingTurnsMu.Lock()
	pending, found := s.pendingTurns[key]
	s.pendingTurnsMu.Unlock()

	// The use case lives in turn_cancel.go: this handler only resolves the
	// caller and maps the outcome onto HTTP.
	canceled, wasRunning, err := cancelTurn(r.Context(), s.dataStore, pending, found, uid, ag.Name(), req.SessionID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"canceled":   canceled,
		"wasRunning": wasRunning,
		"isRunning":  false,
	})
}
