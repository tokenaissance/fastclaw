package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Undo cursor lives in configs_kv per (agent, session) so undo never leaks
// across chats: name = "undo:<sessionKey>" under scope=agent. The consumed
// set holds the session_events identities already replayed; `mcp undo`
// replays the newest not-yet-consumed add/remove of the CURRENT session.
const (
	mcpUndoCursorKind  = "mcp_undo"
	mcpUndoCursorScope = "agent"
	mcpUndoCursorMax   = 50
)

// mcpToolUndo replays the inverse of the most recent recorded mcp
// add/remove operation IN THE CURRENT SESSION, in LIFO order, from the
// persistent session_events trace (type='tool_result' rows carrying the
// <mcp-undo> marker).
//
// Scope is intentionally per-session (direction A): a live session's rows
// are guaranteed present (deleting a session deletes its events), so undo
// never silently skips a newer operation whose session was removed.
// Only boundary-inside operations (server declarations) are replayed;
// login/logout involve provider consent/revocation (see §13.6).
func mcpToolUndo(ctx context.Context, ag *Agent, rc config.ResolvedAgent) (string, error) {
	if ag == nil || ag.dataStore == nil {
		return "", fmt.Errorf("mcp undo: unavailable — agent config store is not wired in this deployment")
	}
	sessionKey := ""
	if ag.registry != nil {
		sessionKey = ag.registry.SessionID()
	}
	if sessionKey == "" {
		return "", fmt.Errorf("mcp undo: no active chat journal — run it in the session that made the change")
	}
	events, err := ag.dataStore.ListSessionEventsSince(ctx, rc.UserID, rc.ID, sessionKey, -1)
	if err != nil {
		return "", fmt.Errorf("mcp undo: journal read failed: %w", err)
	}
	type candidate struct {
		id      string
		payload mcpUndoPayload
	}
	var candidates []candidate
	// ListSessionEventsSince is ascending by seq; undo needs newest first.
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.UserID != "" && rc.UserID != "" && ev.UserID != rc.UserID {
			continue
		}
		payload, ok := toolResultUndoPayload(ev.Data)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{
			id:      ev.SessionKey + ":" + fmt.Sprintf("%d", ev.Seq),
			payload: payload,
		})
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("mcp undo: no recorded add/remove in this session to undo")
	}

	consumed := map[string]bool{}
	cursorName := "undo:" + sessionKey
	if raw, err := ag.dataStore.GetConfigValue(ctx, mcpUndoCursorKind, mcpUndoCursorScope, rc.ID, cursorName); err == nil && raw != "" {
		var ids []string
		if json.Unmarshal([]byte(raw), &ids) == nil {
			for _, id := range ids {
				consumed[id] = true
			}
		}
	}
	idx := -1
	for i, c := range candidates {
		if !consumed[c.id] {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("mcp undo: no un-replayed add/remove left in this session")
	}
	chosen := candidates[idx]
	payload := chosen.payload
	switch payload.Action {
	case "remove":
		out, err := applyMCPRemove(ctx, ag, rc, payload.ServerName)
		if err != nil {
			return "", fmt.Errorf("mcp undo: remove of %q failed: %v (state changed since the op — check with mcp status)", payload.ServerName, err)
		}
		if err := markUndoCursor(ctx, ag.dataStore, rc.ID, cursorName, consumed, chosen.id); err != nil {
			return "", fmt.Errorf("mcp undo: remove of %q applied but failed to record the cursor: %v (repeating undo may replay it; check with mcp status)", payload.ServerName, err)
		}
		return "mcp undo: " + out, nil
	case "add":
		if payload.Config == nil {
			return "", fmt.Errorf("mcp undo: add of %q failed: recorded undo payload is missing the server config", payload.ServerName)
		}
		in := mcpAddInput{
			ServerName:    payload.ServerName,
			URL:           payload.Config.URL,
			OAuthResource: payload.Config.OAuthResource,
			Scopes:        payload.Config.Scopes,
		}
		out, err := applyMCPAdd(ctx, ag, rc, in)
		if err != nil {
			return "", fmt.Errorf("mcp undo: add of %q failed: %v (already exists or undo order broken — check with mcp status)", payload.ServerName, err)
		}
		if err := markUndoCursor(ctx, ag.dataStore, rc.ID, cursorName, consumed, chosen.id); err != nil {
			return "", fmt.Errorf("mcp undo: add of %q applied but failed to record the cursor: %v (repeating undo may replay it; check with mcp status)", payload.ServerName, err)
		}
		return "mcp undo: " + out, nil
	}
	return "", fmt.Errorf("mcp undo: unsupported undo action %q", payload.Action)
}

// markUndoCursor appends the replayed event id to the per-session consumed
// set and returns the persistence error: the inverse is already applied,
// so a failure is surfaced explicitly rather than silently risking a
// duplicate replay on the next undo.
func markUndoCursor(ctx context.Context, st store.Store, agentID, cursorName string, consumed map[string]bool, id string) error {
	consumed[id] = true
	ids := make([]string, 0, len(consumed))
	for k := range consumed {
		ids = append(ids, k)
	}
	raw, _ := json.Marshal(ids)
	if len(ids) > mcpUndoCursorMax {
		trimmed := ids[len(ids)-mcpUndoCursorMax:]
		raw, _ = json.Marshal(trimmed)
	}
	return st.SetConfigValue(ctx, mcpUndoCursorKind, mcpUndoCursorScope, agentID, cursorName, string(raw))
}

// toolResultUndoPayload extracts the <mcp-undo> marker from a persisted
// tool_result event. Event data shape is
// {"id":"…","name":"mcp","result":"…<mcp-undo>{…}"} (see loop.go).
func toolResultUndoPayload(data []byte) (mcpUndoPayload, bool) {
	var ev struct {
		Name   string `json:"name"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &ev); err != nil || ev.Name != "mcp" {
		return mcpUndoPayload{}, false
	}
	return extractUndoPayload(ev.Result)
}
