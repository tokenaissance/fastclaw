package store

import (
	"context"
	"testing"
	"time"
)

// Seeds one agent owned by ownerID with two knowledge files (raw rows +
// chunk rows), matching what the upload handler writes.
func seedKnowledgeAgent(t *testing.T, db *DBStore, agentID, ownerID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.SaveAgent(ctx, &AgentRecord{
		ID: agentID, UserID: ownerID, Name: "kb agent",
		Config: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	files := map[string]string{
		"knowledge/aaaaaaaaaaaa-faq.md":    "# FAQ\nPro plan includes web search. 退款政策见 refund 文件。",
		"knowledge/bbbbbbbbbbbb-refund.md": "# Refund\n30 天内可以退款。Refunds within 30 days.",
		"SOUL.md":                          "not knowledge",
		"sessions/chat1/report.md":         "not knowledge either",
	}
	for name, content := range files {
		if err := db.SaveAgentFile(ctx, agentID, ownerID, name, []byte(content)); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	for _, name := range []string{"knowledge/aaaaaaaaaaaa-faq.md", "knowledge/bbbbbbbbbbbb-refund.md"} {
		if err := db.SaveAgentKnowledgeChunks(ctx, agentID, ownerID, name, "hash-"+name, []string{files[name]}); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
	}
}

func TestListAgentKnowledgeDocsOwnerFallback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	seedKnowledgeAgent(t, db, "agt_kb", "u_owner")

	// Owner sees exactly the knowledge/ rows, in path order.
	docs, err := db.ListAgentKnowledgeDocs(ctx, "agt_kb", "u_owner")
	if err != nil {
		t.Fatalf("list as owner: %v", err)
	}
	if len(docs) != 2 || docs[0].Path != "knowledge/aaaaaaaaaaaa-faq.md" || docs[1].Path != "knowledge/bbbbbbbbbbbb-refund.md" {
		t.Fatalf("owner docs = %+v", docs)
	}

	// A chatter on a shared agent resolves the owner's corpus via the
	// in-query fallback.
	docs, err = db.ListAgentKnowledgeDocs(ctx, "agt_kb", "u_chatter")
	if err != nil {
		t.Fatalf("list as chatter: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("chatter should inherit owner corpus, got %+v", docs)
	}

	// A chatter's own row for the same path shadows the owner's copy.
	if err := db.SaveAgentFile(ctx, "agt_kb", "u_chatter", "knowledge/aaaaaaaaaaaa-faq.md", []byte("chatter override")); err != nil {
		t.Fatalf("save override: %v", err)
	}
	docs, err = db.ListAgentKnowledgeDocs(ctx, "agt_kb", "u_chatter")
	if err != nil {
		t.Fatalf("list with override: %v", err)
	}
	if len(docs) != 2 || docs[0].Content != "chatter override" {
		t.Fatalf("override should shadow owner row once, got %+v", docs)
	}
}

func TestSearchAgentKnowledgeChunksOwnerFallback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	seedKnowledgeAgent(t, db, "agt_kb2", "u_owner")

	// English keyword, queried as a chatter (rows live under the owner).
	chunks, err := db.SearchAgentKnowledgeChunks(ctx, "agt_kb2", "u_chatter", "refund", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(chunks) == 0 || chunks[0].Path != "knowledge/bbbbbbbbbbbb-refund.md" {
		t.Fatalf("refund search top hit = %+v", chunks)
	}

	// CJK keyword hits via the bigram terms.
	chunks, err = db.SearchAgentKnowledgeChunks(ctx, "agt_kb2", "u_chatter", "退款", 5)
	if err != nil {
		t.Fatalf("cjk search: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("cjk search found nothing")
	}

	// Deleting the file's chunks removes it from search.
	if err := db.DeleteAgentKnowledgeChunks(ctx, "agt_kb2", "u_owner", "knowledge/bbbbbbbbbbbb-refund.md"); err != nil {
		t.Fatalf("delete chunks: %v", err)
	}
	chunks, err = db.SearchAgentKnowledgeChunks(ctx, "agt_kb2", "u_chatter", "Refunds within", 5)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	for _, c := range chunks {
		if c.Path == "knowledge/bbbbbbbbbbbb-refund.md" {
			t.Fatalf("deleted file still searchable: %+v", c)
		}
	}
}
