package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/eswan18/bifrost/internal/previewapi"
)

// This file is `bif preview up`'s progress rendering and the pure functions
// around it: the spinner, elapsed-time formatting, and the before/after
// comparison behind the "nothing rebuilt" line. It is separate from preview.go
// only for size — none of it touches the network, and main_test.go's import ban
// applies to this file in full.

// previewPollInterval is how often the poll loop asks bifrost for the record,
// straight from ib.py. It is slow on purpose: a create takes minutes, and the
// spinner redraws from a locally recomputed elapsed time between polls, so a
// tighter interval would buy nothing but load.
const previewPollInterval = 3 * time.Second

// previewWaitTimeout bounds the whole wait, matching ib.py's 30 minutes and
// internal/web's asyncOrchestrationTimeout — the CLI stops waiting at the same
// point bifrost stops working.
const previewWaitTimeout = 30 * time.Minute

// previewEnv is everything outside the process that `preview up` depends on:
// the clock, the pause between polls, and whether stdout is a terminal.
//
// It is a parameter rather than three package-level calls because a Go test's
// stdout is never a terminal, so the TTY branch could not otherwise be reached
// at all — it would be dead code that only runs in production, which is the
// worst possible place for a rendering path to first execute. Injecting it
// also means the poll loop's tests neither sleep nor depend on wall-clock
// time: a 30-minute timeout is asserted in microseconds.
type previewEnv struct {
	now   func() time.Time
	sleep func(time.Duration)
	isTTY bool
}

// defaultPreviewEnv is the real world: the real clock, a real sleep, and a
// terminal check against the stream progress will actually be drawn on.
//
// The check is against `stdout` rather than os.Stdout because that is where
// the escape sequences would go. `bif preview up > log` must take the plain
// path even though the process still has a terminal on stderr.
func defaultPreviewEnv(stdout io.Writer) previewEnv {
	return previewEnv{now: time.Now, sleep: time.Sleep, isTTY: isTerminal(stdout)}
}

// isTerminal reports whether w is a terminal.
//
// Anything that is not an *os.File — a bytes.Buffer in a test, a pipe wrapper —
// is not, and neither is a redirect to a file or a pipe to `tee`. The
// os.ModeCharDevice heuristic is deliberately avoided: /dev/null is a character
// device, and a CI job redirecting there would get the spinner path.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// spinnerFrames are ib.py's braille spinner glyphs.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// spinnerFrame is the glyph for poll tick `tick`.
func spinnerFrame(tick int) string {
	return string(spinnerFrames[tick%len(spinnerFrames)])
}

// formatElapsed renders a duration like "47s" or "2m03s".
//
// Negative clamps to "0s" rather than rendering a minus sign: the only way to
// get one is a server clock ahead of this one, and "-3s of building" is a
// worse answer to "how long has this been running" than "0s".
func formatElapsed(d time.Duration) string {
	total := int(d / time.Second)
	if total < 0 {
		total = 0
	}
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm%02ds", total/60, total%60)
}

// stepElapsed reports how long the current step has been running.
//
// It prefers the server's stepSince, recomputed fresh against `now` on every
// call so the number does not go stale between three-second polls, and falls
// back to a locally tracked start when bifrost reported no usable timestamp —
// an older server, a ready preview with no step, or a value previewapi's
// decoder rejected as unparseable or timezone-naive (see Record.UnmarshalJSON,
// which is where that judgement is made and why this function sees only a
// clean zero).
func stepElapsed(stepSince, fallbackStart, now time.Time) time.Duration {
	if !stepSince.IsZero() {
		return now.Sub(stepSince)
	}
	return now.Sub(fallbackStart)
}

// progress draws the poll loop's output, either as a redrawn single line with
// a spinner (a terminal) or as one plain line per step (anything else).
//
// The split is not cosmetic. Piped output is what lands in CI logs and in
// `| tee` transcripts, where a carriage-returned spinner renders as one
// unreadable line of overwritten fragments, so the non-TTY path emits NO '\r'
// and no escape sequences at all — pinned by
// TestPreviewUpPipedProgressHasNoTerminalControlBytes. Making write/finish/
// clear no-ops off a terminal rather than guarding each call site is what makes
// that property structural: there is no way to reach an escape sequence from
// the plain path.
type progress struct {
	w   io.Writer
	tty bool
	// lineLen is the display width of whatever is currently drawn on the
	// progress line, in runes. It is what the next redraw pads over so a
	// shorter line does not leave the tail of a longer one behind.
	lineLen int
}

// write redraws the progress line in place.
func (p *progress) write(text string) {
	if !p.tty {
		return
	}
	outf(p.w, "\r%s%s", text, p.padding(text))
	p.lineLen = utf8.RuneCountInString(text)
}

// finish replaces the progress line with a final one and moves to the next,
// leaving completed steps in the scrollback.
func (p *progress) finish(text string) {
	if !p.tty {
		return
	}
	outf(p.w, "\r%s%s\n", text, p.padding(text))
	p.lineLen = 0
}

// clear erases the progress line, so whatever prints next (URLs, an error on
// stderr) does not land beside a half-drawn spinner.
func (p *progress) clear() {
	if !p.tty || p.lineLen == 0 {
		return
	}
	outf(p.w, "\r%s\r", strings.Repeat(" ", p.lineLen))
	p.lineLen = 0
}

// padding is the blanks needed to cover the tail of a longer previous line.
//
// Runes, not bytes, for the same reason the table's pad counts runes: every
// glyph on this line that is not ASCII — the braille spinner, the "✓", the em
// dash — is three bytes and one column, so a byte count would over-pad by six
// on every redraw and leave a drifting trail of spaces.
func (p *progress) padding(text string) string {
	if n := p.lineLen - utf8.RuneCountInString(text); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

// builtCommits maps each member of a preview to the commit its current image
// was built from, or nil when that is unknowable.
//
// nil means "can't tell", and there are three ways to get there: no record at
// all, no builtImages on it (the field is omitempty, so it is plain absent on
// an older bifrost, on a preview created before it existed, and when every
// entry was malformed enough for bifrost to drop it), or no entry in it with a
// usable commit.
//
// `commit` identifies a build; `shortSha` is derived from it — it is the image
// tag suffix — and is deliberately not what is compared. An entry with no
// commit is skipped rather than compared as empty-equals-empty, which would
// read as "reused".
func builtCommits(rec *previewapi.Record) map[string]string {
	if rec == nil || len(rec.BuiltImages) == 0 {
		return nil
	}
	commits := make(map[string]string, len(rec.BuiltImages))
	for member, built := range rec.BuiltImages {
		if built.Commit == "" {
			continue
		}
		commits[member] = built.Commit
	}
	if len(commits) == 0 {
		return nil
	}
	return commits
}

// rebuildSummary is the one line an `up` adds saying what it actually rebuilt,
// or "" to say nothing.
//
// A re-run against an unchanged branch reuses every member's image and finishes
// in seconds, which otherwise looks exactly like a full rebuild — Step cannot
// tell them apart, since it is cleared on success and a reused-everything run
// can finish inside a single poll interval. Comparing the pre-POST snapshot
// against the finished record is the only way to know from the client side.
//
// Only members present on BOTH sides are compared. bifrost drops malformed
// entries individually, so the map can legitimately be present but incomplete:
// a member on one side only is "can't tell" for that member, never "changed",
// and a comparison with nothing in common is "can't tell" outright.
//
// Silence, not a guess, whenever either side is unknowable — including for a
// brand-new preview, which has no "before" and is a creation rather than a
// no-op. Unlike a silently dropped --ttl this is never warned about: it is
// cosmetic, and a confident "nothing rebuilt" in front of a deploy that
// changed everything is worse than saying nothing at all.
func rebuildSummary(before, after *previewapi.Record) string {
	beforeCommits, afterCommits := builtCommits(before), builtCommits(after)
	var rebuilt []string
	comparable := false
	for member, commit := range afterCommits {
		was, ok := beforeCommits[member]
		if !ok {
			continue
		}
		comparable = true
		if was != commit {
			rebuilt = append(rebuilt, member)
		}
	}
	// One check for every way of not knowing, rather than an unknowable-side
	// guard in front of it: a nil map on either side produces no comparable
	// member, exactly as two populated maps with nothing in common do, so
	// there is nothing an earlier nil test would catch that this does not.
	if !comparable {
		return ""
	}
	if len(rebuilt) == 0 {
		return "  nothing rebuilt — all images reused"
	}
	sort.Strings(rebuilt)
	return "  rebuilt: " + strings.Join(rebuilt, ", ")
}
