package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// WriteSessionAttachments materializes user-attached image bytes into the
// agent's session workspace so skills (image-tool, etc.) can read them as
// /workspace/<filename>. Each url is one of:
//   - data URL:  "data:image/png;base64,iVBORw..."
//   - HTTPS URL: "https://example.com/photo.jpg"
//
// Per-image errors are logged and skipped — a single bad URL must not sink
// the whole turn. Returns the relative filenames (e.g. "in_<ts>_0.png") in
// input order, omitting any that failed.
//
// Why three writes:
//
//   - Host workspace dir: covers the no-sandbox case (host exec uses this
//     dir as cwd) AND the docker case (a.workspacePath is bind-mounted at
//     /workspace inside the container).
//   - workspace.Store.Put: durable handoff for E2B / multi-pod — the
//     LifecyclePool's hydrate-on-create copies it into /workspace on the
//     next sandbox spin-up.
//   - sandbox executor.WriteFile: covers the E2B mid-session case where
//     the sandbox is already hydrated and won't re-pull from the Store.
//
// Docker doesn't need the third write (bind mount makes host writes show
// up instantly), but calling it is harmless. The host write is also
// harmless for E2B (gateway-local bytes nobody reads).
func (a *Agent) WriteSessionAttachments(ctx context.Context, sessionID string, urls []string) []string {
	if len(urls) == 0 {
		return nil
	}
	var paths []string
	// Short, base36-ish token derived from the millisecond timestamp.
	// Long enough to avoid collision in any realistic chat cadence (a
	// human would have to upload twice within the same millisecond);
	// short enough that the resulting filename — `image_3jk7l_0.jpg` —
	// reads as a normal user attachment, not a system-generated temp
	// file. The earlier `in_1777972819289_0.jpg` shape made models
	// reach for read_file/`file`/`identify` to "verify" the upload
	// before passing it to a skill.
	token := strconv.FormatInt(time.Now().UnixMilli(), 36)
	if len(token) > 5 {
		token = token[len(token)-5:]
	}
	for i, u := range urls {
		data, ext, err := decodeAttachment(ctx, u)
		if err != nil {
			slog.Warn("attachment decode failed", "agent", a.name, "session", sessionID, "index", i, "error", err)
			continue
		}
		name := fmt.Sprintf("image_%s_%d%s", token, i, ext)

		// 1. Host workspace dir (covers no-sandbox + docker via bind mount)
		if a.workspacePath != "" {
			full := filepath.Join(a.workspacePath, name)
			if mkErr := os.MkdirAll(a.workspacePath, 0o755); mkErr == nil {
				if wErr := os.WriteFile(full, data, 0o644); wErr != nil {
					slog.Warn("attachment host write failed", "agent", a.name, "session", sessionID, "path", full, "error", wErr)
				}
			}
		}

		// 2. Durable store (covers E2B / multi-pod via hydrate-on-create)
		if a.workspaceStore != nil {
			if pErr := a.workspaceStore.Put(ctx, a.agentID, sessionID, name, strings.NewReader(string(data)), int64(len(data)), contentTypeFromExt(ext)); pErr != nil {
				slog.Warn("attachment store put failed", "agent", a.name, "session", sessionID, "path", name, "error", pErr)
			}
		}

		// 3. Live sandbox (covers E2B mid-session). Best-effort; missing
		// pool / get failure just means the next exec will pull from the
		// store via hydrate-on-create.
		if a.sandboxPool != nil {
			if ex, gErr := a.sandboxPool.Get(ctx, a.name, sessionID); gErr == nil && ex != nil {
				if _, wErr := ex.WriteFile(ctx, "/workspace/"+name, string(data)); wErr != nil {
					slog.Warn("attachment sandbox write failed", "agent", a.name, "session", sessionID, "path", name, "error", wErr)
				}
			}
		}

		paths = append(paths, name)
	}
	return paths
}

// decodeAttachment turns a data URL or HTTPS URL into raw bytes plus a
// best-effort filename extension (".png", ".jpg", …). Unknown / missing
// MIME maps to ".bin".
func decodeAttachment(ctx context.Context, u string) ([]byte, string, error) {
	if strings.HasPrefix(u, "data:") {
		return decodeDataURL(u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, "", fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	httpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(httpCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBytes = 25 * 1024 * 1024 // 25 MB ceiling — same envelope as Anthropic's per-image limit
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxBytes {
		return nil, "", fmt.Errorf("attachment exceeds %d bytes", maxBytes)
	}
	ext := extFromMIME(resp.Header.Get("Content-Type"))
	if ext == "" {
		ext = filepath.Ext(parsed.Path) // fallback to URL extension
	}
	if ext == "" {
		ext = ".bin"
	}
	return body, ext, nil
}

func decodeDataURL(u string) ([]byte, string, error) {
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("data url missing comma")
	}
	header := u[5:comma] // strip "data:"
	payload := u[comma+1:]

	var mime string
	isB64 := false
	for _, part := range strings.Split(header, ";") {
		switch {
		case part == "base64":
			isB64 = true
		case part == "":
			// noop — leading "data:," with no MIME is legal
		case mime == "":
			mime = part
		}
	}
	var data []byte
	if isB64 {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode: %w", err)
		}
		data = decoded
	} else {
		decoded, err := url.QueryUnescape(payload)
		if err != nil {
			return nil, "", fmt.Errorf("urlencoded decode: %w", err)
		}
		data = []byte(decoded)
	}
	ext := extFromMIME(mime)
	if ext == "" {
		ext = ".bin"
	}
	return data, ext, nil
}

func extFromMIME(ct string) string {
	// Strip params: "image/png; charset=…" → "image/png"
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.TrimSpace(strings.ToLower(ct)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/heic":
		return ".heic"
	case "image/svg+xml":
		return ".svg"
	}
	return ""
}

func contentTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}
