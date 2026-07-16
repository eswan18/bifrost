package web

import (
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero time renders nothing", time.Time{}, ""},
		{"future clock skew reads as just now", now.Add(5 * time.Minute), "just now"},
		{"under a minute", now.Add(-30 * time.Second), "just now"},
		{"exactly one minute", now.Add(-time.Minute), "1m ago"},
		{"minutes", now.Add(-5 * time.Minute), "5m ago"},
		{"under an hour", now.Add(-59 * time.Minute), "59m ago"},
		{"one hour", now.Add(-time.Hour), "1h ago"},
		{"hours", now.Add(-5 * time.Hour), "5h ago"},
		{"just under a day", now.Add(-23 * time.Hour), "23h ago"},
		{"one day", now.Add(-24 * time.Hour), "1d ago"},
		{"days", now.Add(-5 * 24 * time.Hour), "5d ago"},
		{"just under a week", now.Add(-6 * 24 * time.Hour), "6d ago"},
		{"one week", now.Add(-7 * 24 * time.Hour), "1w ago"},
		{"weeks", now.Add(-29 * 24 * time.Hour), "4w ago"},
		{"one month", now.Add(-30 * 24 * time.Hour), "1mo ago"},
		{"months", now.Add(-90 * 24 * time.Hour), "3mo ago"},
		{"one year", now.Add(-365 * 24 * time.Hour), "1y ago"},
		{"years", now.Add(-2 * 365 * 24 * time.Hour), "2y ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeTime(tc.t, now); got != tc.want {
				t.Errorf("relativeTime(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}

// TestDayRelative covers the day-relative wall-clock formatter used for build
// and job times. 2026-06-15 is a Monday, so a Friday 3 days back and a Friday 4
// days ahead both render as "Friday".
func TestDayRelative(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero renders nothing", time.Time{}, ""},
		{"same day", time.Date(2026, 6, 15, 9, 58, 0, 0, time.UTC), "today 09:58"},
		{"yesterday", time.Date(2026, 6, 14, 16, 3, 0, 0, time.UTC), "yesterday 16:03"},
		{"tomorrow (next run)", time.Date(2026, 6, 16, 0, 5, 0, 0, time.UTC), "tomorrow 00:05"},
		{"three days ago is a weekday", time.Date(2026, 6, 12, 14, 10, 0, 0, time.UTC), "Friday 14:10"},
		{"four days ahead is a weekday", time.Date(2026, 6, 19, 8, 0, 0, 0, time.UTC), "Friday 08:00"},
		{"far past is a date", time.Date(2026, 1, 2, 15, 4, 0, 0, time.UTC), "Jan 2 15:04"},
		{"far future is a date", time.Date(2030, 1, 1, 14, 0, 0, 0, time.UTC), "Jan 1 14:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dayRelative(tc.t, now, time.UTC); got != tc.want {
				t.Errorf("dayRelative(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}

// TestDayRelativeHonorsLocation renders in the display timezone, not UTC.
func TestDayRelativeHonorsLocation(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) // 08:00 EDT, still the 15th
	// 01:30 UTC on the 15th is 21:30 EDT on the 14th → "yesterday 21:30".
	in := time.Date(2026, 6, 15, 1, 30, 0, 0, time.UTC)
	if got, want := dayRelative(in, now, ny), "yesterday 21:30"; got != want {
		t.Errorf("dayRelative = %q, want %q", got, want)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{42 * time.Second, "42s"},
		{72 * time.Second, "1m 12s"},
		{362 * time.Second, "6m 02s"},
		{time.Hour, "1h 00m"},
		{time.Hour + 61*time.Second, "1h 01m"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestRunningFor(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{2 * time.Minute, "2m"},
		{4 * time.Minute, "4m"},
		{time.Hour + time.Minute, "1h 1m"},
	}
	for _, tc := range cases {
		if got := runningFor(tc.d); got != tc.want {
			t.Errorf("runningFor(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestAbsTime(t *testing.T) {
	if got := absTime(time.Time{}); got != "" {
		t.Errorf("absTime(zero) = %q, want empty", got)
	}
	// A non-UTC input must be normalized to UTC so the tooltip is unambiguous.
	loc := time.FixedZone("PST", -8*3600)
	in := time.Date(2026, 6, 14, 13, 47, 29, 0, loc) // 21:47:29 UTC
	if got, want := absTime(in), "2026-06-14 21:47 UTC"; got != want {
		t.Errorf("absTime = %q, want %q", got, want)
	}
}
