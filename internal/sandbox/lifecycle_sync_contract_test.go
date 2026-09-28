package sandbox

// The reconcile contract, in its memory-free form (docs/文件系统形式化证明/07 §3.3).
//
// The baseline table is gone. It was pod-local state deciding the fate of a
// sandbox that is shared across replicas: the same sandbox would be judged
// differently by whichever pod handled the turn, and a restart would silently
// change the rules. What replaced it is what the two copies already carry —
// the store object's size/write time and the sandbox file's size/mtime — plus a
// byte comparison when those disagree.
//
// The tests below pin the four outcomes that matter:
//
//	store has no such path                → push (sandbox artefact)
//	same size + same write time           → skip (the mirror stamps this)
//	bytes equal                           → skip
//	bytes differ                          → BLOCK and report; the store keeps
//	                                        its copy (the incident's rule)

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// countingWorkspace counts writes (zero-migration is only checkable by
// counting) and stamps ModTime on every object, because the reconcile compares
// the store's write time against the sandbox file's.
type countingWorkspace struct {
	*fakeWorkspace
	puts int
	mods map[string]time.Time
	tick time.Duration
	// conflictOnce makes the NEXT PutIfVersion answer as a backend that found the
	// version changed under it. That is the shape the reconcile's belt must
	// survive: a verdict about a version that somebody replaced in between.
	conflictOnce bool
}

func newCountingWorkspace() *countingWorkspace {
	return &countingWorkspace{fakeWorkspace: newFakeWorkspace(), mods: map[string]time.Time{}}
}

func (c *countingWorkspace) Put(ctx context.Context, agentID, projectID, sessionID, p string, r io.Reader, size int64, contentType string) error {
	c.puts++
	c.tick += time.Second
	c.mods[wsScopeKey(agentID, scopeForKey(projectID, sessionID))+"/"+p] = time.Unix(1700000000, 0).Add(c.tick)
	return c.fakeWorkspace.Put(ctx, agentID, projectID, sessionID, p, r, size, contentType)
}

func (c *countingWorkspace) Stat(ctx context.Context, agentID, projectID, sessionID, p string) (*workspace.ObjectInfo, error) {
	info, err := c.fakeWorkspace.Stat(ctx, agentID, projectID, sessionID, p)
	if err != nil {
		return nil, err
	}
	if m, ok := c.mods[wsScopeKey(agentID, scopeForKey(projectID, sessionID))+"/"+p]; ok {
		info.ModTime = m
	}
	return info, nil
}

func (c *countingWorkspace) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	objs, err := c.fakeWorkspace.List(ctx, agentID, projectID, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range objs {
		if m, ok := c.mods[wsScopeKey(agentID, scopeForKey(projectID, sessionID))+"/"+objs[i].Path]; ok {
			objs[i].ModTime = m
		}
	}
	return objs, nil
}

func (c *countingWorkspace) keys(agentID, projectID, sessionID string) map[string]bool {
	out := map[string]bool{}
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))] {
		out[p] = true
	}
	return out
}

// putAt writes one object with an explicit store write time. The counter-based
// Put cannot place the store's write on the same timeline as the sandbox's clock,
// and the ordering rule (row 87) is precisely about two writes on one timeline.
func (c *countingWorkspace) putAt(ctx context.Context, agentID, projectID, sessionID, p, body string, at time.Time) error {
	c.mods[wsScopeKey(agentID, scopeForKey(projectID, sessionID))+"/"+p] = at
	return c.fakeWorkspace.Put(ctx, agentID, projectID, sessionID, p, strings.NewReader(body), int64(len(body)), "")
}

// syncFixture builds the production shape: a store object and a sandbox
// snapshot. `storeBytes` and `sandboxBytes` are where each side is NOW.
//
// The ORDER inside this fixture is load-bearing. It used to seed the store with
// `storeBytes`, set the sandbox to `sandboxBytes`, and only then hydrate — but a
// hydrate copies the store INTO the sandbox, which erased the divergence before
// the reconcile ever saw it. Every "the store's version survives" assertion
// below therefore passed even against the pre-repair rule ("same size ⇒ skip,
// otherwise overwrite"): there was nothing left to overwrite with. The fixture
// now hands both sides the SAME bytes (the hand-off), hydrates, and only then
// applies the divergence — which is the order production has.
func syncFixture(t *testing.T, storeBytes, sandboxBytes string) (*LifecyclePool, *countingWorkspace, *snappingPool) {
	t.Helper()
	const handed = "WHAT THE SANDBOX WAS HANDED AT BIRTH"
	ws := newCountingWorkspace()
	ctx := context.Background()
	if err := ws.Put(ctx, "erin", "", "", "report.html", strings.NewReader(handed), int64(len(handed)), ""); err != nil {
		t.Fatalf("seed store (the hand-off): %v", err)
	}
	pool := newSnappingPool(map[string][]byte{"report.html": []byte(handed)})
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(ws)
	// Birth: the instance is created and hydrated from the store.
	if _, err := lp.getInner(ctx, sandboxScope{agentID: "erin"}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Now move each side to where the test says it is: the sandbox first (a
	// script editing in place), then the store (a file tool writing), so both
	// divergences are post-hand-off and survive to the reconcile.
	if sandboxBytes != handed {
		pool.current.files["report.html"] = []byte(sandboxBytes)
	}
	if storeBytes != handed {
		hostWrite(t, ws, storeBytes)
	}
	return lp, ws, pool
}

func hostWrite(t *testing.T, ws *countingWorkspace, body string) {
	t.Helper()
	if err := ws.Put(context.Background(), "erin", "", "", "report.html", strings.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatalf("host write: %v", err)
	}
}

func reconcile(lp *LifecyclePool, pool *snappingPool) {
	ex, err := pool.Get(context.Background(), "erin", "", "")
	if err != nil {
		panic(err)
	}
	lp.syncSnapshot(context.Background(), sandboxScope{agentID: "erin"}, ex, "test")
}

func storeBody(t *testing.T, ws *countingWorkspace, path string) string {
	t.Helper()
	rc, err := ws.Get(context.Background(), "erin", "", "", path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// The incident, as a regression test: the store moved, the sandbox did not.
// The reconcile must keep the store's copy — this is the rule that cannot be
// traded away for convenience.
// A store-owned path is delivered INTO the sandbox and never collected back: the identity files
// (SOUL/IDENTITY/MEMORY/USER/…, `tools.IsIdentityFile`) have exactly one writer — the store — and
// the sandbox holds a readable copy of it.
//
// Production, 2026-09-26/28: `MEMORY.md` was refused **268 times** in one session. The sandbox held
// what it had been delivered, the store held what its own writer had produced, and the reconcile
// compared the two and refused — the right answer when both sides are writers; here only one is.
//
// Falsification: drop the `storeOwned` guard in `syncSnapshot` and the refused path reappears in
// `delta.blocked` (this test names it); the store's copy is untouched either way, which is exactly
// why the assertion has to be about the refusal and not about the bytes.
func TestSyncContract_StoreOwnedPathsAreNeverCollected(t *testing.T) {
	const born = "MEMORY AS DELIVERED BY THE STORE"
	const scratch = "MEMORY EDITED INSIDE THE SANDBOX"
	ws := newCountingWorkspace()
	ctx := context.Background()
	seed := func(path, body string) {
		t.Helper()
		if err := ws.Put(ctx, "erin", "", "", path, strings.NewReader(body), int64(len(body)), ""); err != nil {
			t.Fatalf("seed store %s: %v", path, err)
		}
	}
	seed("MEMORY.md", born)
	seed("notes.md", "store notes")

	pool := newSnappingPool(map[string][]byte{
		"MEMORY.md": []byte(born),
		"notes.md":  []byte("store notes"),
	})
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(ws)
	lp.SetStoreOwnedPaths(func(path string) bool { return path == "MEMORY.md" })
	if _, err := lp.getInner(ctx, sandboxScope{agentID: "erin"}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// The sandbox edits both files. Only the ordinary one may come back.
	pool.current.files["MEMORY.md"] = []byte(scratch)
	pool.current.files["notes.md"] = []byte("sandbox notes")

	ex, err := pool.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	d := lp.syncSnapshot(ctx, sandboxScope{agentID: "erin"}, ex, "test")

	// The owned path is silent; the ordinary one is still refereed — that difference IS the fix.
	for _, blocked := range d.blocked {
		if blocked == "MEMORY.md" {
			t.Fatalf("a store-owned path was refereed as a conflict; blocked=%v", d.blocked)
		}
	}
	var sawControl bool
	for _, blocked := range d.blocked {
		if blocked == "notes.md" {
			sawControl = true
		}
	}
	if !sawControl {
		t.Fatalf("an ordinary sandbox edit stopped being reported; blocked=%v", d.blocked)
	}
	if got := storeBody(t, ws, "MEMORY.md"); got != born {
		t.Fatalf("a store-owned path was collected back\n  store: %q\n  want:  %q", got, born)
	}
}

func TestSyncContract_StoreEditIsNotOverwritten(t *testing.T) {
	const born = "OLD SNAPSHOT VERSION"
	const host = "THE HOST WROTE THIS LONGER VERSION"
	lp, ws, pool := syncFixture(t, host, born) // store ahead, sandbox older
	before := ws.puts

	reconcile(lp, pool)

	if got := storeBody(t, ws, "report.html"); got != host {
		t.Fatalf("the sandbox copy overwrote the host's version\n  store: %q\n  want:  %q", got, host)
	}
	if ws.puts != before {
		t.Fatalf("a blocked path was still written (%d writes)", ws.puts-before)
	}
}

// The same, through the trigger production actually uses: an exec after the
// host write. Repeated reconciles must not drift toward the sandbox's copy.
func TestSyncContract_StoreEditSurvivesPostExecTrigger(t *testing.T) {
	lp, ws, pool := syncFixture(t, "HOST VERSION", "OLD")
	reconcile(lp, pool)
	reconcile(lp, pool)
	if got := storeBody(t, ws, "report.html"); got != "HOST VERSION" {
		t.Fatalf("a follow-up reconcile reverted the host version: %q", got)
	}
}

// The verdict must be a function of the TWO COPIES, not of who asks.
//
// A sandbox lease is adopted across replicas, so a pod that attached to an
// instance WITHOUT having hydrated it must reach the same verdict as the pod
// that created it. The removed baseline could not promise that — it lived in
// one process, so the same state was judged differently depending on which pod
// handled the turn (docs 07 §3.11.3; change register #2).
//
// The shape of the test matters: the two pools must face the SAME state
// independently, so each gets its own world (store + sandbox). Running them in
// sequence over one world would compare a state against one the first run had
// already changed.
func TestSyncVerdictDoesNotDependOnThePoolThatMakesIt(t *testing.T) {
	const (
		born = "WHAT THE SANDBOX WAS HANDED"
		host = "THE HOST'S NEWER, LONGER VERSION"
		edit = " AND THEN THE SANDBOX EDITED IT"
	)

	// ① the incident's shape: the sandbox was born holding `born`, then the host
	// wrote `host` to the store only. Whoever asks, the store keeps its version.
	t.Run("store ahead of the sandbox", func(t *testing.T) {
		a := newSyncWorld(t, born, true)
		b := newSyncWorld(t, born, false)
		a.hostWrite(t, host)
		b.hostWrite(t, host)
		d := compareVerdicts(t, a, b)
		if len(d.moved) != 0 {
			t.Fatalf("the sandbox copy was taken: moved=%v", d.moved)
		}
		for name, w := range map[string]*syncWorld{"hydrated": a, "adopted": b} {
			if got := storeBodyFrom(t, w, "report.html"); got != host {
				t.Fatalf("[%s] the store lost the host's version: %q", name, got)
			}
		}
	})

	// ② a sandbox-born artefact: both must take it, and land it in the same place.
	t.Run("sandbox-born artefact", func(t *testing.T) {
		a := newSyncWorld(t, born, true)
		b := newSyncWorld(t, born, false)
		a.sandboxWrite("artifact.png", "png bytes")
		b.sandboxWrite("artifact.png", "png bytes")
		d := compareVerdicts(t, a, b)
		if len(d.moved) != 1 || d.moved[0] != "artifact.png" {
			t.Fatalf("moved = %v; want [artifact.png]", d.moved)
		}
		for name, w := range map[string]*syncWorld{"hydrated": a, "adopted": b} {
			if got := storeBodyFrom(t, w, "artifact.png"); got != "png bytes" {
				t.Fatalf("[%s] the artefact did not reach the store: %q", name, got)
			}
		}
	})

	// ③ the sandbox edited a path the store still has, and the store has not
	// moved. Until the ordering rule (row 87) this was refused: with no record of
	// what the sandbox had been handed, the two writers were indistinguishable.
	// The bytes settle it now — the store's copy is a strict prefix of the
	// sandbox's, so the sandbox's is the descendant — and what this subtest still
	// pins is unchanged: BOTH pools must reach that verdict for the same reason
	// they both refused before. A verdict that came from pod-local memory would
	// still split the two worlds here (the adopted one would refuse).
	//
	// (This fixture models no guest clock, so the containment instrument speaks;
	// the clock's own cases are in lifecycle_sync_order_test.go.)
	t.Run("sandbox edit with an unchanged store", func(t *testing.T) {
		a := newSyncWorld(t, born, true)
		b := newSyncWorld(t, born, false)
		a.sandboxWrite("report.html", born+edit)
		b.sandboxWrite("report.html", born+edit)
		d := compareVerdicts(t, a, b)
		if len(d.moved) != 1 || d.moved[0] != "report.html" {
			t.Fatalf("the sandbox's newer copy was not taken by both pools: moved=%v", d.moved)
		}
		for name, w := range map[string]*syncWorld{"hydrated": a, "adopted": b} {
			if got := storeBodyFrom(t, w, "report.html"); got != born+edit {
				t.Fatalf("[%s] the store did not receive the sandbox's copy: %q", name, got)
			}
		}
	})
}

// syncWorld is one store + one sandbox + one pool. `hydrated` distinguishes the
// pod that created the instance from a peer that attached to it.
type syncWorld struct {
	ws   *countingWorkspace
	pool *snappingPool
	lp   *LifecyclePool
	ex   Executor
}

// newSyncWorld seeds the store and the sandbox with the same bytes — the
// hand-off — and, for the hydrated pool, performs the hydrate. Divergence is
// applied by the caller afterwards, which is the order production uses.
func newSyncWorld(t *testing.T, born string, hydrated bool) *syncWorld {
	t.Helper()
	ctx := context.Background()
	ws := newCountingWorkspace()
	if err := ws.Put(ctx, "erin", "", "", "report.html", strings.NewReader(born), int64(len(born)), ""); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	pool := newSnappingPool(map[string][]byte{"report.html": []byte(born)})
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(ws)

	sc := sandboxScope{agentID: "erin"}
	if hydrated {
		// This pool created the instance, so it is the one that hydrated it.
		if _, err := lp.getInner(ctx, sc); err != nil {
			t.Fatalf("hydrate: %v", err)
		}
	} else {
		// The peer attaches to the instance the other pod created; no hydrate.
		if _, err := pool.Get(ctx, "erin", "", ""); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}
	ex, err := pool.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("sandbox up: %v", err)
	}
	return &syncWorld{ws: ws, pool: pool, lp: lp, ex: ex}
}

// hostWrite is a file tool writing the store only (the sandbox is untouched).
func (w *syncWorld) hostWrite(t *testing.T, body string) {
	t.Helper()
	if err := w.ws.Put(context.Background(), "erin", "", "", "report.html",
		strings.NewReader(body), int64(len(body)), ""); err != nil {
		t.Fatalf("host write: %v", err)
	}
}

// sandboxWrite is the sandbox writing a path itself — a script, not a tool.
func (w *syncWorld) sandboxWrite(path, body string) {
	w.pool.current.files[path] = []byte(body)
}

// compareVerdicts reconciles the same state in both worlds and fails if they
// disagree — a pod-local judgement cannot hide behind an average.
func compareVerdicts(t *testing.T, a, b *syncWorld) delta {
	t.Helper()
	ctx := context.Background()
	sc := sandboxScope{agentID: "erin"}
	da := a.lp.syncSnapshot(ctx, sc, a.ex, "world-hydrated")
	db := b.lp.syncSnapshot(ctx, sc, b.ex, "world-adopted")
	if fmt.Sprint(da) != fmt.Sprint(db) {
		t.Fatalf("two pools reached different verdicts on the same state\n"+
			"  pool that hydrated it: %s\n  pool that adopted it:   %s\n"+
			"  a verdict that depends on the asker is not a verdict (docs 07 §3.11.3)",
			fmt.Sprint(da), fmt.Sprint(db))
	}
	return da
}

func storeBodyFrom(t *testing.T, w *syncWorld, path string) string {
	t.Helper()
	rc, err := w.ws.Get(context.Background(), "erin", "", "", path)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// The branch that makes exec useful: a path the store never had is a sandbox
// artefact and must be taken.
func TestSyncContract_NewPathIsFlushed(t *testing.T) {
	lp, ws, pool := syncFixture(t, "host", "host")
	pool.current.files["artifact.png"] = []byte("png bytes")

	reconcile(lp, pool)

	if got := storeBody(t, ws, "artifact.png"); got != "png bytes" {
		t.Fatalf("sandbox artefact not flushed: %q", got)
	}
}

// A sandbox edit of a path the store ALSO has is refused and reported. This is
// the trade-off of dropping the baseline: without a record of "what the sandbox
// was handed", the two writers are indistinguishable, so the reconcile refuses
// to choose and the agent settles it (it is told — see the exec report tests).
func TestSyncContract_SandboxEditOfStorePathIsRefused(t *testing.T) {
	const stored = "THE STORE'S VERSION"
	const edited = "A DIFFERENT VERSION THE SANDBOX WROTE"
	lp, ws, pool := syncFixture(t, stored, edited)
	before := ws.puts

	reconcile(lp, pool)

	if got := storeBody(t, ws, "report.html"); got != stored {
		t.Fatalf("the sandbox's version was taken without being asked for: %q", got)
	}
	if ws.puts != before {
		t.Fatalf("a refused path was written anyway")
	}
}

// Convergence: reconciling twice over an unchanged pair writes nothing the
// second time.
func TestSyncContract_SecondReconcileWritesNothing(t *testing.T) {
	lp, ws, pool := syncFixture(t, "same", "same")
	reconcile(lp, pool)
	before := ws.puts
	reconcile(lp, pool)
	if ws.puts != before {
		t.Fatalf("second reconcile wrote %d object(s)", ws.puts-before)
	}
}

// Locality: one reconcile never adds or removes keys.
func TestSyncContract_DomainUnchanged(t *testing.T) {
	lp, ws, pool := syncFixture(t, "host", "sandbox")
	before := ws.keys("erin", "", "")
	reconcile(lp, pool)
	after := ws.keys("erin", "", "")
	if len(before) != len(after) {
		t.Fatalf("domain changed: %d keys before, %d after", len(before), len(after))
	}
}

// A store-only key (an upload that never reached the sandbox) is untouched.
func TestSyncContract_StoreOnlyKeySurvives(t *testing.T) {
	lp, ws, pool := syncFixture(t, "host", "host")
	if err := ws.Put(context.Background(), "erin", "", "", "attachment.md",
		strings.NewReader("uploaded by the user"), 19, ""); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	reconcile(lp, pool)
	if got := storeBody(t, ws, "attachment.md"); got != "uploaded by the user" {
		t.Fatalf("store-only key was touched: %q", got)
	}
}

// ── write-through ──────────────────────────────────────────────────────────

// The write-through's STAMP (docs 07 §3.11.3 step 3): the sandbox copy must be
// stamped with the STORE OBJECT's write time, not with the mirror's own clock —
// that equality is what lets the reconcile skip the bytes without remembering
// anything.
//
// Pinned on the command, not on a later read count, and that choice is itself a
// measurement: the sync's "same version" test allows ±1s, so when the store
// write and the mirror land in the same second (they do — a tool writes the
// store and mirrors immediately) the tolerance absorbs the mirror's own clock.
// On live E2B, REMOVING the stamp still produced 0 whole-object reads, i.e. the
// read-count test cannot tell a stamp from the clock. The command can.
func TestWriteThroughStampsTheSandboxCopyWithTheStoreTime(t *testing.T) {
	ctx := context.Background()
	ws := newCountingWorkspace()
	// countingWorkspace stamps objects from 1700000000 + 1s per write, so this
	// object's write time is far from "now" — a stamp taken from the mirror's
	// clock could never be mistaken for it.
	if err := ws.Put(ctx, "erin", "proj-1", "", "app/notes.md",
		strings.NewReader("old body\n"), -1, ""); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	pool := newSnappingPool(map[string][]byte{})
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(ws)

	// A project session: the sandbox INSTANCE keeps the chat, the STORE scope is
	// the project root (workspace.WriteScope) — the split that G22 got wrong.
	sc := sandboxScope{agentID: "erin", projectID: "proj-1", sessionID: "chat-9"}
	if _, err := lp.WriteThrough(ctx, sc, StoreScope{ProjectID: "proj-1"},
		"app/notes.md", "/workspace/app/notes.md", "new body\n", "old body\n"); err != nil {
		t.Fatalf("write-through: %v", err)
	}

	const want = "touch -d @1700000001 '/workspace/app/notes.md'"
	var found bool
	for _, cmd := range pool.current.execCommands() {
		if cmd == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("the mirror never stamped the sandbox copy with the store's write time.\n"+
			"  want a command: %s\n  got: %v\n"+
			"  (docs 07 §3.11.3 step 3; docs 10 §4 G22)", want, pool.current.execCommands())
	}
}

// The mirror reports a version it replaced — using the CALLER's expectation,
// not a stored table (edit_file knows what the file held; write_file does not).
func TestSyncContract_WriteThroughSignalsReplacedVersion(t *testing.T) {
	const born = "BORN"
	const previous = "WHAT THE TOOL READ"
	const sandboxEdit = "WHAT THE SANDBOX WROTE"
	lp, _, _ := syncFixture(t, born, born)
	sc := sandboxScope{agentID: "erin"}
	pool := newSnappingPool(map[string][]byte{"report.html": []byte(born)})
	lp2 := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp2.SetWorkspace(lp.workspace)
	if _, err := lp2.getInner(context.Background(), sc); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// Only now put the sandbox's own version in place: the hydrate above copies
	// the STORE into /workspace, so anything set before it would be wiped — the
	// same ordering trap the production flow has (a conflict is only possible
	// for a sandbox that existed before the write).
	pool.current.files["report.html"] = []byte(sandboxEdit)

	outcome, err := lp2.WriteThrough(context.Background(), sc, StoreScope{}, "report.html", "/workspace/report.html", "NEW STORE CONTENT", previous)
	if err != nil {
		t.Fatalf("write-through: %v", err)
	}
	if outcome.Comparison != CompareReplacedVerified {
		t.Fatalf("the mirror did not signal a verified replacement: %+v", outcome)
	}
	if outcome.SandboxBytes != int64(len(sandboxEdit)) {
		t.Fatalf("reported %d bytes, want %d", outcome.SandboxBytes, len(sandboxEdit))
	}
}

// Without an expectation from the caller there is nothing to compare the
// sandbox's bytes AGAINST — but the fact that something was replaced is still
// observable, and stating it must not smuggle in a comparison that never
// happened. (Before 2026-09-18 this shipped as "over 2 MiB" for every such
// write, including 5-byte files: docs 10 §2.1, G5.)
func TestSyncContract_WriteThroughWithoutExpectationClaimsNothing(t *testing.T) {
	lp, _, _ := syncFixture(t, "born", "born")
	sc := sandboxScope{agentID: "erin"}
	outcome, err := lp.WriteThrough(context.Background(), sc, StoreScope{}, "report.html", "/workspace/report.html", "NEW", "")
	if err != nil {
		t.Fatalf("write-through: %v", err)
	}
	if outcome.Comparison != CompareReplacedUnchecked {
		t.Fatalf("want an unchecked replacement (the sandbox held %d bytes and no expectation was given), got %+v",
			len("born"), outcome)
	}
	if outcome.SandboxBytes != int64(len("born")) {
		t.Fatalf("the replaced version's size is part of the fact: got %d", outcome.SandboxBytes)
	}
}

// sharedExecutor is the docker shape: identical behaviour, but /workspace IS the
// host directory, so it deliberately does not implement RemoteWorkspace.
type sharedExecutor struct{ inner Executor }

func (s sharedExecutor) Exec(ctx context.Context, cmd string, d time.Duration) (string, error) {
	return s.inner.Exec(ctx, cmd, d)
}
func (s sharedExecutor) ReadFile(ctx context.Context, p string) (string, error) {
	return s.inner.ReadFile(ctx, p)
}
func (s sharedExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	return s.inner.WriteFile(ctx, p, c)
}
func (s sharedExecutor) ListDir(ctx context.Context, p string) (string, error) {
	return s.inner.ListDir(ctx, p)
}
func (s sharedExecutor) Backend() string { return s.inner.Backend() }
func (s sharedExecutor) Close() error    { return s.inner.Close() }

type sharedPool struct{ inner ExecutorPool }

func (p sharedPool) Get(ctx context.Context, a, pr, s string) (Executor, error) {
	ex, err := p.inner.Get(ctx, a, pr, s)
	if err != nil {
		return nil, err
	}
	return sharedExecutor{inner: ex}, nil
}
func (p sharedPool) Release(a, pr, s string) error { return p.inner.Release(a, pr, s) }
func (p sharedPool) CloseAll()                     { p.inner.CloseAll() }
func (p sharedPool) Backend() string               { return p.inner.Backend() }

// The docker shape: there is no second copy, so there is nothing to compare and
// nothing that could have been replaced. The tool layer must stay silent; it
// used to announce "(over 2 MiB)" here (docs 10 §2.1, G5).
func TestSyncContract_WriteThroughOnSharedBackendIsSilent(t *testing.T) {
	lp, _, _ := syncFixture(t, "born", "born")
	lp.inner = sharedPool{inner: lp.inner}
	sc := sandboxScope{agentID: "erin"}
	outcome, err := lp.WriteThrough(context.Background(), sc, StoreScope{}, "report.html", "/workspace/report.html", "NEW", "born")
	if err != nil {
		t.Fatalf("write-through: %v", err)
	}
	if outcome.Comparison != CompareNoSecondCopy {
		t.Fatalf("a shared backend must report that there was nothing to mirror: %+v", outcome)
	}
}

var _ workspace.Store = (*countingWorkspace)(nil)

// PutIfVersion makes this test double satisfy workspace.Store after the port
// gained the conditional write (B2). The doubles model "no versioning": they
// delegate, which is exactly what a backend declaring no Version does — unless a
// test arms conflictOnce, which is how the reconcile's refusal path is witnessed.
func (f *countingWorkspace) PutIfVersion(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string, expected workspace.Version) error {
	if f.conflictOnce {
		f.conflictOnce = false
		return workspace.ErrVersionConflict
	}
	return f.Put(ctx, agentID, projectID, sessionID, path, r, size, contentType)
}
