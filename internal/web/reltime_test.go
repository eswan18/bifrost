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
