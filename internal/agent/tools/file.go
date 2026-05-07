package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
)

type readFileArgs struct {
	Path string `json:"path"`
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type listDirArgs struct {
	Path string `json:"path"`
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// editSchema is the JSON schema advertised for edit_file. Defined once and
// reused by registerFile / registerSandboxedFile so the two registration
// paths can't drift on parameter shape.
var editSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"path": map[string]interface{}{
			"type":        "string",
			"description": "File path (relative to your working directory or absolute)",
		},
		"old_string": map[string]interface{}{
			"type":        "string",
			"description": "Exact text to replace. Must match a unique substring in the file unless replace_all is true.",
		},
		"new_string": map[string]interface{}{
			"type":        "string",
			"description": "Replacement text. Must differ from old_string.",
		},
		"replace_all": map[string]interface{}{
			"type":        "boolean",
			"description": "Replace every occurrence of old_string instead of requiring uniqueness. Defaults to false.",
		},
	},
	"required": []string{"path", "old_string", "new_string"},
}

const editDescription = "Edit a file by replacing an exact substring. Prefer this over write_file when changing only part of a file (especially identity files like SOUL.md / MEMORY.md): it's cheaper, can't drop unrelated content, and validates the replacement was applied. old_string must match a unique substring unless replace_all is true; new_string must differ from old_string. Read the file first if you're unsure of the exact text."

// applyEdit performs the in-memory string replacement that backs edit_file.
// Centralised so every backend (filesystem, workspaceStore, systemFileStore,
// sandbox executor) shares the same uniqueness / not-found / no-op rules.
// Returns the new content and a count of replacements; an error if the edit
// can't be applied as requested.
func applyEdit(path, content, oldStr, newStr string, replaceAll bool) (string, int, error) {
	if oldStr == "" {
		return "", 0, fmt.Errorf("edit_file: old_string is empty (use write_file to create a file)")
	}
	if oldStr == newStr {
		return "", 0, fmt.Errorf("edit_file: new_string must differ from old_string")
	}
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", 0, fmt.Errorf("edit_file: old_string not found in %s — re-read the file and copy the exact text (whitespace/indentation matters)", path)
	}
	if count > 1 && !replaceAll {
		return "", 0, fmt.Errorf("edit_file: old_string matches %d locations in %s — provide more surrounding context to make it unique, or set replace_all=true", count, path)
	}
	if replaceAll {
		return strings.ReplaceAll(content, oldStr, newStr), count, nil
	}
	return strings.Replace(content, oldStr, newStr, 1), 1, nil
}

var errOutsideSandbox = fmt.Errorf("access denied: path is outside the allowed sandbox directory")

// globalSkillsDirSuffix is used to detect attempts to write into the
// admin-managed global skills directory (~/.fastclaw/skills/). Reads are
// fine — the skills layer already exposes this content — but writes from
// chat would let agents silently install/overwrite skills for every other
// agent on the host.
const globalSkillsDirSuffix = "/.fastclaw/skills"

// errGlobalSkillsDirWrite is returned when write_file targets
// ~/.fastclaw/skills/ from inside an agent chat. The message tells the model
// exactly how to recover.
var errGlobalSkillsDirWrite = fmt.Errorf("access denied: ~/.fastclaw/skills/ is the admin-managed global skills directory. To create a new skill, load the \"skill-creator\" skill and follow its workflow (it scaffolds into this agent's private skills dir). To install an existing one, use the install_skill tool")

// systemFiles are the agent metadata/identity files. When a relative path
// references one of these by basename, file tools resolve it against the
// system root rather than the user root.
var systemFiles = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
	"agent.json":   true,
}

// isWorkspacePath decides whether a write/read/list_dir path belongs in the
// workspace store (vs. the agent's home / systemRoot on disk). Uses the same
// rules as rootForPath: identity filenames, the `skills/` subtree, and
// absolute paths stay on disk; everything else is workspace-scoped.
func (r *Registry) isWorkspacePath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if clean == "skills" || strings.HasPrefix(clean, "skills"+string(filepath.Separator)) {
		return false
	}
	if !strings.ContainsRune(clean, filepath.Separator) && systemFiles[clean] {
		return false
	}
	return true
}

// rootForPath returns the root a relative path should resolve against:
//   - systemRoot (agent home) for identity files (SOUL.md, IDENTITY.md, …)
//     and for anything under the `skills/` subtree, which must land in the
//     agent's private skills dir so SkillsLoader actually discovers it;
//   - userRoot (agent workspace) for everything else, which is user-facing
//     artifact territory.
//
// Absolute paths are returned as-is.
func (r *Registry) rootForPath(path string) string {
	if filepath.IsAbs(path) {
		return ""
	}
	clean := filepath.Clean(path)
	// `skills/...` always routes to the agent's home so write_file
	// ("skills/foo/SKILL.md") ends up in the location SkillsLoader scans.
	// Without this, skill-creator's scaffolding ends up in the workspace
	// where nothing ever finds it.
	if clean == "skills" || strings.HasPrefix(clean, "skills"+string(filepath.Separator)) {
		return r.systemRoot
	}
	// Single-segment system files (SOUL.md, IDENTITY.md, ...) also route
	// home; nested paths like "notes/SOUL.md" stay in user content.
	if !strings.ContainsRune(clean, filepath.Separator) && systemFiles[clean] {
		return r.systemRoot
	}
	return r.userRoot
}

func registerFile(r *Registry) {
	r.Register("read_file", "Read the contents of a file", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to your working directory or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeReadFile(r))

	r.Register("write_file", "Write content to a file (creates directories as needed)", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to your working directory or absolute)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}, makeWriteFile(r))

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (relative to your working directory or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeListDir(r))

	r.Register("edit_file", editDescription, editSchema, makeEditFile(r))
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

// isGlobalSkillsPath reports whether absPath points at or under the
// admin-managed ~/.fastclaw/skills/ directory. Works across user home
// locations by matching the stable suffix.
func isGlobalSkillsPath(absPath string) bool {
	clean := filepath.Clean(absPath)
	return strings.HasSuffix(clean, globalSkillsDirSuffix) || strings.Contains(clean, globalSkillsDirSuffix+string(filepath.Separator))
}

// resolvePathSandboxed resolves a path and validates that it stays within
// sandboxRoot. Returns an error when the resolved path escapes.
func resolvePathSandboxed(root, sandboxRoot, path string) (string, error) {
	full := resolvePath(root, path)
	if sandboxRoot == "" {
		return full, nil
	}
	absRoot, err := filepath.Abs(sandboxRoot)
	if err != nil {
		return "", fmt.Errorf("invalid sandbox root: %w", err)
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) && absFull != absRoot {
		return "", errOutsideSandbox
	}
	return absFull, nil
}

// effectiveSandboxRoot picks the bound that file ops should enforce for a
// path resolving against `root`. Identity files (SOUL.md / IDENTITY.md /
// …) live in r.systemRoot — agent home, OUTSIDE the workspace sandbox
// mount — so the workspace sandbox bound would always reject them.
// Confine system-file operations to systemRoot itself instead, which
// keeps zip-slip-style escapes blocked without breaking the legitimate
// "agent reads its own IDENTITY.md" flow when the systemFileStore lookup
// misses (fresh agent, store not yet hydrated, no store configured at
// all).
func (r *Registry) effectiveSandboxRoot(root string) string {
	if root == r.systemRoot && r.systemRoot != "" {
		return r.systemRoot
	}
	return r.sandboxRoot
}

// looksBinary returns true when the payload contains a NUL byte in the
// first 8KB — a near-perfect signal for JPEG/PNG/PDF/zip/wasm/etc. We
// refuse to read binary files via read_file because the bytes get coerced
// into a Go string, then sent to the LLM as tool_result text: a 5MB JPG
// becomes ~1.5M garbled UTF-8 tokens, blowing past every model's context
// limit and turning the next inference into a multi-minute "thinking..."
// stall (or an outright API error). The right path for binary files is
// to feed the path directly to whatever skill handles that format
// (image-tool's `input`, etc.) — never inline the bytes.
func looksBinary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func binaryRefusal(path string, size int) string {
	// Skill-agnostic by design: read_file is a system tool, but which
	// skill is the right consumer for a binary path depends on what
	// the host agent actually has installed (image editing, OCR,
	// archive extract, …). Naming a specific skill here would mislead
	// agents that don't have it. Per-skill guidance belongs in that
	// skill's SKILL.md / the agent's SOUL.md, not in a system tool's
	// error path.
	//
	// What this message MUST do: stop the model's "let me probe the
	// file first" reflex (file / identify / inline python on a 5MB
	// JPEG burns turns and never produces a useful result). The
	// "Don't probe" line is the load-bearing part.
	return fmt.Sprintf("[read_file refused: %q is a binary file (%d bytes). Binary bytes don't decode as text — loading them would blow past the context window. Don't probe with `file`, `identify`, `ls`, `python`, or any inline script — pass the path directly to whichever skill in your toolset handles this format (e.g. an image-editing skill for images). If your toolset doesn't have a skill for this format, tell the user instead of trying to inline-process the bytes.]", path, size)
}

func makeReadFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Mirror makeWriteFile's routing: userRoot-destined paths go to the
		// workspace store when one is configured.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.sessionID, args.Path)
			if err != nil {
				return "", fmt.Errorf("workspace get: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return "", fmt.Errorf("workspace read: %w", err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			return string(data), nil
		}

		// Identity file reads always go through the durable store first
		// (db is source of truth; on-disk is fallback). Use the lenient
		// basename match so an LLM that expands "IDENTITY.md" into the
		// full host path it saw in the prompt's "Working Directory"
		// line still hits the store — earlier we required a bare
		// filename and absolute paths bypassed the store entirely,
		// reading from a workspace dir where identity files don't live.
		if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(args.Path) {
			name := filepath.Base(filepath.Clean(args.Path))
			if data, err := r.systemFileStore.GetWorkspaceFile(ctx, r.agentID, r.userID, name); err == nil {
				return string(data), nil
			}
			// Store miss: try the agent's systemRoot on disk directly,
			// bypassing resolvePathSandboxed. systemRoot is the agent
			// metadata dir (e.g. ~/.fastclaw/agents/<id>/agent) which
			// in K8s deployments lives OUTSIDE sandboxRoot, so the
			// sandbox bound would always reject identity files even
			// though the filename is a fixed whitelist with no escape
			// surface. "Not found" is legitimate (a fresh agent may
			// have no IDENTITY.md row yet) — return empty so the agent
			// treats the field as unset, matching how
			// ContextBuilder.loadFile loads identity files for the
			// system prompt.
			if r.systemRoot != "" {
				if data, err := os.ReadFile(filepath.Join(r.systemRoot, name)); err == nil {
					return string(data), nil
				}
			}
			return "", nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			// Identity files (SOUL/IDENTITY/BOOTSTRAP/...) are routinely unset
			// on a fresh sqlite install — the store has only what the wizard
			// wrote (typically just SOUL.md) and the agent's host home dir
			// isn't even created. Surface "" instead of a not-found error so
			// the agent treats the file as blank and continues, matching how
			// ContextBuilder.loadFile loads them for the system prompt.
			if os.IsNotExist(err) && isSingleSegmentSystemFile(args.Path) {
				return "", nil
			}
			return "", fmt.Errorf("read file: %w", err)
		}

		if looksBinary(data) {
			return binaryRefusal(args.Path, len(data)), nil
		}
		return string(data), nil
	}
}

func makeWriteFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// When a workspace store is configured, route userRoot-destined
		// writes through it. Identity files (systemRoot) still hit the
		// filesystem because the memory store already covers their
		// durability via a separate path.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			if err := r.workspaceStore.Put(ctx, r.agentID, r.sessionID, args.Path,
				strings.NewReader(args.Content), int64(len(args.Content)), ""); err != nil {
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		}

		// Identity files (SOUL.md / IDENTITY.md / ...) need to land in the
		// same durable store the admin UI reads from — otherwise the
		// agent's BOOTSTRAP flow would write to pod-local disk and the
		// Customize page would show blanks. Route through the
		// systemFileStore when available.
		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.userID, name, []byte(args.Content)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			// Keep a filesystem mirror so the agent runtime (context
			// builder, skills loader, etc.) which still reads from disk
			// sees the same content on this pod. Other pods will pick
			// up the next call via their own store reads.
			if r.systemRoot != "" {
				disk := filepath.Join(r.systemRoot, name)
				_ = os.MkdirAll(filepath.Dir(disk), 0o755)
				_ = os.WriteFile(disk, []byte(args.Content), 0o644)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), name), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		if isGlobalSkillsPath(fullPath) {
			return "", errGlobalSkillsDirWrite
		}
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create directory: %w", err)
		}

		if err := os.WriteFile(fullPath, []byte(args.Content), 0o644); err != nil {
			return "", fmt.Errorf("write file: %w", err)
		}

		return fmt.Sprintf("Written %d bytes to %s", len(args.Content), fullPath), nil
	}
}

func makeEditFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args editFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Mirror makeWriteFile's routing precedence: workspace store first
		// (user artifacts), then identity-file store (SOUL.md / IDENTITY.md /
		// MEMORY.md …), then filesystem. The read and the write must hit
		// the same backend or an edit could silently land in a different
		// store than the one the agent later reads from.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.sessionID, args.Path)
			if err != nil {
				return "", fmt.Errorf("workspace get: %w", err)
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				return "", fmt.Errorf("workspace read: %w", readErr)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.workspaceStore.Put(ctx, r.agentID, r.sessionID, args.Path,
				strings.NewReader(updated), int64(len(updated)), ""); err != nil {
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
		}

		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			data, err := r.systemFileStore.GetWorkspaceFile(ctx, r.agentID, r.userID, name)
			if err != nil {
				return "", fmt.Errorf("system file get: %w", err)
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.userID, name, []byte(updated)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			// Same disk-mirror invariant as makeWriteFile so this pod's
			// in-process readers (context builder, skills loader) see the
			// new content immediately.
			if r.systemRoot != "" {
				disk := filepath.Join(r.systemRoot, name)
				_ = os.MkdirAll(filepath.Dir(disk), 0o755)
				_ = os.WriteFile(disk, []byte(updated), 0o644)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", name, count), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		if isGlobalSkillsPath(fullPath) {
			return "", errGlobalSkillsDirWrite
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		if looksBinary(data) {
			return binaryRefusal(args.Path, len(data)), nil
		}
		updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(fullPath, []byte(updated), 0o644); err != nil {
			return "", fmt.Errorf("write file: %w", err)
		}
		return fmt.Sprintf("Edited %s (%d replacement(s))", fullPath, count), nil
	}
}

// isSingleSegmentSystemFile matches "SOUL.md", "IDENTITY.md", etc. —
// the allow-listed identity file names, and only when the write targets
// the top-level directory (no slashes). Nested paths like
// "notes/SOUL.md" deliberately don't qualify. Used by the WRITE path
// where over-broad matching would let users hijack identity rows by
// putting an arbitrary file at /any/path/IDENTITY.md.
func isSingleSegmentSystemFile(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if strings.ContainsRune(clean, filepath.Separator) {
		return false
	}
	return systemFiles[clean]
}

// basenameIsSystemFile is the lenient READ-side variant: it accepts
// absolute paths and nested paths as long as the *basename* is one of
// the identity filenames. Identity files are the source of truth in
// systemFileStore (db); the on-disk view is only a fallback. The LLM
// frequently expands a bare "IDENTITY.md" into the full host path it
// saw in the system prompt's "Working Directory" line — without this
// lenient match, those reads bypass the store entirely and miss the
// real content. Read-only, so the write-path attack surface above
// stays unchanged.
func basenameIsSystemFile(path string) bool {
	return systemFiles[filepath.Base(filepath.Clean(path))]
}

func makeListDir(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Workspace store has a flat key namespace; we synthesise a "dir
		// listing" by filtering List output to entries whose agent-relative
		// path sits under args.Path's prefix.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.sessionID)
			if err != nil {
				return "", fmt.Errorf("workspace list: %w", err)
			}
			prefix := strings.Trim(filepath.ToSlash(filepath.Clean(args.Path)), "/")
			if prefix == "." {
				prefix = ""
			}
			var sb strings.Builder
			seenDirs := map[string]bool{}
			for _, o := range objs {
				p := filepath.ToSlash(o.Path)
				if prefix != "" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
					continue
				}
				rel := strings.TrimPrefix(p, prefix)
				rel = strings.TrimPrefix(rel, "/")
				if rel == "" {
					continue
				}
				if i := strings.IndexByte(rel, '/'); i >= 0 {
					dirName := rel[:i]
					if !seenDirs[dirName] {
						seenDirs[dirName] = true
						fmt.Fprintf(&sb, "d %s/\n", dirName)
					}
					continue
				}
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", rel, o.Size)
			}
			return sb.String(), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return "", fmt.Errorf("read dir: %w", err)
		}

		var sb strings.Builder
		for _, entry := range entries {
			info, _ := entry.Info()
			if entry.IsDir() {
				fmt.Fprintf(&sb, "d %s/\n", entry.Name())
			} else if info != nil {
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", entry.Name(), info.Size())
			} else {
				fmt.Fprintf(&sb, "f %s\n", entry.Name())
			}
		}

		return sb.String(), nil
	}
}

// registerSandboxedFile re-registers file tools so they delegate to a
// sandbox.Executor for paths that don't belong to a store.
//
// IMPORTANT: identity files (SOUL.md, USER.md, MEMORY.md, …) live in
// `systemFileStore` (Postgres in cloud mode) and workspace artifacts live
// in `workspaceStore`. If we routed every path straight to the sandbox
// executor, the agent would 404 on its own identity files — they simply
// don't exist in the sandbox fs. Mirror the store-routing from the
// non-sandboxed path; only hit the sandbox executor when no store handles
// the path (absolute paths, `skills/...`, ad-hoc scripts, etc.). The
// sandbox badge is emitted only for the executor-fallback path — store
// hits intentionally don't badge, since they didn't run in the sandbox.
func registerSandboxedFile(r *Registry, ex sandbox.Executor) {
	r.Register("read_file", "Read the contents of a file", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (identity file, workspace-relative, or absolute inside the sandbox)",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(args.Path) {
			// Lenient match (basename, not just bare filename) so that
			// LLM-expanded absolute paths like
			// /data/.fastclaw/workspaces/<id>/IDENTITY.md still hit the
			// store. Without this the read goes to the sandbox executor
			// which 404s because identity files live in db, not the
			// sandbox FS.
			name := filepath.Base(filepath.Clean(args.Path))
			if data, err := r.systemFileStore.GetWorkspaceFile(ctx, r.agentID, r.userID, name); err == nil {
				return string(data), nil
			}
			// Store miss: treat as unset (a fresh agent may have no
			// IDENTITY.md row yet). Don't fall through to the sandbox
			// executor — identity files don't live there.
			return "", nil
		}
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.sessionID, args.Path)
			if err == nil {
				defer rc.Close()
				data, readErr := io.ReadAll(rc)
				if readErr == nil {
					if looksBinary(data) {
						return binaryRefusal(args.Path, len(data)), nil
					}
					return string(data), nil
				}
			}
		}
		out, err := ex.ReadFile(ctx, args.Path)
		if err == nil && looksBinary([]byte(out)) {
			return binaryRefusal(args.Path, len(out)), nil
		}
		return MetaSandboxPrefix + out, err
	})

	r.Register("write_file", "Write content to a file (creates directories as needed)", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (identity file, workspace-relative, or absolute inside the sandbox)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.userID, name, []byte(args.Content)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), name), nil
		}
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			if err := r.workspaceStore.Put(ctx, r.agentID, r.sessionID, args.Path,
				strings.NewReader(args.Content), int64(len(args.Content)), ""); err != nil {
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		}
		out, err := ex.WriteFile(ctx, args.Path, args.Content)
		return MetaSandboxPrefix + out, err
	})

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (workspace-relative or absolute inside the sandbox)",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.sessionID)
			if err == nil {
				prefix := strings.Trim(filepath.ToSlash(filepath.Clean(args.Path)), "/")
				if prefix == "." {
					prefix = ""
				}
				var sb strings.Builder
				seenDirs := map[string]bool{}
				for _, o := range objs {
					p := filepath.ToSlash(o.Path)
					if prefix != "" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
						continue
					}
					rel := strings.TrimPrefix(p, prefix)
					rel = strings.TrimPrefix(rel, "/")
					if rel == "" {
						continue
					}
					if i := strings.IndexByte(rel, '/'); i >= 0 {
						dirName := rel[:i]
						if !seenDirs[dirName] {
							seenDirs[dirName] = true
							fmt.Fprintf(&sb, "d %s/\n", dirName)
						}
						continue
					}
					fmt.Fprintf(&sb, "f %s (%d bytes)\n", rel, o.Size)
				}
				return sb.String(), nil
			}
		}
		out, err := ex.ListDir(ctx, args.Path)
		return MetaSandboxPrefix + out, err
	})

	r.Register("edit_file", editDescription, editSchema, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args editFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Identity files live in the systemFileStore (Postgres in cloud
		// deployments) — the sandbox FS doesn't have them. Read+write must
		// hit the same backend, otherwise a successful edit on disk would
		// be invisible to the next read that goes through the store.
		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			data, err := r.systemFileStore.GetWorkspaceFile(ctx, r.agentID, r.userID, name)
			if err != nil {
				return "", fmt.Errorf("system file get: %w", err)
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.userID, name, []byte(updated)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", name, count), nil
		}

		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.sessionID, args.Path)
			if err == nil {
				data, readErr := io.ReadAll(rc)
				rc.Close()
				if readErr == nil {
					if looksBinary(data) {
						return binaryRefusal(args.Path, len(data)), nil
					}
					updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
					if err != nil {
						return "", err
					}
					if err := r.workspaceStore.Put(ctx, r.agentID, r.sessionID, args.Path,
						strings.NewReader(updated), int64(len(updated)), ""); err != nil {
						return "", fmt.Errorf("workspace put: %w", err)
					}
					return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
				}
			}
			// Fall through to sandbox executor on store miss.
		}

		// Read-modify-write through the sandbox executor for paths that
		// don't belong to a store (skills/, /tmp/, ad-hoc scripts, …).
		// post-exec sync mirrors the resulting file back to workspace.Store
		// just like a write_file call would.
		content, err := ex.ReadFile(ctx, args.Path)
		if err != nil {
			return "", err
		}
		if looksBinary([]byte(content)) {
			return binaryRefusal(args.Path, len(content)), nil
		}
		updated, count, err := applyEdit(args.Path, content, args.OldString, args.NewString, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		// Discard the executor's confirmation string — we synthesise a
		// "(N replacement(s))" message that's more informative than the
		// generic write echo.
		if _, err := ex.WriteFile(ctx, args.Path, updated); err != nil {
			return "", err
		}
		return MetaSandboxPrefix + fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
	})
}
