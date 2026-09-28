package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
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

	mu sync.Mutex
	// Both maps are keyed on poolKey(agentID, sessionID) so per-session
	// sandboxes are tracked independently. lastUsed drives idle eviction;
	// hydrated tracks whether we've already copied workspace.Store
	// contents into this sandbox (drops to false on eviction so the next
	// lazy-creation re-hydrates from the durable store).
	lastUsed map[string]time.Time
	hydrated map[string]bool
	// unhydrated records the scopes whose CURRENT instance was handed out with
	// an unfilled /workspace (Policy C: the store listing failed after retries).
	// The tool layer reads it through lazyExecutor.WorkspaceUnhydrated to put the
	// fact in front of the model — the whole damage of 2026-09-16 was an empty
	// workspace nobody mentioned (docs/sandbox-scope-leak.md §8–§9).
	unhydrated map[string]bool
	// rebuildAt rate-limits the Policy C repair per scope: an instance whose
	// /workspace came up empty is destroyed and rebuilt (nothing else re-runs a
	// hydrate for a live instance), but a store outage must not turn every tool
	// call into a new sandbox. A clock rather than a one-shot flag because the
	// only signal that the store came back is a successful hydrate, and we need
	// to keep asking for one — a one-shot flag would leave the scope empty until
	// idle eviction happened to recycle the instance.
	rebuildAt map[string]time.Time
	// inUse counts operations currently running against a scope. lastUsed is
	// stamped when an operation STARTS, so an exec longer than idleTTL used to
	// look idle and had its sandbox destroyed underneath it — a 20-minute build
	// against a 10-minute TTL. The sweeper skips busy scopes instead.
	inUse map[string]int
	// scopes maps the same composite key back to (agentID, sessionID) so
	// flush + release paths can talk to the right workspace scope without
	// re-parsing the key.
	scopes map[string]sandboxScope
	// storeOwned answers "does the store own the writes to this path?" (SetStoreOwnedPaths). The
	// reconcile delivers these into the sandbox but never collects them back.
	storeOwned func(string) bool
	// signals is where a delta waits when it is produced while nobody is
	// reading — the idle-eviction sync. It is a PORT, not a field: the durable
	// implementation lives outside this package, because an in-process queue
	// loses an undelivered signal the moment a pod restarts or another replica
	// adopts the scope, and that is exactly the state the fleet shares
	// (docs 09, G3; removed 2026-09-18).
	//
	// What the old queue carried is now split by what each fact actually needs:
	//   blocked / problem → re-derived by the next sync (a refusal leaves the
	//                       divergence in place, a persistent failure fails again)
	//   a sandbox rebuild → threaded through the call that discovered it
	//   moved (evict)     → this port: durable, delivered once, then deleted
	signals SignalStore
	// workspace is the optional blob store that bootstraps /workspace on
	// sandbox creation. When nil, sandboxes start empty and rely on
	// write_file tool calls (which already write through workspace.Store)
	// to produce files the agent later reads via read_file.
	workspace workspace.Store

	stopCh chan struct{}
	done   chan struct{}
	// launched records that Start has run, so a second Start does not spawn a
	// second sweeper and CloseAll knows whether there is a loop to wait for —
	// without it, CloseAll on a never-started pool blocked forever.
	launched bool

	// longOpRenew is how long an operation may run before the lease is renewed
	// again when it finishes. The lease is already renewed when an operation
	// starts (Get → reconcile), so only operations long enough to outlive the
	// TTL on their own need the trailing renew; keeping it conditional keeps the
	// common path free of an extra round trip.
	longOpRenew time.Duration

	// rebuildCooldown is the shortest interval between two Policy C rebuilds of
	// the same scope. Zero disables the rate limit (every unhydrated call
	// rebuilds) — that is what the unit tests use, and it is the honest setting
	// for an operator who would rather spend sandboxes than serve an empty
	// workspace.
	rebuildCooldown time.Duration
}

// sandboxScope is the (agentID, projectID, sessionID) tuple a sandbox
// belongs to. Stored alongside the composite map key so lifecycle code
// can call back into ExecutorPool.Get/Release with the right scope
// without re-parsing.
type sandboxScope struct {
	agentID   string
	projectID string
	sessionID string
}

// NewLifecyclePool wraps inner with idle tracking. idleTTL=0 disables
// eviction (everything stays alive); sweep=0 uses a sensible default.
func NewLifecyclePool(inner ExecutorPool, idleTTL, sweep time.Duration) *LifecyclePool {
	if sweep <= 0 {
		sweep = 30 * time.Second
	}
	return &LifecyclePool{
		inner:           inner,
		idleTTL:         idleTTL,
		sweep:           sweep,
		lastUsed:        make(map[string]time.Time),
		hydrated:        make(map[string]bool),
		unhydrated:      make(map[string]bool),
		rebuildAt:       make(map[string]time.Time),
		rebuildCooldown: defaultRebuildCooldown,
		inUse:           make(map[string]int),
		scopes:          make(map[string]sandboxScope),
		longOpRenew:     defaultLongOpRenew,
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// defaultLongOpRenew is the operation length past which the trailing lease
// renew is worth its round trip. Comfortably below the 15-minute lease TTL, so
// an operation that could outlive the TTL always renews, while the common
// sub-second tool call never pays for it.
const defaultLongOpRenew = 2 * time.Minute

// defaultRebuildCooldown is how long a scope is spared a second Policy C
// rebuild. The failure it bounds is a store listing that is down for minutes
// (§8.3 measured a ten-minute window of `context deadline exceeded`); at a
// minute, that outage costs one extra sandbox per scope per minute instead of
// one per tool call, while a store that recovers inside the minute gets its
// workspace back without waiting for the idle TTL.
const defaultRebuildCooldown = time.Minute

// LeaseRenewer is implemented by pools that hold a time-limited shared lease
// for a scope (E2B does; docker has none). The lifecycle layer calls it after
// an operation long enough to have outlived the lease, so a busy sandbox is not
// declared lapsed while its task is still running.
type LeaseRenewer interface {
	RenewLease(ctx context.Context, agentID, projectID, sessionID string) error
}

// ScopeSleeper is implemented by pools whose backend can put a scope's sandbox
// to sleep instead of destroying it. E2B pauses the instance — filesystem and
// memory, running processes included — and keeps it, unbilled and outside the
// concurrency limit, so the next caller resumes the same sandbox rather than
// paying for a new one. A pool without it (docker) is released as before.
//
// SleepScope reports paused=true when the instance is now asleep. paused=false
// with a nil error means there was nothing to sleep; an error means the attempt
// failed and the caller must decide — see evictIdle, which never destroys a
// sandbox it could not sleep.
type ScopeSleeper interface {
	SleepScope(ctx context.Context, agentID, projectID, sessionID string) (paused bool, err error)
}

// UnusableClassifier is implemented by inner pools that can tell whether an
// error indicts the instance rather than the command that ran on it. Same split
// as UnhydratedWorkspace: the provider's vocabulary lives in the adapter, and
// the policy layer only asks the question (docs/sandbox-scope-leak.md §7.4).
type UnusableClassifier interface {
	Unusable(err error) bool
}

// unusable asks the inner pool whether this failure means the sandbox is bad.
// A pool that cannot answer is treated as "no", which keeps every existing
// backend on its current behaviour.
func (p *LifecyclePool) unusable(err error) bool {
	c, ok := p.inner.(UnusableClassifier)
	return ok && c.Unusable(err)
}

// ScopeExtender is implemented by pools whose backend can push out a running
// sandbox's expiry. It exists for one hazard: an operation that starts near the
// end of the instance's life and outlives it. With autoPause on the expiry
// pauses the sandbox mid-operation and cuts our stream, so the caller sees a
// truncated response even though the process survives in the snapshot.
//
// A pool without it (docker) simply has no expiry to move.
type ScopeExtender interface {
	ExtendScope(ctx context.Context, agentID, projectID, sessionID string, d time.Duration) error
}

// extendThreshold is the operation budget past which moving the expiry is worth
// an API round trip. Shorter operations can in principle still straddle an
// expiry that is seconds away; that surfaces as a truncated stream, which the
// caller already classifies and can retry. Paying a call per tool call to
// close that last few-seconds window would cost more than it saves.
const extendThreshold = 60 * time.Second

// extendSlack is added to the operation's budget so the expiry lands after the
// operation ends rather than exactly at it.
const extendSlack = 2 * time.Minute

// beginUse marks one operation as running against sc and refreshes the idle
// clock. Paired with endUse.
func (p *LifecyclePool) beginUse(sc sandboxScope) {
	k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	p.mu.Lock()
	p.inUse[k]++
	p.lastUsed[k] = time.Now()
	p.scopes[k] = sc
	p.mu.Unlock()
}

// endUse releases the in-use marker and, when the operation ran long enough to
// have outlived the lease, renews it before anyone else can take the scope
// over mid-task.
func (p *LifecyclePool) endUse(ctx context.Context, sc sandboxScope, started time.Time) {
	k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	p.mu.Lock()
	if p.inUse[k] > 0 {
		p.inUse[k]--
	}
	if p.inUse[k] == 0 {
		delete(p.inUse, k)
	}
	p.lastUsed[k] = time.Now()
	p.mu.Unlock()

	if time.Since(started) < p.longOpRenew {
		return
	}
	renewer, ok := p.inner.(LeaseRenewer)
	if !ok {
		return
	}
	// Detached from the operation's ctx: the renew has to happen even when the
	// operation was cut short by a deadline, which is exactly when the lease is
	// most likely to be stale.
	renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := renewer.RenewLease(renewCtx, sc.agentID, sc.projectID, sc.sessionID); err != nil {
		slog.Warn("sandbox lease renew after a long operation failed",
			"agent", sc.agentID, "session", sc.sessionID, "error", err)
	}
}

// SetWorkspace installs the durable blob store used to bootstrap each
// sandbox on first tool call. Pass nil to disable hydrate (sandboxes start
// with empty /workspace).
//
// Pools that hydrate themselves at create time (E2B uses one tar+exec
// round-trip for skills + workspace; see E2BExecutorPool.Get → Hydrate)
// receive the same store via SetWorkspace on the inner pool so they
// have a chance to fold it into the bulk upload — that's much faster
// and more reliable than the per-file fallback we still keep here for
// docker.
func (p *LifecyclePool) SetWorkspace(ws workspace.Store) {
	p.workspace = ws
	if sw, ok := p.inner.(workspaceAware); ok {
		sw.SetWorkspace(ws)
	}
}

// SignalStore is where a delta waits when it is produced while nobody is
// reading — the idle-eviction sync. The pool defines the port; the runtime
// implements it over a durable scope-level store, so the sandbox package stays
// ignorant of the relational store (the same shape as workspace.Store).
//
// It replaces the in-process map removed on 2026-09-18 (docs 09, G3): a pod
// restart, or another replica adopting the lease, dropped a note only that
// process held. AppendSignal must accumulate (two evictions before the next turn
// are two facts), and TakeSignals must return and clear in one step, because the
// note is delivered exactly once.
type SignalStore interface {
	AppendSignal(ctx context.Context, agentID, projectID, sessionID, text string) error
	TakeSignals(ctx context.Context, agentID, projectID, sessionID string) (string, error)
}

// SetSignalStore wires the durable carrier. Nil is allowed: the pool then logs
// what it would have carried (see parkSignal) instead of pretending.
func (p *LifecyclePool) SetSignalStore(s SignalStore) { p.signals = s }

// SetStoreOwnedPaths names the paths whose WRITES belong to the store, not to the sandbox: they
// are delivered into the sandbox for reading, and the reconcile must never collect them back.
//
// Why this exists (2026-09-28 forensics): the identity files (SOUL/IDENTITY/MEMORY/USER/… — see
// tools.IsIdentityFile) have exactly one writer in the store, but the sandbox also holds a
// delivered copy. The reconcile compared the two, found them different as soon as the store had
// moved, and refused with `BLOCKED` — 268 times on `MEMORY.md` in a single production session.
// Refusing is right when both sides are legitimate writers; here there is only one, so the second
// expression is deleted instead of guarded.
func (p *LifecyclePool) SetStoreOwnedPaths(isStoreOwned func(string) bool) {
	p.storeOwned = isStoreOwned
}

// workspaceAware is implemented by inner pools that fold workspace
// hydration into their own create-time bulk upload (so LifecyclePool
// shouldn't double-hydrate via the per-file path).
type workspaceAware interface {
	SetWorkspace(ws workspace.Store)
}

// Start the idle sweep goroutine. Safe to call multiple times; only the
// first start actually kicks off the loop.
func (p *LifecyclePool) Start() {
	p.mu.Lock()
	first := !p.launched
	p.launched = true
	p.mu.Unlock()
	if !first {
		return
	}
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
	toEvict := make([]sandboxScope, 0)
	for k, t := range p.lastUsed {
		// An operation is running against this scope: its lastUsed was stamped
		// when that operation started, so age here means "busy", not "idle".
		if p.inUse[k] > 0 {
			continue
		}
		if t.Before(cutoff) {
			toEvict = append(toEvict, p.scopes[k])
		}
	}
	// Remove from maps under lock so a racing Get doesn't mistake an
	// evicted sandbox for a live one. A scope we are going to SLEEP keeps its
	// hydrated flag: the sandbox still holds its filesystem, so re-uploading the
	// workspace on the next use would be pure waste. A scope we are going to
	// release loses it, so the next lazy-creation re-syncs from the store.
	sleeper, canSleep := p.inner.(ScopeSleeper)
	for _, sc := range toEvict {
		k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
		delete(p.lastUsed, k)
		if !canSleep {
			delete(p.hydrated, k)
		}
		delete(p.scopes, k)
	}
	p.mu.Unlock()

	for _, sc := range toEvict {
		// Best-effort flush: if the executor implements
		// WorkspaceSnapshotter and we have a workspace store, upload
		// anything the sandbox wrote (that wasn't already written via
		// write_file) before it goes away or to sleep.
		p.flushIfSupported(sc)

		if canSleep && p.sleepOrRelease(sleeper, sc) {
			continue
		}
		if err := p.inner.Release(sc.agentID, sc.projectID, sc.sessionID); err != nil {
			slog.Warn("sandbox evict failed", "agent", sc.agentID, "session", sc.sessionID, "error", err)
			continue
		}
		slog.Info("sandbox evicted (idle)", "agent", sc.agentID, "session", sc.sessionID, "idleTTL", p.idleTTL)
	}
}

// sleepOrRelease puts an idle scope's sandbox to sleep. It reports true when the
// caller should stop — either because the instance is asleep, or because the
// attempt failed in a way that must NOT lead to destruction.
//
// The direction matters: releasing a sandbox we merely failed to pause would
// destroy a healthy instance (a provider that cannot pause, a transient API
// error). Only "there was nothing to sleep" — no cached executor, or one the
// sleeper already found gone and cleaned up — falls through to Release.
func (p *LifecyclePool) sleepOrRelease(sleeper ScopeSleeper, sc sandboxScope) bool {
	paused, err := sleeper.SleepScope(context.Background(), sc.agentID, sc.projectID, sc.sessionID)
	switch {
	case err == nil && paused:
		slog.Info("sandbox asleep (idle)",
			"agent", sc.agentID, "session", sc.sessionID, "idleTTL", p.idleTTL)
		return true
	case err == nil:
		// Nothing to sleep: fall through and release.
		return false
	default:
		// Keep the sandbox: never destroy what we could not sleep — and keep it
		// in the sweep set, or it would drop out of idle tracking entirely and
		// run (and bill) until the process exits. A sleep that fails now may work
		// at the next sweep.
		slog.Warn("sandbox sleep failed; leaving it running and retrying next sweep",
			"agent", sc.agentID, "session", sc.sessionID, "error", err)
		p.keepForNextSweep(sc)
		return true
	}
}

// keepForNextSweep restores the idle bookkeeping evictIdle removes before a
// teardown attempt, timestamped now so the next attempt happens one idleTTL from
// here rather than immediately. It is a no-op when a racing Get already
// re-registered the scope (which is the other reason an entry can reappear).
func (p *LifecyclePool) keepForNextSweep(sc sandboxScope) {
	k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, live := p.lastUsed[k]; live {
		return
	}
	p.lastUsed[k] = time.Now()
	p.scopes[k] = sc
}

// extendBudget moves the sandbox's expiry past the end of an operation whose
// budget is long enough to matter. Best-effort for the transient case: the
// operation still runs — it just runs with the extra risk of being paused
// mid-flight, and the truncation that follows is classified like any other cut
// stream. Since §7 that classification is acted on (the instance is destroyed
// and the caller is told to resend), so a failed extend is a real cost — just
// never a silent one.
//
// It returns an error for exactly ONE class: the failure indicts the INSTANCE
// (the same `Unusable` predicate the post-exec path asks, so the two paths
// cannot disagree about what "gone" means). Measured on production 2026-09-28:
// all four occurrences were `e2b extend timeout <id> HTTP 404` — the sandbox the
// scope named no longer existed, which the old text reported as "could not
// extend the sandbox timeout", a sentence about the operation that sends a
// reader looking for a capacity problem. The caller acts on the returned error
// by not starting the operation (see execOnce); a transient failure is logged
// and swallowed, because there is nothing the caller could do differently.
func (p *LifecyclePool) extendBudget(ctx context.Context, sc sandboxScope, opBudget time.Duration) error {
	if opBudget < extendThreshold {
		return nil
	}
	ext, ok := p.inner.(ScopeExtender)
	if !ok {
		return nil
	}
	extendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ext.ExtendScope(extendCtx, sc.agentID, sc.projectID, sc.sessionID, opBudget+extendSlack); err != nil {
		if p.unusable(err) {
			// The instance, not the operation: say so, and let the caller refuse to start the work.
			slog.Warn("the sandbox this scope held is gone; the long operation will not be started on it",
				"agent", sc.agentID, "session", sc.sessionID,
				"opBudget", opBudget, "error", err)
			return err
		}
		slog.Warn("could not extend the sandbox timeout before a long operation",
			"agent", sc.agentID, "session", sc.sessionID,
			"opBudget", opBudget, "error", err)
	}
	return nil
}

// flushIfSupported snapshots the sandbox workspace and uploads anything
// that isn't already in the durable store. Skips silently when the backend
// doesn't implement WorkspaceSnapshotter (docker is the only current
// implementer besides E2B) or when no workspace.Store is configured.
func (p *LifecyclePool) flushIfSupported(sc sandboxScope) {
	if p.workspace == nil {
		return
	}
	ex, err := p.inner.Get(context.Background(), sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		return
	}
	// This sync has no tool result to deliver with — it happens between turns.
	// Only the facts a later sync CANNOT re-derive are carried: paths this sync
	// just wrote into the store (the write erased the evidence of the
	// divergence). A refusal and a failed sync are not carried — the next sync
	// sees the same two copies and says the same thing again, and carrying them
	// would report the same path twice (docs 09, G3).
	p.parkSignal(sc, movedLine(p.syncSnapshot(context.Background(), sc, ex, "evict").moved))
}

// parkSignal hands a delta to the durable carrier. The pool never holds it: the
// in-process map this replaced lost the note on a pod restart or when another
// replica adopted the scope (docs 09, G3).
//
// Without a carrier the signal is logged rather than kept — a runtime that has
// nowhere durable to put it must say so, not pretend it will be delivered.
func (p *LifecyclePool) parkSignal(sc sandboxScope, signal string) {
	if signal == "" {
		return
	}
	if p.signals == nil {
		slog.Warn("sandbox sync produced a signal with no durable carrier wired; only the log carries it",
			"agent", sc.agentID, "session", sc.sessionID, "signal", signal)
		return
	}
	if err := p.signals.AppendSignal(context.Background(), sc.agentID, sc.projectID, sc.sessionID, signal); err != nil {
		slog.Warn("sandbox sync could not park its signal; the change stays in the log only",
			"agent", sc.agentID, "session", sc.sessionID, "error", err)
	}
}

// takeSignals collects whatever earlier syncs parked for this scope, deleting it
// as it is handed over: exactly one delivery per fact, across processes and
// replicas.
func (p *LifecyclePool) takeSignals(ctx context.Context, sc sandboxScope) string {
	if p.signals == nil {
		return ""
	}
	// A turn whose ctx is already over has no reader left, and the note is only consumed by a
	// SUCCESSFUL read+delete (see `sandboxSignalStore.TakeSignals`: the delete runs after the read
	// returns, and a failed delete still returns the text). So asking here buys nothing and costs
	// two things: a round trip that is already canceled, and a WARN — "could not read parked
	// signals; the agent will not be told this time" — that reads like a store fault while the
	// store is fine. Production 2026-09-28 had exactly one such line in the window
	// (`error="context canceled"`); the same shape the setup package stopped reporting for its own
	// abandoned reads (fastagent change-register row 79). The next turn's sync — the one that has a
	// reader — takes the note.
	if ctx.Err() != nil {
		return ""
	}
	out, err := p.signals.TakeSignals(ctx, sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		slog.Warn("sandbox sync could not read parked signals; the agent will not be told this time",
			"agent", sc.agentID, "session", sc.sessionID, "error", err)
		return ""
	}
	return out
}

// syncSnapshot does the actual snapshot+diff+Put work. Pulled out of
// flushIfSupported so post-exec sync (lazyExecutor.Exec) can reuse it
// without re-fetching the executor through the inner pool. `cause` is a
// log tag so we can tell evict-flushes from per-exec syncs in slog.
//
// ── Why this is a reconcile and not a copy ───────────────────────────────
//
// 2026-09-17: deliverables were silently reverted in production. The old rule
// was "store and snapshot sizes differ ⇒ take the snapshot", which is only
// sound when the store can contain nothing the sandbox did not write. Host
// tools (write_file / edit_file / apply_patch) break that: they write the
// store without touching the sandbox, so the sandbox's stale copy looked
// "newer" and overwrote the good one — once per exec, on every scope whose
// sandbox had outlived a host edit. Full analysis:
// docs/文件系统形式化证明/07-formal-rootcause-and-fix.md §2.
//
// The repair is a precondition, not a smarter heuristic:
//
//   - A path the store does NOT have is a sandbox artefact → push (unchanged).
//   - A path the store HAS is pushed while this sandbox still holds the copy it
//     was handed — the read that decides that is the cheap one: the store
//     object's size+mtime against the sandbox file's own (statsFor). Then the
//     sandbox's difference is the sandbox's own work.
//   - Otherwise the two copies disagree, and the precondition is false: migrate
//     NOTHING and say so. Overwriting here is what destroyed the deliverables;
//     skipping keeps the store intact and merely leaves the sandbox stale
//     (which the next hydrate/reconcile repulls).
//
// Two mechanisms keep that decision from being a guess:
//
//   - WriteThrough mirrors every host write into the live sandbox, so the two
//     copies of a path are equal at the moment of the write — same bytes AND,
//     because the mirror stamps the sandbox file with the store object's mtime,
//     the same version marker. A sandbox edit that follows is therefore
//     unambiguously the sandbox's, and the cheap check above passes.
//   - Nothing is remembered between calls. A mirror that failed leaves the two
//     copies different, which is exactly the state the check sees on its own; a
//     path whose store copy cannot be read back for a byte comparison is refused
//     rather than overwritten. No per-pod table means the same sandbox is judged
//     the same way by whichever replica handles the turn (07 §3.3).
//
// 01 §3.1 explains why the docker backend needs neither: there is one copy.
// ReplacedWorkspace is the read side of a rebuild: an executor whose /workspace
// was re-created from the store after its previous instance died states it
// once. The lifecycle pool turns that into a signal on the next tool result —
// without it, a rebuild is invisible (the next exec succeeds normally) while
// anything that existed only inside the old sandbox is gone (docs 09, G1/G2).
type ReplacedWorkspace interface {
	TakeWorkspaceReplaced() bool
}

// statsFor reads the sandbox's own size/mtime for every path in the snapshot.
// This is the memory-free substitute for a baseline table: the sandbox
// filesystem already records when each file last changed, and that record
// travels WITH the sandbox across replicas and restarts — which a pod-local map
// cannot. The agent can see the same numbers with `ls -l`.
//
// Best-effort: on failure the map is empty, and sameVersion falls through to
// comparing the bytes — slower, never wrong.
func statsFor(ctx context.Context, ex Executor, files map[string][]byte) map[string]fileStat {
	out := make(map[string]fileStat, len(files))
	res, err := ex.Exec(ctx, `find /workspace -type f -printf '%P\t%s\t%T@\n' 2>/dev/null`, 30*time.Second)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(res, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		size, sizeErr := strconv.ParseInt(parts[1], 10, 64)
		secs, modErr := strconv.ParseFloat(parts[2], 64)
		if sizeErr != nil || modErr != nil {
			continue
		}
		out[parts[0]] = fileStat{size: size, modUnix: int64(secs)}
	}
	return out
}

// fileStat is one sandbox file's own metadata.
type fileStat struct {
	size    int64
	modUnix int64
}

// sameVersion reports whether the store object and the sandbox copy are the SAME
// version without reading either: same byte count, same write time. The mirror
// stamps the sandbox file with the store's write time, so equality is exact for
// mirrored content and merely uncertain for anything else — and uncertain falls
// through to comparing the bytes.
//
// A stat we could not read never claims equality.
func sameVersion(info *workspace.ObjectInfo, st fileStat, data []byte) bool {
	if st.modUnix == 0 {
		return false
	}
	if info.Size != int64(len(data)) || st.size != int64(len(data)) {
		return false
	}
	diff := info.ModTime.Unix() - st.modUnix
	if diff < 0 {
		diff = -diff
	}
	return diff <= 1
}

// equalToStore compares the store's bytes with the sandbox's. Called only when
// the metadata check could not settle it, so the read is paid once per
// genuinely-ambiguous path, not per sync.
func equalToStore(ctx context.Context, ws workspace.Store, sc sandboxScope, path string, data []byte) bool {
	rc, err := ws.Get(ctx, sc.agentID, sc.projectID, sc.sessionID, path)
	if err != nil {
		return false
	}
	defer rc.Close()
	stored, err := io.ReadAll(rc)
	if err != nil {
		return false
	}
	return bytes.Equal(stored, data)
}

// delta is what one reconcile observed, in the three shapes that matter to the
// agent: paths it moved into the store, paths it refused, and paths the store
// has that the sandbox does not. It is the δ of docs 08 §2 — a fact about how
// the world changed; signalsFor turns it into the σ the agent reads.
//
// storeOnly is that third shape, and it closes the walk's blind spot without any
// memory: the walk's domain is the sandbox snapshot, so "the store has a path the
// sandbox does not" was never examined. Listing the store once per sync gives it,
// and it is the trace a deletion inside the sandbox leaves — but the SAME trace
// as an upload that arrived after the sandbox started, so the signal states the
// fact and says plainly that this runtime cannot attribute it (docs 10 §4, G4;
// saying more would be the class of error G16 was).
type delta struct {
	moved     []string
	blocked   []string
	storeOnly []string
	// problem is set when the reconcile itself could not run — the snapshot
	// failed (workspace over the cap) or the store listing did. The agent has to
	// know: the sandbox's changes are NOT in the store, and it will otherwise
	// read the store's older copies and conclude its work vanished (production
	// hit this 64 times in one session; the operator log said so, the agent's
	// context did not).
	problem string
}

// changed reports whether the delta has anything to say.
func (r delta) changed() bool {
	return len(r.moved) > 0 || len(r.blocked) > 0 || len(r.storeOnly) > 0 || r.problem != ""
}

// snapshotFailureProblem is the sentence the agent gets when the reconcile could not RUN. It states
// what is known — the sandbox still holds the changes, the store may be older — and then names the
// one cause it can actually identify, instead of asserting a cause for every failure.
//
// Why it is a function (row 84 of docs/fs-formal-proof/11-change-register.md): the sentence used to be
// a constant — "Typical cause: /workspace grew past the snapshot cap (32 MiB) … Move those to /tmp" —
// written 2026-09-18 out of that week's incident (the 09-14 over-cap OOM, `5f38348`). This branch
// catches EVERY failure of `SnapshotWorkspace`, whose ctx is the tool call's own (`527b8fb`), so a cut
// turn or a caller that left lands here too — and in the production window measured 2026-09-28 those
// were 2 of 2 failures while the cap was 0 of 2. Naming the wrong cause is worse than naming none:
// `/tmp` is not mirrored, so a model that follows "move those to /tmp" with a deliverable loses it.
//
// Ordering, and why: an over-cap refusal is a fact about the SANDBOX and is actionable (the executor's
// own text carries the largest paths), so it wins when both are true; next is "this call's ctx ended",
// which is a fact about this call and is stated without guessing which of the three ways it ended
// (the same rule as rows 79 and 81); anything else is reported as itself. Nothing here invents a cause,
// and nothing here tells the model how to tidy /workspace — that belongs to whatever wrote the file.
func snapshotFailureProblem(ctx context.Context, err error) string {
	const consequence = "The sandbox still holds them; the store — and anything you read with read_file — may be older."
	switch {
	case errors.Is(err, errSnapshotOverCap):
		return fmt.Sprintf("the sandbox's changes could NOT be synced to the workspace store: /workspace is over the snapshot cap (%v). %s",
			err, consequence)
	case ctx.Err() != nil:
		return fmt.Sprintf("the sandbox's changes could NOT be synced to the workspace store: this turn's context ended before the sync could run. %s", consequence)
	default:
		return fmt.Sprintf("the sandbox's changes could NOT be synced to the workspace store (%v). %s", err, consequence)
	}
}

// syncStoreScope is the store scope a sync's write-backs belong to.
//
// A project is one tree: its tools write the project root and hydrate fills the
// sandbox from it, so the sync has to write back there too — otherwise every
// project file the sandbox holds gets copied into the chat's own subdir (docs
// 10 §4 G17 residual A). A loose chat keeps its session segment.
//
// The rule itself is workspace.WriteScope's — the same call the file tools make
// (docs 10 §4 G23); this function only re-attaches the agent.
func syncStoreScope(sc sandboxScope) sandboxScope {
	ws := workspace.WriteScope(sc.projectID, sc.sessionID)
	return sandboxScope{agentID: sc.agentID, projectID: ws.ProjectID, sessionID: ws.SessionID}
}

// syncSnapshot returns what it moved, refused and lost, so the caller can tell
// the agent what the sandbox did (zero value when there is nothing to observe
// and nothing to sync).
func (p *LifecyclePool) syncSnapshot(ctx context.Context, sc sandboxScope, ex Executor, cause string) delta {
	var d delta
	if p.workspace == nil {
		return d
	}
	snapper, ok := ex.(WorkspaceSnapshotter)
	if !ok {
		return d
	}
	files, err := snapper.SnapshotWorkspace(ctx)
	if err != nil {
		slog.Warn("sandbox sync: snapshot failed", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "error", err)
		d.problem = snapshotFailureProblem(ctx, err)
		return d
	}
	// Memory-free decision procedure.
	//
	// There is deliberately no per-pod state here (no baseline table, no digest
	// index, no stale set). A sandbox lease is adopted ACROSS REPLICAS, so a
	// judgement that depends on "what this process saw earlier" is not a
	// judgement: the same sandbox would be treated differently by whichever pod
	// handled the turn, and a pod restart would silently change the rules. What
	// the two copies carry themselves is enough —
	//
	//   the store object's size + write time   (from Stat; one round trip each)
	//   the sandbox file's size + mtime        (from one stat pass, statsFor)
	//
	// — plus the invariant the write-through maintains: after a host write both
	// copies hold the same bytes AND the mirror stamps the sandbox file with the
	// store's write time. So "same size and same mtime" means "same version",
	// and the cheap skip needs no memory.
	//
	// When they look different, comparing the bytes decides: equal ⇒ skip;
	// different ⇒ refuse to choose, and record the δ. A host write whose mirror
	// failed lands here with the sandbox holding older bytes and is refused
	// rather than overwritten (the incident's shape); a sandbox edit lands here
	// too and is signalled as "not synced" for the agent to settle.
	stats := statsFor(ctx, ex, files)

	// A (docs 10 §4 G17): a project is ONE file tree, so a sandbox-born file
	// belongs to the PROJECT ROOT — the key the file tools read, the key hydrate
	// fills from, and the key the preview's dev server serves. Writing it to the
	// chat's own subdir instead (which is what this loop used to do, because it
	// took the scope from the container) duplicated every project file once per
	// chat and put the agent's script output where no tool would look for it.
	//
	// Loose chats have no project to share, so their session segment stays.
	ws := syncStoreScope(sc)

	written := 0
	for path, data := range files {
		// A store-owned path is delivered INTO the sandbox and never collected back: the tools
		// write it through the store, so a copy that differs here is not a conflict to referee —
		// it is the second expression of one fact, and the store's copy is the one that stands.
		if p.storeOwned != nil && p.storeOwned(path) {
			continue
		}
		info, statErr := p.workspace.Stat(ctx, ws.agentID, ws.projectID, ws.sessionID, path)
		switch {
		case errors.Is(statErr, workspace.ErrNotFound):
			slog.Info("sandbox sync: new path from sandbox",
				"agent", sc.agentID, "session", sc.sessionID, "cause", cause,
				"path", path, "snapshotBytes", len(data))
		case statErr != nil:
			slog.Warn("sandbox sync: store stat failed, leaving object untouched",
				"agent", sc.agentID, "session", sc.sessionID, "cause", cause,
				"path", path, "error", statErr)
			continue
		case sameVersion(info, stats[path], data):
			continue
		default:
			if equalToStore(ctx, p.workspace, ws, path, data) {
				continue
			}
			slog.Warn("sandbox sync: BLOCKED — the two copies of this path hold different bytes",
				"agent", sc.agentID, "session", sc.sessionID, "cause", cause,
				"path", path, "storeBytes", info.Size, "snapshotBytes", len(data))
			d.blocked = append(d.blocked, path)
			continue
		}
		if err := p.workspace.Put(ctx, ws.agentID, ws.projectID, ws.sessionID, path, bytesReader(data), int64(len(data)), ""); err != nil {
			slog.Warn("sandbox sync: put failed", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "path", path, "error", err)
			continue
		}
		d.moved = append(d.moved, path)
		written++
	}
	if written > 0 {
		slog.Info("sandbox synced to workspace store", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "files", written)
	}
	// The loop above walks the SANDBOX SNAPSHOT, so the mirror image — a path the
	// STORE has and the sandbox does not — is outside its domain and used to go
	// unexamined. One List closes that: it is the trace a deletion inside the
	// sandbox leaves, and also the one an upload leaves, so the signal states the
	// fact and refuses to attribute it (docs 10 §4, G4).
	//
	// Best-effort: a listing failure reports nothing rather than guessing, and the
	// skills namespace is skipped because those objects live in the read-only
	// /skills mount, not in /workspace (they would otherwise look "missing" from
	// every agent-scope sandbox).
	if objs, listErr := p.workspace.List(ctx, ws.agentID, ws.projectID, ws.sessionID); listErr != nil {
		slog.Warn("sandbox sync: store listing failed; paths the sandbox lacks are not reported",
			"agent", sc.agentID, "session", sc.sessionID, "cause", cause, "error", listErr)
	} else {
		for _, o := range objs {
			if strings.HasPrefix(o.Path, "skills/") {
				continue
			}
			if _, inSandbox := files[o.Path]; !inSandbox {
				d.storeOnly = append(d.storeOnly, o.Path)
			}
		}
		sort.Strings(d.storeOnly)
	}
	return d
}

// Get returns a lazy proxy: tool calls on it will fetch the underlying
// executor from the inner pool on demand (creating a new sandbox if
// needed) and tick the last-used timestamp.
//
// Contract matches ExecutorPool.Get so LifecyclePool is a drop-in wrapper.
func (p *LifecyclePool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	return &lazyExecutor{pool: p, scope: sandboxScope{agentID: agentID, projectID: projectID, sessionID: sessionID}}, nil
}

// Release forwards to the inner pool and drops the lastUsed entry. Useful
// for explicit teardown (agent deletion) — normal flow relies on idle
// eviction.
func (p *LifecyclePool) Release(agentID, projectID, sessionID string) error {
	k := poolKey(agentID, projectID, sessionID)
	p.mu.Lock()
	delete(p.lastUsed, k)
	delete(p.hydrated, k)
	delete(p.unhydrated, k)
	delete(p.rebuildAt, k)
	delete(p.scopes, k)
	p.mu.Unlock()
	return p.inner.Release(agentID, projectID, sessionID)
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
	p.mu.Lock()
	launched := p.launched
	p.mu.Unlock()
	if launched {
		<-p.done
	}
	p.inner.CloseAll()
	p.mu.Lock()
	p.lastUsed = make(map[string]time.Time)
	p.hydrated = make(map[string]bool)
	p.unhydrated = make(map[string]bool)
	p.rebuildAt = make(map[string]time.Time)
	p.inUse = make(map[string]int)
	p.scopes = make(map[string]sandboxScope)
	p.mu.Unlock()
}

// inner fetches the underlying Executor, creating on first call. Separate
// from Get() so lazyExecutor can update lastUsed each time. On first
// creation (either fresh or post-eviction) it hydrates /workspace from the
// configured workspace.Store so exec'd commands see the files that
// write_file has produced in previous sessions.
func (p *LifecyclePool) getInner(ctx context.Context, sc sandboxScope) (Executor, error) {
	k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	p.mu.Lock()
	needsHydrate := !p.hydrated[k]
	p.lastUsed[k] = time.Now()
	p.scopes[k] = sc
	if needsHydrate {
		p.hydrated[k] = true // set eagerly so a concurrent second call doesn't double-hydrate
	}
	p.mu.Unlock()

	ex, err := p.inner.Get(ctx, sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		// Roll back the hydrated flag so a retry will try again.
		p.mu.Lock()
		p.hydrated[k] = false
		p.mu.Unlock()
		return nil, err
	}

	// A rebuild replaces the whole sandbox: /workspace is re-created from the
	// store, and whatever lived only inside the old instance (a script's changes
	// that never got synced) is gone. The rebuild is otherwise silent — the next
	// exec succeeds — so the fact rides the call that discovered it
	// (takeReplacedNote), as a return value on the call stack rather than in a
	// map that a restart would empty (docs 09, G1/G2/G3).
	// Policy C (docs/sandbox-scope-leak.md §9): a store listing that failed
	// leaves a usable sandbox with an EMPTY /workspace. The original shape of
	// this fix only rolled hydrated[k] back, on the theory that the next Get
	// would retry the hydrate. On E2B it does not: a live cached executor is
	// returned without re-hydrating (E2BExecutorPool.Get's cachedExecutor
	// branch), so an empty workspace would have been served until the instance
	// was evicted for idleness. The only thing that re-runs a hydrate is a
	// recreated instance, so that is what an unhydrated one gets — rate-limited
	// by rebuildCooldown, never while the scope is in use.
	if ws, ok := ex.(UnhydratedWorkspace); ok {
		if ws.WorkspaceUnhydrated() {
			recovered, rebuildErr := p.recoverUnhydrated(ctx, sc, k, ex)
			if rebuildErr != nil {
				return nil, rebuildErr
			}
			ex = recovered
			ws, _ = ex.(UnhydratedWorkspace)
		}
		p.mu.Lock()
		if ws != nil && ws.WorkspaceUnhydrated() {
			p.unhydrated[k] = true
			p.hydrated[k] = false
		} else {
			delete(p.unhydrated, k)
		}
		p.mu.Unlock()
	}
	// Skip the per-file fallback when the inner pool already pushed
	// /workspace as part of its own bulk hydrate (E2B does this — one
	// tar.gz over exec covers /skills and /workspace in one shot).
	// Otherwise (docker), copy each object via ex.WriteFile.
	if needsHydrate && p.workspace != nil {
		if _, selfHydrates := p.inner.(workspaceAware); !selfHydrates {
			hydrateWorkspace(ctx, p.workspace, ex, sc.agentID, sc.projectID, sc.sessionID, defaultSandboxRoot)
		}
		// The instance's /workspace now holds the store's objects as of this
		// moment. That is what syncSnapshot compares against, and it needs no
		// record of its own: the hydrate stamps each file with the store
		// object's mtime, so the comparison reads the marker off the two copies
		// themselves (07 §3.11.3).
	}
	return ex, nil
}

// takeReplacedNote reads and clears the one-shot "your sandbox was replaced"
// flag from an executor, and renders it. It is consumed by the operation that
// found it, so the note is delivered in that operation's own result — no queue,
// no process memory, and no risk of being delivered to whichever call happens
// next in some other replica (docs 09, G1/G2/G3).
func (p *LifecyclePool) takeReplacedNote(ex Executor) string {
	rw, ok := ex.(ReplacedWorkspace)
	if !ok || !rw.TakeWorkspaceReplaced() {
		return ""
	}
	return signalsFor(delta{problem: "the sandbox was REPLACED — the previous instance had expired or died, " +
		"so /workspace was re-created from the workspace store. Anything that existed only inside the old sandbox " +
		"(changes a script made that were never synced) is gone; the store's copies are intact. " +
		"Keep long-running output in /tmp so a replacement cannot strand it."})
}

// recoverUnhydrated is Policy C's repair half (§9.2 step 5, strengthened): the
// scope's instance is alive but its /workspace was never filled, and no live
// instance can be re-hydrated in place — so it is destroyed and rebuilt, and
// the hydrate runs again on the fresh one.
//
// Two refusals, both deliberate:
//
//   - an operation is already in flight on this scope (inUse > 0). Releasing
//     the instance underneath a running command truncates that command's
//     stream, which is the second failure mode in §4 (M2) of the incident doc.
//     The unhydrated instance is handed out for now; the next getInner after
//     the in-flight operation finishes does the rebuild.
//   - a rebuild this scope already had inside rebuildCooldown. A store that is
//     down for minutes would otherwise mint a sandbox per tool call — twenty
//     calls in a turn, twenty instances, all empty anyway.
//
// When the replacement comes up empty too (the store is still failing), it is
// handed out regardless: refusing here would turn a read-only store outage into
// an outage of the whole agent. The caller marks the scope unhydrated and the
// tools declare it into the turn.
//
// The one path that returns an error is "the instance is gone and its
// replacement could not be built" — there is nothing left to hand out, and the
// caller has to fail the call rather than run a command against a destroyed
// sandbox.
func (p *LifecyclePool) recoverUnhydrated(ctx context.Context, sc sandboxScope, k string, ex Executor) (Executor, error) {
	p.mu.Lock()
	last, rebuilt := p.rebuildAt[k]
	if p.inUse[k] > 0 || (rebuilt && time.Since(last) < p.rebuildCooldown) {
		p.mu.Unlock()
		return ex, nil
	}
	p.rebuildAt[k] = time.Now()
	p.mu.Unlock()

	slog.Warn("workspace not hydrated: rebuilding the sandbox to re-run the hydrate",
		"agent", sc.agentID, "project", sc.projectID, "session", sc.sessionID)
	// Release is the DESTROY path. The pause path is evictIdle → SleepScope,
	// and pausing here would just park the empty instance for the next wake-up
	// to hand out again.
	if err := p.inner.Release(sc.agentID, sc.projectID, sc.sessionID); err != nil {
		slog.Warn("workspace rebuild: release failed, keeping the unhydrated instance",
			"agent", sc.agentID, "project", sc.projectID, "session", sc.sessionID, "error", err)
		return ex, nil
	}

	p.mu.Lock()
	// inner.Release does not touch the lifecycle maps, so drop the dead
	// instance's entries here and re-arm the hydrate for the replacement.
	delete(p.lastUsed, k)
	delete(p.scopes, k)
	p.hydrated[k] = true
	p.lastUsed[k] = time.Now()
	p.scopes[k] = sc
	p.mu.Unlock()

	replacement, err := p.inner.Get(ctx, sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		slog.Error("workspace rebuild: recreate failed after the empty instance was destroyed",
			"agent", sc.agentID, "project", sc.projectID, "session", sc.sessionID, "error", err)
		p.mu.Lock()
		p.hydrated[k] = false
		p.mu.Unlock()
		return nil, fmt.Errorf("workspace rebuild: %w", err)
	}
	return replacement, nil
}

// lazyExecutor is what Get() hands back. Each tool call routes through
// pool.getInner which (a) refreshes the idle timer and (b) lazily creates
// the real sandbox if this is the first call since last eviction.
type lazyExecutor struct {
	pool  *LifecyclePool
	scope sandboxScope
}

// WorkspaceUnhydrated reports whether the scope's current instance was handed
// out with an unfilled /workspace — the state the tool layer turns into a
// declaration on workspace-touching results (docs/sandbox-scope-leak.md §9.5).
//
// Answered from the pool's bookkeeping rather than by asking the inner
// executor: the inner executor is created lazily, and a question about the
// workspace must never be the thing that spins a sandbox up (or, worse, blocks
// behind one being created).
func (l *lazyExecutor) WorkspaceUnhydrated() bool {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	return l.pool.unhydrated[poolKey(l.scope.agentID, l.scope.projectID, l.scope.sessionID)]
}

// Exec runs one command, and when the failure indicts the INSTANCE rather than
// the command, replaces the instance instead of handing a broken sandbox the
// model will keep tripping over (docs/sandbox-scope-leak.md §7.4).
//
// Deliberately NOT a re-run. A cut stream may have left the command's side
// effects half-applied, and the tool layer's own stance is to hint rather than
// fall back automatically (internal/agent/tools/exec.go). So the command's
// verdict — including a non-zero exit — is returned untouched, and a replaced
// instance says so in the error text so the model can decide to re-send.
//
// The replacement happens outside the in-use window on purpose: execOnce
// brackets beginUse/endUse, and both the idle sweep and the unhydrated-rebuild
// path deliberately skip scopes with an operation in flight.
func (l *lazyExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	out, err := l.execOnce(ctx, command, timeout)
	if err == nil || !l.pool.unusable(err) {
		return out, err
	}
	// Release is the DESTROY path; the pause path is evictIdle → SleepScope, and
	// pausing here would just park a broken instance for the next wake-up.
	if relErr := l.pool.Release(l.scope.agentID, l.scope.projectID, l.scope.sessionID); relErr != nil {
		slog.Warn("sandbox could not be replaced after an unusable instance; leaving it in place",
			"agent", l.scope.agentID, "session", l.scope.sessionID, "error", relErr)
		return out, err
	}
	return out, fmt.Errorf("%w\n[sandbox replaced: the previous instance could not serve this command "+
		"(its exec stream was cut, or it was gone), so it was discarded and the next call gets a fresh one. "+
		"The command did not run to completion — re-run it if it is safe to repeat.]", err)
}

// execOnce is the operation itself, its in-use marker and its post-exec sync.
func (l *lazyExecutor) execOnce(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	// Hold the scope's in-use marker for the whole operation: an exec may run
	// longer than idleTTL (600s budgets against a 10-minute TTL), and without
	// this the sweeper would destroy the sandbox underneath it.
	started := time.Now()
	l.pool.beginUse(l.scope)
	if goneErr := l.pool.extendBudget(ctx, l.scope, timeout); goneErr != nil {
		// ③′ — do not start a long operation on an instance the scope has already lost. Returning
		// here hands the verdict to `Exec`, which owns the replacement: it destroys the instance and
		// appends the "sandbox replaced … re-run it if it is safe to repeat" note, so the model's
		// retry lands on a fresh sandbox instead of on the corpse.
		//
		// Two things make this the SAFE half of a replacement, where the post-exec path is the
		// dangerous one: nothing has run yet (no side effect to replay, so "re-run" cannot double
		// anything), and the deferred endUse above fires before Exec's Release, so the swap still
		// happens outside the in-use window the idle sweep and the rebuild path respect.
		//
		// The narrowness matters as much as the action: a transient extend failure returns nil here
		// and the operation proceeds on the healthy instance it already has.
		return "", fmt.Errorf("the sandbox this scope held is gone, so this command was not started: %w", goneErr)
	}
	// Deferred rather than called inline: the post-exec sync below reads the
	// sandbox, and a scope that is no longer marked in use could be swept
	// (paused) in the middle of it.
	defer l.pool.endUse(ctx, l.scope, started)
	// The call that discovers a rebuild carries the note about it (docs 09, G3).
	replaced := l.pool.takeReplacedNote(ex)
	out, execErr := ex.Exec(ctx, command, timeout)
	// Post-exec sync only for cloud sandboxes (RemoteWorkspace marker).
	// Docker's /workspace is bind-mounted to host so files appear
	// instantly with no sync needed; rerunning the snapshot+Put cycle
	// after every exec would just churn the workspace.Store
	// (especially expensive when it's S3-backed). E2B's /workspace
	// lives inside the cloud sandbox; without this pull, files the
	// skill writes (image-tool's /workspace/gen_xxx.webp) never
	// reach the host and the UI shows broken images.
	// Best-effort — never overrides the exec result.
	if _, remote := ex.(RemoteWorkspace); remote {
		// This signal is the agent's only channel for SANDBOX-side change: its
		// own tool calls tell it what the store got, but a script that edits a
		// file inside /workspace is invisible to it (07 §3.11.1). Attach what
		// moved, and what could not be moved, to the exec result — the shortest
		// path from "the sandbox did something" to "the agent knows".
		d := l.pool.syncSnapshot(ctx, l.scope, ex, "post-exec")
		// Whatever an earlier (evict-time) sync parked comes out here, with this
		// call's own delta after it: one delivery point, delivered once.
		out += l.pool.takeSignals(ctx, l.scope) + signalsFor(d)
	}
	return replaced + out, execErr
}

// signalsFor renders one delta as the σ the agent reads (docs 08 §2).
// Empty when there is nothing to say: this is an exception channel, and a line
// on every exec would train the model to skim past it.
//
// Paths are the sandbox's own (relative to /workspace), which is what the agent
// sees when it lists the sandbox — and the same spelling read_file accepts.
func signalsFor(d delta) string {
	if !d.changed() {
		return ""
	}
	var sb strings.Builder
	if d.problem != "" {
		sb.WriteString("\n[workspace] ")
		sb.WriteString(d.problem)
	}
	if len(d.moved) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(movedLine(d.moved))
	}
	if len(d.blocked) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[workspace] NOT synced (the store's copy differs and neither side may be chosen automatically): ")
		sb.WriteString(strings.Join(d.blocked, ", "))
		sb.WriteString(" — both versions are intact.")
	}
	if len(d.storeOnly) > 0 {
		sb.WriteString(StoreOnlyLine(d.storeOnly))
	}
	return sb.String()
}

// StoreOnlyLine states the divergence one walk cannot see by construction: paths
// the store has and the sandbox does not.
//
// It is deliberately one shared sentence, because two producers state this same
// fact at two different moments — the reconcile (whose walk domain is the
// sandbox snapshot) and list_dir (which lists the store and asks the sandbox what
// it holds). Two wordings for one fact is how a rule drifts.
//
// The origin is NOT claimed: "deleted inside the sandbox" and "uploaded to the
// store after this sandbox started" leave the identical trace, and only a durable
// store-side manifest could tell them apart (docs 10 §4, G4). What matters to the
// agent is the consequence, which is stated exactly: exec cannot see them.
func StoreOnlyLine(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	const shownMax = 5
	shown := paths
	more := ""
	if len(shown) > shownMax {
		shown = shown[:shownMax]
		more = fmt.Sprintf(" (+%d more)", len(paths)-shownMax)
	}
	return fmt.Sprintf("\n[workspace] %d path(s) are in the workspace store but NOT in this sandbox, "+
		"so anything run with exec will not find them: %s%s — either they were added to the store after this "+
		"sandbox started (an upload), or they were deleted inside it; this runtime cannot tell which. "+
		"read_file still sees them: if a script needs one, read it and write it again (read_file + write_file copies it in).",
		len(paths), strings.Join(shown, ", "), more)
}

// movedLine renders the one delta shape that has to survive the moment it is
// produced: paths a sync wrote into the store. A refusal leaves the divergence
// in place and a failed sync fails again, so the next sync re-derives both — but
// a write erases its own evidence, and it happens during idle eviction, when
// nobody is reading (docs 09, G3).
func movedLine(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return "\n[workspace] the sandbox changed " + strings.Join(paths, ", ") + " — synced to the workspace store."
}

func (l *lazyExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	started := time.Now()
	l.pool.beginUse(l.scope)
	defer l.pool.endUse(ctx, l.scope, started)
	out, readErr := ex.ReadFile(ctx, path)
	if readErr != nil {
		// Leave the replacement note where it is: this call cannot carry it, and
		// swallowing it here would lose the fact (docs 09, G3).
		return out, readErr
	}
	return l.pool.takeReplacedNote(ex) + out, nil
}

func (l *lazyExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	started := time.Now()
	l.pool.beginUse(l.scope)
	defer l.pool.endUse(ctx, l.scope, started)
	out, writeErr := ex.WriteFile(ctx, path, content)
	// Mirror writes to the durable store on cloud sandboxes — same
	// reasoning as the post-exec sync above. Without this, write_file
	// (and apply_patch) calls that fall through to ex.WriteFile (any
	// absolute /workspace path — see file.go's isWorkspacePath, which
	// rejects abs paths) only land in the E2B sandbox and disappear on
	// idle eviction, never reaching the host workspace.Store the UI and
	// signed URLs read from. Targeted single-file Put rather than
	// syncSnapshot: we already have the bytes in memory, no need for a
	// full tar round-trip per write. Best-effort — never overrides the
	// write result.
	if writeErr == nil {
		if _, remote := ex.(RemoteWorkspace); remote {
			l.pool.mirrorSandboxWrite(ctx, l.scope, path, content)
		}
		return l.pool.takeReplacedNote(ex) + out, nil
	}
	return out, writeErr
}

// mirrorSandboxWrite copies a single sandbox-side write into the durable
// workspace.Store. Skips paths outside /workspace (e.g. /tmp/, /home/user/)
// since those have no store mapping. Mirrors the sessionID-scoped Put that
// syncSnapshot uses, so write_file and exec-generated files land in the
// same store location.
func (p *LifecyclePool) mirrorSandboxWrite(ctx context.Context, sc sandboxScope, sandboxPath, content string) {
	if p.workspace == nil {
		return
	}
	const prefix = "/workspace/"
	if !strings.HasPrefix(sandboxPath, prefix) {
		return
	}
	key := strings.TrimPrefix(sandboxPath, prefix)
	if key == "" {
		return
	}
	if err := p.workspace.Put(ctx, sc.agentID, sc.projectID, sc.sessionID, key,
		bytesReader([]byte(content)), int64(len(content)), ""); err != nil {
		slog.Warn("sandbox sync: write_file mirror failed",
			"agent", sc.agentID, "project", sc.projectID, "session", sc.sessionID,
			"path", key, "error", err)
		return
	}
	slog.Debug("sandbox synced to workspace store", "agent", sc.agentID, "session", sc.sessionID, "cause", "post-write", "path", key)
}

func (l *lazyExecutor) ListDir(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	started := time.Now()
	l.pool.beginUse(l.scope)
	defer l.pool.endUse(ctx, l.scope, started)
	out, listErr := ex.ListDir(ctx, path)
	if listErr != nil {
		return out, listErr
	}
	return l.pool.takeReplacedNote(ex) + out, nil
}

// Close on a lazy proxy is a no-op — the underlying executor's lifetime is
// owned by the LifecyclePool, not by any individual caller holding a
// handle. Real teardown happens via LifecyclePool.Release / CloseAll.
func (l *lazyExecutor) Close() error { return nil }

// Backend reports the provider name by delegating to the inner pool, which
// knows it as a type-level constant. No need to materialize a real
// executor — the answer is static for the pool's lifetime.
func (l *lazyExecutor) Backend() string { return l.pool.inner.Backend() }

// Backend on LifecyclePool delegates to the wrapped inner pool. Mirrors
// the per-executor Backend so callers can ask "which provider is this?"
// at any layer of the wrapper stack.
func (p *LifecyclePool) Backend() string { return p.inner.Backend() }

// Ensure interfaces are satisfied.
var (
	_ Executor     = (*lazyExecutor)(nil)
	_ ExecutorPool = (*LifecyclePool)(nil)
)

// WriteThrough mirrors one host-tool write into the live sandbox, so the two
// copies of a path move together instead of diverging until the next reconcile.
//
// Why this exists (docs/文件系统形式化证明/07 §2): the incident needed four
// conditions, and the third — "the host wrote a path the sandbox also has, and
// the two now differ" — cannot happen on docker, whose /workspace IS the host
// directory. Remote backends have two copies, so the divergence window is
// structural. Mirroring at the moment of the write closes it at the source
// instead of arbitrating it later: after a successful mirror the sandbox copy
// holds the host's bytes AND the store object's mtime, so the reconcile's cheap
// "same version" check is exact and only sandbox-side edits can differ.
//
// It does NOT prevent a divergence, and it never refuses to write. An earlier
// version compared the sandbox's copy against a digest it was handed and blocked
// the mirror on a mismatch (a compare-and-set). That is the wrong shape here:
// refusing created a state with no way out (a rewrite was refused again by the
// same check, and a write inside the sandbox was refused by the reconcile), and
// the choice it avoided making is not the code's to make anyway.
//
// What it does instead:
//
//  1. OBSERVE — compare the sandbox's copy against what the CALLER says it was
//     (write_file passes "", edit_file and apply_patch pass the bytes they
//     read). No stored table: the expectation comes from the call, which is
//     what keeps the judgement valid across replicas and pod restarts.
//  2. WRITE — apply the host's content, as asked.
//  3. STAMP — set the sandbox file's mtime to the store object's, so the two
//     copies are recognisable as one version without any memory.
//  4. SIGNAL — return the delta (WriteThroughOutcome). The tool renders it into
//     a σ and hands it to the package's single signal exit; the mirror itself
//     never keeps a copy of what it replaced, because the agent decides what to
//     do with the fact (07 §3.11.1).
//
// Granularity is one path per call, deliberately: the caller knows exactly which
// file it changed, so a mirror failure stays per-path instead of stranding
// unrelated exec artefacts.
//
// sandboxPath is the ABSOLUTE path inside the sandbox; callers pass the same
// mapping the hydrate uses, so the mirrored file is the one the sandbox's
// tooling already reads.
func (p *LifecyclePool) WriteThrough(ctx context.Context, sc sandboxScope, store StoreScope, storeKey, sandboxPath, content, previous string) (WriteThroughOutcome, error) {
	var outcome WriteThroughOutcome
	if p.workspace == nil || sandboxPath == "" || storeKey == "" {
		return outcome, nil
	}
	ex, err := p.getInner(ctx, sc)
	if err != nil {
		return outcome, fmt.Errorf("sandbox write-through: no sandbox: %w", err)
	}
	if _, ok := ex.(RemoteWorkspace); !ok {
		outcome.Comparison = CompareNoSecondCopy
		return outcome, nil // docker: /workspace IS the host directory, nothing to mirror
	}

	// OBSERVE before overwriting. The expectation comes from the CALLER, not
	// from a table this process keeps: edit_file, write_file and apply_patch all
	// know what the store held before their write. That keeps the step
	// memory-free (the sandbox's own bytes are the record) while still being
	// able to state a replaced version instead of destroying it silently.
	//
	// A read error is deliberately reported as "nothing to say" rather than
	// "the copy could not be read": a missing file and an unreadable one are
	// indistinguishable through the Executor port, and a missing file is the
	// normal case for a new path. Claiming a fact we cannot phrase would trade
	// C2 for C3 — see docs 10 §2.1.
	if current, readErr := ex.ReadFile(ctx, sandboxPath); readErr == nil && current != "" {
		outcome.SandboxBytes = int64(len(current))
		switch {
		case previous != "" && current != previous && current != content:
			outcome.Comparison = CompareReplacedVerified
		case previous == "" && current != content:
			outcome.Comparison = CompareReplacedUnchecked
		default:
			outcome.Comparison = CompareNothingReplaced
		}
	}

	if _, err := ex.WriteFile(ctx, sandboxPath, content); err != nil {
		return outcome, fmt.Errorf("sandbox write-through: %w", err)
	}
	// Stamp the sandbox file with the store's write time. This is the whole
	// "baseline" now: it lives in the sandbox filesystem (survives restarts,
	// travels across replicas, visible to the agent with ls -l), and it is what
	// makes the reconcile's cheap "same version" check exact.
	if info, statErr := p.workspace.Stat(ctx, sc.agentID, store.ProjectID, store.SessionID, storeKey); statErr == nil && !info.ModTime.IsZero() {
		if _, touchErr := ex.Exec(ctx, fmt.Sprintf("touch -d @%d %s", info.ModTime.Unix(), shellQuote(sandboxPath)), 15*time.Second); touchErr != nil {
			slog.Warn("sandbox write-through: could not stamp the sandbox copy with the store's write time",
				"path", storeKey, "error", touchErr)
		}
	}
	// H (docs 10 §4 G17): one project is one file tree, and the preview's dev
	// server may be running in a DIFFERENT container of that project — a sibling
	// chat's, or the project-addressed slot the console starts. Docker closes that
	// gap with a bind mount; without one, the store is the channel and the write
	// has to reach every live container of the project.
	outcome.BroadcastFailures = p.mirrorToProjectPeers(ctx, sc, store, ex, storeKey, sandboxPath, content)
	return outcome, nil
}

// mirrorToProjectPeers copies one already-mirrored write into the project's OTHER
// live containers, and stamps them the same way the primary copy is stamped.
//
// Best-effort by construction: the primary copy is already correct, so a peer
// that cannot be reached costs a stale preview — not a wrong file, and never a
// failed tool call. The count comes back so the tool layer can say so (C3: a
// signal only when something went wrong).
func (p *LifecyclePool) mirrorToProjectPeers(ctx context.Context, sc sandboxScope, store StoreScope, primary Executor, storeKey, sandboxPath, content string) int {
	if sc.projectID == "" {
		return 0
	}
	inner, ok := p.inner.(LiveExecutorPool)
	if !ok {
		return 0
	}
	peers := inner.LiveProjectExecutors(sc.agentID, sc.projectID)
	if len(peers) == 0 {
		return 0
	}
	// The stamp is whatever the primary copy was stamped with; one Stat serves
	// every peer.
	var stamp int64
	if info, statErr := p.workspace.Stat(ctx, sc.agentID, store.ProjectID, store.SessionID, storeKey); statErr == nil && !info.ModTime.IsZero() {
		stamp = info.ModTime.Unix()
	}
	failed := 0
	for _, peer := range peers {
		if peer == nil || peer == primary {
			continue
		}
		if _, werr := peer.WriteFile(ctx, sandboxPath, content); werr != nil {
			failed++
			slog.Warn("sandbox write-through: another container of this project could not be updated; "+
				"a dev server running there may show an older version",
				"agent", sc.agentID, "project", sc.projectID, "path", sandboxPath, "error", werr)
			continue
		}
		if stamp != 0 {
			if _, terr := peer.Exec(ctx, fmt.Sprintf("touch -d @%d %s", stamp, shellQuote(sandboxPath)), 15*time.Second); terr != nil {
				slog.Warn("sandbox write-through: could not stamp another container's copy",
					"path", sandboxPath, "error", terr)
			}
		}
	}
	return failed
}

// LifecyclePool also serves as the write-through entry point the tool layer
// reaches through the executor it was handed. Narrow capability interfaces keep
// the tools from needing the pool: they ask the executor, and a docker-backed
// executor answers "nothing to do".
//
// WriteThroughExecutor is what internal/agent/tools type-asserts against.

// CompareResult is what the write-through established about the sandbox's copy of
// one path. Four facts used to be carried by a single bool, and the tool layer
// read that bool as "over the fingerprint cap" — which is how a 5-byte file came
// to be announced as "too large to compare" (docs 10 §2.1, G5). Naming the facts
// is what makes each sentence the exit produces true.
type CompareResult uint8

const (
	// CompareNoSecondCopy: there is nothing to compare, because there is no
	// second copy — the sandbox's /workspace IS the host directory (docker).
	// Nothing was mirrored and nothing was replaced: stay silent.
	CompareNoSecondCopy CompareResult = iota
	// CompareNothingReplaced: the sandbox copy is what the caller expected, or
	// is already the content being written, or could not be read at all.
	// Nothing was replaced: stay silent.
	CompareNothingReplaced
	// CompareReplacedVerified: the sandbox held a version different from the one
	// the caller said it should hold, and the write overwrote it.
	CompareReplacedVerified
	// CompareReplacedUnchecked: the sandbox held bytes different from what is
	// being written, but the caller had no expectation to check them against —
	// something was replaced and we cannot say what it was.
	CompareReplacedUnchecked
)

// WriteThroughOutcome is the delta the mirror observed on its way through. It is
// metadata only: the mirror never keeps a copy of what it replaced, because the
// agent was already told what the sandbox changed (the exec signal) before it
// decided to write. Stating the fact keeps the write honest; duplicating the
// bytes would be a second, redundant safety net for a decision the agent has
// already made.
type WriteThroughOutcome struct {
	// Comparison is what the mirror could establish (see CompareResult). The
	// zero value — no second copy — is the docker case.
	Comparison CompareResult
	// SandboxBytes is the size of the version the mirror found before
	// overwriting it (0 when it found none, or could not read it).
	SandboxBytes int64
	// BroadcastFailures counts the OTHER containers of this project that could
	// not be updated (docs 10 §4 G17, option H). The container this write went to
	// is excluded: it is accounted for by Comparison. Non-zero means a preview
	// running in another container may still show an older version.
	BroadcastFailures int
}

// WriteThroughExecutor mirrors one host write into the sandbox that serves the
// same scope. Implemented by LifecyclePool; the tools call it after a
// successful store write and never let its result fail the tool call — the
// store write already happened.
type WriteThroughExecutor interface {
	WriteThroughScope(store StoreScope, storeKey, sandboxPath, content, previous string) (WriteThroughOutcome, error)
}

// WriteThroughScope is the executor-facing form of WriteThrough: the caller
// carries the scope identity it was constructed with, so the tool layer does
// not have to know how the pool keys sandboxes.
func (l *lazyExecutor) WriteThroughScope(store StoreScope, storeKey, sandboxPath, content, previous string) (WriteThroughOutcome, error) {
	return l.pool.WriteThrough(context.Background(), l.scope, store, storeKey, sandboxPath, content, previous)
}

// RemoveLiveWorkspaceFile is the panel's delete side, on the same proxy the
// write-through uses: remove the path inside the scope's CURRENT sandbox.
//
// Two deliberate properties:
//
//   - It never creates an instance. `liveInstance` asks the inner pool for a
//     cached executor only; a scope nobody is running means "nothing to remove",
//     which is correct — the store is the authority and the next hydrate reads
//     the store, where the file is already gone.
//   - It reuses the same store→sandbox mapping as hydrate (SandboxPathForStorePath),
//     so the file it deletes is the file the sandbox actually has. Getting this
//     wrong is what made the panel delete a silent no-op (10 §4 G21), and the way
//     it failed (address the wrong key, report success) is the way this whole
//     family fails.
func (l *lazyExecutor) RemoveLiveWorkspaceFile(ctx context.Context, storePath string) error {
	sandboxPath, err := SandboxPathForStorePath(storePath)
	if err != nil {
		return err
	}
	// Every live container of the project holds its own copy, and every one of
	// them syncs its /workspace back to the store — so a copy left behind in a
	// peer is not just stale, it is a file that comes back (the loop G21/d1
	// exists to close, one container further out). The scope's own instance is
	// included when it is live.
	var targets []Executor
	if ex, ok := l.pool.liveInstance(l.scope); ok && ex != nil {
		targets = append(targets, ex)
	}
	if l.scope.projectID != "" {
		if inner, ok := l.pool.inner.(LiveExecutorPool); ok {
			for _, peer := range inner.LiveProjectExecutors(l.scope.agentID, l.scope.projectID) {
				if peer == nil {
					continue
				}
				dup := false
				for _, t := range targets {
					if t == peer {
						dup = true
						break
					}
				}
				if !dup {
					targets = append(targets, peer)
				}
			}
		}
	}
	var firstErr error
	for _, target := range targets {
		if _, err := target.Exec(ctx, "rm -f -- "+shellQuote(sandboxPath), 30*time.Second); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sandbox: remove %s: %w", sandboxPath, err)
			}
			slog.Warn("sandbox: could not remove a copy of the deleted file; "+
				"this container's next sync may write it back",
				"agent", l.scope.agentID, "project", l.scope.projectID, "session", l.scope.sessionID,
				"path", storePath, "error", err)
		}
	}
	return firstErr
}

// liveInstance returns the scope's current instance without creating one. Pools
// that cannot answer (no cache, or a backend that keeps no instance) say false,
// and every caller of this must treat false as "there is no second copy to
// touch".
func (p *LifecyclePool) liveInstance(sc sandboxScope) (Executor, bool) {
	inner, ok := p.inner.(LiveExecutorPool)
	if !ok {
		return nil, false
	}
	return inner.LiveExecutor(sc.agentID, sc.projectID, sc.sessionID)
}
