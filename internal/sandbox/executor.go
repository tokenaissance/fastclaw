package sandbox

import (
	"context"
	"time"
)

// Executor abstracts a sandboxed execution environment for one user.
// All agent tool calls (exec, read_file, write_file, list_dir) are routed
// through this interface in cloud mode so that each user gets an isolated
// filesystem and runtime. Implementations can be Docker containers,
// Firecracker microVMs, E2B hosted sandboxes, or any other backend.
type Executor interface {
	// Exec runs a shell command and returns combined stdout+stderr.
	Exec(ctx context.Context, command string, timeout time.Duration) (string, error)
	// ReadFile reads a file from the sandbox filesystem.
	ReadFile(ctx context.Context, path string) (string, error)
	// WriteFile writes content to a file (creating parent dirs as needed).
	WriteFile(ctx context.Context, path, content string) (string, error)
	// ListDir lists a directory and returns a human-readable listing.
	ListDir(ctx context.Context, path string) (string, error)
	// Close destroys the sandbox and releases resources.
	Close() error
}

// ExecutorPool manages per-user sandbox lifecycles. Get lazily creates a
// sandbox on first access; Release tears it down.
type ExecutorPool interface {
	Get(ctx context.Context, userID string) (Executor, error)
	Release(userID string) error
	CloseAll()
}

// WorkspaceSnapshotter is an optional capability an Executor can implement
// to support flush-on-evict. Returns a map of sandbox-relative path →
// file contents for everything under /workspace.
//
// Implementations are expected to be best-effort: a file's content should
// reflect what the sandbox sees at the time of the call, but perfect
// consistency with a live shell is not promised (agent should not be
// writing during flush). Large files / binaries are returned as-is.
//
// Not part of the base Executor interface because not every backend can
// cheaply enumerate its workspace (e.g. E2B requires an extra API call);
// callers should type-assert and skip gracefully when absent.
type WorkspaceSnapshotter interface {
	SnapshotWorkspace(ctx context.Context) (map[string][]byte, error)
}

// PoolConfig holds configuration for creating sandbox pools.
type PoolConfig struct {
	Backend   string // "docker", "e2b" (future)
	Image     string // container image (for docker backend)
	Policy    *Policy
	// E2B-specific fields (future)
	E2BTemplate string
	E2BAPIKey   string
}
