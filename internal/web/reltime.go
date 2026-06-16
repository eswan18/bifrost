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

// absTime renders t as an unambiguous UTC timestamp for a tooltip, or "" for
// the zero time.
func absTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}
