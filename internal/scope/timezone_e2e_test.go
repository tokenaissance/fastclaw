package scope

import (
	"context"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// End-to-end timezone coverage for two upstream commits:
//
//   - 303b8aa / 983df17 "resolve the chatter's timezone from USER.md,
//     infer it proactively": the agent's chatterLocation parses the
//     chatter's USER.md profile with LocationFromText first, then DB
//     prefs, then UTC. The tests below pin that parser against
//     realistic USER.md content (every format the runtime promises to
//     understand) and confirm that non-timezone profiles fall back
//     cleanly instead of false-positiving.
//
//   - d4b7a63 / 1676128 "per-chatter timezone for prompts and cron
//     scheduling": scope prefs resolve chatter-first and independently
//     per chatter. The tests below pin that two chatters of the same
//     agent each resolve their own wall clock and never bleed into
//     each other, and that unknown/empty names fall back to server
//     local time.
//
// These run at the pure-function / scope layer (no network, no agent
// loop); the agent-loop wiring itself is covered in
// internal/agent/timezone_e2e_test.go.

// Covers 303b8aa: the exact free-form timezone the parser must extract
// from a chatter's USER.md, in the natural-language forms the agent is
// told to write. Each entry is a realistic snippet of a profile file.
func TestLocationFromText_UserMDProfileFormats(t *testing.T) {
	cases := []struct {
		name       string
		userMD     string // body of a USER.md profile
		wantOffset int    // seconds east of UTC on 2026-06-14
	}{
		{"iana shanghai in zh profile", "# 基本信息\n- 时区: Asia/Shanghai\n- 职业: 会计", 8 * 3600},
		{"utc plus 8", "Timezone: UTC+8", 8 * 3600},
		{"gmt colon offset", "我的时区：GMT+08:00", 8 * 3600},
		{"bare hhmm", "- 时区: +08:00", 8 * 3600},
		{"iana los angeles pdt", "Timezone: America/Los_Angeles", -7 * 3600}, // PDT in June
		{"named beijing time", "I'm on Beijing time.", 8 * 3600},
		{"chinese east eight", "我在东八区，是一名工程师", 8 * 3600},
		{"iana berlin cest", "Timezone: Europe/Berlin", 2 * 3600}, // CEST in June
	}
	for _, c := range cases {
		loc := LocationFromText(c.userMD)
		if loc == nil {
			t.Errorf("%s: LocationFromText(%q) = nil, want offset %d", c.name, c.userMD, c.wantOffset)
			continue
		}
		if got := offsetOf(loc); got != c.wantOffset {
			t.Errorf("%s: LocationFromText(%q) offset = %d, want %d", c.name, c.userMD, got, c.wantOffset)
		}
	}
}

// Covers 303b8aa: a profile that contains no recognizable timezone must
// resolve to nil so the caller falls back (DB prefs → server local)
// instead of guessing. The "上海" city without a zone token and a bare
// "+8 分" (no UTC/GMT prefix, no colon) are exactly the inputs that
// must NOT false-positive.
func TestLocationFromText_InvalidUserMDFallsBackNil(t *testing.T) {
	cases := []struct {
		name   string
		userMD string
	}{
		{"empty", ""},
		{"city without zone token", "# 基本信息\n- 城市：上海\n- 职业：会计"},
		{"slash prose", "家乡：她/他 都很喜欢这里"},
		{"bare plus score", "上次比赛得了 +8 分"},
		{"unrelated profile", "name: Alice\njob: accountant\nhobby: hiking"},
	}
	for _, c := range cases {
		if loc := LocationFromText(c.userMD); loc != nil {
			t.Errorf("%s: LocationFromText(%q) = %v, want nil", c.name, c.userMD, loc)
		}
	}
}

// Covers d4b7a63: timezone is a property of the chatter, not the agent.
// Two chatters of the same agent resolve independently; mutating one
// chatter's preference must never leak into the other's resolution.
func TestPerChatterTimezoneIndependent_ScopePrefs(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	agentID := "agent-shared"

	const (
		chatterA = "chatter-a" // Asia/Shanghai
		chatterB = "chatter-b" // America/Los_Angeles
	)

	if err := SaveUserTimezone(ctx, db, chatterA, "Asia/Shanghai"); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := SaveUserTimezone(ctx, db, chatterB, "America/Los_Angeles"); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// Each chatter resolves their own wall clock against the same agent.
	if tz := Timezone(ctx, db, chatterA, agentID); tz != "Asia/Shanghai" {
		t.Fatalf("A: want Asia/Shanghai, got %q", tz)
	}
	if tz := Timezone(ctx, db, chatterB, agentID); tz != "America/Los_Angeles" {
		t.Fatalf("B: want America/Los_Angeles, got %q", tz)
	}

	// The resolved locations are genuinely different offsets.
	locA := LoadLocationOrLocal(Timezone(ctx, db, chatterA, agentID))
	locB := LoadLocationOrLocal(Timezone(ctx, db, chatterB, agentID))
	if offsetOf(locA) == offsetOf(locB) {
		t.Fatalf("expected A and B to resolve to different offsets, both %d", offsetOf(locA))
	}

	// Mutating A must not disturb B.
	if err := SaveUserTimezone(ctx, db, chatterA, "Europe/Berlin"); err != nil {
		t.Fatalf("re-save A: %v", err)
	}
	if tz := Timezone(ctx, db, chatterA, agentID); tz != "Europe/Berlin" {
		t.Fatalf("A after change: want Europe/Berlin, got %q", tz)
	}
	if tz := Timezone(ctx, db, chatterB, agentID); tz != "America/Los_Angeles" {
		t.Fatalf("B after A changed: want America/Los_Angeles, got %q", tz)
	}
}

// Covers d4b7a63 (fallback half): an empty or unknown timezone name must
// resolve to server-local time, never nil and never a fabricated zone.
func TestLoadLocationOrLocal_FallbackDefaults(t *testing.T) {
	if loc := LoadLocationOrLocal(""); loc != time.Local {
		t.Errorf("empty name: want server-local, got %v", loc)
	}
	if loc := LoadLocationOrLocal("Not/AZone"); loc != time.Local {
		t.Errorf("unknown name: want server-local, got %v", loc)
	}
	if loc := LoadLocationOrLocal("Asia/Shanghai"); loc == nil {
		t.Fatal("valid name: nil location")
	} else if offsetOf(loc) != 8*3600 {
		t.Errorf("Asia/Shanghai offset = %d, want %d", offsetOf(loc), 8*3600)
	}
}
