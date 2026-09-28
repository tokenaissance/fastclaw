package sandbox

// The reconcile's ordering rule (change register row 87), witnessed.
//
// The rule answers one question: which of the two copies is the SUCCESSOR? Two
// instruments, neither of which remembers anything between calls:
//
//	the file's own mtime, translated by the guest-clock offset measured in the
//	  same exec as the listing — the general instrument, orders any pair, and a
//	  verdict must hold under BOTH readings of that number (a sandbox write, or
//	  the stamp we wrote earlier);
//	the bytes — a strict prefix is ancestry, exact and clock-free, and it speaks
//	  when the clock cannot.
//
// Only "no evidence at all" still refuses, and the refusal is what these tests
// keep measuring: `TestSyncContract_SandboxEditOfStorePathIsRefused` and the two
// byte-identity cases in the contract file pin the conservative default on
// fixtures that model no clock at all.
//
// The production shapes behind each witness are in docs/fs-formal-proof/11
// §13.4 (the 2026-09-26/27 scene: 446 refusals, 142 of them a growing log, 269 an
// identity file rewritten against a frozen store copy, 4 deliverable standoffs
// where the STORE was the newer side and the agent kept reading the old one).

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// guestSkew is the guest clock's offset from this pod's, as measured on the live
// runtime (2026-09-28: six samples, +1.0…+1.4 s, warm round trip ≈2 s —
// TestE2BLiveGuestClockOffset). The tests below use a skewed clock on purpose: a
// rule that only works when the two clocks agree is the rule this replaces.
const guestSkew = 1200 * time.Millisecond

// orderWorld is one store + one sandbox whose clock is MODELLED, so a test can
// state when each side wrote instead of inferring it.
type orderWorld struct {
	ws   *countingWorkspace
	pool *snappingPool
	lp   *LifecyclePool
	ex   Executor
	// born is the store's write time at the hand-off — the instant both copies
	// were known to be equal.
	born time.Time
}

// newOrderWorld seeds the store with `born` bytes, hands the same bytes to a
// sandbox whose file carries the STORE's write time (what hydrate does: the tar
// header's mtime), and sets the guest clock one `guestSkew` ahead of the pod's.
func newOrderWorld(t *testing.T, born string) *orderWorld {
	t.Helper()
	ctx := context.Background()
	ws := newCountingWorkspace()
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := ws.putAt(ctx, "erin", "", "", "report.md", born, at); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	files := map[string][]byte{"report.md": []byte(born)}
	pool := newSnappingPool(files)
	pool.current.mtimes = map[string]time.Time{"report.md": at}
	// The guest's clock runs one skew ahead of the pod's NOW (not of the hand-off):
	// it is a clock, and the sandbox's writes are read against it.
	pool.current.clock = time.Now().Add(guestSkew)
	lp := NewLifecyclePool(pool, time.Hour, time.Hour)
	lp.SetWorkspace(ws)
	if _, err := lp.getInner(ctx, sandboxScope{agentID: "erin"}); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ex, err := pool.Get(ctx, "erin", "", "")
	if err != nil {
		t.Fatalf("sandbox up: %v", err)
	}
	return &orderWorld{ws: ws, pool: pool, lp: lp, ex: ex, born: at}
}

// guest is this pod-time expressed on the modelled guest clock.
func (w *orderWorld) guest(podTime time.Time) time.Time { return podTime.Add(guestSkew) }

// sandboxWrote is a script editing /workspace directly: the bytes change and the
// file's mtime becomes the GUEST clock's reading of that instant.
func (w *orderWorld) sandboxWrote(path, body string, when time.Time) {
	w.pool.current.files[path] = []byte(body)
	w.pool.current.mtimes[path] = w.guest(when)
}

// hostWrote is any store-side writer that is not this sandbox: a file tool on
// another pod, the panel, an upload. Only the store's copy moves, at `when`.
func (w *orderWorld) hostWrote(t *testing.T, path, body string, when time.Time) {
	t.Helper()
	if err := w.ws.putAt(context.Background(), "erin", "", "", path, body, when); err != nil {
		t.Fatalf("host write: %v", err)
	}
}

func (w *orderWorld) reconcile(t *testing.T) delta {
	t.Helper()
	return w.lp.syncSnapshot(context.Background(), sandboxScope{agentID: "erin"}, w.ex, "order-test")
}

func (w *orderWorld) sandboxBody(path string) string {
	return string(w.pool.current.files[path])
}

func (w *orderWorld) storeBody(t *testing.T, path string) string {
	t.Helper()
	rc, err := w.ws.Get(context.Background(), "erin", "", "", path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// ── the sandbox is the successor ───────────────────────────────────────────

// The log family (§13.4): the sandbox keeps writing a path the store also has,
// and the store has not been written by anyone else. The sandbox's copy is the
// successor, so it is collected — which is what the sync exists to do.
//
// The bytes here deliberately do NOT nest (a rewrite, not an append), so this
// witness rests on the clock instrument alone: `_p11.log` was the append-shaped
// member of the family, `MEMORY.md` the rewritten one.
//
// Falsification: drop the clock instrument (`orderByClock` → orderUnknown) and
// the sandbox's copy stops being taken — the refusal comes back.
func TestSyncOrder_TakesASandboxRewriteTheStoreNeverFollowed(t *testing.T) {
	w := newOrderWorld(t, "STORE COPY: THE DELIVERED ONE\n")
	const rewritten = "COMPLETELY DIFFERENT BODY WRITTEN INSIDE THE SANDBOX\n"
	w.sandboxWrote("report.md", rewritten, w.born.Add(30*time.Minute))

	d := w.reconcile(t)

	if len(d.moved) != 1 || d.moved[0] != "report.md" {
		t.Fatalf("the sandbox's newer copy was not collected: moved=%v blocked=%v refreshed=%v",
			d.moved, d.blocked, d.refreshed)
	}
	if got := w.storeBody(t, "report.md"); got != rewritten {
		t.Fatalf("the store did not receive the sandbox's version: %q", got)
	}
}

// ── the store is the successor ─────────────────────────────────────────────

// The deliverable family (§13.4, S3's paper files): the STORE's copy is newer,
// the sandbox still holds the copy it was handed, and those bytes do not nest.
// The store's copy is delivered INTO the sandbox — the direction the reconcile
// never had, and the one whose absence cost the run `KeyError: 'current_pnl'`
// against a stale deliverable.
//
// Falsification: drop the delivery branch (`orderStoreIsNewer` → refuse) and the
// sandbox keeps the old bytes — the production shape, verbatim.
func TestSyncOrder_DeliversTheStoresNewerCopyIntoTheSandbox(t *testing.T) {
	w := newOrderWorld(t, "THE COPY THE SANDBOX WAS HANDED\n")
	const newer = "THE STORE'S NEWER BODY, WRITTEN BY A TOOL ELSEWHERE\n"
	newerAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	w.hostWrote(t, "report.md", newer, newerAt)

	d := w.reconcile(t)

	if len(d.refreshed) != 1 || d.refreshed[0] != "report.md" {
		t.Fatalf("the store's newer copy was not delivered: moved=%v blocked=%v refreshed=%v",
			d.moved, d.blocked, d.refreshed)
	}
	if got := w.sandboxBody("report.md"); got != newer {
		t.Fatalf("the sandbox did not receive the store's version: %q", got)
	}
	// Delivered AND stamped with the store's write time, like the mirror's copy:
	// that stamp is what makes the next sync free.
	if got := w.pool.current.mtimes["report.md"]; !got.Equal(newerAt) {
		t.Fatalf("the delivered copy carries mtime %v; want the store's write time %v", got, newerAt)
	}
	var stamped bool
	for _, cmd := range w.pool.current.execCommands() {
		if strings.HasPrefix(cmd, "touch -d @") && strings.Contains(cmd, "/workspace/report.md") {
			stamped = true
		}
	}
	if !stamped {
		t.Fatalf("the delivered copy was not stamped with the store's write time: %v",
			w.pool.current.execCommands())
	}
}

// Convergence, and the reason the rule exists: after a delivery the pair is
// equal, so the next reconcile has nothing to say. The refusal this replaces said
// the same thing every turn, forever (§13.4: 446 lines, the oldest path refused
// for 10+ hours).
func TestSyncOrder_TheDeliveryEndsTheStandoff(t *testing.T) {
	w := newOrderWorld(t, "OLD\n")
	w.hostWrote(t, "report.md", "NEWER AND LONGER, SO NOT A PREFIX EITHER WAY\n",
		time.Now().Add(-10*time.Minute).Truncate(time.Second))
	if d := w.reconcile(t); len(d.refreshed) != 1 {
		t.Fatalf("first reconcile: refreshed=%v blocked=%v", d.refreshed, d.blocked)
	}

	second := w.reconcile(t)

	if second.changed() {
		t.Fatalf("the standoff was not over after the delivery: %+v", second)
	}
}

// ── the clock-free instrument, and the ambiguity it is kept for ────────────

// With no readable clock — the sandbox answered no stat pass, or its guest clock
// never arrived — nesting still settles a pair. That is the fallback's whole job,
// and it is worth keeping because it needs no clock at all: for anything that
// grows forward, the longer copy IS the descendant, in both directions.
//
// Falsification: drop `orderByContainment` and both halves of this test refuse.
func TestSyncOrder_NestingDecidesWhenTheClockCannot(t *testing.T) {
	t.Run("sandbox appended", func(t *testing.T) {
		w := newOrderWorld(t, "line one\n")
		w.pool.current.mtimes = nil // the stat pass is unavailable
		w.pool.current.files["report.md"] = []byte("line one\nline two\n")

		d := w.reconcile(t)

		if len(d.moved) != 1 {
			t.Fatalf("a nesting sandbox copy was not collected: %+v", d)
		}
		if got := w.storeBody(t, "report.md"); got != "line one\nline two\n" {
			t.Fatalf("store = %q", got)
		}
	})
	t.Run("store appended", func(t *testing.T) {
		w := newOrderWorld(t, "line one\nline two\n")
		w.pool.current.mtimes = nil
		w.pool.current.files["report.md"] = []byte("line one\n")

		d := w.reconcile(t)

		if len(d.refreshed) != 1 {
			t.Fatalf("a nesting store copy was not delivered: %+v", d)
		}
		if got := w.sandboxBody("report.md"); got != "line one\nline two\n" {
			t.Fatalf("sandbox = %q", got)
		}
	})
}

// When the two instruments disagree, the pair is refused — and there is a real
// shape behind each direction of disagreement.
//
// Here: whoever writes the store TRUNCATED the file (so the store's copy is
// still a prefix of the sandbox's, which reads as "the sandbox is ahead") while
// the store's write is the LATER one (which reads as "the store is ahead"). The
// bytes cannot see the truncation; the clock cannot see the ancestry. Neither is
// wrong about its own fact and nothing on hand reconciles them, so the honest
// answer is the one the whole design starts from: refuse, and say which path.
//
// Falsification: let either instrument win alone, and this test reddens in the
// direction that instrument is blind to — the store's truncation is overwritten
// (bytes win) or the sandbox's appended copy is (clock wins).
func TestSyncOrder_TheTwoInstrumentsMustAgree(t *testing.T) {
	w := newOrderWorld(t, "line one\nline two\n")
	const appended = "line one\nline two\nline three\n"
	w.sandboxWrote("report.md", appended, w.born.Add(5*time.Minute))
	w.hostWrote(t, "report.md", "line one\n", w.born.Add(10*time.Minute)) // a truncation, and later
	before := w.ws.puts

	d := w.reconcile(t)

	if len(d.blocked) != 1 || d.blocked[0] != "report.md" {
		t.Fatalf("an instrument was allowed to decide alone: %+v", d)
	}
	if len(d.moved) != 0 || len(d.refreshed) != 0 {
		t.Fatalf("a verdict was formed from disagreeing instruments: %+v", d)
	}
	if got := w.storeBody(t, "report.md"); got != "line one\n" {
		t.Fatalf("the store's truncated copy was overwritten: %q", got)
	}
	if got := w.sandboxBody("report.md"); got != appended {
		t.Fatalf("the sandbox's appended copy was replaced: %q", got)
	}
	if w.ws.puts != before {
		t.Fatalf("the disagreement was resolved by writing: %d writes", w.ws.puts-before)
	}
}

// The two readings of one number, and why the rule needs both.
//
// The sandbox file's mtime is either a sandbox write (a GUEST clock reading, which
// needs the offset subtracted) or the stamp the hydrate wrote (a STORE clock
// reading, which needs nothing). When the guest clock runs an hour BEHIND, reading
// a stamp as a write translates it an hour into the future — and the single-reading
// rule would take the sandbox's stale copy over the store's newer one, silently.
// Requiring the verdict to hold under both readings turns that into a refusal.
//
// Falsification: keep only the write reading (`asWrite`), and this test reddens —
// the store's newer version is overwritten by the copy the sandbox was handed.
func TestSyncOrder_AVerdictMustSurviveBothReadingsOfTheMtime(t *testing.T) {
	w := newOrderWorld(t, "THE COPY THE SANDBOX WAS HANDED\n")
	const newer = "THE STORE'S NEWER VERSION\n"
	w.hostWrote(t, "report.md", newer, w.born.Add(10*time.Minute))
	// The guest's clock runs an hour behind, so a stamp read as a write lands an
	// hour in the future: `born + 1h` looks newer than the store's write.
	w.pool.current.clock = w.pool.current.mtimes["report.md"].Add(-time.Hour)
	before := w.ws.puts

	d := w.reconcile(t)

	if got := w.storeBody(t, "report.md"); got != newer {
		t.Fatalf("the store's newer version was overwritten: %q", got)
	}
	if len(d.moved) != 0 {
		t.Fatalf("a verdict that does not survive both readings was acted on: moved=%v", d.moved)
	}
	if len(d.blocked) != 1 || d.blocked[0] != "report.md" {
		t.Fatalf("the ambiguous pair was not refused and reported: %+v", d)
	}
	if w.ws.puts != before {
		t.Fatalf("the ambiguity was resolved by writing: %d writes", w.ws.puts-before)
	}
}

// Two writes inside `versionSlack` are not orderable, and the rule says so rather
// than guessing — which keeps the shape every pair had before row 87 for exactly
// the cases where no instrument can tell them apart.
func TestSyncOrder_PairsCloserThanTheSlackStayRefused(t *testing.T) {
	w := newOrderWorld(t, "THE COPY THE SANDBOX WAS HANDED\n")
	const newer = "A DIFFERENT BODY ALTOGETHER\n"
	// The store moved one second ago; the sandbox holds the stamped copy, so the
	// gap between the two writes is inside the slack.
	w.hostWrote(t, "report.md", newer, w.born.Add(time.Second))

	d := w.reconcile(t)

	if len(d.blocked) != 1 || d.blocked[0] != "report.md" {
		t.Fatalf("a pair inside the slack was not refused: %+v", d)
	}
	if len(d.moved) != 0 || len(d.refreshed) != 0 {
		t.Fatalf("a pair inside the slack was acted on: %+v", d)
	}
}

// ── the belt on the write the verdict authorizes ───────────────────────────

// The push is conditional on the version that was JUDGED. If the store moved
// between the comparison and the write, the verdict was about a version that no
// longer exists, and the honest answer is to refuse and decide again on the new
// pair — the same rule as G24's, applied to the reconcile's own write.
//
// Falsification: push unconditionally (as before row 87) and the store's newer
// version is destroyed by a verdict formed against the older one.
func TestSyncOrder_APushRefusesWhenTheStoreMovedUnderTheVerdict(t *testing.T) {
	w := newOrderWorld(t, "THE COPY THE SANDBOX WAS HANDED\n")
	const sandboxVersion = "THE SANDBOX'S OWN, NEWER BODY\n"
	w.sandboxWrote("report.md", sandboxVersion, w.born.Add(time.Hour))
	w.ws.conflictOnce = true
	before := w.ws.puts

	d := w.reconcile(t)

	if len(d.moved) != 0 {
		t.Fatalf("a push landed on a store that had moved: %+v", d)
	}
	if len(d.blocked) != 1 || d.blocked[0] != "report.md" {
		t.Fatalf("the lost race was not reported as unsynced: %+v", d)
	}
	if w.ws.puts != before {
		t.Fatalf("the losing push was written anyway: %d writes", w.ws.puts-before)
	}
}
