package sandbox

// The message the agent gets when the post-exec reconcile could not run.
//
// The defect this file witnesses (row 84 of docs/fs-formal-proof/11-change-register.md): the sentence
// was a constant, written 2026-09-18 out of that week's incident — the over-cap OOM of 09-14 — while
// the branch it lives in catches EVERY failure of SnapshotWorkspace. In the production window
// measured 2026-09-28 the cap was 0 of 2 failures and "the turn's ctx died" was 2 of 2, so every
// agent that hit it was told to go move files to /tmp — a place that is not mirrored, which is how
// "follow the advice" becomes "lose the deliverable".
//
// Row 85 moved the second case: the post-exec sync no longer runs on the turn's ctx, so "the turn's
// context ended" is not a cause this branch can have any more (`…/post_exec_sync_test.go` holds that
// end). What the ctx branch reports now is the sync's OWN budget running out — the same rule, one
// layer in: say the cause you actually have.
//
// Falsification: put the constant back (or drop the switch) and case 2 reddens — the budget-shaped
// failure starts naming the cap again.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTheSyncFailureMessageNamesOnlyTheCauseItKnows(t *testing.T) {
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	const capText = "workspace snapshot is over the 32.0 MB cap — refusing to flush /workspace after every exec; move large or growing files out of /workspace (use /tmp for run logs) and retry. Largest entries: 26 MB /workspace/run.log"

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want []string
		// notWant are the words that would be a claim about a cause this failure does not have.
		notWant []string
	}{
		{
			name: "over the cap",
			ctx:  live,
			err:  fmt.Errorf("%s [%w]", capText, errSnapshotOverCap),
			// The instruction is part of this class's message on purpose: it is true here, and the
			// runtime is the only layer that gets to say "this name is over the cap" at all. It is
			// carried in the message's own words as well as the executor's, so a backend that
			// reworded its error would not silently take the advice away.
			want:    []string{"could NOT be synced", "snapshot cap", "Move the large or growing files out of /workspace", "/tmp", "not mirrored", "Largest entries", "read_file"},
			notWant: []string{"turn's context ended"},
		},
		{
			name:    "the sync ran out of its own budget",
			ctx:     dead,
			err:     fmt.Errorf("snapshot workspace exec: %w", context.Canceled),
			want:    []string{"could NOT be synced", "did not finish inside its own", "next sync", "read_file"},
			notWant: []string{"snapshot cap", "/tmp", "32 MiB", "over the snapshot cap", "turn's context ended"},
		},
		{
			name:    "something else",
			ctx:     live,
			err:     errors.New("snapshot workspace exec: e2b exec did not exit cleanly (frames=1)"),
			want:    []string{"could NOT be synced", "frames=1", "read_file"},
			notWant: []string{"snapshot cap", "turn's context ended", "/tmp"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := snapshotFailureProblem(tc.ctx, tc.err)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the message lost %q:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("the message claims a cause this failure does not have (%q):\n%s", nw, got)
				}
			}
		})
	}
}

// The cap class is decided by a value, and the value survives the executor boundary — otherwise the
// classification above would only ever see the string.
func TestTheOverCapClassSurvivesTheExecutorBoundary(t *testing.T) {
	real := fmt.Errorf("workspace snapshot is over the 32.0 MB cap — … [%w]", errSnapshotOverCap)
	if !errors.Is(real, errSnapshotOverCap) {
		t.Fatal("the sentinel does not survive its own wrapping")
	}
	if got := snapshotFailureProblem(context.Background(), real); !strings.Contains(got, "snapshot cap") {
		t.Fatalf("the over-cap class was not recognised:\n%s", got)
	}
}
