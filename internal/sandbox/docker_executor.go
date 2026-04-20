package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DockerExecutor wraps DockerSandbox to implement Executor. The container
// has the user's workspace mounted at /workspace and all tool calls are
// forwarded as docker exec commands.
type DockerExecutor struct {
	sb *DockerSandbox
}

// NewDockerExecutor creates a sandbox Executor backed by a Docker container.
// workspace is the host-side directory to mount (e.g. the user's workspace
// synced from S3, or a tmpdir for ephemeral use).
func NewDockerExecutor(image, workspace string, policy *Policy) (*DockerExecutor, error) {
	sb := NewDockerSandbox(image, workspace, policy)
	if err := sb.Create(); err != nil {
		return nil, fmt.Errorf("create docker sandbox: %w", err)
	}
	return &DockerExecutor{sb: sb}, nil
}

func (d *DockerExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return d.sb.Exec(execCtx, command, "/workspace")
}

func (d *DockerExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	return d.sb.Exec(ctx, fmt.Sprintf("cat %s", shellQuote(path)), "/workspace")
}

func (d *DockerExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	// Create parent dirs, then write via heredoc.
	cmd := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cat > %s << 'FASTCLAW_EOF'\n%s\nFASTCLAW_EOF",
		shellQuote(path), shellQuote(path), content)
	out, err := d.sb.Exec(ctx, cmd, "/workspace")
	if err != nil {
		return out, err
	}
	return fmt.Sprintf("Written to %s", path), nil
}

func (d *DockerExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return d.sb.Exec(ctx, fmt.Sprintf("ls -la %s", shellQuote(path)), "/workspace")
}

func (d *DockerExecutor) Close() error {
	return d.sb.Close()
}

// SnapshotWorkspace walks the host-side mounted workspace dir (which is
// bind-mounted into the container at /workspace) and returns every
// regular file's bytes keyed by its container-relative path. Used by
// LifecyclePool for flush-on-evict so files the agent created via
// `exec` (not via write_file) still make it to the durable store.
//
// Walking the host dir directly is faster and more reliable than doing
// tar-over-exec: the mount already gives us a POSIX view of the same
// bytes the sandbox sees.
func (d *DockerExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	root := d.sb.workspace
	if root == "" {
		return nil, nil
	}
	out := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// Ensure DockerExecutor satisfies the optional snapshot contract. A compile
// error here would flag any accidental interface drift.
var _ WorkspaceSnapshotter = (*DockerExecutor)(nil)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// DockerExecutorPool manages per-user DockerExecutor instances.
type DockerExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*DockerExecutor
	image     string
	policy    *Policy
	// workspaceRoot is the base dir; each user gets workspaceRoot/{userID}/
	workspaceRoot string
}

// NewDockerExecutorPool creates a pool of Docker-backed executors.
func NewDockerExecutorPool(image, workspaceRoot string, policy *Policy) *DockerExecutorPool {
	if image == "" {
		image = "fastclaw/sandbox:latest"
	}
	return &DockerExecutorPool{
		executors:     make(map[string]*DockerExecutor),
		image:         image,
		policy:        policy,
		workspaceRoot: workspaceRoot,
	}
}

func (p *DockerExecutorPool) Get(ctx context.Context, userID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ex, ok := p.executors[userID]; ok {
		return ex, nil
	}

	workspace := fmt.Sprintf("%s/%s", p.workspaceRoot, userID)
	ex, err := NewDockerExecutor(p.image, workspace, p.policy)
	if err != nil {
		return nil, err
	}
	p.executors[userID] = ex
	return ex, nil
}

func (p *DockerExecutorPool) Release(userID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ex, ok := p.executors[userID]; ok {
		delete(p.executors, userID)
		return ex.Close()
	}
	return nil
}

func (p *DockerExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, ex := range p.executors {
		ex.Close()
		delete(p.executors, uid)
	}
}

// Ensure interfaces are satisfied.
var (
	_ Executor     = (*DockerExecutor)(nil)
	_ ExecutorPool = (*DockerExecutorPool)(nil)
)
