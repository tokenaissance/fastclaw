package agent

// e2e for commit 920d7e6 "Add agent knowledge base (size-adaptive
// prompt injection)".
//
// Cloud zero-impact rationale: the knowledge base is assembled server-side
// by the FastAgent gateway (ContextBuilder.buildKnowledgeSection) into the
// system prompt, and retrieval happens via the knowledge_search tool back
// through the gateway's own /api/skills/knowledge handler. Cloud (Next.js
// app) reaches the corpus only through the /api/fastagent proxy's
// chat/session handlers — it never reads agent_files/knowledge rows or
// agent_knowledge_chunks directly, and this commit added no endpoint or
// response-shape change (knowledge upload reuses the existing file-upload
// path; the tool list is a gateway-internal registry). Only the assembled
// system prompt text is affected.
//
// What is pinned down (mirrors the Cloud call path — real DBStore + real
// MemoryStoreAdapter wired exactly as loop.go reloadWorkspaceFiles wires
// a.ctxBuilder, then BuildSystemPromptAs):
//   - Full mode (small corpus): the whole corpus is injected verbatim with
//     per-file source ids — `<agent_knowledge_base mode="full">`,
//     `## [K1] <file>`, `source_id: K1`, `file: <file>`, `path: <path>`,
//     and the "cite it inline with the source_id, e.g. [K1]" instruction.
//   - Owner-fallback overlay: a chatter whose userID has no knowledge rows
//     still inherits the agent owner's corpus (ListKnowledgeDocs resolves
//     the owner in-query), so shared-agent conversations keep the source
//     files without a per-chatter copy.
//   - Index mode (corpus > 24000 runes): full files are NOT inlined; a
//     pinned KNOWLEDGE.md (capped) + a `## Files` index are injected and
//     the model is pointed at knowledge_search for on-demand retrieval.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

func TestKnowledgeBase_FullMode_CloudPathE2E(t *testing.T) {
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
	chatter := &store.UserRecord{ID: "u_chatter", Username: "chatter", Email: "c@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		OwnerUserID: owner.ID, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, chatter); err != nil {
		t.Fatalf("create chatter: %v", err)
	}
	ag := &store.AgentRecord{ID: "agt_kb", UserID: owner.ID, Name: "kb agent",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	// Owner-curated corpus: a pinned KNOWLEDGE.md plus two uploaded files
	// (raw agent_files rows + chunk rows), matching the upload handler.
	pinned := "# Pinned\n公司退款政策以 refund 文档为准。"
	faq := "# FAQ\nPro plan includes web search. 退款政策见 refund 文件。"
	refund := "# Refund\n30 天内可以退款。Refunds within 30 days."
	if err := db.SaveAgentFile(ctx, "agt_kb", owner.ID, "KNOWLEDGE.md", []byte(pinned)); err != nil {
		t.Fatalf("save KNOWLEDGE.md: %v", err)
	}
	files := map[string]string{
		"knowledge/aaaaaaaaaaaa-faq.md":    faq,
		"knowledge/bbbbbbbbbbbb-refund.md": refund,
	}
	for name, content := range files {
		if err := db.SaveAgentFile(ctx, "agt_kb", owner.ID, name, []byte(content)); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
		if err := db.SaveAgentKnowledgeChunks(ctx, "agt_kb", owner.ID, name, "hash-"+name, []string{content}); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
	}

	// Wire the ContextBuilder exactly as reloadWorkspaceFiles does.
	cb := NewContextBuilder("", NewMemory(""), "")
	cb.store = NewMemoryStoreAdapter(db)
	cb.agentID = "agt_kb"
	cb.userID = owner.ID
	cb.SetDisplayName("kb agent")
	chatMem := NewMemory("")

	p := cb.BuildSystemPromptAs(chatter.ID, chatMem)

	for _, want := range []string{
		"<agent_knowledge_base mode=\"full\">",
		"## [K1] KNOWLEDGE.md",
		"source_id: K1",
		"file: KNOWLEDGE.md",
		"path: KNOWLEDGE.md",
		"## [K2] faq.md",
		"source_id: K2",
		"file: faq.md",
		"path: knowledge/aaaaaaaaaaaa-faq.md",
		"## [K3] refund.md",
		"cite it inline with the source_id, e.g. [K1]",
		pinned,
		faq,
		refund,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("full-mode prompt missing %q", want)
		}
	}
	if strings.Contains(p, "mode=\"index\"") {
		t.Errorf("small corpus should select full mode, not index mode")
	}
	// The corpus must render BEFORE the operational middle modules — the
	// module list puts knowledge right after bootstrap_files.
	if !strings.Contains(p, "</agent_knowledge_base>") {
		t.Errorf("knowledge block not closed")
	}
}

func TestKnowledgeBase_OwnerFallback_CloudPathE2E(t *testing.T) {
	// The chatter owns no knowledge rows; the owner's corpus must still
	// be injected because ListKnowledgeDocs/GetAgentFile resolve the
	// owner in-query (store-level overlay, same as the identity files).
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
	chatter := &store.UserRecord{ID: "u_chatter", Username: "chatter", Email: "c@example.com",
		PasswordHash: "x", Role: "app_user", Status: "active",
		OwnerUserID: owner.ID, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateUser(ctx, chatter); err != nil {
		t.Fatalf("create chatter: %v", err)
	}
	ag := &store.AgentRecord{ID: "agt_kb2", UserID: owner.ID, Name: "kb agent",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	faq := "# FAQ\nPro plan includes web search."
	if err := db.SaveAgentFile(ctx, "agt_kb2", owner.ID, "knowledge/aaaaaaaaaaaa-faq.md", []byte(faq)); err != nil {
		t.Fatalf("save faq: %v", err)
	}
	if err := db.SaveAgentKnowledgeChunks(ctx, "agt_kb2", owner.ID, "knowledge/aaaaaaaaaaaa-faq.md", "hash", []string{faq}); err != nil {
		t.Fatalf("index faq: %v", err)
	}

	cb := NewContextBuilder("", NewMemory(""), "")
	cb.store = NewMemoryStoreAdapter(db)
	cb.agentID = "agt_kb2"
	cb.userID = owner.ID
	cb.SetDisplayName("kb agent")
	chatMem := NewMemory("")

	p := cb.BuildSystemPromptAs(chatter.ID, chatMem)
	for _, want := range []string{
		"<agent_knowledge_base mode=\"full\">",
		"## [K1] faq.md",
		"path: knowledge/aaaaaaaaaaaa-faq.md",
		faq,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("owner-fallback prompt missing %q", want)
		}
	}
}

func TestKnowledgeBase_IndexMode_CloudPathE2E(t *testing.T) {
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
	ag := &store.AgentRecord{ID: "agt_kb3", UserID: owner.ID, Name: "kb agent",
		IsPublic: true, CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, ag); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	// Corpus larger than knowledgeFullInjectMaxChars (24000 runes): the
	// full file must NOT be inlined; an index + pinned notes must appear.
	// (each repetition is 32 runes → 900 repeats ≈ 28.8k runes)
	big := strings.Repeat("这是一个很长的知识库段落，用于触发 index 模式的超限分支。", 900) // ~24k runes
	if err := db.SaveAgentFile(ctx, "agt_kb3", owner.ID, "knowledge/aaaaaaaaaaaa-big.md", []byte(big)); err != nil {
		t.Fatalf("save big: %v", err)
	}
	if err := db.SaveAgentKnowledgeChunks(ctx, "agt_kb3", owner.ID, "knowledge/aaaaaaaaaaaa-big.md", "hash-big", []string{big}); err != nil {
		t.Fatalf("index big: %v", err)
	}

	cb := NewContextBuilder("", NewMemory(""), "")
	cb.store = NewMemoryStoreAdapter(db)
	cb.agentID = "agt_kb3"
	cb.userID = owner.ID
	cb.SetDisplayName("kb agent")

	p := cb.BuildSystemPromptAs(owner.ID, NewMemory(""))
	for _, want := range []string{
		"<agent_knowledge_base mode=\"index\">",
		"too large to include in full",
		"knowledge_search",
		"## Files",
		"- big.md",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("index-mode prompt missing %q", want)
		}
	}
	for _, forbid := range []string{
		"<agent_knowledge_base mode=\"full\">",
		"source_id: K1",
		big, // the >24k file body must not be inlined
	} {
		if strings.Contains(p, forbid) {
			t.Errorf("index-mode prompt should NOT contain %q", forbid)
		}
	}
}
