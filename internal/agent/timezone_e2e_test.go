package agent

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// E2E coverage for the two timezone commits at the agent layer:
//
//   - 303b8aa / 983df17: Agent.chatterLocation parses the chatter's
//     USER.md FIRST and lets it beat the DB prefs, so editing USER.md
//     is enough to fix the clock without running set_timezone.
//   - d4b7a63 / 1676128: timezone is a property of the chatter, not the
//     agent — different chatters of the same agent each resolve their
//     own wall clock, and the system prompt's date line is rendered in
//     it (tzResolver).
//
// NOTE: we deliberately exercise Agent.chatterLocation and the
// ContextBuilder date line, NOT the full agent loop. chatterLocation is
// a pure read of (memory, dataStore, agentID) so a bare *Agent with
// those three fields wired reproduces the exact code path the loop uses
// at turn time — spinning up a Manager/loop would pull in providers,
// sessions, sandboxes, and MCP managers for no additional coverage.

// zoneOffsetOf returns the seconds-east-of-UTC a location applies on a
// fixed reference instant (2026-06-14, no DST oddities for the zones
// tested). Comparing offsets is more robust than comparing
// Location.String().
func zoneOffsetOf(t *testing.T, loc *time.Location) int {
	t.Helper()
	ref := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	_, off := ref.In(loc).Zone()
	return off
}

// Covers 303b8aa: chatterLocation must parse the chatter's USER.md and
// let it beat the DB prefs (the commit's core behavior — editing
// USER.md fixes the clock without running set_timezone). With no
// USER.md it falls to DB prefs; with neither it returns server-local.
func TestAgentChatterLocation_UserMDWinsOverDBPrefs(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	const agentID = "agent-tz"

	// u_a: DB prefs say Europe/Berlin, but USER.md says Asia/Shanghai.
	if err := scope.SaveUserTimezone(ctx, db, "u_a", "Europe/Berlin"); err != nil {
		t.Fatalf("save u_a prefs: %v", err)
	}
	if err := db.SaveAgentFile(ctx, agentID, "u_a", "USER.md", []byte("基本信息\n- 时区: Asia/Shanghai\n- 职业: 会计")); err != nil {
		t.Fatalf("save u_a USER.md: %v", err)
	}
	// u_b: DB prefs Europe/Berlin, no USER.md row.
	if err := scope.SaveUserTimezone(ctx, db, "u_b", "Europe/Berlin"); err != nil {
		t.Fatalf("save u_b prefs: %v", err)
	}
	// u_c: nothing set anywhere.

	mem := NewMemoryWithStoreForUser("", NewMemoryStoreAdapter(db), "owner", agentID)
	a := &Agent{memory: mem, dataStore: db, agentID: agentID}

	// USER.md beats DB prefs.
	if loc := a.chatterLocation("u_a"); zoneOffsetOf(t, loc) != 8*3600 {
		t.Errorf("u_a: want Asia/Shanghai (+8h), got %v", loc)
	}
	// No USER.md → DB prefs.
	if loc := a.chatterLocation("u_b"); zoneOffsetOf(t, loc) != 2*3600 {
		t.Errorf("u_b: want Europe/Berlin (+2h), got %v", loc)
	}
	// Neither → server-local, never nil.
	if loc := a.chatterLocation("u_c"); loc == nil || loc != time.Local {
		t.Errorf("u_c: want server-local, got %v", loc)
	}
}

// Covers d4b7a63 + 303b8aa: two chatters of the same agent each resolve
// their own wall clock from their own USER.md and never bleed into each
// other.
func TestAgentChatterLocation_PerChatterIsolation(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	const agentID = "agent-shared"

	if err := db.SaveAgentFile(ctx, agentID, "chatter-a", "USER.md", []byte("时区: Asia/Shanghai")); err != nil {
		t.Fatalf("save A USER.md: %v", err)
	}
	if err := db.SaveAgentFile(ctx, agentID, "chatter-b", "USER.md", []byte("Timezone: America/Los_Angeles")); err != nil {
		t.Fatalf("save B USER.md: %v", err)
	}

	mem := NewMemoryWithStoreForUser("", NewMemoryStoreAdapter(db), "owner", agentID)
	a := &Agent{memory: mem, dataStore: db, agentID: agentID}

	locA := a.chatterLocation("chatter-a")
	locB := a.chatterLocation("chatter-b")

	if offA, offB := zoneOffsetOf(t, locA), zoneOffsetOf(t, locB); offA == offB {
		t.Fatalf("expected distinct offsets, both %d", offA)
	}
	if zoneOffsetOf(t, locA) != 8*3600 {
		t.Errorf("chatter-a: want Asia/Shanghai (+8h), got %v", locA)
	}
	if zoneOffsetOf(t, locB) != -7*3600 {
		t.Errorf("chatter-b: want America/Los_Angeles (-7h PDT), got %v", locB)
	}
}

// Covers d4b7a63 (injection half): the system prompt's date line is
// rendered in the CHATTER's timezone via the ContextBuilder's
// tzResolver, so the model sees a wall clock that matches the person
// typing — and each chatter sees their own. Uses Customize mode, whose
// prompt is exactly the date line, to keep the assertion surgical.
func TestSystemPromptDateLine_RenderedInChatterTimezone(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}

	resolver := func(uid string) *time.Location {
		switch uid {
		case "chatter-a":
			return sh
		case "chatter-b":
			return la
		}
		return time.Local
	}

	store := newFakeMemoryStore()
	mem := NewMemoryWithStoreForUser("", store, ownerUID, testAgentID)
	cb := NewContextBuilder("", mem, "")
	cb.SetPromptMode(config.PromptModeCustomize)
	cb.SetTimezoneResolver(resolver)

	promptA := cb.BuildSystemPromptAs("chatter-a", mem.WithUserID("chatter-a"))
	promptB := cb.BuildSystemPromptAs("chatter-b", mem.WithUserID("chatter-b"))

	// The zone marker on the date line is what proves per-chatter
	// injection. Assert on the full "X — the chatter's local timezone"
	// phrase, not the bare zone name: the date line's inference
	// instruction (commit 303b8aa) legitimately mentions "Asia/Shanghai"
	// as a 浦东 example, so a bare-name negative would false-positive.
	mustContain(t, promptA, "Asia/Shanghai — the chatter's local timezone")
	mustNotContain(t, promptA, "America/Los_Angeles — the chatter's local timezone")
	mustContain(t, promptB, "America/Los_Angeles — the chatter's local timezone")
	mustNotContain(t, promptB, "Asia/Shanghai — the chatter's local timezone")
}
