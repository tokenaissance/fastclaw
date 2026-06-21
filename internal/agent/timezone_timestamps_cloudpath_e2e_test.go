package agent

// e2e for commit 61c9e5c "fix agent chatter timezone timestamps".
//
// Cloud zero-impact rationale: this is prompt-rendering only. Cloud
// (Next.js app) posts /chat messages through the /api/fastagent proxy
// and reads back the reply; the timezone of the rendered "[HH:MM]"
// message prefixes and the "[Runtime Context]" Time/Timezone lines is
// invisible to the client — it only changes what the LLM sees as the
// current wall clock. No endpoint, response shape, or auth change.
//
// The bug being fixed: message timestamps were rendered in
// a.registry.ChatterUserID()'s timezone, while the runtime context's
// Time line used a different resolution — so in paths where the
// registry's current chatter disagreed with the turn's actual chatter,
// the LLM saw two different clocks in one prompt. The fix threads an
// explicit chatterUID through withMessageTimestampsForChatter and
// BuildRuntimeContextAs so both render in the same per-turn chatter
// zone.
//
// These tests drive the REAL renamed helpers
// (withMessageTimestampsForChatter + BuildRuntimeContextAs) with a
// bare Agent / ContextBuilder, mirroring the loop's turn-time usage —
// the same pattern as timezone_e2e_test.go, which deliberately avoids
// spinning up a full Manager/loop for a pure-rendering path.

import (
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

// Runtime context's Time + Timezone lines follow the chatter, not
// server-local. Delegation BuildRuntimeContext → As uses cb.userID.
func TestTimezoneTimestamps_RuntimeContext_InChatterZone(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	cb := NewContextBuilder("", nil, "")
	cb.userID = "u_owner"
	cb.SetTimezoneResolver(func(uid string) *time.Location {
		if uid == "u_other" {
			return sh
		}
		return time.UTC
	})

	// Explicit chatter — the fix: rendered in the chatter's zone.
	got := cb.BuildRuntimeContextAs("u_other", "web", "chat-1")
	if !strings.Contains(got, "Timezone: Asia/Shanghai") {
		t.Errorf("BuildRuntimeContextAs(u_other): no chatter zone, got %q", got)
	}
	// The Time line itself must be a wall clock in that zone (not UTC).
	if strings.Contains(got, "Timezone: UTC") {
		t.Errorf("BuildRuntimeContextAs(u_other): leaked server/UTC zone, got %q", got)
	}
}

// Same message list rendered for two chatters of the same agent gets
// two different timestamp prefixes — timestamps follow the per-turn
// chatter (the explicit chatterUID argument), not a shared
// registry.ChatterUserID() value.
func TestTimezoneTimestamps_MessagePrefix_PerChatterZone(t *testing.T) {
	store := newFakeMemoryStore()
	a := &Agent{
		memory:  NewMemoryWithStoreForUser("", store, ownerUID, testAgentID),
		agentID: testAgentID,
	}
	store.put(testAgentID, chatterUID, "USER.md", "# Current Chatter\n- Timezone: Asia/Shanghai")
	store.put(testAgentID, "la_chatter", "USER.md", "# Current Chatter\n- Timezone: America/Los_Angeles")

	msgs := []provider.Message{{
		Role:      "user",
		Content:   "为什么是下午好",
		Timestamp: time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli(),
	}}

	// Same messages, two chatters → two clocks. 15:09 UTC = 23:09 +08 =
	// 08:09 PDT (June, DST active). A shared/registry-based renderer
	// could only produce one of these for the whole session; the fix
	// renders per per-turn chatter.
	gotSH := a.withMessageTimestampsForChatter(msgs, chatterUID)
	gotLA := a.withMessageTimestampsForChatter(msgs, "la_chatter")

	if !strings.HasPrefix(gotSH[0].Content, "[2026-06-21 23:09 Sun] ") {
		t.Errorf("Asia/Shanghai chatter: prefix %q, want [2026-06-21 23:09 Sun]", gotSH[0].Content)
	}
	if !strings.HasPrefix(gotLA[0].Content, "[2026-06-21 08:09 Sun] ") {
		t.Errorf("America/Los_Angeles chatter: prefix %q, want [2026-06-21 08:09 Sun]", gotLA[0].Content)
	}
}

// The fix's intent: runtime context Time line and message timestamp
// prefixes now agree on the SAME chatter zone inside one prompt.
func TestTimezoneTimestamps_RuntimeAndMessages_Agree(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	cb := NewContextBuilder("", nil, "")
	cb.userID = ownerUID
	cb.SetTimezoneResolver(func(uid string) *time.Location { return sh })

	store := newFakeMemoryStore()
	a := &Agent{
		memory:  NewMemoryWithStoreForUser("", store, ownerUID, testAgentID),
		agentID: testAgentID,
	}
	store.put(testAgentID, chatterUID, "USER.md", "时区: Asia/Shanghai")

	// Both renderers resolve Asia/Shanghai for the same chatter.
	rc := cb.BuildRuntimeContextAs(chatterUID, "web", "chat-1")
	if !strings.Contains(rc, "Timezone: Asia/Shanghai") {
		t.Errorf("runtime context: %q, want Asia/Shanghai", rc)
	}
	got := a.withMessageTimestampsForChatter([]provider.Message{{
		Role:      "user",
		Content:   "hi",
		Timestamp: time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli(),
	}}, chatterUID)
	if !strings.HasPrefix(got[0].Content, "[2026-06-21 23:09 Sun] ") {
		t.Errorf("message prefix %q, want [2026-06-21 23:09 Sun] (same zone as runtime context)", got[0].Content)
	}
}
