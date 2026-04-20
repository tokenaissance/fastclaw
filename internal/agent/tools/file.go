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

func makeReadFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Mirror makeWriteFile's routing: userRoot-destined paths go to the
		// workspace store when one is configured.
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, args.Path)
			if err != nil {
				return "", fmt.Errorf("workspace get: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return "", fmt.Errorf("workspace read: %w", err)
			}
			return string(data), nil
		}

		fullPath, err := resolvePathSandboxed(r.rootForPath(args.Path), r.sandboxRoot, args.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
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
			if err := r.workspaceStore.Put(ctx, r.agentID, args.Path,
				strings.NewReader(args.Content), int64(len(args.Content)), ""); err != nil {
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		}

		fullPath, err := resolvePathSandboxed(r.rootForPath(args.Path), r.sandboxRoot, args.Path)
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
			objs, err := r.workspaceStore.List(ctx, r.agentID)
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

		fullPath, err := resolvePathSandboxed(r.rootForPath(args.Path), r.sandboxRoot, args.Path)
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
// sandbox.Executor instead of operating on the host filesystem.
func registerSandboxedFile(r *Registry, ex sandbox.Executor) {
	r.Register("read_file", "Read the contents of a file", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path inside the sandbox workspace",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		return ex.ReadFile(ctx, args.Path)
	})

	r.Register("write_file", "Write content to a file (creates directories as needed)", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path inside the sandbox workspace",
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
		return ex.WriteFile(ctx, args.Path, args.Content)
	})

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path inside the sandbox workspace",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		return ex.ListDir(ctx, args.Path)
	})
}
