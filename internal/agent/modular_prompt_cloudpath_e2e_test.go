package agent

// e2e for commit 267fcd4 "refactor(prompt): modular system-prompt
// assembly with identity-first ordering".
//
// Cloud zero-impact rationale: the system prompt is built server-side by
// the FastAgent gateway (ContextBuilder.BuildSystemPromptAs) and injected
// into every model turn. Cloud (Next.js app) reaches it only through the
// /api/fastagent proxy's session/chat handlers — it never assembles the
// prompt itself. This commit restructured HOW the prompt is composed
// (module lists per mode, identity files placed first for primacy bias,
// identity tail last for recency bias) plus six bundled features:
// chatbot tool allowlist, FASTAGENT_DEBUG_MODE prompt dump, set_timezone
// dual-write, WeChat image decrypt. No endpoint, response shape, or auth
// change; only the text content/ordering of the system prompt.
//
// What is pinned down (mirrors the Cloud call path — real DBStore + real
// MemoryStoreAdapter wired exactly as loop.go reloadWorkspaceFiles wires
// a.ctxBuilder, then BuildSystemPromptAs):
//   - Agent mode: SOUL.md / IDENTITY.md (identity) appear BEFORE the
//     operational modules (confidentiality, tool routing) and the
//     identity anchor + identity tail bracket the whole prompt
//     (primacy + recency bias).
//   - Agent mode: identity files are loaded from the store owner-fallback
//     overlay, so a chatter without their own row still inherits the
//     agent owner's SOUL.md / IDENTITY.md in the assembled prompt.
//   - Mode-specific assembly: chatbot drops agent-loop modules and uses
//     the chatbot tool allowlist (web_search/web_fetch/exec/load_skill,
//     no pip/npm); customize emits only date + bootstrap files + memory.
//   - displayName renders as the identity anchor name (model must not
//     fall back to its base-model identity).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestModularPrompt_IdentityFirst_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Now().UTC()
	owner := &store.UserRecord{ID: "u_owner", Username: "owner", Email: "o@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	// Chatter app_user — same setup as the #27 e2e: a visitor whose
	// UserSpace owner differs from the agent owner.
	chatter := &store.UserRecord{ID: "u_chatter", Username: "chatter", Email: "c@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		OwnerUserID: owner.ID, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, chatter); err != nil {
		t.Fatalf("create chatter: %v", err)
	}

	ag := &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "拽姐",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	soul := "我是拽姐，一个帮你处理事务的智能体。性格沉稳，言简意赅。"
	identity := "Name: 拽姐\nRole: 事务处理助手\nSpecialization: 日程、纪要、代办"
	if err := db.SaveAgentFile(ctx, "agt_1", owner.ID, "SOUL.md", []byte(soul)); err != nil {
		t.Fatalf("save SOUL.md: %v", err)
	}
	if err := db.SaveAgentFile(ctx, "agt_1", owner.ID, "IDENTITY.md", []byte(identity)); err != nil {
		t.Fatalf("save IDENTITY.md: %v", err)
	}

	// Wire the ContextBuilder exactly as reloadWorkspaceFiles does
	// (loop.go): store + agentID + userID pinned to the DB-backed
	// identity store, plus the operator-given display name.
	cb := NewContextBuilder("", NewMemory(""), "")
	cb.store = NewMemoryStoreAdapter(db)
	cb.agentID = "agt_1"
	cb.userID = owner.ID
	cb.SetDisplayName("拽姐")
	// Chatter memory must be non-nil (modMemory dereferences it).
	chatMem := NewMemory("")

	p := cb.BuildSystemPromptAs(chatter.ID, chatMem)

	// ── Presence of the identity block ──
	for _, want := range []string{
		"# IDENTITY OVERRIDE (non-negotiable)", // identity anchor (top)
		"You are **拽姐**",
		"You run on the FastAgent runtime", // agent intro
		"File-tool routing:",               // agent intro operational guidance
		"# SOUL.md",                        // bootstrap files
		soul,
		"# IDENTITY.md",
		"Current date/time:",               // date line
		"# Confidentiality (load-bearing)", // operational module
		"# CRITICAL REMINDER",              // identity tail (bottom)
	} {
		if !strings.Contains(p, want) {
			t.Errorf("agent-mode prompt missing %q", want)
		}
	}

	// ── Identity-first ordering (primacy + recency bias) ──
	// identity anchor → agent intro → SOUL.md → IDENTITY.md →
	// confidentiality (operational) → identity tail at the very end.
	assertBefore := func(earlier, later string) {
		t.Helper()
		i := strings.Index(p, earlier)
		j := strings.Index(p, later)
		if i < 0 {
			t.Errorf("marker %q not in prompt", earlier)
			return
		}
		if j < 0 {
			t.Errorf("marker %q not in prompt", later)
			return
		}
		if i >= j {
			t.Errorf("ordering: %q (idx %d) should appear BEFORE %q (idx %d)", earlier, i, later, j)
		}
	}
	assertBefore("# IDENTITY OVERRIDE (non-negotiable)", "You run on the FastAgent runtime")
	assertBefore("You run on the FastAgent runtime", "# SOUL.md")
	assertBefore("# SOUL.md", "# IDENTITY.md")
	assertBefore("# IDENTITY.md", "# Confidentiality (load-bearing)")
	assertBefore("# Confidentiality (load-bearing)", "# CRITICAL REMINDER")

	// ── Identity files inherited via the store owner-fallback ──
	// The chatter has no SOUL.md row of their own, so the agent owner's
	// SOUL.md must still appear in the assembled prompt (GetAgentFile
	// owner-fallback overlay, fixed by #27's agentID wiring).
	if !strings.Contains(p, soul) {
		t.Errorf("agent-mode prompt missing owner SOUL.md content %q (owner-fallback overlay broken)", soul)
	}
	if !strings.Contains(p, identity) {
		t.Errorf("agent-mode prompt missing owner IDENTITY.md content %q (owner-fallback overlay broken)", identity)
	}
}

func TestModularPrompt_ModeSpecificAssembly_CloudPathE2E(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	now := time.Now().UTC()
	owner := &store.UserRecord{ID: "u_owner", Username: "owner", Email: "o@example.com",
		PasswordHash: "x", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ag := &store.AgentRecord{ID: "agt_1", UserID: owner.ID, Name: "拽姐",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	if err := db.SaveAgentFile(ctx, "agt_1", owner.ID, "SOUL.md", []byte("我是拽姐。")); err != nil {
		t.Fatalf("save SOUL.md: %v", err)
	}

	newCB := func(mode string) *ContextBuilder {
		cb := NewContextBuilder("", NewMemory(""), "")
		cb.store = NewMemoryStoreAdapter(db)
		cb.agentID = "agt_1"
		cb.userID = owner.ID
		cb.SetDisplayName("拽姐")
		cb.SetPromptMode(mode)
		return cb
	}

	// ── Agent mode: full framework prompt with delegation/todo ──
	agentPrompt := newCB(config.PromptModeAgent).BuildSystemPromptAs(owner.ID, NewMemory(""))
	for _, want := range []string{
		"# IDENTITY OVERRIDE (non-negotiable)",
		"You run on the FastAgent runtime",
		"File-tool routing:",
		"# Confidentiality (load-bearing)",
		"# Tool Use", // tool_discipline content
	} {
		if !strings.Contains(agentPrompt, want) {
			t.Errorf("agent-mode prompt missing %q", want)
		}
	}
	// The chatbot tool allowlist line is chatbot-only; agent mode uses
	// its own tool discipline section.
	if strings.Contains(agentPrompt, "web_search, web_fetch, exec, and load_skill tools.") {
		t.Errorf("agent-mode prompt should NOT contain the chatbot tool-allowlist line")
	}

	// ── Chatbot mode: slim scaffolding, tool allowlist, no delegation ──
	chatbotPrompt := newCB(config.PromptModeChatbot).BuildSystemPromptAs(owner.ID, NewMemory(""))
	for _, want := range []string{
		"# IDENTITY OVERRIDE (non-negotiable)",
		"You CAN remember chatters across sessions", // chatbot_intro memory section
		"web_search, web_fetch, exec, and load_skill tools.",
	} {
		if !strings.Contains(chatbotPrompt, want) {
			t.Errorf("chatbot-mode prompt missing %q", want)
		}
	}
	for _, forbid := range []string{
		"File-tool routing:",               // agent_intro only
		"You run on the FastAgent runtime", // agent_intro only
	} {
		if strings.Contains(chatbotPrompt, forbid) {
			t.Errorf("chatbot-mode prompt should NOT contain %q", forbid)
		}
	}

	// ── Customize mode: date + bootstrap files + memory only ──
	customizePrompt := newCB(config.PromptModeCustomize).BuildSystemPromptAs(owner.ID, NewMemory(""))
	for _, want := range []string{
		"Current date/time:",
		"# SOUL.md",
		"我是拽姐。",
	} {
		if !strings.Contains(customizePrompt, want) {
			t.Errorf("customize-mode prompt missing %q", want)
		}
	}
	for _, forbid := range []string{
		"# IDENTITY OVERRIDE (non-negotiable)",
		"# Confidentiality (load-bearing)",
		"web_search, web_fetch",
	} {
		if strings.Contains(customizePrompt, forbid) {
			t.Errorf("customize-mode prompt should NOT contain %q", forbid)
		}
	}
}
