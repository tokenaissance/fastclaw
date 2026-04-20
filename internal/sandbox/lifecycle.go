package sandbox

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// bytesReader wraps a byte slice as an io.Reader — inlined helper so flush
// code doesn't clutter with bytes.NewReader calls.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// LifecyclePool wraps any ExecutorPool with two knobs that matter for cost
// in multi-tenant cloud deployments:
//
//  1. Lazy creation — sandboxes aren't spun up until the first tool call.
//     An agent that just chats (no exec/read_file/write_file) never starts
//     one, so idle users pay nothing for sandbox compute.
//  2. Idle eviction — a background sweeper Release()s sandboxes that have
//     been unused for IdleTTL. The next call recreates them; in the
//     meantime nothing is running.
//
// Backend-agnostic: works with DockerExecutorPool, E2B, or any future
// implementation. The inner pool still handles the actual create/destroy.
type LifecyclePool struct {
	inner   ExecutorPool
	idleTTL time.Duration
	sweep   time.Duration

	mu       sync.Mutex
	lastUsed map[string]time.Time // agentID → last activity timestamp
	// hydrated tracks whether we've already copied workspace.Store contents
	// into this agent's sandbox. Drops back to false on eviction so the
	// next lazy-creation re-hydrates from the durable store.
	hydrated map[string]bool

	// workspace is the optional blob store that bootstraps /workspace on
	// sandbox creation. When nil, sandboxes start empty and rely on
	// write_file tool calls (which already write through workspace.Store)
	// to produce files the agent later reads via read_file.
	workspace workspace.Store

	stopCh chan struct{}
	done   chan struct{}
}

// NewLifecyclePool wraps inner with idle tracking. idleTTL=0 disables
// eviction (everything stays alive); sweep=0 uses a sensible default.
func NewLifecyclePool(inner ExecutorPool, idleTTL, sweep time.Duration) *LifecyclePool {
	if sweep <= 0 {
		sweep = 30 * time.Second
	}
	return &LifecyclePool{
		inner:    inner,
		idleTTL:  idleTTL,
		sweep:    sweep,
		lastUsed: make(map[string]time.Time),
		hydrated: make(map[string]bool),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetWorkspace installs the durable blob store used to bootstrap each
// sandbox on first tool call. Pass nil to disable hydrate (sandboxes start
// with empty /workspace).
func (p *LifecyclePool) SetWorkspace(ws workspace.Store) { p.workspace = ws }

// Start the idle sweep goroutine. Safe to call multiple times; only the
// first start actually kicks off the loop.
func (p *LifecyclePool) Start() {
	if p.idleTTL <= 0 {
		close(p.done) // nothing to do; keep Shutdown() cheap
		return
	}
	go p.loop()
}

func (p *LifecyclePool) loop() {
	defer close(p.done)
	t := time.NewTicker(p.sweep)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

// evictIdle scans lastUsed and Release()s anything older than idleTTL.
// Held per-iteration lock; Release may be slow (destroys a container), so
// we release the map lock before the actual teardown to avoid blocking new
// Get()s on other agents.
func (p *LifecyclePool) evictIdle() {
	cutoff := time.Now().Add(-p.idleTTL)
	p.mu.Lock()
	toEvict := make([]string, 0)
	for agentID, t := range p.lastUsed {
		if t.Before(cutoff) {
			toEvict = append(toEvict, agentID)
		}
	}
	// Remove from map under lock so a racing Get doesn't mistake an
	// evicted sandbox for a live one. Also clear hydrated flag: the next
	// lazy-creation will re-sync from the workspace store.
	for _, id := range toEvict {
		delete(p.lastUsed, id)
		delete(p.hydrated, id)
	}
	p.mu.Unlock()

	for _, id := range toEvict {
		// Best-effort flush: if the executor implements
		// WorkspaceSnapshotter and we have a workspace store, upload
		// anything the sandbox wrote (that wasn't already written via
		// write_file) before destroying it.
		p.flushIfSupported(id)

		if err := p.inner.Release(id); err != nil {
			slog.Warn("sandbox evict failed", "agent", id, "error", err)
			continue
		}
		slog.Info("sandbox evicted (idle)", "agent", id, "idleTTL", p.idleTTL)
	}
}

// flushIfSupported snapshots the sandbox workspace and uploads anything
// that isn't already in the durable store. Skips silently when the backend
// doesn't implement WorkspaceSnapshotter (E2B / future backends) or when
// no workspace.Store is configured.
func (p *LifecyclePool) flushIfSupported(agentID string) {
	if p.workspace == nil {
		return
	}
	ex, err := p.inner.Get(context.Background(), agentID)
	if err != nil {
		return
	}
	snapper, ok := ex.(WorkspaceSnapshotter)
	if !ok {
		return
	}
	files, err := snapper.SnapshotWorkspace(context.Background())
	if err != nil {
		slog.Warn("sandbox flush: snapshot failed", "agent", agentID, "error", err)
		return
	}
	written := 0
	for path, data := range files {
		// Skip files that the store already has with identical size —
		// avoids rewriting every file every eviction when nothing
		// changed. Content equality would be stricter but requires a
		// full round-trip per file; size is usually enough.
		if info, err := p.workspace.Stat(context.Background(), agentID, path); err == nil && info.Size == int64(len(data)) {
			continue
		}
		if err := p.workspace.Put(context.Background(), agentID, path, bytesReader(data), int64(len(data)), ""); err != nil {
			slog.Warn("sandbox flush: put failed", "agent", agentID, "path", path, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		slog.Info("sandbox flushed to workspace store", "agent", agentID, "files", written)
	}
}

// Get returns a lazy proxy: tool calls on it will fetch the underlying
// executor from the inner pool on demand (creating a new sandbox if
// needed) and tick the last-used timestamp.
//
// Contract matches ExecutorPool.Get so LifecyclePool is a drop-in wrapper.
func (p *LifecyclePool) Get(ctx context.Context, agentID string) (Executor, error) {
	return &lazyExecutor{pool: p, agentID: agentID}, nil
}

// Release forwards to the inner pool and drops the lastUsed entry. Useful
// for explicit teardown (agent deletion) — normal flow relies on idle
// eviction.
func (p *LifecyclePool) Release(agentID string) error {
	p.mu.Lock()
	delete(p.lastUsed, agentID)
	p.mu.Unlock()
	return p.inner.Release(agentID)
}

// CloseAll stops the sweeper and tears down every live sandbox. Called on
// gateway shutdown; skipping this would leak E2B instances that cost money
// until their max-TTL expires.
func (p *LifecyclePool) CloseAll() {
	select {
	case <-p.stopCh:
		// already stopped
	default:
		close(p.stopCh)
	}
	<-p.done
	p.inner.CloseAll()
	p.mu.Lock()
	p.lastUsed = make(map[string]time.Time)
	p.hydrated = make(map[string]bool)
	p.mu.Unlock()
}

// touch updates the last-used timestamp for agentID. Kept for external use;
// internal getInner now touches directly so it can batch the update with
// the hydrated-flag check under one lock.
func (p *LifecyclePool) touch(agentID string) {
	p.mu.Lock()
	p.lastUsed[agentID] = time.Now()
	p.mu.Unlock()
}

// inner fetches the underlying Executor, creating on first call. Separate
// from Get() so lazyExecutor can update lastUsed each time. On first
// creation (either fresh or post-eviction) it hydrates /workspace from the
// configured workspace.Store so exec'd commands see the files that
// write_file has produced in previous sessions.
func (p *LifecyclePool) getInner(ctx context.Context, agentID string) (Executor, error) {
	p.mu.Lock()
	needsHydrate := !p.hydrated[agentID]
	p.lastUsed[agentID] = time.Now()
	if needsHydrate {
		p.hydrated[agentID] = true // set eagerly so a concurrent second call doesn't double-hydrate
	}
	p.mu.Unlock()

	ex, err := p.inner.Get(ctx, agentID)
	if err != nil {
		// Roll back the hydrated flag so a retry will try again.
		p.mu.Lock()
		p.hydrated[agentID] = false
		p.mu.Unlock()
		return nil, err
	}
	if needsHydrate && p.workspace != nil {
		hydrateWorkspace(ctx, p.workspace, ex, agentID, defaultSandboxRoot)
	}
	return ex, nil
}

// lazyExecutor is what Get() hands back. Each tool call routes through
// pool.getInner which (a) refreshes the idle timer and (b) lazily creates
// the real sandbox if this is the first call since last eviction.
type lazyExecutor struct {
	pool    *LifecyclePool
	agentID string
}

func (l *lazyExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ex, err := l.pool.getInner(ctx, l.agentID)
	if err != nil {
		return "", err
	}
	return ex.Exec(ctx, command, timeout)
}

func (l *lazyExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.agentID)
	if err != nil {
		return "", err
	}
	return ex.ReadFile(ctx, path)
}

func (l *lazyExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.agentID)
	if err != nil {
		return "", err
	}
	return ex.WriteFile(ctx, path, content)
}

func (l *lazyExecutor) ListDir(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.agentID)
	if err != nil {
		return "", err
	}
	return ex.ListDir(ctx, path)
}

// Close on a lazy proxy is a no-op — the underlying executor's lifetime is
// owned by the LifecyclePool, not by any individual caller holding a
// handle. Real teardown happens via LifecyclePool.Release / CloseAll.
func (l *lazyExecutor) Close() error { return nil }

// Ensure interfaces are satisfied.
var (
	_ Executor     = (*lazyExecutor)(nil)
	_ ExecutorPool = (*LifecyclePool)(nil)
)
