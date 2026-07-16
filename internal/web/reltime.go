package web

import (
	"fmt"
	"time"
)

// relativeTime renders how long ago t was, relative to now, in a single
// coarse unit ("5m ago", "3h ago", "2d ago"). It returns "" for the zero
// time so callers can omit the element entirely, and "just now" for anything
// under a minute (including small future skew, since the clock that stamped t
// and our own can drift). The ladder is intentionally lossy — the status page
// wants a glanceable "how fresh is this" signal, not a precise duration.
func relativeTime(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", d/time.Hour)
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", d/(24*time.Hour))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dw ago", d/(7*24*time.Hour))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", d/(30*24*time.Hour))
	default:
		return fmt.Sprintf("%dy ago", d/(365*24*time.Hour))
	}
}

// dayRelative renders a wall-clock timestamp in the operator's display
// timezone with a day-relative prefix, matching the design's build/job times:
//
//	same calendar day → "today 09:58"
//	the day before    → "yesterday 16:03"
//	the day after     → "tomorrow 00:05"   (next-run times run into the future)
//	within a week     → weekday "Monday 14:10"
//	otherwise         → "Jan 2 15:04"
//
// It returns "" for the zero time so callers can omit the element. A nil loc
// falls back to UTC.
func dayRelative(t, now time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	if loc == nil {
		loc = time.UTC
	}
	tl := t.In(loc)
	nl := now.In(loc)
	ty, tm, td := tl.Date()
	ny, nm, nd := nl.Date()
	tDay := time.Date(ty, tm, td, 0, 0, 0, 0, loc)
	nDay := time.Date(ny, nm, nd, 0, 0, 0, 0, loc)
	// Rounding to whole days absorbs the ±1h a DST transition adds to the raw
	// difference between two local midnights.
	days := int(nDay.Sub(tDay).Round(24*time.Hour) / (24 * time.Hour))
	hm := tl.Format("15:04")
	switch {
	case days == 0:
		return "today " + hm
	case days == 1:
		return "yesterday " + hm
	case days == -1:
		return "tomorrow " + hm
	case days > -7 && days < 7:
		return tl.Format("Monday") + " " + hm
	default:
		return tl.Format("Jan 2 15:04")
	}
}

// humanDuration renders a completed run's duration the way the design does:
// "42s" under a minute, "1m 12s" / "6m 02s" with zero-padded seconds above it,
// "1h 04m" once it crosses an hour. Negative inputs clamp to zero.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %02dm", s/3600, (s%3600)/60)
	}
}

// runningFor renders a coarse elapsed time for an in-flight run or build, as
// the design's "running 2m" / "Running 4m" labels do: seconds under a minute,
// whole minutes under an hour, "Xh Ym" above.
func runningFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}

// absTime renders t as an unambiguous UTC timestamp for a tooltip, or "" for
// the zero time.
func absTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}
