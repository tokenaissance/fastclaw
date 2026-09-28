package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// The two failures the §7.4 tests have to tell apart: one that indicts the
// INSTANCE (a stream cut mid-flight — the prod text is "the connection to
// sandbox … ended before the stream completed") and one that is the command's
// own verdict. Only the first may cost a sandbox.
var (
	errFakeCutStream      = errors.New("fake e2b: the connection to sandbox sb-1 ended before the stream completed")
	errFakeCommandVerdict = errors.New("exit code 1")
	errFakeReleaseFailed  = errors.New("fake pool: release failed (502)")
)

// fakeExecutor counts Exec calls so tests can prove the sandbox was actually
// invoked (or wasn't). Also records WriteFile targets so hydrate tests can
// check which paths landed inside the sandbox.
type fakeExecutor struct {
	execs   int32
	closed  int32
	agentID string

	mu     sync.Mutex
	writes map[string]string
	// commands records what Exec was asked to run, in order. The write-through's
	// stamp is only observable there (a fake filesystem has no mtime), so tests
	// that pin "the sandbox copy carries the STORE's write time" read this
	// instead of counting later reads — see TestWriteThroughStampsTheSandboxCopyWithTheStoreTime.
	commands []string
	// execErr, when set, is what this instance's Exec returns. Armed per
	// INSTANCE on purpose: the test that arms it wants the first instance to
	// fail and its replacement to be healthy — the shape of §7.4, where the
	// answer to a bad instance is a new instance, not a re-run of the command.
	execErr error

	// workspaceUnhydrated is what WorkspaceUnhydrated() reports. The zero value
	// — a workspace that was filled from the store — is what every test written
	// before Policy C assumes, so they keep exercising the plain path.
	workspaceUnhydrated atomic.Bool
}

func (f *fakeExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	atomic.AddInt32(&f.execs, 1)
	f.mu.Lock()
	f.commands = append(f.commands, command)
	err := f.execErr
	f.mu.Unlock()
	if err != nil {
		// Real cut streams still deliver whatever the command printed before
		// the cut, so the fake does too: the fix must not turn a partial result
		// into a silent one.
		return "partial output\n", err
	}
	return "ok", nil
}

// execCommands is the recorded Exec stream, copied so callers can read it
// without holding the mutex.
func (f *fakeExecutor) execCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeExecutor) ReadFile(ctx context.Context, path string) (string, error) { return "", nil }
func (f *fakeExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	f.mu.Lock()
	if f.writes == nil {
		f.writes = map[string]string{}
	}
	f.writes[p] = c
	f.mu.Unlock()
	return "", nil
}
func (f *fakeExecutor) ListDir(ctx context.Context, path string) (string, error) { return "", nil }
func (f *fakeExecutor) Backend() string                                          { return "fake" }
func (f *fakeExecutor) WorkspaceUnhydrated() bool                                { return f.workspaceUnhydrated.Load() }
func (f *fakeExecutor) Close() error {
	atomic.AddInt32(&f.closed, 1)
	return nil
}

// fakePool tracks created/released executors so tests can assert lifecycle
// events happened in the right order.
type fakePool struct {
	creates   int32
	releases  int32
	closedAll int32
	// unhydrated arms every executor this pool creates to report a workspace
	// whose listing never succeeded — the prod shape of 2026-09-16, where the
	// store answered `context deadline exceeded` and the sandbox came up with
	// an empty /workspace (docs/sandbox-scope-leak.md §8).
	unhydrated atomic.Bool
	// liveMu guards live. The lifecycle pool sweeps on a background goroutine,
	// so a test goroutine calling Get/Release runs concurrently with the
	// sweeper's own Get — an unsynchronised map here is a data race, not just
	// a hypothetical one (`go test -race` finds it).
	liveMu sync.Mutex
	live   map[string]*fakeExecutor
	// releaseErr, when set, makes Release fail — the double failure of §7.4's
	// third case: the instance is known to be bad and cannot be destroyed.
	releaseErr error
}

func newFakePool() *fakePool { return &fakePool{live: map[string]*fakeExecutor{}} }

func (p *fakePool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	key := poolKey(agentID, projectID, sessionID)
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	if ex, ok := p.live[key]; ok {
		return ex, nil
	}
	atomic.AddInt32(&p.creates, 1)
	ex := &fakeExecutor{agentID: key}
	ex.workspaceUnhydrated.Store(p.unhydrated.Load())
	p.live[key] = ex
	return ex, nil
}

func (p *fakePool) Release(agentID, projectID, sessionID string) error {
	atomic.AddInt32(&p.releases, 1)
	key := poolKey(agentID, projectID, sessionID)
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	if p.releaseErr != nil {
		return p.releaseErr
	}
	if ex, ok := p.live[key]; ok {
		delete(p.live, key)
		return ex.Close()
	}
	return nil
}

// Unusable is the classifier the lifecycle layer asks (§7.4). The fake answers
// for one sentinel so a test can say "this failure indicts the instance"
// without speaking e2b's wire vocabulary; the real classifier is pinned against
// real error shapes in e2b_unusable_test.go.
func (p *fakePool) Unusable(err error) bool { return errors.Is(err, errFakeCutStream) }

func (p *fakePool) Backend() string { return "fake" }

func (p *fakePool) CloseAll() {
	atomic.AddInt32(&p.closedAll, 1)
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	for id, ex := range p.live {
		ex.Close()
		delete(p.live, id)
	}
}

// TestLifecycle_LazyCreation proves that calling Get on the LifecyclePool
// does NOT hit the inner pool until the first real tool call. This is the
// main cost-saver: an agent that just chats never spawns a sandbox.
func TestLifecycle_LazyCreation(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, 0, 0) // eviction disabled
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "alice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 0 {
		t.Fatalf("expected 0 creates after Get; got %d", got)
	}

	// First tool call should trigger lazy creation.
	if _, err := ex.Exec(context.Background(), "echo hi", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected 1 create after Exec; got %d", got)
	}

	// Second call reuses the cached executor.
	if _, err := ex.Exec(context.Background(), "echo bye", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected still 1 create after second Exec; got %d", got)
	}
}

// TestLifecycle_IdleEviction proves the sweeper Release()s sandboxes after
// they've been unused for IdleTTL.
func TestLifecycle_IdleEviction(t *testing.T) {
	inner := newFakePool()
	// Tight times so the test is fast; sweep twice per TTL window.
	lp := NewLifecyclePool(inner, 50*time.Millisecond, 20*time.Millisecond)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "bob", "", "")
	ex.Exec(context.Background(), "ls", time.Second)

	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected 1 create; got %d", got)
	}

	// Wait longer than the TTL — sweeper should release.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&inner.releases); got < 1 {
		t.Fatalf("expected release after idle; got %d", got)
	}

	// A new call after eviction recreates.
	ex.Exec(context.Background(), "ls", time.Second)
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("expected 2 creates (first + after eviction); got %d", got)
	}
}

// fakeWorkspace is an in-memory workspace.Store for tests that stashes
// objects under {agent}/{path}. Minimal fields — enough for hydrate to
// iterate and read.
type fakeWorkspace struct {
	mu      sync.Mutex
	objects map[string]map[string][]byte // agentID → path → bytes
}

func newFakeWorkspace() *fakeWorkspace {
	return &fakeWorkspace{objects: map[string]map[string][]byte{}}
}

func (w *fakeWorkspace) put(agentID, path string, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.objects[agentID]; !ok {
		w.objects[agentID] = map[string][]byte{}
	}
	w.objects[agentID][path] = data
}

// scopeKey collapses (agent, session) into the per-test in-memory key.
// Empty session keeps the "agent shared" bucket so existing tests that
// don't care about sessions still work without changes.
func wsScopeKey(agentID, sessionID string) string {
	if sessionID == "" {
		return agentID
	}
	return agentID + ":" + sessionID
}

func (w *fakeWorkspace) Put(ctx context.Context, agentID, projectID, sessionID, p string, r io.Reader, _ int64, _ string) error {
	buf, _ := io.ReadAll(r)
	w.put(wsScopeKey(agentID, scopeForKey(projectID, sessionID)), p, buf)
	return nil
}

func (w *fakeWorkspace) Get(ctx context.Context, agentID, projectID, sessionID, p string) (io.ReadCloser, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, ok := w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))][p]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (w *fakeWorkspace) Stat(ctx context.Context, agentID, projectID, sessionID, p string) (*workspace.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, ok := w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))][p]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return &workspace.ObjectInfo{Path: p, Size: int64(len(data))}, nil
}

func (w *fakeWorkspace) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []workspace.ObjectInfo
	for p, data := range w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))] {
		out = append(out, workspace.ObjectInfo{Path: p, Size: int64(len(data))})
	}
	return out, nil
}

func (w *fakeWorkspace) Delete(ctx context.Context, agentID, projectID, sessionID, p string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))], p)
	return nil
}

func (w *fakeWorkspace) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	srcKey := wsScopeKey(agentID, scopeForKey(fromProjectID, fromSessionID))
	dstKey := wsScopeKey(agentID, scopeForKey(toProjectID, toSessionID))
	if srcKey == dstKey {
		return nil
	}
	if existing, ok := w.objects[dstKey]; ok && len(existing) > 0 {
		return workspace.ErrMoveDestinationExists
	}
	if src, ok := w.objects[srcKey]; ok {
		w.objects[dstKey] = src
		delete(w.objects, srcKey)
	}
	return nil
}

func (w *fakeWorkspace) SignedURL(ctx context.Context, agentID, projectID, sessionID, p string, ttl time.Duration) (string, error) {
	return "", workspace.ErrSignedURLUnsupported
}

// scopeForKey collapses the (project, session) tuple to a single string
// the test fakes can use as a map sub-key. Project wins so all chats in
// one project share a slot, mirroring production scopeDir logic.
func scopeForKey(projectID, sessionID string) string {
	if projectID != "" {
		return "p:" + projectID
	}
	return sessionID
}

// TestLifecycle_HydrateOnCreate proves that the first tool call triggers
// a copy from workspace.Store into the sandbox, and that a second call on
// the same live sandbox does not re-hydrate.
// flakyList fails its first N List calls, then defers to the fake. That is the
// prod shape this guards: an intermittent `context deadline exceeded` on the
// store listing, which a retry is exactly the right answer for. Without the
// retry the hydrate uploads an EMPTY workspace and reports success
// (docs/sandbox-scope-leak.md §8).
type flakyList struct {
	*fakeWorkspace
	remainingFailures int32
	calls             int32
}

func (f *flakyList) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	atomic.AddInt32(&f.calls, 1)
	if atomic.AddInt32(&f.remainingFailures, -1) >= 0 {
		return nil, errors.New("context deadline exceeded")
	}
	return f.fakeWorkspace.List(ctx, agentID, projectID, sessionID)
}

func TestE2BHydrateListRetriesThenSucceeds(t *testing.T) {
	ws := newFakeWorkspace()
	ws.put("agent-retry", "report.pdf", []byte("pdf-bytes"))
	flaky := &flakyList{fakeWorkspace: ws, remainingFailures: 1}
	ex := &E2BExecutor{workspace: flaky, agentID: "agent-retry"}

	// Same scope the fake stored under: its put() keys on the agent alone.
	objs, err := ex.listWorkspaceWithRetry(context.Background(), "", "")
	if err != nil {
		t.Fatalf("a listing that fails once must be retried, got %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("workspace files = %d, want 1 (the retry must return the real listing)", len(objs))
	}
	if got := atomic.LoadInt32(&flaky.calls); got != 2 {
		t.Fatalf("List calls = %d, want 2 (one failure, one success)", got)
	}
	// A hydrate that eventually listed is NOT unhydrated: the mark means "the
	// workspace may be missing files", and this one is complete.
	if ex.WorkspaceUnhydrated() {
		t.Fatal("executor marked unhydrated after a successful retry")
	}
}

// The boundary that keeps the unhydrated signal from doing damage of its own: a
// listing that SUCCEEDS and returns nothing is a genuinely empty scope (a new
// session, a fresh agent), not a failure. Treating it as one would break every
// new conversation.
func TestE2BHydrateEmptyScopeIsNotMarkedUnhydrated(t *testing.T) {
	ex := &E2BExecutor{workspace: newFakeWorkspace(), agentID: "agent-empty"}

	objs, err := ex.listWorkspaceWithRetry(context.Background(), "", "")
	if err != nil {
		t.Fatalf("an empty scope is a normal outcome, got %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("workspace files = %d, want 0", len(objs))
	}
	if ex.WorkspaceUnhydrated() {
		t.Fatal("empty scope marked unhydrated: listing nothing successfully is not a failure")
	}
}

// hydratedFor reports what the lifecycle pool believes about a scope's hydrate.
// Read under the pool's mutex: the sweeper goroutine touches the same map.
func hydratedFor(lp *LifecyclePool, agentID, projectID, sessionID string) bool {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	return lp.hydrated[poolKey(agentID, projectID, sessionID)]
}

// liveExecutor returns the instance the fake pool currently holds for a scope.
func liveExecutor(t *testing.T, p *fakePool, agentID, projectID, sessionID string) *fakeExecutor {
	t.Helper()
	key := poolKey(agentID, projectID, sessionID)
	p.liveMu.Lock()
	defer p.liveMu.Unlock()
	ex, ok := p.live[key]
	if !ok {
		t.Fatalf("no live executor for %q", key)
	}
	return ex
}

// Policy C's bookkeeping (docs/sandbox-scope-leak.md §9.4 case 2): when the
// store keeps failing to list, the call still goes through — a store hiccup
// must not cost the scope its instance, and must not fail the user's command —
// but the scope is NOT recorded as hydrated. That flag is what separates "we
// served an empty /workspace" from "we served an empty /workspace and will try
// again"; without it the incident of 2026-09-16 is silent.
func TestLifecycle_UnhydratedWorkspaceIsNotRecordedHydrated(t *testing.T) {
	inner := newFakePool()
	inner.unhydrated.Store(true) // every instance this pool creates comes up empty
	lp := NewLifecyclePool(inner, 0, 0)
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "erin", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "cat /workspace/refresh_paper_review.py", time.Second); err != nil {
		t.Fatalf("an unhydrated workspace must not fail the call: %v", err)
	}

	uw, ok := ex.(UnhydratedWorkspace)
	if !ok {
		t.Fatal("the executor handed to the tools must report its workspace state")
	}
	if !uw.WorkspaceUnhydrated() {
		t.Fatal("a sandbox whose listing never succeeded reported a hydrated workspace")
	}
	if hydratedFor(lp, "erin", "", "") {
		t.Fatal("scope recorded as hydrated although the listing never succeeded — the next use would reuse the empty workspace without retrying")
	}
}

// Case 4 of docs/sandbox-scope-leak.md §9.4: a scope whose workspace WAS filled
// and then comes back empty is a contradiction — the instance in hand is not
// the one this scope's files live in. It must be destroyed and replaced, not
// handed to the model as an empty world.
//
// (This is the general repair as well as the contradiction case: a live E2B
// instance is never re-hydrated, so a destroy + recreate is the only path that
// runs the hydrate again at all.)
func TestLifecycle_UnhydratedWorkspaceIsRebuilt(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, 0, 0)
	lp.rebuildCooldown = 0
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "frank", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
		t.Fatal(err)
	}
	first := liveExecutor(t, inner, "frank", "", "")

	// The store starts failing: the same instance now reports that its
	// /workspace was never filled.
	first.workspaceUnhydrated.Store(true)
	if _, err := ex.Exec(context.Background(), "cat /workspace/refresh_paper_review.py", time.Second); err != nil {
		t.Fatalf("the call itself must still go through: %v", err)
	}

	if got := atomic.LoadInt32(&inner.releases); got != 1 {
		t.Fatalf("releases = %d, want 1 (the empty instance must be destroyed, not paused)", got)
	}
	if got := atomic.LoadInt32(&first.closed); got != 1 {
		t.Fatalf("the empty instance was not closed (closed = %d)", got)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("creates = %d, want 2 (a replacement must be built so the hydrate runs again)", got)
	}
	if replacement := liveExecutor(t, inner, "frank", "", ""); replacement == first {
		t.Fatal("the unhydrated instance was handed out again")
	}
	if uw, ok := ex.(UnhydratedWorkspace); !ok || uw.WorkspaceUnhydrated() {
		t.Fatal("the scope still reports an unfilled workspace after a rebuild that listed successfully")
	}
	if !hydratedFor(lp, "frank", "", "") {
		t.Fatal("the rebuilt instance was hydrated, but the scope is not recorded as hydrated")
	}
}

// The other half of the repair: a store that stays down must not become a
// sandbox factory (one instance per tool call), and a store that comes back
// must not leave the scope empty until the idle sweeper happens to recycle it.
func TestLifecycle_UnhydratedRebuildIsRateLimitedButRepeatable(t *testing.T) {
	inner := newFakePool()
	inner.unhydrated.Store(true) // the store is down
	lp := NewLifecyclePool(inner, 0, 0)
	lp.rebuildCooldown = time.Minute
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "gwen", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
		t.Fatal(err)
	}
	// The first call escalates: create, rebuild, create again — and both
	// instances are empty, so the scope is handed out unhydrated and says so.
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("creates = %d, want 2 (one build + one rebuild)", got)
	}
	uw, ok := ex.(UnhydratedWorkspace)
	if !ok || !uw.WorkspaceUnhydrated() {
		t.Fatal("the scope must report its workspace as unhydrated while the store is down")
	}
	if hydratedFor(lp, "gwen", "", "") {
		t.Fatal("scope recorded as hydrated while the store was down")
	}

	// Further calls inside the cooldown do not rebuild.
	for i := 0; i < 3; i++ {
		if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("creates = %d after three more calls, want 2: a store outage must not mint a sandbox per tool call", got)
	}

	// The store comes back. The cooldown is what lets us find out: without a
	// rebuild we would keep serving the instance that came up empty, and the
	// scope would stay empty until idle eviction recycled it.
	inner.unhydrated.Store(false)
	lp.mu.Lock()
	lp.rebuildAt[poolKey("gwen", "", "")] = time.Now().Add(-2 * time.Minute)
	lp.mu.Unlock()
	if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 3 {
		t.Fatalf("creates = %d, want 3 (the scope must retry the hydrate once the cooldown lapses)", got)
	}
	if uw, ok := ex.(UnhydratedWorkspace); !ok || uw.WorkspaceUnhydrated() {
		t.Fatal("scope still unhydrated after the store recovered and the instance was rebuilt")
	}
	if !hydratedFor(lp, "gwen", "", "") {
		t.Fatal("scope not recorded as hydrated after the store recovered")
	}
}

// Destroying an instance underneath a running command truncates that command's
// stream — the M2 failure mode of the same incident (§4). So the repair waits
// for the scope to be idle. White-box (inUse is otherwise only non-zero from
// inside an in-flight Exec) because the assertion is about the guard itself.
func TestLifecycle_UnhydratedRebuildWaitsForInFlightWork(t *testing.T) {
	inner := newFakePool()
	inner.unhydrated.Store(true)
	lp := NewLifecyclePool(inner, 0, 0)
	lp.rebuildCooldown = 0
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "hana", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
		t.Fatal(err)
	}
	inner.liveMu.Lock()
	before := inner.live[poolKey("hana", "", "")]
	inner.liveMu.Unlock()

	lp.mu.Lock()
	lp.inUse[poolKey("hana", "", "")] = 1
	lp.mu.Unlock()
	if _, err := ex.Exec(context.Background(), "ls /workspace", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.releases); got != 1 {
		t.Fatalf("releases = %d, want 1 (still just the first rebuild): a busy scope must not be destroyed underneath its own command", got)
	}
	inner.liveMu.Lock()
	after := inner.live[poolKey("hana", "", "")]
	inner.liveMu.Unlock()
	if before != after {
		t.Fatal("the busy scope's instance was replaced while an operation was in flight")
	}
}

// §7.4 case ①: an exec stream that dies mid-flight indicts the INSTANCE, and
// the repair is to destroy it so the next call gets a healthy one.
//
// What this deliberately does NOT do is re-run the command. A cut stream can
// leave the command's side effects half-applied, and the tool layer's own
// stance is "hint, don't auto-fall-back" (internal/agent/tools/exec.go) — so
// the provider's verdict goes back untouched, plus one sentence saying the
// instance was replaced and the command did not finish.
func TestLifecycle_CutStreamReplacesTheInstanceWithoutReRunning(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, 0, 0) // eviction disabled
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "iris", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "echo warm", time.Second); err != nil {
		t.Fatal(err)
	}
	first := liveExecutor(t, inner, "iris", "", "")
	first.mu.Lock()
	first.execErr = errFakeCutStream
	first.mu.Unlock()

	out, callErr := ex.Exec(context.Background(), "python refresh_paper_review.py", time.Minute)
	if callErr == nil {
		t.Fatal("a cut stream must not be reported as success")
	}
	if !errors.Is(callErr, errFakeCutStream) {
		t.Fatalf("the provider's own error must survive into what the model reads, got %v", callErr)
	}
	if out == "" {
		t.Fatal("the output that did arrive before the cut must still be delivered")
	}
	if !strings.Contains(callErr.Error(), "sandbox replaced") {
		t.Fatalf("nothing tells the model the instance was replaced, so it cannot tell this from a plain failure: %v", callErr)
	}

	// Destroyed, not paused: the pause path (evictIdle → sleepOrRelease →
	// SleepScope) would park the broken instance for the next wake-up to hand
	// out again. Getting this wrong is the whole bug (§7.2).
	if got := atomic.LoadInt32(&inner.releases); got != 1 {
		t.Fatalf("releases = %d, want 1 (the cut instance must be destroyed)", got)
	}
	if got := atomic.LoadInt32(&first.closed); got != 1 {
		t.Fatalf("the cut instance was not closed (closed = %d)", got)
	}
	// Not re-run. A re-run would have to build the replacement inside this very
	// call, so "no replacement yet" is the observable difference between
	// replace and retry.
	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("creates = %d, want 1 — the replacement belongs to the NEXT call, which is what makes this a replace and not a retry", got)
	}
	if got := atomic.LoadInt32(&first.execs); got != 2 {
		t.Fatalf("execs on the cut instance = %d, want 2 (the warm-up plus the one cut command; a retry would add a third)", got)
	}

	// The next call is what mints the replacement, and it serves normally.
	if _, err := ex.Exec(context.Background(), "echo after", time.Second); err != nil {
		t.Fatalf("the replacement must serve the next command: %v", err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("creates = %d, want 2 (one replacement, built on the next call)", got)
	}
	if next := liveExecutor(t, inner, "iris", "", ""); next == first {
		t.Fatal("the cut instance was handed out again")
	}
}

// §7.4 case ②, the narrowness half: "the command exited non-zero" is the
// command's verdict. Treating it as an instance failure would destroy a healthy
// sandbox and re-run side effects the command already had, so it must leave the
// scope completely alone.
func TestLifecycle_CommandFailureIsNotAnInstanceFailure(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, 0, 0)
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "jules", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "echo warm", time.Second); err != nil {
		t.Fatal(err)
	}
	first := liveExecutor(t, inner, "jules", "", "")
	first.mu.Lock()
	first.execErr = errFakeCommandVerdict
	first.mu.Unlock()

	if _, callErr := ex.Exec(context.Background(), "pytest tests/", time.Minute); callErr == nil {
		t.Fatal("a non-zero exit must still fail the call")
	} else if !errors.Is(callErr, errFakeCommandVerdict) {
		t.Fatalf("the command's own error must reach the model unchanged, got %v", callErr)
	} else if strings.Contains(callErr.Error(), "sandbox replaced") {
		t.Fatalf("a command verdict was reported as an instance replacement: %v", callErr)
	}

	if got := atomic.LoadInt32(&inner.releases); got != 0 {
		t.Fatalf("releases = %d, want 0 — a failing command must not cost the scope its sandbox", got)
	}
	if got := atomic.LoadInt32(&first.closed); got != 0 {
		t.Fatalf("the instance was closed over a command's non-zero exit (closed = %d)", got)
	}
	if liveExecutor(t, inner, "jules", "", "") != first {
		t.Fatal("the instance was replaced although only the command had failed")
	}
}

// §7.4 case ③: both halves of the repair can fail independently. When the bad
// instance cannot even be destroyed, the honest outcome is the original error —
// the one thing that must not happen is an error claiming a replacement that
// did not take place.
func TestLifecycle_FailedReplaceDoesNotClaimARepair(t *testing.T) {
	inner := newFakePool()
	inner.releaseErr = errFakeReleaseFailed
	lp := NewLifecyclePool(inner, 0, 0)
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "kira", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "echo warm", time.Second); err != nil {
		t.Fatal(err)
	}
	first := liveExecutor(t, inner, "kira", "", "")
	first.mu.Lock()
	first.execErr = errFakeCutStream
	first.mu.Unlock()

	_, callErr := ex.Exec(context.Background(), "python refresh_paper_review.py", time.Minute)
	if callErr == nil {
		t.Fatal("a cut stream must not be reported as success")
	}
	if !errors.Is(callErr, errFakeCutStream) {
		t.Fatalf("the provider's own error must survive a failed replace, got %v", callErr)
	}
	if strings.Contains(callErr.Error(), "sandbox replaced") {
		t.Fatalf("the error claims a replacement that did not happen: %v", callErr)
	}
	if got := atomic.LoadInt32(&first.closed); got != 0 {
		t.Fatalf("the instance was closed even though Release failed (closed = %d)", got)
	}
}

func TestLifecycle_HydrateOnCreate(t *testing.T) {
	inner := newFakePool()
	ws := newFakeWorkspace()
	ws.put("dave", "report.pdf", []byte("pdf-bytes"))
	ws.put("dave", "scripts/gen.py", []byte("print(1)"))

	lp := NewLifecyclePool(inner, 0, 0)
	lp.SetWorkspace(ws)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "dave", "", "")
	ex.Exec(context.Background(), "ls /workspace", time.Second)

	// Grab the underlying fake executor to inspect writes.
	inner.liveMu.Lock()
	underlying := inner.live["dave"]
	inner.liveMu.Unlock()
	if underlying == nil {
		t.Fatal("expected inner pool to hold the created executor")
	}
	underlying.mu.Lock()
	writes := len(underlying.writes)
	hasReport := strings.Contains(strings.Join(mapKeys(underlying.writes), ","), "report.pdf")
	hasScript := strings.Contains(strings.Join(mapKeys(underlying.writes), ","), "gen.py")
	underlying.mu.Unlock()

	if writes != 2 {
		t.Fatalf("expected 2 hydrate writes; got %d", writes)
	}
	if !hasReport || !hasScript {
		t.Fatalf("hydrate missed files: writes=%+v", underlying.writes)
	}

	// Second tool call should NOT trigger another hydrate (idempotent
	// during the sandbox's life).
	ex.Exec(context.Background(), "ls", time.Second)
	underlying.mu.Lock()
	writesAfter := len(underlying.writes)
	underlying.mu.Unlock()
	if writesAfter != writes {
		t.Fatalf("second exec triggered re-hydrate; writes grew from %d to %d", writes, writesAfter)
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// snapshottingExecutor is a fakeExecutor that also implements
// WorkspaceSnapshotter so we can verify flush-on-evict copies the right
// bytes to the store.
type snapshottingExecutor struct {
	fakeExecutor
	files map[string][]byte
	// mtimes and clock model the sandbox's own filesystem clock, and they are
	// OPT-IN: nil means "this fake cannot report a stat pass" — the behaviour
	// every test written before the ordering rule (row 87) assumes, and the
	// conservative one (the reconcile refuses when it cannot read the files).
	//
	// With them set, the fake answers the two commands the reconcile itself runs
	// (see execInstrumented): the listing + guest-clock probe in one exec, and the
	// `touch -d @…` stamp. mtimes is the sandbox's own view; clock is the guest's
	// clock, which the live runtime measures ≈1.2 s ahead of the pod's.
	mtimes map[string]time.Time
	clock  time.Time
	// onExec runs at the START of Exec — the place a test puts "the turn dies while the command is
	// running". Used by the post-exec-sync witness (row 85): the sync must survive it.
	onExec func()
	// snapshotErr, when set, is what SnapshotWorkspace reports — the shape of a
	// workspace too large to snapshot.
	snapshotErr error
	// replaced, when set, makes TakeWorkspaceReplaced report a rebuild once —
	// the shape of an expired instance being re-created from the store.
	replaced bool
	// readErr, when set, makes ReadFile fail — the shape of a call that cannot
	// carry a note back to the agent.
	readErr error
}

// TakeWorkspaceReplaced implements ReplacedWorkspace.
func (s *snapshottingExecutor) TakeWorkspaceReplaced() bool {
	if !s.replaced {
		return false
	}
	s.replaced = false
	return true
}

// Exec delegates to the fake and gives the test its one hook: a turn that dies while its command is
// running. (The fake's Exec ignores the ctx on purpose — the command itself ran; what a cut takes is
// the stream and, without row 85, the sync.)
func (s *snapshottingExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if s.onExec != nil {
		s.onExec()
	}
	if s.mtimes != nil {
		if out, ok := s.execInstrumented(command); ok {
			// Record it like any other exec: the write-through's stamp is only
			// observable in the command stream, and the delivery's stamp is the
			// same fact one direction over.
			atomic.AddInt32(&s.fakeExecutor.execs, 1)
			s.fakeExecutor.mu.Lock()
			s.fakeExecutor.commands = append(s.fakeExecutor.commands, command)
			s.fakeExecutor.mu.Unlock()
			return out, nil
		}
	}
	return s.fakeExecutor.Exec(ctx, command, timeout)
}

// guestNow is the modelled guest clock; without an explicit one, the host's time,
// which is what a fake that never set it is implicitly claiming.
func (s *snapshottingExecutor) guestNow() time.Time {
	if s.clock.IsZero() {
		return time.Now()
	}
	return s.clock
}

// execInstrumented answers the commands the reconcile runs to measure the world:
// the one exec that reads the guest clock and lists the files (statsFor), and the
// stamp (`touch -d @…`) that hydrate, the mirror and the delivery all use.
//
// It exists so a test can state the sandbox's clock and each file's mtime as
// facts, instead of asserting against an unreadable stat pass. A path with no
// recorded mtime reads as "written by the sandbox at the modelled instant".
func (s *snapshottingExecutor) execInstrumented(command string) (string, bool) {
	switch {
	case strings.Contains(command, `find /workspace -type f -printf`):
		var sb strings.Builder
		sb.WriteString("NOW " + strconv.FormatFloat(float64(s.guestNow().Unix())+float64(s.guestNow().Nanosecond())/1e9, 'f', 6, 64) + "\n")
		paths := make([]string, 0, len(s.files))
		for p := range s.files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			at, ok := s.mtimes[p]
			if !ok {
				at = s.guestNow()
			}
			sb.WriteString(fmt.Sprintf("%s\t%d\t%d\n", p, len(s.files[p]), at.Unix()))
		}
		return sb.String(), true
	case strings.HasPrefix(command, "touch -d @"):
		rest := strings.TrimPrefix(command, "touch -d @")
		secs, quoted, found := strings.Cut(rest, " ")
		if !found {
			return "", true
		}
		unix, err := strconv.ParseInt(secs, 10, 64)
		if err != nil {
			return "", true
		}
		if rel, ok := strings.CutPrefix(strings.Trim(quoted, "'"), "/workspace/"); ok {
			s.mtimes[rel] = time.Unix(unix, 0)
		}
		return "ok", true
	}
	return "", false
}

func (s *snapshottingExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	// A cancelled request fails, the way e2b's does — without this the fake would answer a dead ctx
	// with a full snapshot and the post-exec-sync witness could not tell the two behaviours apart.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(s.files))
	for k, v := range s.files {
		out[k] = v
	}
	return out, nil
}

// IsRemoteWorkspace marks the snapshotting fake as a DETACHED backend, which is
// what it models: it carries its own copy of /workspace (that is why it can be
// snapshotted), so host writes have to be mirrored into it and a failed mirror
// has to be remembered. Docker — the backend where /workspace IS the host
// directory — deliberately does NOT implement this.
func (s *snapshottingExecutor) IsRemoteWorkspace() {}

// WriteFile keeps the snapshot and the write record in step. A real sandbox has
// ONE copy of /workspace, so a mirror write and a snapshot of that path cannot
// disagree; this fake used to keep them in separate maps, which let a test
// assert against the wrong one. written(relative) is the read side.
func (s *snapshottingExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	if _, err := s.fakeExecutor.WriteFile(ctx, p, c); err != nil {
		return "", err
	}
	rel, ok := strings.CutPrefix(p, "/workspace/")
	if !ok {
		rel, ok = strings.CutPrefix(p, defaultSandboxRoot+"/")
	}
	if ok && rel != "" {
		s.files[rel] = []byte(c)
	}
	return "", nil
}

// ReadFile reads the sandbox's copy — the same single copy the snapshot reports,
// which is what a real executor does. Without this the fake answered "" for
// every path, so anything reasoning about the sandbox's CURRENT content (the
// write-through precondition, conflict resolution) saw an empty file.
func (s *snapshottingExecutor) ReadFile(ctx context.Context, p string) (string, error) {
	if s.readErr != nil {
		return "", s.readErr
	}
	rel, ok := strings.CutPrefix(p, "/workspace/")
	if !ok {
		rel, ok = strings.CutPrefix(p, defaultSandboxRoot+"/")
	}
	if !ok {
		rel = p
	}
	if data, ok := s.files[rel]; ok {
		return string(data), nil
	}
	return "", errors.New("sandbox: no such file")
}

// fakePool that returns a snapshottingExecutor on first Get, so the
// lifecycle pool sees a backend that supports SnapshotWorkspace.
type snappingPool struct {
	fakePool
	current *snapshottingExecutor
	// getErr, when set, makes Get fail — the shape of a scope whose sandbox is
	// gone, which is how a write-through loses its mirror.
	getErr error
}

func newSnappingPool(files map[string][]byte) *snappingPool {
	return &snappingPool{
		fakePool: *newFakePool(),
		current:  &snapshottingExecutor{files: files},
	}
}

func (p *snappingPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	if p.getErr != nil {
		return nil, p.getErr
	}
	key := poolKey(agentID, projectID, sessionID)
	p.fakePool.liveMu.Lock()
	defer p.fakePool.liveMu.Unlock()
	if _, ok := p.fakePool.live[key]; !ok {
		atomic.AddInt32(&p.fakePool.creates, 1)
		// Lie: stash the underlying fakeExecutor in fakePool.live so
		// Release knows to clean, but return the snapshotter so type-
		// assertion works.
		p.fakePool.live[key] = &p.current.fakeExecutor
	}
	return p.current, nil
}

// TestLifecycle_FlushOnEvict proves that files the sandbox wrote (but
// weren't routed through write_file) get copied into workspace.Store when
// the sandbox is idle-evicted.
func TestLifecycle_FlushOnEvict(t *testing.T) {
	ws := newFakeWorkspace()
	files := map[string][]byte{
		"new_artifact.txt": []byte("hello from exec"),
		"subdir/data.json": []byte(`{"ok":true}`),
	}
	pool := newSnappingPool(files)

	lp := NewLifecyclePool(pool, 40*time.Millisecond, 15*time.Millisecond)
	lp.SetWorkspace(ws)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "erin", "", "")
	ex.Exec(context.Background(), "python generate.py", time.Second)

	// Wait past idle TTL so the sweeper evicts — flush should fire.
	time.Sleep(150 * time.Millisecond)

	// Both files should now be in the workspace store.
	for path, want := range files {
		got, err := ws.Get(context.Background(), "erin", "", "", path)
		if err != nil {
			t.Fatalf("expected flushed file %q in store: %v", path, err)
		}
		data, _ := io.ReadAll(got)
		got.Close()
		if string(data) != string(want) {
			t.Fatalf("flushed %q content mismatch: got %q want %q", path, data, want)
		}
	}
}

// TestLifecycle_CloseAll stops the sweeper and tears down everything.
// Important on pod shutdown so we don't leak billable sandboxes.
func TestLifecycle_CloseAll(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, time.Second, 50*time.Millisecond)
	lp.Start()

	ex, _ := lp.Get(context.Background(), "carol", "", "")
	ex.Exec(context.Background(), "echo", time.Second)

	lp.CloseAll()

	if got := atomic.LoadInt32(&inner.closedAll); got != 1 {
		t.Fatalf("expected inner CloseAll to be called once; got %d", got)
	}

	// Safe to call twice.
	lp.CloseAll()
}

// PutIfVersion makes this test double satisfy workspace.Store after the port
// gained the conditional write (B2). The doubles model "no versioning": they
// delegate, which is exactly what a backend declaring no Version does.
func (f *fakeWorkspace) PutIfVersion(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string, expected workspace.Version) error {
	return f.Put(ctx, agentID, projectID, sessionID, path, r, size, contentType)
}
