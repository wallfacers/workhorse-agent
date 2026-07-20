// Package schedule implements the in-process cron scheduler
// (001-agent-orchestration US3): a minute-aligned worker that fires persisted
// schedules via unattended sessions. This file holds the self-contained
// five-field cron matcher (no third-party dependency).
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	fieldMinute = iota
	fieldHour
	fieldDayOfMonth
	fieldMonth
	fieldDayOfWeek
	fieldCount
)

// fieldRange bounds each cron field's legal integer domain.
type fieldRange struct{ min, max int }

var fieldRanges = [fieldCount]fieldRange{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 6},  // day of week (0 = Sunday)
}

// cronSchedule is the parsed, ready-to-match form of a cron expression: one
// membership set per field.
type cronSchedule struct {
	fields [fieldCount]map[int]bool
}

// parseField expands one comma-separated field (with *, N, N-M, */S, N-M/S)
// into the membership set.
func parseField(field string, r fieldRange) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		if err := parsePart(part, r, set); err != nil {
			return nil, err
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("empty field %q", field)
	}
	return set, nil
}

func parsePart(part string, r fieldRange, set map[int]bool) error {
	step := 1
	rangePart := part
	if idx := strings.Index(part, "/"); idx >= 0 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		step = s
		rangePart = part[:idx]
	}
	var lo, hi int
	switch {
	case rangePart == "*" || rangePart == "":
		lo, hi = r.min, r.max
	case strings.Contains(rangePart, "-"):
		dash := strings.Index(rangePart, "-")
		a, err1 := strconv.Atoi(rangePart[:dash])
		b, err2 := strconv.Atoi(rangePart[dash+1:])
		if err1 != nil || err2 != nil {
			return fmt.Errorf("invalid range %q", part)
		}
		lo, hi = a, b
	default:
		n, err := strconv.Atoi(rangePart)
		if err != nil {
			return fmt.Errorf("invalid value %q", part)
		}
		lo, hi = n, n
	}
	if lo < r.min || hi > r.max || lo > hi {
		return fmt.Errorf("value out of range [%d,%d]: %q", r.min, r.max, part)
	}
	for i := lo; i <= hi; i += step {
		set[i] = true
	}
	return nil
}

func parseExpr(expr string) (cronSchedule, error) {
	var sched cronSchedule
	fields := strings.Fields(expr)
	if len(fields) != fieldCount {
		return sched, fmt.Errorf("cron expression must have %d fields, got %d", fieldCount, len(fields))
	}
	for i, f := range fields {
		set, err := parseField(f, fieldRanges[i])
		if err != nil {
			return sched, fmt.Errorf("cron field %d %q: %w", i, f, err)
		}
		sched.fields[i] = set
	}
	return sched, nil
}

// Validate returns an error if expr is not a well-formed five-field cron
// expression within each field's domain.
func Validate(expr string) error {
	_, err := parseExpr(expr)
	return err
}

// Matches reports whether the given local time falls on the schedule's current
// minute. The caller supplies a local time (the worker uses time.Now()); day-of-
// month and day-of-week are combined with AND, which matches every common
// scheduling use case (where at most one of them is restricted). This is a
// deliberate simplification from vixie-cron's OR semantics.
func Matches(expr string, t time.Time) bool {
	sched, err := parseExpr(expr)
	if err != nil {
		return false
	}
	return sched.fields[fieldMinute][t.Minute()] &&
		sched.fields[fieldHour][t.Hour()] &&
		sched.fields[fieldMonth][int(t.Month())] &&
		sched.fields[fieldDayOfMonth][t.Day()] &&
		sched.fields[fieldDayOfWeek][int(t.Weekday())]
}
