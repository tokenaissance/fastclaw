package sandbox

// ① of the ctx-death pair (§13.3 row 85): the post-exec sync runs DETACHED from the turn, so a cut
// turn cannot take the delivery of its own work with it.
//
// The window this closes: the sync used to share the tool call's ctx, so a turn cut by its budget —
// or superseded, or abandoned by its caller — killed the snapshot with it. The sandbox's changes were
// then stranded until the NEXT exec's post-exec sync or the eviction flush, and the next turn's first
// action is usually a read: it would see the store's older copy. The eviction flush has always run on
// a background ctx (flushIfSupported), so this is the same class of write, just earlier.
//
// Falsification: pass the turn's ctx to syncSnapshot again ⇒ this witness reddens (the store never
// receives the sandbox's file), which is exactly what the production window measured.

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestThePostExecSyncOutlivesTheTurnThatProducedIt(t *testing.T) {
	lp, ws, pool := syncFixture(t, "stored", "stored")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// What the command left behind, and the moment the turn dies: the command returns and the turn's
	// ctx ends in the same breath (a budget cut, a supersede, a caller that left).
	pool.current.files["late.md"] = []byte("written while the command ran")
	pool.current.onExec = cancel

	ex, err := lp.Get(context.Background(), "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := ex.Exec(ctx, "python build.py", 30*time.Second); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the fixture did not cut the turn; this witness would be vacuous")
	}

	// The sync had to run anyway: the sandbox's file is in the store before anybody reads it again.
	rc, err := ws.Get(context.Background(), "erin", "", "", "late.md")
	if err != nil {
		t.Fatalf("the post-exec sync did not deliver the sandbox's change after the turn was cut: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "written while the command ran" {
		t.Fatalf("the store holds %q, want the sandbox's bytes", data)
	}
}

// The other half of the split: the signal read stays on the TURN's ctx, because it delivers to a
// reader. A turn that is already over has no reader, and the note must keep for the next one
// (row 81's guard) — this pins that the detach above did not sweep it along.
func TestTheSignalReadStillBelongsToTheTurn(t *testing.T) {
	carrier := newDurableSignals()
	lp, _, pool := syncFixture(t, "stored", "stored")
	lp.SetSignalStore(carrier)
	if err := carrier.AppendSignal(context.Background(), "erin", "", "", movedLine([]string{"while_away.md"})); err != nil {
		t.Fatalf("park: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool.current.onExec = cancel

	ex, err := lp.Get(context.Background(), "erin", "", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	out, err := ex.Exec(ctx, "python build.py", 30*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.Contains(out, "while_away.md") {
		t.Fatalf("a turn that was already over still took the parked signal:\n%q", out)
	}
	if carrier.parked() != 1 {
		t.Fatal("the note did not survive for the next turn")
	}
}
