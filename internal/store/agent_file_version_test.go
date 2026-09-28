package store

// The version precondition on agent_files (§13.2 of docs/fs-formal-proof/11-change-register.md).
//
// Five writers used to share one key with no shared precondition: the file tools,
// the memory distiller, the sandbox sync, the panel, the CLI. Every one of them is
// a read-modify-write, so the rule cannot live in a caller — it is one statement
// in the resource (obligation L4(a)/L7, docs/fs-formal-proof/12-lease-formal-design.md).
//
// Falsification for the whole file: drop the `WHERE agent_files.content = ?` from
// SaveAgentFileIfVersion and the stale writer's row wins — every "was refused" case
// below goes green on a lost update it cannot see.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// versionScope hands out a fresh (agent, user) pair and deletes its rows on
// cleanup. The key is the row, so a test that reused a literal agent id would
// leave a row behind on a long-lived Postgres and pass on the first run and fail
// on the second (measured: the create-only case did exactly that).
func versionScope(t *testing.T, db *DBStore) (agent, user string) {
	t.Helper()
	agent = fmt.Sprintf("agt_cas_%d", time.Now().UnixNano())
	user = "u_cas"
	t.Cleanup(func() {
		for _, name := range []string{"MEMORY.md", "USER.md"} {
			_ = db.DeleteAgentFile(context.Background(), agent, user, name)
		}
	})
	return agent, user
}

// versionDialects runs fn against sqlite always, and against a real Postgres when
// FASTAGENT_TEST_PG_DSN is set. The two dialects build the same precondition with
// different SQL, and the claim ("the resource evaluates it") is only worth
// anything on the dialect that has real concurrency.
func versionDialects(t *testing.T, fn func(t *testing.T, db *DBStore)) {
	t.Helper()
	sqlite, err := NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "agent_files.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	if err := sqlite.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })
	t.Run("sqlite", func(t *testing.T) { fn(t, sqlite) })

	dsn := os.Getenv("FASTAGENT_TEST_PG_DSN")
	if dsn == "" {
		t.Log("FASTAGENT_TEST_PG_DSN unset — the Postgres leg of this witness did not run")
		return
	}
	pg, err := NewDBStore("postgres", dsn)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	if err := pg.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate postgres: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	t.Run("postgres", func(t *testing.T) { fn(t, pg) })
}

func TestSaveAgentFileIfVersionRefusesTheLoserOfARace(t *testing.T) {
	versionDialects(t, func(t *testing.T, db *DBStore) {
		ctx := context.Background()
		agent, user := versionScope(t, db)
		const name = "MEMORY.md"
		if err := db.SaveAgentFile(ctx, agent, user, name, []byte("v1")); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Two writers read the same row — the shape of the file tool and the
		// memory distiller in the same turn.
		read := AgentFileVersion{Content: []byte("v1")}

		// The first one lands.
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("v1 + A"), read); err != nil {
			t.Fatalf("the first writer was refused: %v", err)
		}
		// The second one must be refused, not merged and not overwritten.
		err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("v1 + B"), read)
		if !errors.Is(err, ErrAgentFileConflict) {
			t.Fatalf("the stale writer was not refused: err=%v", err)
		}

		got, err := db.GetAgentFileExact(ctx, agent, user, name)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if string(got) != "v1 + A" {
			t.Fatalf("the winner's content was lost: %q", got)
		}
	})
}

// A create-only write is a different precondition from "the row is empty": an
// empty row would satisfy a content comparison of "", and the create would not be
// create-only. This is the case the Absent flag exists for.
func TestSaveAgentFileIfVersionTellsAbsentFromEmpty(t *testing.T) {
	versionDialects(t, func(t *testing.T, db *DBStore) {
		ctx := context.Background()
		agent, user := versionScope(t, db)
		const name = "MEMORY.md"

		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("first"), AgentFileVersionAbsent); err != nil {
			t.Fatalf("create-only against a free key: %v", err)
		}
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("second"), AgentFileVersionAbsent); !errors.Is(err, ErrAgentFileConflict) {
			t.Fatalf("create-only against a taken key: err=%v", err)
		}

		// And the empty row, written as an empty row, is *not* absent.
		const name2 = "USER.md"
		if err := db.SaveAgentFile(ctx, agent, user, name2, []byte("")); err != nil {
			t.Fatalf("seed empty row: %v", err)
		}
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name2, []byte("written"), AgentFileVersionAbsent); !errors.Is(err, ErrAgentFileConflict) {
			t.Fatalf("create-only matched an existing empty row: err=%v", err)
		}
		// …while a caller that read the empty row may append to it.
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name2, []byte("appended"),
			AgentFileVersion{Content: []byte("")}); err != nil {
			t.Fatalf("appending to a row the caller read as empty: %v", err)
		}
	})
}

// A content expectation against a row that does NOT exist is a CREATE, not a conflict.
//
// The tools' read path falls back to the agent's disk copy when the store has no row, so their
// expectation can be non-empty while the row is absent — and "expected" is only a statement about
// the row when there is one. This case exists because a hand-written fake in the tools package had
// the stricter reading, which made a pretend conflict look like a store bug; pinning the real
// semantics is what tells the two apart.
func TestSaveAgentFileIfVersionTreatsAMissingRowAsACreate(t *testing.T) {
	versionDialects(t, func(t *testing.T, db *DBStore) {
		ctx := context.Background()
		agent, user := versionScope(t, db)
		const name = "MEMORY.md"

		// No row, a non-empty expectation (the base came from the disk copy): a create.
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("from disk + edit"),
			AgentFileVersion{Content: []byte("from disk")}); err != nil {
			t.Fatalf("a create carrying a disk-derived expectation was refused: %v", err)
		}
		// Now the row exists, so the same expectation is a conflict rather than a second create.
		if err := db.SaveAgentFileIfVersion(ctx, agent, user, name, []byte("second"),
			AgentFileVersion{Content: []byte("from disk")}); !errors.Is(err, ErrAgentFileConflict) {
			t.Fatalf("an existing row was overwritten by a stale expectation: err=%v", err)
		}
		got, err := db.GetAgentFileExact(ctx, agent, user, name)
		if err != nil || string(got) != "from disk + edit" {
			t.Fatalf("row = %q, %v; want the created content intact", got, err)
		}
	})
}

// The formal claim the SQL is doing the work for: with N writers holding the same
// expectation, exactly one write lands. On sqlite this is serialized by the
// database; the Postgres leg (with FASTAGENT_TEST_PG_DSN) is the one where the
// INSERT ... WHERE genuinely races.
func TestSaveAgentFileIfVersionHasExactlyOneWinner(t *testing.T) {
	versionDialects(t, func(t *testing.T, db *DBStore) {
		ctx := context.Background()
		agent, user := versionScope(t, db)
		const name = "MEMORY.md"
		if err := db.SaveAgentFile(ctx, agent, user, name, []byte("base")); err != nil {
			t.Fatalf("seed: %v", err)
		}

		const racers = 8
		wins, conflicts, other := 0, 0, []error{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				err := db.SaveAgentFileIfVersion(ctx, agent, user, name,
					[]byte("base+"+string(rune('a'+i))), AgentFileVersion{Content: []byte("base")})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, ErrAgentFileConflict):
					conflicts++
				default:
					other = append(other, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if len(other) > 0 {
			t.Fatalf("a racer failed for a reason that is not a conflict: %v", other)
		}
		if wins != 1 || conflicts != racers-1 {
			t.Fatalf("wins=%d conflicts=%d, want 1/%d", wins, conflicts, racers-1)
		}
	})
}
