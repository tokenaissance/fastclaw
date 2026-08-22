package setup

// e2e for commit 35197e9 "shared_identity toggle".
//
// The upstream delta this commit carries lets a channel (or, via the
// agent-level update, every channel bound to an agent) opt in to shared
// identity: inbound IM messages then resolve to the channel owner's
// user_id instead of a per-platform chatter, so sessions and memory are
// shared across web + WeChat / Telegram / Discord / Slack / LINE / Feishu.
//
// These tests drive the real handlers through the same HTTP surfaces the
// Cloud proxy exercises — PATCH /api/agents/{id}/channels/{type}/{accountId}
// (handleUpdateAgentChannel) and PATCH /api/agents/{id} with a
// sharedIdentity payload (handleUpdateAgent) — and assert the toggles
// persist onto the channels table and surface back through
// handleGetAgent's agent-scope readback.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// seedSharedIdentityChannel creates a user + agent + a bound telegram
// channel row, returning (s, uid, aid) for handler driving.
func seedSharedIdentityChannel(t *testing.T, sharedIdentity bool) (*Server, string, string) {
	t.Helper()
	s, uid, aid := setupFileUploadTest(t)
	ctx := context.Background()
	if err := s.dataStore.SaveChannel(ctx, &store.ChannelRecord{
		UserID:         uid,
		AgentID:        aid,
		Type:           "telegram",
		AccountID:      "e2e_shared_bot",
		Enabled:        true,
		BotToken:       "FAKE",
		SharedIdentity: sharedIdentity,
	}); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return s, uid, aid
}

// ---------------------------------------------------------------------------
// PATCH /api/agents/{id}/channels/{type}/{accountId} — handleUpdateAgentChannel
// ---------------------------------------------------------------------------

func TestSharedIdentity_CloudPathE2E_PatchChannelToggle(t *testing.T) {
	s, uid, aid := seedSharedIdentityChannel(t, false)

	// Turn shared identity ON for the channel via the real PATCH handler.
	req := httptest.NewRequest(http.MethodPatch,
		"/api/agents/"+aid+"/channels/telegram/e2e_shared_bot",
		strings.NewReader(`{"sharedIdentity":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", aid)
	req.SetPathValue("type", "telegram")
	req.SetPathValue("accountId", "e2e_shared_bot")
	req = stampAuthAndUserID(req, uid)

	w := httptest.NewRecorder()
	s.handleUpdateAgentChannel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH toggle-on status = %d; body = %s", w.Code, w.Body.String())
	}

	// The row must have flipped in the store (channels table is the
	// read path for the gateway routing hot-path).
	row, err := s.dataStore.LookupChannel(context.Background(), "telegram", "e2e_shared_bot")
	if err != nil || row == nil {
		t.Fatalf("LookupChannel after toggle-on: rec=%+v err=%v", row, err)
	}
	if !row.SharedIdentity {
		t.Errorf("SharedIdentity after toggle-on = false; want true (row %+v)", row)
	}

	// Toggle OFF again through the same handler.
	req2 := httptest.NewRequest(http.MethodPatch,
		"/api/agents/"+aid+"/channels/telegram/e2e_shared_bot",
		strings.NewReader(`{"sharedIdentity":false}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.SetPathValue("id", aid)
	req2.SetPathValue("type", "telegram")
	req2.SetPathValue("accountId", "e2e_shared_bot")
	req2 = stampAuthAndUserID(req2, uid)

	w2 := httptest.NewRecorder()
	s.handleUpdateAgentChannel(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("PATCH toggle-off status = %d; body = %s", w2.Code, w2.Body.String())
	}
	row2, err := s.dataStore.LookupChannel(context.Background(), "telegram", "e2e_shared_bot")
	if err != nil || row2 == nil {
		t.Fatalf("LookupChannel after toggle-off: rec=%+v err=%v", row2, err)
	}
	if row2.SharedIdentity {
		t.Errorf("SharedIdentity after toggle-off = true; want false")
	}
}

// ---------------------------------------------------------------------------
// PATCH /api/agents/{id} with sharedIdentity — handleUpdateAgent batch toggle
// ---------------------------------------------------------------------------

func TestSharedIdentity_CloudPathE2E_UpdateAgentBatchToggle(t *testing.T) {
	s, uid, aid := seedSharedIdentityChannel(t, false)

	// The dashboard's agent-level toggle batches every channel bound to
	// the agent. Two channels to prove the batch covers all rows.
	ctx := context.Background()
	if err := s.dataStore.SaveChannel(ctx, &store.ChannelRecord{
		UserID: uid, AgentID: aid, Type: "feishu", AccountID: "e2e_feishu",
		Enabled: true, BotToken: "FAKE",
	}); err != nil {
		t.Fatalf("seed second channel: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/api/agents/"+aid,
		strings.NewReader(`{"sharedIdentity":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", aid)
	req = stampAuthAndUserID(req, uid)

	w := httptest.NewRecorder()
	s.handleUpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update-agent toggle-on status = %d; body = %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct{ typ, acct string }{
		{"telegram", "e2e_shared_bot"},
		{"feishu", "e2e_feishu"},
	} {
		row, err := s.dataStore.LookupChannel(ctx, tc.typ, tc.acct)
		if err != nil || row == nil {
			t.Fatalf("LookupChannel(%s,%s) after batch toggle: rec=%+v err=%v", tc.typ, tc.acct, row, err)
		}
		if !row.SharedIdentity {
			t.Errorf("batch toggle did not reach %s/%s: SharedIdentity=false", tc.typ, tc.acct)
		}
	}

	// handleGetAgent must surface sharedIdentity=true for the agent.
	getReq := httptest.NewRequest(http.MethodGet, "/api/agents/"+aid, nil)
	getReq.SetPathValue("id", aid)
	getReq = stampAuthAndUserID(getReq, uid)
	getW := httptest.NewRecorder()
	s.handleGetAgent(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("handleGetAgent status = %d; body = %s", getW.Code, getW.Body.String())
	}
	var resp struct {
		Agent map[string]any `json:"agent"`
	}
	if err := json.Unmarshal(getW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if got, _ := resp.Agent["sharedIdentity"].(bool); !got {
		t.Errorf("agent.sharedIdentity = %v; want true", resp.Agent["sharedIdentity"])
	}
}
