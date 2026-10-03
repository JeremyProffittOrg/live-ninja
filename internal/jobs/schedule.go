package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // AWS provided runtime and Windows preview use the same IANA data.
)

func validateSchedule(s Schedule) error {
	if s.Kind == "" {
		s.Kind = "once"
	}
	switch s.Kind {
	case "once":
		if s.Time != "" || s.Weekday != 0 {
			return fmt.Errorf("%w: once accepts only at", ErrValidation)
		}
		if s.At != "" {
			if _, err := time.Parse(time.RFC3339, s.At); err != nil {
				return fmt.Errorf("%w: at must include a UTC offset", ErrValidation)
			}
		}
	case "daily", "weekdays", "weekly":
		if s.At != "" {
			return fmt.Errorf("%w: recurring schedules cannot have at", ErrValidation)
		}
		if len(s.Time) != 5 || s.Time[2] != ':' {
			return fmt.Errorf("%w: time must be HH:MM", ErrValidation)
		}
		h, he := strconv.Atoi(s.Time[:2])
		m, me := strconv.Atoi(s.Time[3:])
		if he != nil || me != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return fmt.Errorf("%w: invalid time", ErrValidation)
		}
		if s.Time != fmt.Sprintf("%02d:%02d", h, m) {
			return fmt.Errorf("%w: time must be HH:MM", ErrValidation)
		}
		if s.Weekday < 0 || s.Weekday > 6 {
			return fmt.Errorf("%w: weekday must be 0 through 6", ErrValidation)
		}
		if s.Timezone == "" || s.Timezone == "Local" || strings.Contains(s.Timezone, "\\") {
			return fmt.Errorf("%w: choose an IANA timezone", ErrValidation)
		}
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("%w: unknown IANA timezone", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: schedule kind is not supported", ErrValidation)
	}
	return nil
}

// NextOccurrences returns UTC instants strictly after after. A spring gap skips
// that day; a fall overlap fires at the FIRST occurrence only. Zone changes never
// silently follow a device: the saved IANA zone controls each civil occurrence.
func NextOccurrences(s Schedule, after time.Time, count int) ([]string, error) {
	if err := validateSchedule(s); err != nil {
		return nil, err
	}
	if count < 1 || count > 10 {
		return nil, fmt.Errorf("%w: count must be 1 through 10", ErrValidation)
	}
	out := make([]string, 0, count)
	if s.Kind == "once" || s.Kind == "" {
		if s.At != "" {
			at, _ := time.Parse(time.RFC3339, s.At)
			if at.After(after) {
				out = append(out, stamp(at))
			}
		}
		return out, nil
	}
	loc, _ := time.LoadLocation(s.Timezone)
	local := after.In(loc)
	year, month, day := local.Date()
	h, _ := strconv.Atoi(s.Time[:2])
	m, _ := strconv.Atoi(s.Time[3:])
	// Build dates in UTC to avoid midnight transitions changing the civil day.
	date := time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
	for d := 0; d < 100 && len(out) < count; d++ {
		civil := date.AddDate(0, 0, d)
		if s.Kind == "weekdays" && (civil.Weekday() == time.Saturday || civil.Weekday() == time.Sunday) {
			continue
		}
		if s.Kind == "weekly" && int(civil.Weekday()) != s.Weekday {
			continue
		}
		y, mo, da := civil.Date()
		approx := time.Date(y, mo, da, h, m, 0, 0, loc)
		// Check both sides of offset transitions, including 30-minute transitions.
		// Select first matching instant before testing after (so overlap's second
		// instance never becomes a second logical daily occurrence).
		var first time.Time
		for offset := -180; offset <= 180; offset++ {
			candidate := approx.Add(time.Duration(offset) * time.Minute)
			v := candidate.In(loc)
			yy, mm, dd := v.Date()
			if yy == y && mm == mo && dd == da && v.Hour() == h && v.Minute() == m {
				first = candidate
				break
			}
		}
		if !first.IsZero() && first.After(after) {
			out = append(out, stamp(first))
		}
	}
	return out, nil
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parse(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }
func next(s Schedule, now time.Time) string {
	v, _ := NextOccurrences(s, now, 1)
	if len(v) > 0 {
		return v[0]
	}
	return ""
}
