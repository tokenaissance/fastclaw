package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Drives the real HTTP handlers end-to-end: upload → chunks indexed →
// listed → duplicate detected → invalid types rejected → delete removes
// the raw row and its search chunks.
func TestKnowledgeUploadSearchDeleteEndToEnd(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, owner := newAuthTestServer(t, ctx)

	const agentID = "agt_kb_e2e"
	now := time.Now().UTC()
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: agentID, UserID: owner.ID, Name: "kb agent",
		Config: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	do := func(handler http.HandlerFunc, req *http.Request, pathValues map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		cookie, err := resolver.IssueSession(ctx, owner.ID)
		if err != nil {
			t.Fatalf("IssueSession: %v", err)
		}
		req.AddCookie(cookie)
		for k, v := range pathValues {
			req.SetPathValue(k, v)
		}
		rr := httptest.NewRecorder()
		s.authMiddleware(handler)(rr, req)
		return rr
	}
	upload := func(filename string, content []byte) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatalf("write form file: %v", err)
		}
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/agents/"+agentID+"/knowledge-files", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return do(s.handleUploadAgentKnowledgeFile, req, map[string]string{"id": agentID})
	}

	// Valid upload lands and is immediately searchable (CJK terms too).
	faq := []byte("# FAQ\n\nPro plan includes web search. 用户可以在 30 天内申请退款。")
	if rr := upload("faq.md", faq); rr.Code != http.StatusOK {
		t.Fatalf("upload = %d: %s", rr.Code, rr.Body.String())
	}
	chunks, err := s.dataStore.SearchAgentKnowledgeChunks(ctx, agentID, owner.ID, "退款", 5)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("uploaded file not searchable: chunks=%v err=%v", chunks, err)
	}

	// Unsupported extension and binary-disguised-as-text are rejected.
	if rr := upload("tool.exe", []byte("MZ...")); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("exe upload = %d, want 415", rr.Code)
	}
	if rr := upload("fake.md", []byte("looks like text\x00but is not")); rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("NUL upload = %d, want 415", rr.Code)
	}

	// Same content under a new name reports duplicate instead of re-storing.
	rr := upload("faq-copy.md", faq)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate upload = %d: %s", rr.Code, rr.Body.String())
	}

	// List shows exactly the one stored file under its display name.
	rr = do(s.handleListAgentKnowledgeFiles,
		httptest.NewRequest(http.MethodGet, "/api/agents/"+agentID+"/knowledge-files", nil),
		map[string]string{"id": agentID})
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
	}
	var listResp struct {
		Files []struct {
			Name       string `json:"name"`
			StoredName string `json:"storedName"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(listResp.Files) != 1 || listResp.Files[0].Name != "faq.md" {
		t.Fatalf("list = %+v, want single faq.md", listResp.Files)
	}

	// Delete removes both the raw file and its search chunks.
	rr = do(s.handleDeleteAgentKnowledgeFile,
		httptest.NewRequest(http.MethodDelete, "/api/agents/"+agentID+"/knowledge-files/"+listResp.Files[0].StoredName, nil),
		map[string]string{"id": agentID, "name": listResp.Files[0].StoredName})
	if rr.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rr.Code, rr.Body.String())
	}
	docs, err := s.dataStore.ListAgentKnowledgeDocs(ctx, agentID, owner.ID)
	if err != nil || len(docs) != 0 {
		t.Fatalf("docs after delete = %v err=%v, want empty", docs, err)
	}
	chunks, err = s.dataStore.SearchAgentKnowledgeChunks(ctx, agentID, owner.ID, "退款", 5)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("chunks after delete = %v err=%v, want empty", chunks, err)
	}
}

func TestKnowledgeFileHashStable(t *testing.T) {
	h1 := knowledgeFileHash([]byte("same content"))
	h2 := knowledgeFileHash([]byte("same content"))
	h3 := knowledgeFileHash([]byte("different content"))
	if h1 != h2 {
		t.Fatalf("same content produced different hashes")
	}
	if h1 == h3 {
		t.Fatalf("different content produced same hash")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex len = %d, want 64", len(h1))
	}
}

func TestValidateKnowledgeFile(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		wantOK bool
	}{
		{"faq.md", []byte("# FAQ\n中文内容也可以"), true},
		{"data.csv", []byte("a,b,c\n1,2,3"), true},
		{"notes.TXT", []byte("case-insensitive ext"), true},
		{"tool.exe", []byte("MZ..."), false},
		{"report.pdf", []byte("%PDF-1.7"), false},
		{"noext", []byte("hello"), false},
		{"bad.md", []byte{0xff, 0xfe, 0x00, 0x41}, false},         // invalid UTF-8
		{"sneaky.md", []byte("text with \x00 NUL inside"), false}, // binary disguised as text
	}
	for _, tc := range cases {
		err := validateKnowledgeFile(tc.name, tc.data)
		if tc.wantOK && err != nil {
			t.Errorf("validateKnowledgeFile(%q) = %v, want ok", tc.name, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("validateKnowledgeFile(%q) accepted, want rejection", tc.name)
		}
	}
}

func TestChunkKnowledgeTextSplitsWithOverlap(t *testing.T) {
	if got := chunkKnowledgeText(nil); got != nil {
		t.Fatalf("empty input should produce no chunks, got %d", len(got))
	}
	small := chunkKnowledgeText([]byte("one short paragraph"))
	if len(small) != 1 || small[0] != "one short paragraph" {
		t.Fatalf("small input should be a single chunk, got %#v", small)
	}

	// Many paragraphs, way over one chunk target: must split, every
	// chunk non-empty and bounded, and all content preserved somewhere.
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "paragraph %03d with some padding text to take up room in the chunk budget\n\n", i)
	}
	chunks := chunkKnowledgeText([]byte(sb.String()))
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	joined := strings.Join(chunks, "\n")
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			t.Fatalf("empty chunk produced")
		}
		if len([]rune(chunk)) > 2400+240+2 {
			t.Fatalf("chunk exceeds target+overlap: %d runes", len([]rune(chunk)))
		}
	}
	if !strings.Contains(joined, "paragraph 000") || !strings.Contains(joined, "paragraph 119") {
		t.Fatalf("chunking dropped content from the start or end")
	}
}

func TestKnowledgePathIncludesHashPrefix(t *testing.T) {
	path := uniqueKnowledgePath(context.Background(), &knowledgePathStore{}, "agt", "usr", strings.Repeat("a", 64), "SKILL.md")
	if path != "knowledge/aaaaaaaaaaaa-SKILL.md" {
		t.Fatalf("path = %q", path)
	}
}

type knowledgePathStore struct{}

func (knowledgePathStore) GetAgentFile(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (knowledgePathStore) GetAgentFileExact(context.Context, string, string, string) ([]byte, error) {
	return nil, store.ErrNotFound
}

func (knowledgePathStore) SaveAgentFile(context.Context, string, string, string, []byte) error {
	return nil
}

func (knowledgePathStore) DeleteAgentFile(context.Context, string, string, string) error {
	return nil
}

func (knowledgePathStore) ListAgentFiles(context.Context, string, string) ([]string, error) {
	return nil, nil
}
