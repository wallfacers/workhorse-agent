package schedule

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"0 9 * * 1-5", true},
		{"*/1 * * * *", true},
		{"0,30 * * * *", true},
		{"0 9 1 * *", true},
		{"30 14 15 6 0", true},
		{"0 0 * * 0", true},
		{"0-59 0-23 1-31 1-12 0-6", true},
		{"*/15 9-17 * * 1-5", true},
		{"60 9 * * *", false}, // minute out of range
		{"0 24 * * *", false}, // hour out of range
		{"0 9 0 * *", false},  // dom out of range (min 1)
		{"0 9 * * 7", false},  // dow out of range (max 6)
		{"0 9 * 13 *", false}, // month out of range
		{"* * * *", false},    // too few fields
		{"0 9 * * 1-5 extra", false},
		{"abc 9 * * *", false}, // non-numeric
		{"*-5 9 * * *", false}, // malformed range
		{"*/0 * * * *", false}, // zero step
		{"0 9 * * 1/0", false}, // zero step on range
		{"", false},            // empty
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			err := Validate(tc.expr)
			if tc.ok && err != nil {
				t.Fatalf("expected valid, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected invalid, got nil")
			}
		})
	}
}

func localTime(year, month, day, hour, min int) time.Time {
	return time.Date(year, time.Month(month), day, hour, min, 0, 0, time.Local)
}

func TestMatches(t *testing.T) {
	monday9am := localTime(2026, 7, 20, 9, 0)  // 2026-07-20 is a Monday
	sunday9am := localTime(2026, 7, 19, 9, 0)  // Sunday
	tuesday9am := localTime(2026, 7, 21, 9, 0) // Tuesday
	fifteenth1430 := localTime(2026, 6, 15, 14, 30)

	cases := []struct {
		name string
		expr string
		t    time.Time
		want bool
	}{
		{"weekday 9am matches Monday", "0 9 * * 1-5", monday9am, true},
		{"weekday 9am rejects Sunday", "0 9 * * 1-5", sunday9am, false},
		{"every 15 min at :00", "*/15 * * * *", localTime(2026, 7, 20, 9, 0), true},
		{"every 15 min at :07", "*/15 * * * *", localTime(2026, 7, 20, 9, 7), false},
		{"every 15 min at :45", "*/15 * * * *", localTime(2026, 7, 20, 9, 45), true},
		{"dom 15 at 14:30", "30 14 15 * *", fifteenth1430, true},
		{"dom 15 wrong minute", "0 14 15 * *", fifteenth1430, false},
		{"full timestamp match", "30 14 15 6 1", fifteenth1430, true}, // 2026-06-15 is a Monday (dow 1)
		{"sunday via 0", "0 0 * * 0", sunday9am.Add(-9 * time.Hour), true},
		{"comma list includes Tuesday", "0 9 * * 1,2,3", tuesday9am, true},
		{"range excludes Sunday", "0 9 * * 1-5", sunday9am, false},
		{"hour range matches 9am", "0 9-17 * * *", monday9am, true},
		{"hour range rejects 18:00", "0 9-17 * * *", localTime(2026, 7, 20, 18, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches(tc.expr, tc.t)
			if got != tc.want {
				t.Fatalf("Matches(%q, %v) = %v, want %v", tc.expr, tc.t, got, tc.want)
			}
		})
	}
}

func TestMatchesInvalidExprIsFalse(t *testing.T) {
	if Matches("not a cron", time.Now()) {
		t.Fatal("invalid expression must not match")
	}
}
