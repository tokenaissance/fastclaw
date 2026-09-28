package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fastclaw-ai/fastclaw/internal/workspace"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
	"github.com/fastclaw-ai/fastclaw/internal/skills"
	"github.com/fastclaw-ai/fastclaw/internal/store"
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

// writeFileDescription is the description advertised for write_file, shared by
// registerFile and registerSandboxedFile for the same reason editSchema is.
//
// The second sentence is load-bearing: one write_file call carries the whole
// document as arguments, and arguments count against the model's output limit.
// On 2026-09-22 two calls hit that limit with a ~17 KB document, arrived as a
// JSON prefix, and each cost the model a full re-emission of the document.
const writeFileDescription = "Write content to a file (creates directories as needed). For a long document this is one call and one set of arguments, and those arguments cannot exceed your output limit — write the first section, then append the rest with edit_file."

// writeFileSchema is the JSON schema advertised for write_file. It is a function
// rather than a var because the path hint differs between the two registration
// paths — the sandbox one also accepts identity files — and sharing the rest is
// what keeps them from drifting.
func writeFileSchema(pathDescription string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": pathDescription,
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}
}

// singlePathSchema is the JSON schema for the tools that take exactly one path
// argument (read_file, list_dir). Like writeFileSchema it is a function because
// the path hint is written from each registration's own vantage point, and the
// two registrations share the rest so only that one sentence can ever drift.
func singlePathSchema(pathDescription string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": pathDescription,
			},
		},
		"required": []string{"path"},
	}
}

const editDescription = "Edit a file by replacing an exact substring. Prefer this over write_file when changing only part of a file (especially identity files like SOUL.md / MEMORY.md): it's cheaper, can't drop unrelated content, and validates the replacement was applied. old_string must match a unique substring unless replace_all is true; new_string must differ from old_string. Read the file first if you're unsure of the exact text."

// validateFileTargetPath rejects path arguments to write-like ops that
// can't refer to a single file. Empty strings, directory-suffix paths
// ("foo/"), and the special directory aliases (".", "..", "/") all slip
// through the downstream routing (isWorkspacePath treats "" as workspace-
// scoped because filepath.Clean("") == ".") and end up at os.OpenFile on
// the session directory, surfacing a cryptic "is a directory" error.
// Refusing early gives the model an actionable, tool-shaped message
// instead.
func validateFileTargetPath(path string) error {
	if strings.TrimSpace(path) == "" {
		// Naming the rule alone is what a caller already believes it did:
		// on 2026-09-22 two write_file calls came back with this message while
		// their arguments were a JSON prefix that *did* carry a path — the key
		// was simply past where the call was cut off. Show a path that
		// satisfies the rule instead, and name no op: write_file and edit_file
		// share this function.
		return fmt.Errorf(`path is required and must include a filename, e.g. "notes.md" or "reports/q3.md" — a relative path lands in your workspace`)
	}
	if strings.HasSuffix(path, "/") || strings.HasSuffix(path, string(filepath.Separator)) {
		return fmt.Errorf("path %q ends in a separator; include a filename at the end", path)
	}
	switch filepath.Clean(path) {
	case ".", "..", "/":
		return fmt.Errorf("path %q is a directory, not a file; include a filename", path)
	}
	return nil
}

// asIsDirToolError detects the "is a directory" failure mode (raised when
// a write/edit resolves to an existing directory rather than a file) and
// promotes it to a tool-level message the model can recover from. Falls
// back to the caller's wrapped error otherwise.
func asIsDirToolError(opName, path string, err error) error {
	if err != nil && strings.Contains(err.Error(), "is a directory") {
		return fmt.Errorf("%s: %q resolves to a directory; include a filename in the path", opName, path)
	}
	return nil
}

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
// admin-managed global skills directory (~/.fastagent/skills/). Reads are
// fine — the skills layer already exposes this content — but writes from
// chat would let agents silently install/overwrite skills for every other
// agent on the host.
const globalSkillsDirSuffix = "/.fastagent/skills"

// errGlobalSkillsDirWrite is returned when write_file targets
// ~/.fastagent/skills/ from inside an agent chat. The message tells the model
// exactly how to recover.
var errGlobalSkillsDirWrite = fmt.Errorf("access denied: ~/.fastagent/skills/ is the admin-managed global skills directory. To create a new skill, load the \"skill-creator\" skill and follow its workflow (it scaffolds into this agent's private skills dir). To install an existing one, use the install_skill tool")

// systemFiles are the agent metadata/identity files. When a relative path
// references one of these by basename, file tools resolve it against the
// system root rather than the user root.
var systemFiles = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"KNOWLEDGE.md": true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
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

// hostHomePath returns the resolved absolute filesystem path when the
// arg looks like an operator-host path the chatter wants to read/write,
// and false otherwise. Three forms are recognised:
//
//	~                 → the operator's home dir
//	~/<rel>           → joined under the operator's home dir
//	/Users/<u>/...    → macOS-style absolute home roots
//	/home/<u>/...     → Linux-style absolute home roots
//
// Used by the sandboxed file tools on SELF-HOSTED installs to route
// requests like "read ~/Downloads/foo.csv" to actual host disk
// instead of 404'ing inside the sandbox FS. Hosted (multi-tenant)
// deployments deliberately don't call this — the chatter doesn't own
// the daemon's filesystem so exposing it would be a privilege leak.
//
// Returns ("", false) when the path is not a host-home reference, OR
// when it falls under one of the FastAgent-managed roots
// (~/.fastagent/...) — those are runtime internals and should keep
// flowing through their existing routing (workspaceStore, identity
// store, etc.) so chat writes can't, say, smash the agents' DB file.
func hostHomePath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		// Sandbox-only / FastAgent-internal subtrees: skip host expansion
		// so the read/write falls through to the sandbox executor instead
		// of trying (and failing) on host disk where the path doesn't
		// exist. Symmetric to the absolute-path guard below.
		//   ~/.fastagent/... — runtime internals (db, workspaces, …)
		//   ~/.agents/...   — sandbox bind-mount target for npx skills.
		//                     Host has these at <FASTAGENT_HOME>/users/<uid>/skills/,
		//                     not under ~. After `ls ~/.agents/skills/<x>/` runs
		//                     in-sandbox the model naturally calls
		//                     read_file with the same path; that path only
		//                     resolves inside the container.
		if strings.HasPrefix(path, "~/.fastagent") || strings.HasPrefix(path, "~/.agents") {
			return "", false
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		if path == "~" {
			return home, true
		}
		return filepath.Join(home, path[2:]), true
	}
	if !filepath.IsAbs(path) {
		return "", false
	}
	if strings.HasPrefix(path, "/Users/") || strings.HasPrefix(path, "/home/") {
		// Refuse FastAgent-internal subpaths even when the chatter
		// reaches them via the host-home channel. Same guard as
		// errGlobalSkillsDirWrite, broader scope.
		if home, err := os.UserHomeDir(); err == nil {
			fastagentDir := filepath.Join(home, ".fastagent")
			if path == fastagentDir || strings.HasPrefix(path, fastagentDir+string(filepath.Separator)) {
				return "", false
			}
		}
		return path, true
	}
	return "", false
}

// isSkillPath reports whether path is a chat-time `skills/<name>/...`
// write — the skill-creator convention. Absolute paths and the bare
// `skills` segment don't qualify (the latter is a directory, not a
// file write). Cleans the path so `skills/./foo/SKILL.md` matches.
func (r *Registry) isSkillPath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "skills" && strings.HasPrefix(clean, "skills"+string(filepath.Separator))
}

// skillRoot returns the host parent of the `skills/` subdir that
// chat-time skill writes should land in. Per-user when configured
// (the chatter's personal bucket), agent home otherwise.
func (r *Registry) skillRoot() string {
	if r.userSkillsRoot != "" {
		return r.userSkillsRoot
	}
	return r.systemRoot
}

// skillStoreOwner returns the workspace.Store pseudo-owner key the
// chat-created skill should mirror to. Per-user when userSkillsRoot
// is set (so the skill follows the chatter across agents); agent ID
// otherwise (legacy / single-user mode).
func (r *Registry) skillStoreOwner() string {
	if r.userSkillsRoot != "" && r.userID != "" {
		return skills.UserSkillOwner(r.userID)
	}
	return r.agentID
}

// writeSkillToHost lands a chat-created `skills/<name>/<rel>` file on
// host disk and mirrors it to the workspace store so SkillsLoader's
// local scan and any sibling pod's hydrate both see it. Used by the
// sandbox-mode write_file path (which would otherwise trap the file
// inside the ephemeral sandbox FS) and by host-mode write_file as a
// post-write store-sync hook.
//
// The path arg must already pass isSkillPath. Returns the absolute
// host path written so the caller can echo it back to the model.
func (r *Registry) writeSkillToHost(ctx context.Context, path, content string) (string, error) {
	root := r.skillRoot()
	if root == "" {
		return "", fmt.Errorf("write_file: no skills root configured for path %q", path)
	}
	full := filepath.Join(root, filepath.Clean(path))
	if isGlobalSkillsPath(full) {
		return "", errGlobalSkillsDirWrite
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	// Mirror to the workspace store so a sibling pod (cloud deploy)
	// hydrates the new skill on its next turn instead of waiting for
	// pod restart. Best-effort; failures here don't unwrite the file.
	if r.workspaceStore != nil {
		if owner := r.skillStoreOwner(); owner != "" {
			rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "skills/")
			parts := strings.SplitN(rel, "/", 2)
			if len(parts) >= 1 && parts[0] != "" {
				skillName := parts[0]
				skillsDir := filepath.Join(root, "skills")
				if err := skills.SyncSkillUp(ctx, r.workspaceStore, owner, skillName, skillsDir); err != nil {
					slog.Warn("skill mirror to store failed",
						"owner", owner, "skill", skillName, "error", err)
				}
			}
		}
	}
	return full, nil
}

// WriteSkillFile is the skills namespace's write entry point for callers that
// are not file tools — today the background skills learner. It goes through
// writeSkillToHost, so a skill written by any path lands in the same place
// (host disk plus the object-store mirror) instead of on one pod's disk:
// a second writer for `skills/` is exactly how a learned SKILL.md ends up
// invisible to sibling pods and lost on restart (the same hole apply_patch
// had for its own write path — docs/fs-formal-proof/11-change-register.md
// row 50).
//
// slug must be a single path segment; rel is relative to the skill dir.
func (r *Registry) WriteSkillFile(ctx context.Context, slug, rel, content string) (string, error) {
	path := filepath.ToSlash(filepath.Join("skills", slug, rel))
	if !r.isSkillPath(path) {
		return "", fmt.Errorf("writeSkillFile: %q is not a skills/<name>/... path", path)
	}
	return r.writeSkillToHost(ctx, path, content)
}

// SkillsRoot reports the host parent of the `skills/` subtree this registry
// writes into — the same root skillRoot picks for a chat-time write, so a
// caller that needs to stat what it is about to write (the skills learner's
// existence check) reads the tree it actually writes to. Empty when no root
// is configured.
func (r *Registry) SkillsRoot() string { return r.skillRoot() }

// rootForPath returns the root a relative path should resolve against:
//   - systemRoot (agent home) for identity files (SOUL.md, IDENTITY.md, …);
//   - userSkillsRoot (~/.fastagent/users/<uid>/skills/) for `skills/...`
//     writes when the chatter's user-skills dir is wired (default in
//     multi-user installs). Routes here so chat-created skills accumulate
//     in the chatter's personal bucket — shared across every agent they
//     chat with, isolated from the agent owner's official skills and from
//     other users on the same shared agent. Falls back to systemRoot when
//     userSkillsRoot is empty (legacy / single-user installs);
//   - userRoot (agent workspace) for everything else, which is user-facing
//     artifact territory.
//
// Absolute paths are returned as-is.
func (r *Registry) rootForPath(path string) string {
	if filepath.IsAbs(path) {
		return ""
	}
	clean := filepath.Clean(path)
	if clean == "skills" || strings.HasPrefix(clean, "skills"+string(filepath.Separator)) {
		// Per-user bucket when configured, otherwise the agent home
		// (legacy behavior). The leading `skills/` prefix is preserved
		// in either case so SkillsLoader's scan picks it up.
		if r.userSkillsRoot != "" {
			return r.userSkillsRoot
		}
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
	r.Register("read_file", "Read the contents of a file",
		singlePathSchema("File path (relative to your working directory or absolute)"), makeReadFile(r))

	r.Register("write_file", writeFileDescription,
		writeFileSchema("File path (relative to your working directory or absolute)"), makeWriteFile(r))

	r.Register("list_dir", "List files and directories in a path",
		singlePathSchema("Directory path (relative to your working directory or absolute)"), makeListDir(r))

	r.Register("edit_file", editDescription, editSchema, makeEditFile(r))
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

// isGlobalSkillsPath reports whether absPath points at or under the
// admin-managed ~/.fastagent/skills/ directory. Works across user home
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

		// Identity-file confidentiality gate. A chatter who asks "show me
		// your SOUL.md" must not get the verbatim persona spec back.
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		// Skill-manifest gate. Same rationale for a bundled SKILL.md —
		// the agent's IP, not for a chatter to pull verbatim.
		if r.skillManifestBlocked(args.Path) {
			return SkillManifestRefusal, nil
		}

		// Mirror makeWriteFile's routing: userRoot-destined paths go to the
		// workspace store when one is configured.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.scopeSessionID(), r.wsPath(args.Path))
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
			// DB first, then the agent's systemRoot on disk, then empty — the
			// shared rule (registry.readSystemFileWithFallback) so this branch
			// can't drift from edit_file's. The disk step bypasses
			// resolvePathSandboxed deliberately: systemRoot is the agent
			// metadata dir (e.g. ~/.fastagent/agents/<id>/agent) which in K8s
			// deployments lives OUTSIDE sandboxRoot, so the sandbox bound would
			// always reject identity files even though the filename is a fixed
			// whitelist with no escape surface. "Not found" is legitimate (a
			// fresh agent may have no IDENTITY.md row yet) — return empty so the
			// agent treats the field as unset, matching how
			// ContextBuilder.loadFile loads identity files for the system
			// prompt.
			return string(r.readSystemFileWithFallback(ctx, r.systemFileUserID(name), name)), nil
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
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("write_file: %w", err)
		}

		// Identity-file confidentiality gate — also blocks a chatter
		// from REWRITING the agent's persona via prompt injection.
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		if r.ownerManagedFileWriteBlocked(args.Path) {
			return OwnerManagedFileWriteRefusal, nil
		}

		// When a workspace store is configured, route userRoot-destined
		// writes through it. Identity files (systemRoot) still hit the
		// filesystem because the memory store already covers their
		// durability via a separate path.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			if err := r.putGuarded(ctx, "write_file", args.Path, []byte(args.Content)); err != nil {
				return "", err
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
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(args.Content)); err != nil {
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

		// Skill scaffolding takes a dedicated path so the same writeSkillToHost
		// helper that handles sandbox-mode also lands the file + mirrors to
		// the workspace store here, instead of duplicating the SyncSkillUp
		// hook in two places.
		if r.isSkillPath(args.Path) && r.skillRoot() != "" {
			full, err := r.writeSkillToHost(ctx, args.Path, args.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
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
			if friendly := asIsDirToolError("write_file", args.Path, err); friendly != nil {
				return "", friendly
			}
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
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("edit_file: %w", err)
		}

		// Identity-file confidentiality gate.
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		if r.ownerManagedFileWriteBlocked(args.Path) {
			return OwnerManagedFileWriteRefusal, nil
		}
		// Skill-manifest gate — block edits to a bundled SKILL.md for
		// non-admin chatters (editing returns surrounding content + lets
		// them tamper with the agent's IP). Owner edits skills via the UI.
		if r.skillManifestBlocked(args.Path) {
			return SkillManifestRefusal, nil
		}

		// Mirror makeWriteFile's routing precedence: workspace store first
		// (user artifacts), then identity-file store (SOUL.md / IDENTITY.md /
		// MEMORY.md …), then filesystem. The read and the write must hit
		// the same backend or an edit could silently land in a different
		// store than the one the agent later reads from.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.scopeSessionID(), r.wsPath(args.Path))
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
			if err := r.putGuarded(ctx, "edit_file", args.Path, []byte(updated)); err != nil {
				return "", err
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
		}

		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			uid := r.systemFileUserID(name)
			// Same resolution as read_file (registry.readSystemFileWithFallback):
			// a never-saved per-chatter MEMORY.md / USER.md row, or an identity
			// file that only exists on disk, is an EMPTY BASE — not the raw
			// store error the model used to be handed ("system file get: store:
			// not found"). When nothing exists at all the base is empty and
			// applyEdit reports an actionable old_string miss instead.
			data := r.readSystemFileWithFallback(ctx, uid, name)
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			// Conditional on the copy this call just read: the identity files have several writers
			// (this tool, the distiller, the panel), and a read-modify-write that ignores that
			// reports "Edited" while somebody else's version is what survives. The workspace path
			// above has had this guard since putGuarded; this branch is the one that was missing it
			// (§13.2 / row 83 of docs/fs-formal-proof/11-change-register.md).
			if err := r.systemFileStore.SaveWorkspaceFileIfUnchanged(ctx, r.agentID, uid, name, []byte(updated), string(data)); err != nil {
				if errors.Is(err, store.ErrAgentFileConflict) {
					return "", fmt.Errorf("another writer changed %s while this turn was working; nothing was overwritten — read it again and re-apply your change", args.Path)
				}
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
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.projectID, r.scopeSessionID())
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

// previousStoreVersion reads the object a store write is about to lose, so the
// mirror can tell "the sandbox still held the version it was handed" from "the
// sandbox held its own edit" (docs 10 §2.1, G6). Only edit_file used to pass an
// expectation, because it had to read the file anyway; this gives write_file the
// same precision for one extra GET.
//
// Best-effort by construction: on any error, or for anything over the cap below,
// it returns "" — the write proceeds with no expectation, and the mirror then
// states only what it can ("a different version was replaced", not "which one").
func (r *Registry) previousStoreVersion(ctx context.Context, key string) string {
	if r.workspaceStore == nil || r.agentID == "" || key == "" {
		return ""
	}
	rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.scopeSessionID(), key)
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, expectationReadMax+1))
	if err != nil || len(data) > expectationReadMax {
		return ""
	}
	return string(data)
}

// expectationReadMax bounds the extra read previousStoreVersion performs: past
// this size the precision is not worth another full object read.
const expectationReadMax = 2 << 20

// putGuarded is the family-B write: the store evaluates "is this still the
// version I saw" inside the same request as the write (docs 12 §3.1, L7), so a
// writer that was superseded by a panel upload, another session on the same
// project key, or a sibling replica is told instead of silently clobbering.
//
// The expectation is read immediately before the write. Two paths could carry
// a cheaper one — edit_file and apply_patch already read the object — and that
// optimisation is deliberately left out of this first cut: one extra HEAD is
// cheaper than threading a version through three call sites.
func (r *Registry) putGuarded(ctx context.Context, tool, path string, data []byte) error {
	key := r.wsPath(path)
	expected := workspace.VersionAbsent
	if info, err := r.workspaceStore.Stat(ctx, r.agentID, r.projectID, r.scopeSessionID(), key); err == nil && info != nil {
		expected = info.Version
	}
	err := r.workspaceStore.PutIfVersion(ctx, r.agentID, r.projectID, r.scopeSessionID(), key,
		strings.NewReader(string(data)), int64(len(data)), "", expected)
	if errors.Is(err, workspace.ErrVersionConflict) {
		return fmt.Errorf("another writer changed %s while this turn was working; nothing was overwritten — read it again and re-apply your change", path)
	}
	if err != nil {
		// One answer is this function's to add — a stale expectation. Everything
		// else the store can say (a 500, a permission error, a closed
		// connection) belongs to the caller: returning nil here reported a write
		// that never happened as a success, the silent-loss class this family
		// exists to remove.
		if friendly := asIsDirToolError(tool, path, err); friendly != nil {
			return friendly
		}
		return fmt.Errorf("workspace put: %w", err)
	}
	return nil
}

// sandboxFileSet returns the paths the live sandbox holds, in the same logical
// space the file tools use (paths relative to /workspace). Returns nil when there
// is no second copy to compare with — docker's /workspace IS the host directory —
// or when the sandbox could not be asked, because a signal we cannot ground is
// worse than no signal.
func (r *Registry) sandboxFileSet(ctx context.Context, ex sandbox.Executor) map[string]bool {
	if ex == nil {
		return nil
	}
	if _, remote := ex.(sandbox.RemoteWorkspace); !remote {
		return nil
	}
	out, err := ex.Exec(ctx, `find /workspace -type f -printf '%P\n' 2>/dev/null`, 20*time.Second)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			set[p] = true
		}
	}
	return set
}

// storeOnlySignal collects the paths a store listing has and the live sandbox
// does not, and renders them with the ONE shared sentence for that fact
// (sandbox.StoreOnlyLine) — the reconcile states the same fact from the other
// side, and two wordings for one fact is how a rule drifts (docs 10 §4: G7a
// for this moment, G4 for the reconcile's).
//
// Empty when the two sides agree: this is an exception channel.
func storeOnlySignal(inSandbox map[string]bool, listed []string) string {
	if inSandbox == nil {
		return ""
	}
	var missing []string
	for _, p := range listed {
		if !inSandbox[p] {
			missing = append(missing, p)
		}
	}
	return sandbox.StoreOnlyLine(missing)
}

// writeThroughSignal mirrors one workspace write into the sandbox that serves
// the same scope, and returns the σ the agent must read when the sandbox was
// left holding something else. Empty string is the normal case (C3: an
// exception channel only).
//
// It generalises what used to be mirrorCodingWriteToSandbox. That function
// existed for live preview (a remote backend shares no host mount, so the dev
// server never saw the edit and HMR looked dead) and only ran when a coding
// subdir was configured. The same mirror is what keeps the workspace coherent:
// with it, a path's sandbox copy equals the host's version at the moment of the
// write, so a later sandbox edit of that path is unambiguously the sandbox's own
// work and the reconcile may take it. Background and proof:
// docs/文件系统形式化证明/07-formal-rootcause-and-fix.md §2, §3.3.
//
// Best-effort by design: the user-visible write already landed in the store, so
// a failed mirror must not fail the tool — but it must not stay silent either.
// "Your write is safe in the store; anything reading it INSIDE the sandbox may
// still see an older version" is exactly the fact the 2026-09-17 incident was
// missing from the agent's context (07 §2).
//
// The destination is ABSOLUTE — /workspace/<wsPath> — because envd resolves a
// bare path against $HOME, and because that is where the hydrate puts the file
// for this scope (01 §3.5).
//
// Each branch states what happened to the OTHER copy; nothing here decides for
// the agent, and nothing keeps a copy of what it replaced — the sandbox changes
// the agent was told about in earlier exec results are what that version was.
func (r *Registry) writeThroughSignal(ctx context.Context, path, content, previous string) string {
	if r.executor == nil {
		return ""
	}
	wt, ok := r.executor.(sandbox.WriteThroughExecutor)
	if !ok {
		return "" // backend without a separate copy (docker) — the host IS /workspace
	}
	storeKey := r.wsPath(path)
	dest := "/workspace/" + strings.TrimPrefix(filepath.ToSlash(storeKey), "/")
	// The store scope travels with the call: this tool knows which key it just
	// wrote (scopeSessionID() collapses in coding-root mode, so the project root
	// and the chat's container can disagree), and the pool must not infer it —
	// that inference is what made the stamp silently miss in project sessions
	// (docs 10 §4 G22).
	outcome, err := wt.WriteThroughScope(
		sandbox.StoreScope{ProjectID: r.projectID, SessionID: r.scopeSessionID()},
		storeKey, dest, content, previous)
	switch {
	case err != nil:
		// The sandbox could not be updated. No second version exists to choose
		// between (the sandbox is unreachable, or its copy is gone), so this is
		// a statement of what could not be checked — not a decision to make.
		return fmt.Sprintf("\n\n[workspace] the sandbox copy of %s could not be updated (%v). "+
			"Your write is safe in the workspace store; anything reading the file INSIDE the sandbox may still see an older version.", path, err)
	case outcome.Comparison == sandbox.CompareReplacedVerified:
		// The sandbox held a different version and the write replaced it. Say
		// so with the size, and with no content: the agent was already told
		// what the sandbox changed (the exec signal lists it), so the write is
		// an informed decision — the signal just keeps the fact on the record.
		return fmt.Sprintf("\n\n[workspace] your write to %s replaced a different version in the sandbox (%d bytes). "+
			"No copy was kept: the sandbox changes you were told about in earlier exec results are what that version was, "+
			"so re-run the producing command if you need it.", path, outcome.SandboxBytes)
	case outcome.Comparison == sandbox.CompareReplacedUnchecked:
		// Something was replaced and we cannot say what it was: there was no
		// expectation to check the sandbox's bytes against. State exactly that —
		// the previous wording claimed "over 2 MiB" for every such write, which
		// was false for all but the largest files (docs 10 §2.1, G5).
		return fmt.Sprintf("\n\n[workspace] the sandbox held a different version of %s (%d bytes) and your write replaced it. "+
			"There was no earlier copy to compare it against, so what that version was cannot be reconstructed here.", path, outcome.SandboxBytes)
	}
	// CompareNoSecondCopy (docker: one copy, nothing to mirror) and
	// CompareNothingReplaced both mean "nothing was destroyed": silence, per C3.
	// The one thing that can still be worth saying is a PARTIAL mirror: this
	// container got the file, another container of the same project did not —
	// which a dev server running there turns into "my edits don't show up".
	if outcome.BroadcastFailures > 0 {
		return fmt.Sprintf("\n\n[workspace] your write to %s landed in this sandbox, but %d other sandbox(es) of this project "+
			"could not be updated — a preview running there may keep showing an older version. "+
			"Writing the file again retries them.", path, outcome.BroadcastFailures)
	}
	return ""
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
	// One line on purpose, same as write_file below: wrapping the argument list
	// would re-indent the closure and bury the change in whitespace.
	r.Register("read_file", "Read the contents of a file", singlePathSchema("File path (identity file, workspace-relative, or absolute inside the sandbox)"), func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		// Identity-file confidentiality gate — same as the host path.
		// Runs BEFORE the systemFileStore lookup so a chatter never
		// reaches the DB row at all.
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		// Skill-manifest gate — refuse before any routing so a chatter
		// can't read a bundled `/skills/<name>/SKILL.md` off the mount.
		if r.skillManifestBlocked(args.Path) {
			return SkillManifestRefusal, nil
		}
		// Identity files (SOUL.md, IDENTITY.md, …) are routed by basename
		// (lenient) instead of the strict isSingleSegmentSystemFile that
		// routeFor uses. Need to be checked separately for reads so an
		// LLM-emitted absolute path like
		// /data/.fastagent/workspaces/<id>/IDENTITY.md still hits the DB.
		if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(args.Path) {
			name := filepath.Base(filepath.Clean(args.Path))
			// Same resolution as the host path: miss → unset (fresh agent),
			// plus the owner-only disk fallback for a file that exists on the
			// agent home but has no row yet.
			return string(r.readSystemFileWithFallback(ctx, r.systemFileUserID(name), name)), nil
		}
		switch r.routeFor(args.Path, OpRead) {
		case RouteWorkspaceStore:
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.scopeSessionID(), r.wsPath(args.Path))
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
			// Fall through to sandbox on store miss so a freshly-written
			// file the agent put inside the sandbox (mid-turn, not yet
			// mirrored to store) is still readable.
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		case RouteSkillStore:
			full := filepath.Join(r.skillRoot(), filepath.Clean(args.Path))
			if data, err := os.ReadFile(full); err == nil {
				if looksBinary(data) {
					return binaryRefusal(args.Path, len(data)), nil
				}
				return string(data), nil
			}
			// Fall through to sandbox so pre-mounted skills under
			// /skills/<name>/ inside the container are still reachable.
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path // explicit absolute non-home path
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("host read %s: %w", full, err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			return string(data), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox (and RouteSystemStore handled above out-of-band)
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		}
	})

	// One line on purpose: wrapping the argument list would re-indent the closure
	// below and bury the change this makes in a hundred lines of whitespace.
	r.Register("write_file", writeFileDescription, writeFileSchema("File path (identity file, workspace-relative, or absolute inside the sandbox)"), func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("write_file: %w", err)
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		if r.ownerManagedFileWriteBlocked(args.Path) {
			return OwnerManagedFileWriteRefusal, nil
		}
		switch r.routeFor(args.Path, OpWrite) {
		case RouteSystemStore:
			name := filepath.Clean(args.Path)
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(args.Content)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), name), nil
		case RouteWorkspaceStore:
			// Read what the store is about to lose *before* the write, so the
			// mirror can tell a stale sandbox copy from the sandbox's own edit
			// (docs 10 §2.1, G6). Best-effort: "" means "no expectation", and the
			// mirror then only states what it could establish.
			previous := r.previousStoreVersion(ctx, r.wsPath(args.Path))
			// The SAME family-B guard as the non-sandbox registration above.
			// These are two separate registrations, not one body called twice:
			// this one is what SetExecutor installs, so it is the branch a cloud
			// turn runs — a guard that lives only in the other branch leaves the
			// production path blind-writing, whatever the tests on that branch
			// say.
			if err := r.putGuarded(ctx, "write_file", args.Path, []byte(args.Content)); err != nil {
				return "", err
			}
			r.addSignal(ctx, r.writeThroughSignal(ctx, args.Path, args.Content, previous))
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		case RouteSkillStore:
			// Skill scaffolding (skill-creator's `skills/<name>/...`) lands
			// on host disk where SkillsLoader scans + mirrors to OSS, NOT
			// in the ephemeral sandbox FS. writeSkillToHost handles both.
			full, err := r.writeSkillToHost(ctx, args.Path, args.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", fmt.Errorf("create directory: %w", err)
			}
			if err := os.WriteFile(full, []byte(args.Content), 0o644); err != nil {
				return "", fmt.Errorf("host write %s: %w", full, err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox
			out, err := ex.WriteFile(ctx, args.Path, args.Content)
			return MetaSandboxPrefix + out, err
		}
	})

	r.Register("list_dir", "List files and directories in a path", singlePathSchema("Directory path (workspace-relative or absolute inside the sandbox)"), func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		switch r.routeFor(args.Path, OpList) {
		case RouteWorkspaceStore:
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.projectID, r.scopeSessionID())
			if err == nil {
				prefix := strings.Trim(filepath.ToSlash(filepath.Clean(args.Path)), "/")
				if prefix == "." {
					prefix = ""
				}
				var sb strings.Builder
				seenDirs := map[string]bool{}
				// The listing below comes from the STORE. `exec` reads the
				// SANDBOX. When the two disagree the agent is the only one who
				// can reconcile them, so the disagreement is stated where it is
				// cheapest to see: next to the listing that would mislead.
				// (docs 10 §3.2, G7 — a running sandbox is not updated by an
				// upload, so "read_file sees it, exec does not" is normal today.)
				inSandbox := r.sandboxFileSet(ctx, ex)
				var listed []string
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
					listed = append(listed, p)
				}
				if note := storeOnlySignal(inSandbox, listed); note != "" {
					r.addSignal(ctx, note)
				}
				return sb.String(), nil
			}
			// Store error → fall through to sandbox so a freshly-written
			// sandbox-only dir is still listable.
			out, err := ex.ListDir(ctx, args.Path)
			return MetaSandboxPrefix + out, err
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return "", fmt.Errorf("host list %s: %w", full, err)
			}
			var sb strings.Builder
			for _, e := range entries {
				if e.IsDir() {
					fmt.Fprintf(&sb, "d %s/\n", e.Name())
					continue
				}
				info, ierr := e.Info()
				if ierr != nil {
					fmt.Fprintf(&sb, "f %s\n", e.Name())
					continue
				}
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", e.Name(), info.Size())
			}
			return sb.String(), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox / RouteSystemStore (no list semantics) / RouteSkillStore
			out, err := ex.ListDir(ctx, args.Path)
			return MetaSandboxPrefix + out, err
		}
	})

	r.Register("edit_file", editDescription, editSchema, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args editFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("edit_file: %w", err)
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		if r.ownerManagedFileWriteBlocked(args.Path) {
			return OwnerManagedFileWriteRefusal, nil
		}
		if r.skillManifestBlocked(args.Path) {
			return SkillManifestRefusal, nil
		}

		// editSandboxRMW is the read-modify-write fallback through the
		// sandbox executor. Used when the store route misses or for any
		// path routeFor sends to the sandbox.
		editSandboxRMW := func() (string, error) {
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
			if _, err := ex.WriteFile(ctx, args.Path, updated); err != nil {
				return "", err
			}
			return MetaSandboxPrefix + fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
		}

		switch r.routeFor(args.Path, OpWrite) {
		case RouteSystemStore:
			if r.ownerManagedFileWriteBlocked(args.Path) {
				return OwnerManagedFileWriteRefusal, nil
			}
			name := filepath.Clean(args.Path)
			uid := r.systemFileUserID(name)
			// See the store-backed branch above: missing row ⇒ empty base, not
			// a leaked storage error.
			data := r.readSystemFileWithFallback(ctx, uid, name)
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, uid, name, []byte(updated)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", name, count), nil
		case RouteWorkspaceStore:
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.scopeSessionID(), r.wsPath(args.Path))
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
					if err := r.putGuarded(ctx, "edit_file", args.Path, []byte(updated)); err != nil {
						return "", err
					}
					r.addSignal(ctx, r.writeThroughSignal(ctx, args.Path, updated, string(data)))
					return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
				}
			}
			// Store miss → sandbox RMW so a freshly-created file (not yet
			// mirrored) is still editable.
			return editSandboxRMW()
		case RouteSkillStore:
			// Skill files have no in-place edit semantics here today; fall
			// through to the sandbox RMW which can read /skills/<name>/...
			// from the read-only mount and write through (write will fail
			// at the FS layer if the mount is RO; that's the right error
			// to surface to the model).
			return editSandboxRMW()
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("host read %s: %w", full, err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
				return "", fmt.Errorf("host write %s: %w", full, err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", full, count), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox
			return editSandboxRMW()
		}
	})
}
