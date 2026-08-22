package cron

import (
	"testing"
	"time"
)

// E2E coverage for d4b7a63 / 1676128 "per-chatter timezone for prompts
// and cron scheduling". The scheduler computes the next run of a cron
// job as NextOccurrenceIn(j.Schedule, now, LocationOf(j.Timezone)),
// where j.Timezone is frozen at job creation from the chatter's
// effective scope-prefs timezone. These tests pin that a single
// wall-clock expression ("0 9 * * *") fires at 09:00 in the job's own
// zone regardless of the server's TZ, and that two jobs with different
// zones drift apart instead of both landing on the server's clock.
//
// Pure-function layer only (no store, no network) — the scheduler's
// call site is verified by construction: it always routes through
// NextOccurrenceIn/ LocationOf.

// Covers d4b7a63: the same expression from the same instant resolves to
// three different instants when the job's timezone differs — and each
// lands at 09:00 on that zone's own wall clock. The original bug: a
// 东八区 chatter's "每天早上 9 点" (stored as "0 9 * * *" with
// Asia/Shanghai) fired at 17:00 Beijing time because the scheduler
// evaluated the expression in UTC.
func TestCronNextOccurrenceInChatterZone_NotServerZone(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	// 2026-06-05 00:00 UTC = 08:00 Beijing = 2026-06-04 17:00 PDT.
	after := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	const schedule = "0 9 * * *"

	gotShanghai := NextOccurrenceIn(schedule, after, sh)
	gotLA := NextOccurrenceIn(schedule, after, la)
	gotUTC := NextOccurrenceIn(schedule, after, time.UTC)

	// Each occurrence must be 09:00 on the zone's own wall clock.
	if h, m := gotShanghai.In(sh).Hour(), gotShanghai.In(sh).Minute(); h != 9 || m != 0 {
		t.Errorf("Shanghai fired at %02d:%02d local, want 09:00", h, m)
	}
	if h, m := gotLA.In(la).Hour(), gotLA.In(la).Minute(); h != 9 || m != 0 {
		t.Errorf("Los_Angeles fired at %02d:%02d local, want 09:00", h, m)
	}
	if h, m := gotUTC.In(time.UTC).Hour(), gotUTC.In(time.UTC).Minute(); h != 9 || m != 0 {
		t.Errorf("UTC fired at %02d:%02d local, want 09:00", h, m)
	}

	// The three instants must be genuinely different (no two zones share
	// a fire time), proving the chatter's zone — not the server's —
	// drives the schedule.
	if gotShanghai.Equal(gotLA) || gotShanghai.Equal(gotUTC) || gotLA.Equal(gotUTC) {
		t.Fatalf("expected three distinct fire instants, got Shanghai=%v LA=%v UTC=%v",
			gotShanghai, gotLA, gotUTC)
	}
	// Cross-check the arithmetic: 09:00 PDT == 16:00 UTC, 09:00 +08 ==
	// 01:00 UTC, so LA fires 15h after Shanghai.
	if d := gotLA.Sub(gotShanghai); d != 15*time.Hour {
		t.Errorf("LA-Shanghai gap = %v, want 15h", d)
	}
}

// Covers d4b7a63 (per-job half): two jobs carrying the same expression
// but different Timezone fields resolve their next occurrence
// independently, exactly as two chatters of the same agent would. Also
// re-pins the legacy "UTC" / empty rows keep server-local semantics
// (LocationOf) that the adapter round-trips from the DB row.
func TestCronPerJobTimezoneIndependent(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	after := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	// Mirror what the scheduler does per job: NextOccurrenceIn(schedule,
	// now, LocationOf(job.Timezone)).
	jobA := LocationOf("Asia/Shanghai")
	jobB := LocationOf("America/Los_Angeles")

	nextA := NextOccurrenceIn("0 9 * * *", after, jobA)
	nextB := NextOccurrenceIn("0 9 * * *", after, jobB)
	if nextA.Equal(nextB) {
		t.Fatalf("jobs with different timezones must not fire together, both %v", nextA)
	}
	if !nextA.In(sh).Equal(time.Date(2026, 6, 5, 9, 0, 0, 0, sh)) {
		t.Errorf("jobA: want 09:00 Asia/Shanghai, got %v", nextA)
	}
	if !nextB.In(la).Equal(time.Date(2026, 6, 5, 9, 0, 0, 0, la)) {
		t.Errorf("jobB: want 09:00 America/Los_Angeles, got %v", nextB)
	}

	// LocationOf fallbacks used by legacy / no-pref rows.
	if loc := LocationOf("UTC"); loc != time.UTC {
		t.Errorf("legacy UTC row: want time.UTC, got %v", loc)
	}
	if loc := LocationOf(""); loc != time.Local {
		t.Errorf("empty timezone row: want server-local, got %v", loc)
	}
	if loc := LocationOf("Not/AZone"); loc != time.Local {
		t.Errorf("unknown timezone: want server-local fallback, got %v", loc)
	}
}
