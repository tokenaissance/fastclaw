package scope

import (
	"testing"
	"time"
)

// offsetOf returns the seconds-east-of-UTC a location applies on a fixed
// reference instant (2026-06-14, a date with no half-hour-DST oddities
// for the zones tested). Comparing offsets is more robust than comparing
// Location.String(), which differs between FixedZone and IANA zones.
func offsetOf(loc *time.Location) int {
	ref := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	_, off := ref.In(loc).Zone()
	return off
}

func TestLocationFromText(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantNil    bool
		wantOffset int // seconds east of UTC, when !wantNil
	}{
		{"iana shanghai", "时区: Asia/Shanghai", false, 8 * 3600},
		{"iana ny dst", "I live in America/New_York", false, -4 * 3600}, // EDT in June
		{"utc plus 8", "my timezone is UTC+8", false, 8 * 3600},
		{"gmt colon", "GMT+08:00 here", false, 8 * 3600},
		{"utc minus 5", "UTC-5 east coast", false, -5 * 3600},
		{"bare hhmm", "offset +08:00", false, 8 * 3600},
		{"bare half hour", "-05:30 somewhere", false, -5*3600 - 30*60},
		{"chinese east 8", "用户在东八区", false, 8 * 3600},
		{"chinese arabic", "东8区", false, 8 * 3600},
		{"chinese west 5", "我在西五区", false, -5 * 3600},
		{"chinese east 12", "东十二区", false, 12 * 3600},
		{"named beijing zh", "默认北京时间", false, 8 * 3600},
		{"named beijing en", "uses Beijing time", false, 8 * 3600},

		// No timezone present / must not false-positive.
		{"empty", "", true, 0},
		{"prose with slash", "她/他 喜欢喝咖啡", true, 0},
		{"bare plus no colon", "得了 +8 分", true, 0},
		{"unrelated", "name: Alice, job: accountant", true, 0},
	}
	for _, c := range cases {
		loc := LocationFromText(c.text)
		if c.wantNil {
			if loc != nil {
				t.Errorf("%s: LocationFromText(%q) = %v, want nil", c.name, c.text, loc)
			}
			continue
		}
		if loc == nil {
			t.Errorf("%s: LocationFromText(%q) = nil, want offset %d", c.name, c.text, c.wantOffset)
			continue
		}
		if got := offsetOf(loc); got != c.wantOffset {
			t.Errorf("%s: LocationFromText(%q) offset = %d, want %d", c.name, c.text, got, c.wantOffset)
		}
	}
}

// The reported bug: a UTC pod renders an 08:12 UTC timestamp, but the
// chatter's USER.md says 东八区, so the resolved instant must read 16:12.
func TestUserMDEastEightConvertsAfternoon(t *testing.T) {
	loc := LocationFromText("基本信息\n- 时区：东八区\n- 职业：会计")
	if loc == nil {
		t.Fatal("expected a location from 东八区")
	}
	utc := time.Date(2026, 6, 14, 8, 12, 0, 0, time.UTC)
	local := utc.In(loc)
	if h, m := local.Hour(), local.Minute(); h != 16 || m != 12 {
		t.Fatalf("08:12 UTC in 东八区 = %02d:%02d, want 16:12", h, m)
	}
}
