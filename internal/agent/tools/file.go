package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func registerFile(r *Registry, workspace string) {
	r.Register("read_file", "Read the contents of a file", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to workspace or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeReadFile(workspace, &r.sandboxRoot))

	r.Register("write_file", "Write content to a file (creates directories as needed)", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to workspace or absolute)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}, makeWriteFile(workspace, &r.sandboxRoot))

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (relative to workspace or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeListDir(workspace, &r.sandboxRoot))
}

func resolvePath(workspace, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(workspace, path))
}

// resolvePathSandboxed resolves a path and validates that it stays within
// sandboxRoot. Returns an error when the resolved path escapes.
func resolvePathSandboxed(workspace, sandboxRoot, path string) (string, error) {
	full := resolvePath(workspace, path)
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

func makeReadFile(workspace string, sandboxRoot *string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		fullPath, err := resolvePathSandboxed(workspace, *sandboxRoot, args.Path)
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

func makeWriteFile(workspace string, sandboxRoot *string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		fullPath, err := resolvePathSandboxed(workspace, *sandboxRoot, args.Path)
		if err != nil {
			return "", err
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

func makeListDir(workspace string, sandboxRoot *string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		fullPath, err := resolvePathSandboxed(workspace, *sandboxRoot, args.Path)
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
