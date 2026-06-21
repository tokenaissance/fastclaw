package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
)

func TestWithMessageTimestampsUsesExplicitChatterTimezone(t *testing.T) {
	store := newFakeMemoryStore()
	store.put(testAgentID, chatterUID, "USER.md", "# Current Chatter\n- Timezone: Asia/Shanghai")

	a := &Agent{
		memory:  NewMemoryWithStoreForUser("", store, ownerUID, testAgentID),
		agentID: testAgentID,
	}
	ts := time.Date(2026, 6, 21, 15, 9, 0, 0, time.UTC).UnixMilli()

	got := a.withMessageTimestampsForChatter([]provider.Message{{
		Role:      "user",
		Content:   "为什么是下午好",
		Timestamp: ts,
	}}, chatterUID)

	if len(got) != 1 {
		t.Fatalf("message count = %d, want 1", len(got))
	}
	if !strings.HasPrefix(got[0].Content, "[2026-06-21 23:09 Sun] ") {
		t.Fatalf("timestamp prefix = %q, want Asia/Shanghai local time", got[0].Content)
	}
}

func TestRuntimeContextUsesChatterTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder("", nil, "")
	cb.userID = ownerUID
	cb.SetTimezoneResolver(func(uid string) *time.Location {
		if uid == chatterUID {
			return loc
		}
		return time.UTC
	})

	got := cb.BuildRuntimeContextAs(chatterUID, "web", "chat-1")

	if !strings.Contains(got, "Timezone: Asia/Shanghai") {
		t.Fatalf("runtime context = %q, want chatter timezone", got)
	}
}
