package jobs

import (
	"reflect"
	"testing"
	"time"
)

func TestScheduleCivilTimePolicies(t *testing.T) {
	tests := []struct {
		name  string
		s     Schedule
		after string
		want  []string
	}{
		{"spring gap skipped", Schedule{Kind: "daily", Time: "02:30", Timezone: "America/New_York"}, "2026-03-07T08:00:00Z", []string{"2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"}},
		{"fall earliest only", Schedule{Kind: "daily", Time: "01:30", Timezone: "America/New_York"}, "2026-11-01T04:00:00Z", []string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
		{"fall duplicate forbidden", Schedule{Kind: "daily", Time: "01:30", Timezone: "America/New_York"}, "2026-11-01T05:31:00Z", []string{"2026-11-02T06:30:00Z", "2026-11-03T06:30:00Z"}},
		{"weekdays skip weekend", Schedule{Kind: "weekdays", Time: "09:00", Timezone: "UTC"}, "2026-10-02T09:00:00Z", []string{"2026-10-05T09:00:00Z", "2026-10-06T09:00:00Z"}},
		{"weekly Sunday", Schedule{Kind: "weekly", Time: "08:00", Timezone: "UTC", Weekday: 0}, "2026-10-03T00:00:00Z", []string{"2026-10-04T08:00:00Z", "2026-10-11T08:00:00Z"}},
		{"leap day", Schedule{Kind: "daily", Time: "12:00", Timezone: "UTC"}, "2028-02-28T13:00:00Z", []string{"2028-02-29T12:00:00Z", "2028-03-01T12:00:00Z"}},
		{"half hour spring gap", Schedule{Kind: "daily", Time: "02:15", Timezone: "Australia/Lord_Howe"}, "2026-10-03T00:00:00Z", []string{"2026-10-04T15:15:00Z", "2026-10-05T15:15:00Z"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			after, _ := time.Parse(time.RFC3339, tc.after)
			got, e := NextOccurrences(tc.s, after, len(tc.want))
			if e != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v %v want %v", got, e, tc.want)
			}
		})
	}
}
func TestInvalidSchedulesAndManual(t *testing.T) {
	bad := []Schedule{{Kind: "hourly"}, {Kind: "daily", Time: "9:00", Timezone: "UTC"}, {Kind: "daily", Time: "25:00", Timezone: "UTC"}, {Kind: "daily", Time: "12:00", Timezone: "Local"}, {Kind: "weekly", Time: "12:00", Timezone: "UTC", Weekday: 7}, {Kind: "once", At: "2026-10-03T12:00:00"}, {Kind: "daily", Time: "12:00", Timezone: "UTC", At: "2026-10-03T12:00:00Z"}}
	for _, s := range bad {
		if _, e := NextOccurrences(s, time.Now(), 1); e == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	got, e := NextOccurrences(Schedule{Kind: "once"}, time.Now(), 3)
	if e != nil || len(got) != 0 {
		t.Fatalf("manual %+v %v", got, e)
	}
}
