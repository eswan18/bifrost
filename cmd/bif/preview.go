package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eswan18/bifrost/internal/previewapi"
	"github.com/eswan18/bifrost/internal/previewclient"
)

// previewAPI is the slice of previewclient.Client that `bif preview` uses. It
// is an interface for the usual reason — the commands are testable without a
// server — but note the preview tests drive the REAL client against an
// httptest server instead, because half of what is worth asserting (the
// User-Agent, the status-code mapping, that a declined teardown sends no
// request) is invisible from above this seam.
type previewAPI interface {
	List(ctx context.Context) ([]previewapi.Record, error)
	Delete(ctx context.Context, tag string) error
}

// previewclient.Client has to satisfy previewAPI, or the seam would be shaped
// like an interface the real client doesn't implement.
var _ previewAPI = (*previewclient.Client)(nil)

// This file is the ONE file in cmd/bif allowed to reach the network, and
// main_test.go's TestNoBifrostServerDependency enforces exactly that: every
// other non-test file here is checked against a ban on net/http,
// internal/previewclient, internal/web and internal/auth. Preview is an HTTP
// client by design — bifrost owns the busy set, the cluster write credentials
// and the Neon and Cloud Build tokens, so there is nothing here to do locally
// — while status and promote must keep working with bifrost down.

const (
	previewUsage     = "Usage: bif preview <list|up|down> ..."
	previewListUsage = "Usage: bif preview list"
	previewDownUsage = "Usage: bif preview down <tag> [-y/--yes]"
)

// dialPreview builds the real client. It lives here rather than beside
// dialCluster in main.go because main.go may not import internal/previewclient
// — that is the boundary TestNoBifrostServerDependency draws, and moving this
// one line would put the preview API's constructor on the same page as
// status's dispatch.
func dialPreview() previewAPI { return previewclient.New() }

// previewCmd dispatches `bif preview <subcommand>`, mirroring ib.py's main().
func previewCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, dial func() previewAPI) int {
	if len(args) == 0 {
		outln(stdout, previewUsage)
		return 1
	}
	switch sub := args[0]; sub {
	case "list":
		return previewListCmd(ctx, args[1:], stdout, stderr, dial)
	case "down":
		return previewDownCmd(ctx, args[1:], stdin, stdout, stderr, dial)
	case "up":
		// Deliberately still the Python's: `up` carries the progress
		// rendering, the TTY spinner and a dozen behaviours each earned by a
		// real bug, and it is its own task rather than a rider on this one.
		outln(stdout, "bif preview up is not ported to Go yet — use `ib preview up` (infra/ib.py) for it.")
		return 1
	default:
		outf(stdout, "Unknown preview subcommand: %s\n", sub)
		outln(stdout, "Available subcommands: list, up, down")
		return 1
	}
}

// previewListCmd implements `bif preview list`.
//
// Note what it does NOT do with a 404: nothing special, which is the correct
// treatment here and the opposite of `up`'s. The collection endpoint has
// nothing to be missing about — an empty fleet is a 200 with an empty list —
// so a 404 is the route itself not being there (a bifrost predating the
// preview API, or something in front of it). That is fatal in the ordinary
// way, and previewclient.NotFoundError already renders as "Preview API error
// 404: ...", so it falls through the same print as every other failure.
// Swallowing it would print an empty table claiming there are no previews.
func previewListCmd(ctx context.Context, args []string, stdout, stderr io.Writer, dial func() previewAPI) int {
	// ib.py ignores leftover tokens here; this rejects them. Silently
	// accepting an argument nobody reads is what let `--ttl=8h` through on
	// `up`, producing a preview that never expired — so the whole preview
	// surface refuses what it will not act on.
	if len(args) > 0 {
		outln(stdout, previewListUsage)
		return 1
	}
	records, err := dial().List(ctx)
	if err != nil {
		outf(stderr, "%v\n", err)
		return 1
	}
	renderPreviewList(stdout, records, time.Now())
	return 0
}

// previewDownCmd implements `bif preview down <tag>`.
//
// A 404 IS an error here, unlike for `up` and for the poll loop: nobody types
// a teardown at a tag they don't believe exists, so "no such preview" is
// either a typo or a preview already gone. Swallowing it would report a
// teardown that never happened, which is worse than a spurious failure.
func previewDownCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, dial func() previewAPI) int {
	args, yes := takeFlag(args, "-y", "--yes")
	// Exactly one tag. ib.py takes args[0] and drops the rest, so
	// `ib preview down old-tag new-tag` tears down one of them without a word
	// about the other; see previewListCmd for why this refuses instead.
	if len(args) != 1 {
		outln(stdout, previewDownUsage)
		return 1
	}
	tag := args[0]

	// The confirmation gates the request, not just the message: declining
	// must not touch bifrost at all, so dial() is not even called above here.
	if !yes && !confirm(stdin, stdout, fmt.Sprintf("Tear down preview %s? [y/N] ", tag)) {
		outln(stdout, "Aborted.")
		return 0
	}

	if err := dial().Delete(ctx, tag); err != nil {
		var missing *previewclient.NotFoundError
		if errors.As(err, &missing) {
			// ib.py points at itself in this hint; this points at the binary
			// that implements it, the same substitution `bif status`'s
			// promote hint makes.
			outf(stderr, "No preview %s — nothing to tear down. `bif preview list` shows what exists.\n", tag)
			return 1
		}
		outf(stderr, "%v\n", err)
		return 1
	}
	outf(stdout, "Tearing down %s.\n", tag)
	return 0
}

// renderPreviewList prints ib.py's preview table. `now` is a parameter so the
// EXPIRES column is testable without a clock.
func renderPreviewList(w io.Writer, records []previewapi.Record, now time.Time) {
	if len(records) == 0 {
		outln(w, "No preview environments.")
		return
	}
	outln(w, previewRow("TAG", "BRANCH", "PHASE", "HEALTH", "EXPIRES", "AUTO", "APPS"))
	for _, p := range records {
		outln(w, previewRow(
			p.Tag,
			orDash(p.Branch),
			previewPhaseCell(p),
			orDash(p.Health),
			previewExpiresCell(p, now),
			previewAutoCell(p),
			strings.Join(p.Apps, ","),
		))
	}
}

// previewRow lays out one line of the table, header included so the widths
// cannot drift apart. The trailing APPS field is unpadded, which means a row
// for a preview with no apps ends in a space — ib.py's does too, and matching
// it byte for byte is the point.
func previewRow(tag, branch, phase, health, expires, auto, apps string) string {
	return pad(tag, 24) + " " + pad(branch, 24) + " " + pad(phase, 10) + " " +
		pad(health, 12) + " " + pad(expires, 10) + " " + pad(auto, 5) + " " + apps
}

// pad left-aligns s in a field of width runes, like Python's `f"{s:<width}"`.
//
// Runes, not bytes, and that is not pedantry: the AUTO column's "✓" is three
// bytes and one character, so fmt's "%-5s" would pad it to three spaces where
// Python pads it to four, and every column after it would shift on exactly the
// rows an auto-updating preview appears in. Neither pads a value already at or
// over the width, so a long tag pushes its row out rather than being cut.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// orDash renders an absent string field as "-".
//
// A tag that is busy with no live namespace behind it (a create still being
// provisioned, or one whose namespace has finished tearing down) arrives as a
// synthesized record with no branch, no apps, no URLs and no health. Every
// cell falls back rather than printing Go's zero values, so that row reads as
// a clean row of dashes instead of leaking "<nil>" or "0001-01-01".
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// previewPhaseCell marks busy on the PHASE cell rather than in a column of its
// own: `creating*`, the same "something is still moving" signal `status -q`
// uses for a mid-deploy image mismatch. That is the fix for the training-plans
// incident, where a stale, unmarked ready-looking table made a still-running
// server-side `up` look finished.
//
// The bare word "busy" is for a record with no phase at all — there is nothing
// server-side to report a phase on — rather than a blank cell. Note that
// bifrost as it stands always sends a phase (previewapi.Record.Phase has no
// omitempty, and a synthesized record carries previewapi.PhaseBusy), so a
// synthesized record renders "busy*" and this branch answers only an older or
// non-bifrost server that omits the field. ib.py's docstring claims otherwise;
// its code, which is the oracle, is what is ported here.
//
// Go collapses one distinction Python draws: an absent phase and an explicit
// empty one both arrive as "". Python renders the explicit empty string as a
// lone "*" (or ""), which is a blank cell by another name, so treating both as
// absent is strictly better and only reachable from a malformed response.
func previewPhaseCell(p previewapi.Record) string {
	if p.Phase == "" {
		if p.Busy {
			return previewapi.PhaseBusy
		}
		return "-"
	}
	if p.Busy {
		return p.Phase + "*"
	}
	return p.Phase
}

// previewExpiresCell is the remaining-TTL cell. Most previews carry no TTL —
// that is the default by design — and ExpiresAt is omitzero server-side, so
// absent is the common case and renders "-", never a zero timestamp.
func previewExpiresCell(p previewapi.Record, now time.Time) string {
	if p.ExpiresAt.IsZero() {
		return "-"
	}
	return formatTTLRemaining(p.ExpiresAt, now)
}

// previewAutoCell is the AUTO column: a "✓" for a preview that follows its
// branch, "-" otherwise, the same convention as EXPIRES.
func previewAutoCell(p previewapi.Record) string {
	if p.AutoUpdate {
		return "✓"
	}
	return "-"
}

// formatTTLRemaining renders time left until expiry, like "8h0m" or "3d2h".
//
// bifrost's sweep for reaping past-due previews runs hourly, so a preview can
// sit expired-but-alive for up to an hour; that whole window is "expired",
// which is the accurate state, rather than a misleading negative duration.
//
// This is not internal/web's expiresIn. That one renders the same underlying
// fact for the Previews tab ("expires in 8h", "" for no TTL) where prose and a
// coarse single unit are right; this one is a fixed-width table cell whose
// format is ib.py's and is what anything parsing `bif preview list` sees. Two
// surfaces, two formats, and neither is a copy of the other's logic.
func formatTTLRemaining(expiresAt, now time.Time) string {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return "expired"
	}
	totalMinutes := int(remaining / time.Minute)
	if totalMinutes < 1 {
		return "<1m"
	}
	days, restOfDay := totalMinutes/(24*60), totalMinutes%(24*60)
	hours, minutes := restOfDay/60, restOfDay%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
