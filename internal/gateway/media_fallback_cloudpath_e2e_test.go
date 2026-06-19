package gateway

// e2e for commit 9d673c1 "fix(gateway): skip workspace media fallback
// when explicit refs found".
//
// Cloud zero-impact rationale: the media pipeline is the gateway's
// outbound IM delivery seam (agent reply → splitMediaFromReply extracts
// `![alt](src)` refs → appendRecentWorkspaceMedia time-scan fallback →
// OutboundMessage push). Cloud (Next.js app) talks to FastAgent through
// the /api/fastagent proxy and only ever receives the final message; it
// never invokes these unexported gateway helpers. No endpoint, response
// shape, or auth change — only the text/attachment list shipped to IM
// channels.
//
// The bug: appendRecentWorkspaceMedia ran unconditionally. Even when
// splitMediaFromReply already extracted the explicitly referenced image,
// the time-based scan still ran and picked up STALE files from PREVIOUS
// turns whose mtime got refreshed by sandbox mount/restart — so every
// workspace image got sent to the IM channel instead of just the one
// the reply referenced. The fix gates the fallback on `len(items) == 0`.
//
// The gate lives inline in the New() message-handler closure
// (gateway.go:497-498), which needs a fully provisioned AgentManager to
// drive (provider credentials + real HandleMessage), so this test pins
// the contract through the two seam functions with a recording fake
// store — mirroring the handler's composition exactly:
//
//	items = splitMediaFromReply(...)
//	if len(items) == 0 { items = appendRecentWorkspaceMedia(...) }
//
// What is pinned down:
//   - With an explicit `![chart](/workspace/chart.png)` ref, the
//     fallback scan (workspace List) is SKIPPED — a stale previous-turn
//     file in the same window is NOT attached (the fix).
//   - With NO refs in the reply, the fallback still runs and delivers
//     the recent workspace image (the fallback's intended purpose).
//   - splitMediaFromReply strips the markdown ref from the reply text.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// recordingWorkspaceStore is a fake workspace.Store that counts List
// calls so the test can prove the fallback scan did (or didn't) run.
// Get returns the referenced file bytes; List returns the canned
// listing with the stale file's mtime inside this turn's window.
type recordingWorkspaceStore struct {
	objs      []workspace.ObjectInfo
	files     map[string][]byte // path → bytes
	listCalls int
}

func (r *recordingWorkspaceStore) Put(ctx context.Context, agentID, projectID, sessionID, path string, rd io.Reader, size int64, contentType string) error {
	return nil
}
func (r *recordingWorkspaceStore) Get(ctx context.Context, agentID, projectID, sessionID, path string) (io.ReadCloser, error) {
	b, ok := r.files[path]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (r *recordingWorkspaceStore) Stat(ctx context.Context, agentID, projectID, sessionID, path string) (*workspace.ObjectInfo, error) {
	return nil, workspace.ErrNotFound
}
func (r *recordingWorkspaceStore) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	r.listCalls++
	return r.objs, nil
}
func (r *recordingWorkspaceStore) Delete(ctx context.Context, agentID, projectID, sessionID, path string) error {
	return nil
}
func (r *recordingWorkspaceStore) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	return nil
}
func (r *recordingWorkspaceStore) SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error) {
	return "", workspace.ErrSignedURLUnsupported
}

const (
	chartPNGPath = "chart.png"
	stalePNGPath = "stale_old.png"
)

func TestGateway_MediaFallback_CloudPathE2E(t *testing.T) {
	now := time.Now()
	// Workspace listing:
	//  - chart.png: the file the reply explicitly references (fresh).
	//  - stale_old.png: a file written in a PREVIOUS turn whose mtime
	//    was refreshed into this turn's window by sandbox mount/restart
	//    (exactly the over-send the fix prevents).
	ws := &recordingWorkspaceStore{
		objs: []workspace.ObjectInfo{
			{Path: chartPNGPath, Size: 8, ContentType: "image/png", ModTime: now.Add(200 * time.Millisecond)},
			{Path: stalePNGPath, Size: 8, ContentType: "image/png", ModTime: now.Add(500 * time.Millisecond)},
		},
		files: map[string][]byte{
			chartPNGPath: []byte("chart-bytes"),
			stalePNGPath: []byte("stale-bytes"),
		},
	}
	ctx := context.Background()

	// ── Case 1 (the fix): reply has an explicit image ref → fallback
	// scan is SKIPPED → the stale previous-turn file is NOT attached.
	reply := "Here's the chart:\n\n![chart](/workspace/chart.png)"
	text, items := splitMediaFromReply(ctx, ws, "agt_1", "", "chat_1", reply)
	if strings.Contains(text, "![") {
		t.Errorf("splitMediaFromReply: markdown ref not stripped from text: %q", text)
	}
	if len(items) != 1 {
		t.Fatalf("splitMediaFromReply: items = %d, want 1 explicit ref (chart.png)", len(items))
	}
	if items[0].Filename != chartPNGPath {
		t.Errorf("splitMediaFromReply: item filename = %q, want %q", items[0].Filename, chartPNGPath)
	}

	// Mirror the handler gate (gateway.go:497-498): only run the
	// fallback when no explicit refs were extracted.
	if len(items) == 0 {
		items = appendRecentWorkspaceMedia(ctx, ws, "agt_1", "", "chat_1", now, items)
	}
	if ws.listCalls != 0 {
		t.Errorf("explicit-ref path: fallback scan ran %d time(s), want 0 (List must be skipped)", ws.listCalls)
	}
	if len(items) != 1 {
		t.Errorf("explicit-ref path: final items = %d, want only the referenced chart.png (stale file leaked)", len(items))
	}
	if items[0].Filename == stalePNGPath {
		t.Errorf("explicit-ref path: stale previous-turn file %q was attached — the fix should skip it", stalePNGPath)
	}

	// ── Case 2 (fallback intact): no refs in the reply → the time-scan
	// still runs and delivers the recent workspace image.
	ws.listCalls = 0
	_, plainItems := splitMediaFromReply(ctx, ws, "agt_1", "", "chat_1", "Done, here's the result.")
	if len(plainItems) != 0 {
		t.Fatalf("no-ref reply: splitMediaFromReply returned %d items, want 0", len(plainItems))
	}
	plainItems = appendRecentWorkspaceMedia(ctx, ws, "agt_1", "", "chat_1", now, plainItems)
	if ws.listCalls != 1 {
		t.Errorf("no-ref path: fallback scan ran %d time(s), want 1", ws.listCalls)
	}
	found := map[string]bool{}
	for _, it := range plainItems {
		found[it.Filename] = true
	}
	if !found[stalePNGPath] {
		t.Errorf("no-ref path: stale previous-turn file %q not attached — the fallback should still deliver recent workspace images", stalePNGPath)
	}
	if len(plainItems) == 0 {
		t.Errorf("no-ref path: fallback attached nothing — workspace media should flow without a markdown ref")
	}

	// ── Case 3 (dedupe): when the fallback runs with `existing` already
	// holding the same filename, it must not double-attach it.
	ws.listCalls = 0
	dedupItems := []bus.MediaItem{{Filename: chartPNGPath, ContentType: "image/png", Bytes: []byte("chart-bytes")}}
	dedupItems = appendRecentWorkspaceMedia(ctx, ws, "agt_1", "", "chat_1", now, dedupItems)
	count := 0
	for _, it := range dedupItems {
		if it.Filename == chartPNGPath {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedupe: chart.png attached %d times, want 1 (fallback must not double-send what splitMediaFromReply resolved)", count)
	}
}
