package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eswan18/bifrost/internal/preview"
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
	Get(ctx context.Context, tag string) (*previewapi.Record, error)
	Create(ctx context.Context, req previewapi.CreateRequest) (*previewapi.Record, error)
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
	previewUpUsage   = "Usage: bif preview up <branch> [--ttl <duration>] [--no-wait] [--auto-update]"
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
		return previewUpCmd(ctx, args[1:], stdout, stderr, dial, defaultPreviewEnv(stdout))
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

// upArgs is the parsed form of everything after `bif preview up`.
//
// ttl and ttlSet are separate because "--ttl not given" and "--ttl given as an
// empty string" are different requests: the first means the preview never
// expires and warns about nothing, the second is sent to bifrost for it to
// adjudicate and still warns if no expiry comes back. ib.py draws the same
// distinction with None versus "".
type upArgs struct {
	branch     string
	ttl        string
	ttlSet     bool
	autoUpdate bool
	noWait     bool
}

// parseUpArgs parses `bif preview up`'s arguments, reporting false when the
// invocation is unusable.
//
// The rule is ib.py's, and the strictness is the whole point: after --no-wait,
// --auto-update and `--ttl <value>` are pulled out, EXACTLY ONE token must
// remain, and it is the branch. Anything else — a second branch, a typo'd
// flag, or the equals form of a real one — is refused rather than ignored.
//
// That last case is why this exists. `--ttl=8h` is not parsed here (ib.py
// doesn't parse it either, and matching the oracle matters more than accepting
// more syntax), so before the leftover check it sat unconsumed in the argument
// list and was silently dropped: the command explicitly asked for an expiry,
// exited 0, and produced a preview that would never expire. The same shape
// swallows `--auto-update=true`. A refused command costs one retry; a silently
// ignored flag costs a preview nobody reclaims.
func parseUpArgs(args []string) (upArgs, bool) {
	var out upArgs
	args, out.noWait = takeFlag(args, "--no-wait")
	args, out.autoUpdate = takeFlag(args, "--auto-update")

	// Only the FIRST --ttl and its value are consumed, so a repeated flag
	// leaves a token behind and fails the check below rather than letting one
	// of two conflicting values win silently.
	for i, arg := range args {
		if arg != "--ttl" {
			continue
		}
		if i+1 >= len(args) {
			return upArgs{}, false
		}
		out.ttl, out.ttlSet = args[i+1], true
		args = append(args[:i:i], args[i+2:]...)
		break
	}

	if len(args) != 1 {
		return upArgs{}, false
	}
	out.branch = args[0]
	return out, true
}

// previewUpCmd implements `bif preview up <branch>`.
//
// The shape is ib.py's preview_up: look the preview up BEFORE asking bifrost
// to build anything, POST, then either return (--no-wait) or poll to a
// terminal phase. The pre-POST lookup buys two things that are only knowable
// beforehand — whether this run creates or updates (the first line is honest
// about which instead of always claiming a create) and what each member's
// image was built from, so the finished record can be compared against it. It
// costs one cheap GET next to a create that takes minutes, and a brand-new
// preview simply 404s it.
func previewUpCmd(ctx context.Context, args []string, stdout, stderr io.Writer, dial func() previewAPI, env previewEnv) int {
	opts, ok := parseUpArgs(args)
	if !ok {
		outln(stdout, previewUpUsage)
		return 1
	}
	api := dial()

	// preview.TagForBranch, called directly — not a copy of its rule. ib.py
	// carried a hand-written mirror of it precisely here, because the pre-POST
	// lookup needs a tag before the server has supplied one, and a mirror is a
	// second implementation of the mapping that decides which namespace an
	// operator's branch lands in. This is where that duplication dies: the CLI
	// and the server now derive the tag from the same function, so they cannot
	// disagree about it.
	//
	// The safety property that made the mirror tolerable is kept anyway. The
	// POST response is authoritative, and if it names a different tag the
	// snapshot below is dropped rather than trusted — a wrong guess costs one
	// wasted GET and today's output, never a wrong verb or a false
	// "nothing rebuilt".
	expectedTag := preview.TagForBranch(opts.branch)
	var before *previewapi.Record
	if expectedTag != "" {
		rec, err := lookupPreview(ctx, api, expectedTag)
		if err != nil {
			// Not a 404 — lookupPreview absorbs those. A 500 or an
			// unreachable bifrost before anything has been asked for is
			// fatal, exactly as it is in ib.py, rather than silently
			// downgrading to a create nobody asked for.
			outf(stderr, "%v\n", err)
			return 1
		}
		before = rec
	}
	if before != nil && before.Branch != "" && before.Branch != opts.branch {
		// The tag is a different branch's preview. Whatever this run turns out
		// to be it is not an update of THAT, and its images are none of our
		// business, so the snapshot goes rather than describing someone else's
		// preview. The checks below still refuse the run itself.
		before = nil
	}

	created, err := api.Create(ctx, previewapi.CreateRequest{
		Branch:     opts.branch,
		TTL:        opts.ttl,
		AutoUpdate: opts.autoUpdate,
	})
	if err != nil {
		// A 404 stays fatal here, unlike everywhere else in this command: the
		// create endpoint cannot answer "no such preview" — creating one is
		// the point — so a 404 is the route missing, not an absence.
		// NotFoundError.Error() already reads "Preview API error 404: ...",
		// which is what ib.py prints for it.
		outf(stderr, "%v\n", err)
		return 1
	}
	tag := created.Tag

	// Checked before anything is printed. bifrost's tag derivation is
	// many-to-one, and its server-side collision refusal happens in a detached
	// goroutine AFTER this 202 landed, so the POST response can already
	// describe someone else's ready preview. Catching it here — ahead of the
	// "Creating preview..." line, let alone the spinner — means a doomed
	// request never looks like it is making progress.
	if msg := branchMismatch(opts.branch, tag, created); msg != "" {
		outln(stderr, msg)
		return 1
	}
	if tag != expectedTag {
		before = nil
	}

	verb := "Creating"
	if before != nil {
		verb = "Updating"
	}
	outf(stdout, "%s preview %s from %s...\n", verb, tag, opts.branch)

	if opts.noWait {
		// No poll loop to piggyback the checks on, so this does one extra GET
		// rather than skipping verification: a flag an old server silently
		// dropped, or a tag collision the POST response did not reveal, is
		// exactly what a --no-wait/CI caller is least likely to notice, and one
		// request is cheap next to the POST that just ran.
		record, err := lookupPreview(ctx, api, tag)
		if err != nil {
			outf(stderr, "%v\n", err)
			return 1
		}
		// A 404 (record == nil) is overwhelmingly the common case here: this
		// GET goes out with ZERO delay after a POST whose work is still queued
		// behind several sequential GitHub calls, so the namespace does not
		// exist yet. There is nothing to check and nothing wrong — running the
		// checks against an empty record instead is what made --no-wait report
		// every flag as silently dropped, and reading the 404 as failure is
		// what made it abort outright on a create that was proceeding fine.
		if record != nil {
			if msg := branchMismatch(opts.branch, tag, record); msg != "" {
				outln(stderr, msg)
				return 1
			}
			warnIfTTLDropped(stderr, opts, record, tag)
			warnIfAutoUpdateDropped(stderr, opts, record, tag)
		}
		outln(stdout, "Not waiting. Check with: bif preview list")
		return 0
	}

	phase := created.Phase
	if phase == "" {
		phase = "creating"
	}
	record, ok := waitForPreview(waitParams{
		tag:    tag,
		phase:  phase,
		branch: opts.branch,
		poll:   func() (*previewapi.Record, error) { return lookupPreview(ctx, api, tag) },
	}, stdout, stderr, env)
	if !ok {
		return 1
	}
	if summary := rebuildSummary(before, record); summary != "" {
		outln(stdout, summary)
	}
	warnIfTTLDropped(stderr, opts, record, tag)
	warnIfAutoUpdateDropped(stderr, opts, record, tag)
	return 0
}

// lookupPreview fetches one record, reporting a missing one as (nil, nil).
//
// This is the tolerant reading of a 404, and it belongs to `up` alone. bifrost
// answers POST /api/previews with a 202 well before the namespace exists, so
// for `up`'s pre-POST lookup and for every poll, "not found" means "not
// created yet" and is waited through. `down` keeps treating the identical
// status as the error it is there — see previewDownCmd. Same status, opposite
// meanings, decided by who asked.
func lookupPreview(ctx context.Context, api previewAPI, tag string) (*previewapi.Record, error) {
	rec, err := api.Get(ctx, tag)
	if err != nil {
		var missing *previewclient.NotFoundError
		if errors.As(err, &missing) {
			return nil, nil
		}
		return nil, err
	}
	return rec, nil
}

// branchMismatch returns the message to print when `tag` already belongs to a
// branch other than the requested one, or "" when it does not.
//
// bifrost derives a preview's tag from its branch name and that mapping is
// many-to-one — slash/dash/underscore variants fold together, long names
// truncate to the same 30 characters — so a create for one branch can land on a
// tag another branch's preview already owns. bifrost refuses that Up
// server-side (ErrTagCollision), but only from inside the detached goroutine
// that runs after the 202, so the refusal never reaches this CLI: polling the
// tag afterward just shows the OTHER branch's preview looking perfectly
// healthy. This is the client-side backstop, and every preview record carries
// the branch its namespace was actually built for.
//
// An absent or empty branch means "can't tell" — an older preview predating the
// annotation, or one from a partial run — and is deliberately NOT a mismatch. A
// false alarm here is worse than the silent-success bug this exists to catch,
// which is also why the message names both branches: the failure is only
// actionable if you can see what the tag is currently holding.
func branchMismatch(requested, tag string, rec *previewapi.Record) string {
	existing := rec.Branch
	if existing == "" || existing == requested {
		return ""
	}
	return fmt.Sprintf("Preview tag %s belongs to branch %q, not %q. Rename this branch, "+
		"or tear down the existing preview: bif preview down %s", tag, existing, requested, tag)
}

// warnIfTTLDropped warns on stderr if a requested TTL is missing from the
// record bifrost produced.
//
// Go's json.Decoder ignores unknown request fields and bifrost's handler does
// not set DisallowUnknownFields, so a server predating `ttl` support accepts
// the field, returns success, and creates a preview with no expiry at all. No
// error anywhere; the only place left to catch it is here, by checking whether
// what was asked for shows up on the result.
//
// Deliberately not an error and deliberately not an exit code: the preview
// really was created and is usable, it just lacks the lifetime that was
// requested. Failing would throw away a working preview over a missing
// convenience.
func warnIfTTLDropped(stderr io.Writer, opts upArgs, record *previewapi.Record, tag string) {
	if !opts.ttlSet || !record.ExpiresAt.IsZero() {
		return
	}
	outf(stderr, "Warning: --ttl %s was requested for %s, but the preview has no expiry "+
		"and will NOT expire automatically. This bifrost may predate --ttl support and "+
		"silently ignored it. Tear it down manually when done: bif preview down %s\n",
		opts.ttl, tag, tag)
}

// warnIfAutoUpdateDropped warns on stderr if a requested --auto-update is
// missing from the record bifrost produced.
//
// The same failure mode as warnIfTTLDropped, and a sibling rather than a shared
// helper because the message content differs — what did not take effect, and
// what that costs. autoUpdate is omitempty on the record, so absence means off.
// Also deliberately not an error: the preview is usable, it just will not
// follow the branch on its own.
func warnIfAutoUpdateDropped(stderr io.Writer, opts upArgs, record *previewapi.Record, tag string) {
	if !opts.autoUpdate || record.AutoUpdate {
		return
	}
	outf(stderr, "Warning: --auto-update was requested for %s, but the preview has no "+
		"autoUpdate flag and will NOT follow the branch. This bifrost may predate "+
		"--auto-update support and silently ignored it. Re-run manually to pick up new "+
		"commits: bif preview up %s --auto-update\n", tag, opts.branch)
}

// waitParams is what the poll loop needs about the preview it is waiting on.
type waitParams struct {
	tag    string
	phase  string
	branch string
	// poll fetches the current record, or (nil, nil) when bifrost has no
	// namespace behind the tag yet.
	poll func() (*previewapi.Record, error)
}

// waitForPreview polls until the preview reaches ready, failed or terminating,
// rendering progress as it goes. It returns the final record, or false when it
// has already reported a failure on stderr.
//
// Three rendering modes, all of them ib.py's. On a terminal it redraws one line
// with a spinner, the current step and elapsed time, finalising each completed
// step with a ✓ so the transcript reads as a checklist. Piped, it prints one
// plain line per step change and nothing else. Against a server that reports no
// steps at all — an older bifrost, or a phase like ready that legitimately
// carries none — both degrade to printing only when the phase changes.
//
// Every polled record is checked against the requested branch before anything
// else, and on EVERY poll rather than just the first: the record backing a tag
// can only be known once bifrost's detached goroutine has actually run, so the
// collision this catches may not exist yet at the first poll.
// It takes no context of its own: the only thing it does that can block is
// p.poll, and the caller's context is already inside that closure.
func waitForPreview(p waitParams, stdout, stderr io.Writer, env previewEnv) (*previewapi.Record, bool) {
	bar := &progress{w: stdout, tty: env.isTTY}
	deadline := env.now().Add(previewWaitTimeout)
	phase := p.phase
	knownPhases := map[string]bool{"creating": true, "ready": true, "failed": true, "terminating": true}
	notedUnknownPhase := false

	// currentStep is "" when no step is being narrated. Go collapses ib.py's
	// distinction between an absent step and an explicitly empty one, which is
	// harmless: bifrost's Step is omitempty, so an empty string never reaches
	// the wire, and ib.py's handling of one would have narrated a step with no
	// name.
	currentStep := ""
	var currentStepSince time.Time
	currentStepStart := env.now()
	tick := 0

	for env.now().Before(deadline) {
		env.sleep(previewPollInterval)
		rec, err := p.poll()
		if err != nil {
			bar.clear()
			outf(stderr, "%v\n", err)
			return nil, false
		}
		if rec == nil {
			// No namespace behind the tag yet (a 404). bifrost claims the tag
			// and answers the create long before EnsureNamespace runs, so this
			// is just "the server hasn't caught up" and gets the same treatment
			// as an unrecognized phase: keep waiting. Nothing is printed or
			// cleared — a spinner line, if drawn, is simply redrawn with a
			// fresh elapsed time on the next poll that does have a record.
			continue
		}
		if msg := branchMismatch(p.branch, p.tag, rec); msg != "" {
			bar.clear()
			outln(stderr, msg)
			return nil, false
		}

		newPhase := rec.Phase
		if rec.Step == "" {
			bar.clear()
			currentStep = ""
			if newPhase != phase {
				outf(stdout, "  %s\n", newPhase)
			}
		} else {
			if rec.Step != currentStep {
				if currentStep != "" && env.isTTY {
					elapsed := stepElapsed(currentStepSince, currentStepStart, env.now())
					bar.finish(fmt.Sprintf("  ✓ %s — %s", currentStep, formatElapsed(elapsed)))
				}
				currentStep = rec.Step
				currentStepStart = env.now()
				if !env.isTTY {
					outf(stdout, "  %s\n", rec.Step)
				}
			}
			currentStepSince = rec.StepSince
			if env.isTTY {
				elapsed := stepElapsed(currentStepSince, currentStepStart, env.now())
				bar.write(fmt.Sprintf("  %s %s — %s", spinnerFrame(tick), rec.Step, formatElapsed(elapsed)))
				tick++
			}
		}
		phase = newPhase

		switch newPhase {
		case "ready":
			bar.clear()
			for _, app := range sortedKeysOf(rec.URLs) {
				outf(stdout, "  %s: %s\n", app, rec.URLs[app])
			}
			return rec, true
		case "failed":
			// Deliberately no bare "  failed" line to go with this, unlike the
			// step-less branch above: the message below is strictly more useful
			// and a generic phase line ahead of it would be noise.
			bar.clear()
			switch {
			case rec.Step != "" && rec.Error != "":
				outf(stderr, "Preview %s failed while %s: %s\n", p.tag, rec.Step, rec.Error)
			case rec.Error != "":
				outf(stderr, "Preview %s failed: %s\n", p.tag, rec.Error)
			default:
				outf(stderr, "Preview %s failed (phase: failed).\n", p.tag)
			}
			outln(stderr, "Check the Previews tab in bifrost for details.")
			return nil, false
		case "terminating":
			bar.clear()
			outf(stderr, "Preview %s's namespace is being torn down, so this create cannot "+
				"proceed. Retry once the teardown finishes (`bif preview list` shows current status).\n", p.tag)
			return nil, false
		}

		if !knownPhases[newPhase] && !notedUnknownPhase {
			// Once, not per poll: an unfamiliar phase is worth mentioning and
			// not worth repeating every three seconds. Polling continues
			// because a phase this CLI does not recognize is a newer bifrost,
			// not a broken one.
			outf(stdout, "  (unrecognized phase %q; continuing to poll)\n", newPhase)
			notedUnknownPhase = true
		}
	}

	bar.clear()
	outf(stderr, "Timed out waiting for %s; check `bif preview list`.\n", p.tag)
	return nil, false
}

// sortedKeysOf returns a map's keys in order, so the URLs an `up` finishes with
// print in a stable order rather than Go's randomized map order.
func sortedKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
