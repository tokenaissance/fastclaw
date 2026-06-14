package scope

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LocationFromText extracts a timezone from free-form profile text (the
// chatter's USER.md, where they record their timezone in their own
// words) and returns the matching *time.Location, or nil when the text
// contains no recognizable timezone.
//
// The deployment clock is UTC and inbound message timestamps are UTC;
// USER.md is the only place the chatter's real timezone lives, so this
// is what lets the agent render "now" and per-message timestamps in the
// chatter's local time. Recognized forms, in priority order (most
// explicit first):
//
//  1. an IANA name token — "Asia/Shanghai", "America/New_York"
//     (validated with time.LoadLocation, so a stray "他/她" never matches)
//  2. a UTC/GMT offset — "UTC+8", "GMT+08:00", "UTC-5"
//  3. a bare signed HH:MM offset — "+08:00", "-05:30"
//  4. a Chinese zone — "东八区" / "西五区" (东 = east/ahead, 西 = west/behind)
//  5. a named zone — "北京时间" / "中国标准时间" / "Beijing time"
//
// Offset and Chinese forms become a fixed-offset zone (no DST — that's
// the correct semantics for "the user said UTC+8"); named zones resolve
// to their IANA location so DST still applies.
func LocationFromText(text string) *time.Location {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if loc := ianaFromText(text); loc != nil {
		return loc
	}
	if loc := offsetFromText(text); loc != nil {
		return loc
	}
	if loc := chineseZoneFromText(text); loc != nil {
		return loc
	}
	return namedZoneFromText(text)
}

// ianaRe matches a "Region/City" token (optionally a three-segment name
// like "America/Argentina/Salta"). Validation against the tz database is
// what filters out non-timezone slashes.
var ianaRe = regexp.MustCompile(`\b([A-Za-z]+(?:_[A-Za-z]+)*(?:/[A-Za-z]+(?:_[A-Za-z]+)*){1,2})\b`)

func ianaFromText(text string) *time.Location {
	for _, m := range ianaRe.FindAllStringSubmatch(text, -1) {
		// "UTC" is handled by the offset branch; skip obvious non-zones
		// fast, but ultimately LoadLocation is the gate.
		if loc, err := time.LoadLocation(m[1]); err == nil {
			return loc
		}
	}
	return nil
}

// offsetRe matches "UTC+8", "GMT+08:00", "UTC-5", and bare "+08:00".
// The UTC/GMT prefix is optional, but a bare offset must carry a colon
// (HH:MM) so we don't grab unrelated numbers like "+8 points".
var offsetRe = regexp.MustCompile(`(?i)(UTC|GMT)?\s*([+-])\s*(\d{1,2})(?::?([0-5]\d))?`)

func offsetFromText(text string) *time.Location {
	for _, m := range offsetRe.FindAllStringSubmatch(text, -1) {
		prefix := m[1]
		hasMinutes := m[4] != ""
		// Bare sign+digits with no UTC/GMT prefix and no minutes is too
		// ambiguous (could be any signed number) — require either the
		// prefix or an explicit HH:MM.
		if prefix == "" && !hasMinutes {
			continue
		}
		hours, _ := strconv.Atoi(m[3])
		if hours > 14 { // max real UTC offset is +14
			continue
		}
		mins := 0
		if hasMinutes {
			mins, _ = strconv.Atoi(m[4])
		}
		secs := hours*3600 + mins*60
		if m[2] == "-" {
			secs = -secs
		}
		return fixedZone(secs)
	}
	return nil
}

// chineseZoneRe matches "东八区" / "西五区" / "东十二区" etc. 东 (east) is
// ahead of UTC, 西 (west) is behind.
var chineseZoneRe = regexp.MustCompile(`([东西])([一二三四五六七八九十]{1,3}|\d{1,2})区`)

func chineseZoneFromText(text string) *time.Location {
	m := chineseZoneRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	n := parseCJKNumeral(m[2])
	if n < 0 || n > 12 {
		return nil
	}
	secs := n * 3600
	if m[1] == "西" {
		secs = -secs
	}
	return fixedZone(secs)
}

// namedZones maps a few well-known colloquial timezone names to IANA
// locations. Kept short on purpose — broad city-name matching invites
// false positives; the model is expected to write an IANA name or an
// offset for anything exotic.
var namedZones = map[string]string{
	"北京时间":    "Asia/Shanghai",
	"中国标准时间":  "Asia/Shanghai",
	"中国时间":    "Asia/Shanghai",
	"beijing": "Asia/Shanghai",
}

func namedZoneFromText(text string) *time.Location {
	lower := strings.ToLower(text)
	for name, iana := range namedZones {
		if strings.Contains(text, name) || strings.Contains(lower, name) {
			if loc, err := time.LoadLocation(iana); err == nil {
				return loc
			}
		}
	}
	return nil
}

// fixedZone builds a fixed-offset *time.Location with a readable name
// like "UTC+08:00" / "UTC-05:30" / "UTC".
func fixedZone(offsetSecs int) *time.Location {
	name := "UTC"
	if offsetSecs != 0 {
		sign := "+"
		s := offsetSecs
		if s < 0 {
			sign = "-"
			s = -s
		}
		name = fmt.Sprintf("UTC%s%02d:%02d", sign, s/3600, (s%3600)/60)
	}
	return time.FixedZone(name, offsetSecs)
}

// parseCJKNumeral parses 1–2 digit Arabic numbers and Chinese numerals
// up to 十二 (12). Returns -1 on anything it can't read.
func parseCJKNumeral(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	digits := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	runes := []rune(s)
	switch {
	case len(runes) == 1 && runes[0] == '十':
		return 10
	case len(runes) == 1:
		if d, ok := digits[runes[0]]; ok {
			return d
		}
	case len(runes) == 2 && runes[0] == '十': // 十一, 十二
		if d, ok := digits[runes[1]]; ok {
			return 10 + d
		}
	case len(runes) == 2 && runes[1] == '十': // 二十 (unlikely for zones)
		if d, ok := digits[runes[0]]; ok {
			return d * 10
		}
	}
	return -1
}
