package sandbox

// The one number the sync's ordering rule rests on: how far is the E2B guest's
// clock from this pod's?
//
// The reconcile compares a sandbox file's mtime (a reading of the GUEST clock)
// with a store object's write time (a reading of the store's clock), so the
// comparison is only an ordering if that offset is either small or measured. The
// design measures it in the same exec as the listing (sandbox.statsFor) and falls
// back to refusing when the two readings cannot be reconciled — this probe says
// whether the measurement is worth anything in practice, and it is also the
// instrument for re-checking after an E2B runtime change.
//
//	FASTAGENT_E2B_LIVE=1 E2B_API_KEY=e2b_... \
//	  go test ./internal/sandbox/ -run TestE2BLiveGuestClockOffset -v -count=1
//
// It prints every sample: the offset, the round trip that bounds it, and whether
// `date` answered at all (the stats exec depends on both).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

func TestE2BLiveGuestClockOffset(t *testing.T) {
	if os.Getenv("FASTAGENT_E2B_LIVE") != "1" {
		t.Skip("live E2B: set FASTAGENT_E2B_LIVE=1 with E2B_API_KEY to run")
	}
	if os.Getenv("E2B_API_KEY") == "" {
		t.Fatal("E2B_API_KEY is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	agent := fmt.Sprintf("live_clock_%d", time.Now().UnixNano())
	_, _, _, ex := liveE2B(t, agent, func(store *workspace.LocalFS) {})

	// The same command statsFor runs, so this also proves the two halves arrive
	// together in one exec.
	const probe = `echo "NOW $(date +%s.%N)"; nproc 2>/dev/null | head -1`

	// Warm up first: the FIRST exec of a fresh instance carries sandbox startup
	// and connection setup (measured: 1.8–11 s round trips), and an offset
	// estimate is only as good as the round trip that bounds it. The reconcile
	// runs right after a command, so the warm round trip is the one that matters.
	for i := 0; i < 3; i++ {
		if _, err := ex.Exec(ctx, "true", 30*time.Second); err != nil {
			t.Fatalf("warm-up exec %d: %v", i, err)
		}
	}

	best := time.Duration(1<<62 - 1)
	var bestOffset time.Duration
	for i := 0; i < 6; i++ {
		before := time.Now()
		out, err := ex.Exec(ctx, probe, 30*time.Second)
		after := time.Now()
		rtt := after.Sub(before)
		if err != nil {
			t.Fatalf("probe exec %d: %v (%q)", i, err, out)
		}
		var guest time.Time
		var known bool
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "NOW "); ok {
				if secs, perr := strconv.ParseFloat(strings.TrimSpace(rest), 64); perr == nil {
					guest = time.Unix(0, int64(secs*float64(time.Second)))
					known = true
				}
			}
		}
		if !known {
			t.Fatalf("the guest never answered the epoch-seconds probe; output was %q "+
				"(statsFor depends on it and would refuse every order)", out)
		}
		offset := guest.Sub(before.Add(rtt / 2))
		t.Logf("sample %d: guest - pod = %v (round trip %v, so ±%v), clockKnown=%v",
			i, guest.Sub(before.Add(rtt/2)), rtt, rtt/2, known)
		if rtt < best {
			best, bestOffset = rtt, offset
		}
	}
	// The estimate with the smallest round trip is the one to read: every other
	// sample is the same clock seen through a slower path.
	t.Logf("best estimate: guest - pod = %v, bounded by ±%v (smallest round trip %v)",
		bestOffset, best/2, best)
	if best/2 > versionSlack {
		t.Errorf("the round trip that bounds the offset (%v, so ±%v) is wider than versionSlack (%v): "+
			"on this runtime the mtime ordering cannot decide gaps smaller than that",
			best, best/2, versionSlack)
	}
}
