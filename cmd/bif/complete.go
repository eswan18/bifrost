// Shell tab-completion for bif: the hidden `bif __complete`, which turns a
// partial command line into candidates, and `bif completion <shell>`, which
// prints the shim that calls it.
//
// The split is the whole design. The shims are three lines of shell that know
// nothing about bif beyond how to hand it the words typed so far, and every
// candidate — the command list, the fleet, the flags — is computed by the
// binary being completed. A static script with the service names baked in
// would go stale the moment registry.yaml changed, and would keep confidently
// offering a service that no longer exists.
//
// This file is scanned by TestNoBifrostServerDependency like every other
// non-preview file here, so it may not import internal/previewclient or
// net/http. `preview down`'s tag lookup therefore goes through the previewAPI
// seam run() already threads in — completion borrows preview's connection, it
// does not open one of its own.
package main

import (
	"context"
	_ "embed"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/eswan18/bifrost/internal/registry"
)

// completionUsage is what `bif completion` prints when it cannot tell which
// shim to emit.
const completionUsage = "Usage: bif completion <zsh|bash>"

// The shims live as real .zsh/.bash files rather than Go string literals so
// they can be read, edited and syntax-checked as shell. TestShimsAreValidShell
// runs `zsh -n` and `bash -n` over exactly these bytes: a shim that does not
// parse breaks the shell startup of anyone who has sourced it, and no other
// test in this package would notice.
//
//go:embed completion.zsh
var zshShim string

//go:embed completion.bash
var bashShim string

// completionCmd implements `bif completion <shell>`, printing a shim to
// stdout. It is the visible half of the feature and reports an unusable
// invocation the way every other command here does: name what was wrong, then
// the usage, exit 1.
func completionCmd(args []string, stdout io.Writer) int {
	if len(args) != 1 {
		outln(stdout, completionUsage)
		return 1
	}
	switch shell := args[0]; shell {
	case "zsh":
		outf(stdout, "%s", zshShim)
	case "bash":
		outf(stdout, "%s", bashShim)
	default:
		outf(stdout, "Unknown shell: %s\n", shell)
		outln(stdout, completionUsage)
		return 1
	}
	return 0
}

// completeCmd implements the hidden `bif __complete <words...>`: argv in,
// candidates out, one per line, exit 0 whatever happens.
//
// It is handed stdout and nothing else — no stderr, no error return — because
// the shims splice its stdout straight into the candidate list. A diagnostic
// printed on the wrong stream does not read as an error to the operator, it
// reads as a completion called "Error: connecting to the cluster". Not having
// the stream is a stronger guarantee than remembering not to use it, and it is
// why this command loads the registry itself instead of calling loadApps,
// which reports parse failures on stderr.
//
// Exit 0 always, for the same reason: a non-zero exit from a completion
// function is noise in the shell, and there is no failure here an operator
// pressing Tab can act on. Every failure renders as "no candidates".
func completeCmd(ctx context.Context, args []string, stdout io.Writer, dial func() previewAPI) int {
	for _, candidate := range completions(ctx, args, dial) {
		outln(stdout, candidate)
	}
	return 0
}

// topLevelCommands is what completes after bare `bif`. __complete is
// deliberately absent: it exists for the shims, an operator has no reason to
// type it, and offering it would advertise an interface this package is free
// to change. It is absent from the usage text and from the unknown-command
// listing for the same reason — see TestCompleteIsHidden.
var topLevelCommands = []string{"status", "promote", "preview", "completion"}

// flagGroup is one flag and its aliases. Grouping them is what makes "already
// given" mean the flag rather than the spelling: `bif status --quiet <TAB>`
// must not go on offering -q, which is the same flag under its other name and
// which takeFlag would strip right back out.
type flagGroup []string

var (
	statusFlags      = []flagGroup{{"-q", "--quiet"}, {"-a", "--attention"}}
	promoteFlags     = []flagGroup{{"-y", "--yes"}}
	previewUpFlags   = []flagGroup{{"--ttl"}, {"--no-wait"}, {"--auto-update"}}
	previewDownFlags = []flagGroup{{"-y", "--yes"}}
)

// previewTagTimeout bounds the ONE network call any completion is allowed to
// make (see previewTags). A var rather than a const so the tests can prove the
// bound is applied without waiting for it, exactly as buildLookupTimeout is.
//
// The value is chosen from what a keystroke can afford, not from what the
// request needs: the shell draws nothing while a completion function runs, so
// the entire budget is time the operator spends staring at an unresponsive
// Tab. Past roughly half a second that reads as a hang rather than as latency.
// 400ms leaves room for a warm round trip to bifrost and stops well short of
// the point where the shell feels stuck. Measured end to end against prod,
// that round trip is 90-140ms including process start.
//
// It fits only because previewclient caches its bearer token on disk. Reading
// the token means shelling out to gcloud, which costs 450-780ms of Python
// startup before a single byte goes to bifrost — no deadline a Tab press can
// afford would ever have covered it, and this completion returned empty every
// single time until the cache existed. The cache is filled by the `bif preview`
// commands an operator runs anyway, which have no deadline; a Tab press reads
// it and never pays to fill it. So the one case that still completes to
// nothing is a machine that has not run a preview command in 12 hours, which
// is the case where `bif preview list` was going to be the next thing typed
// regardless. See internal/previewclient/tokencache.go.
var previewTagTimeout = 400 * time.Millisecond

// completions turns the words typed so far into the candidates for the last
// one.
//
// The contract with the shims: words is everything after `bif` up to AND
// INCLUDING the word being completed, which is "" when the cursor sits after a
// space. So the last element is the prefix to filter on and the rest is
// context. Filtering here rather than in the shims means the two shims stay
// identical in behaviour and testable in Go — bash and zsh each have their own
// idea of what a "prefix" is, and neither is bif's.
func completions(ctx context.Context, words []string, dial func() previewAPI) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	typed, prefix := words[:len(words)-1], words[len(words)-1]

	var out []string
	for _, candidate := range candidatesAt(ctx, typed, prefix, dial) {
		if strings.HasPrefix(candidate, prefix) {
			out = append(out, candidate)
		}
	}
	return out
}

// candidatesAt returns everything valid in the position after typed, before
// the prefix filter. prefix is passed in only so the one networked branch can
// see that it is being asked for a flag and skip the call.
func candidatesAt(ctx context.Context, typed []string, prefix string, dial func() previewAPI) []string {
	if len(typed) == 0 {
		return topLevelCommands
	}
	switch typed[0] {
	case "status":
		return appsAndFlags(typed[1:], statusFlags)
	case "promote":
		return appsAndFlags(typed[1:], promoteFlags)
	case "preview":
		return previewCandidates(ctx, typed[1:], prefix, dial)
	case "completion":
		if len(typed) == 1 {
			return []string{"zsh", "bash"}
		}
		return nil
	default:
		// An unrecognised command completes to nothing rather than falling
		// back to the fleet: `bif stauts <TAB>` offering service names would
		// suggest the typo is a command that takes them.
		return nil
	}
}

// appsAndFlags is the candidate set for `bif status` and `bif promote`: the
// embedded fleet plus that command's flags.
//
// Names already on the line are omitted. Both commands take several services
// and dedupe them (resolveApps), so a name already typed is a candidate that
// can only produce a no-op — noise in the list, and worse, a list that stops
// shrinking as the operator narrows what they want.
func appsAndFlags(typed []string, groups []flagGroup) []string {
	var out []string
	for _, app := range fleetApps() {
		if !slices.Contains(typed, app) {
			out = append(out, app)
		}
	}
	return append(out, unusedFlags(typed, groups)...)
}

// fleetApps is the embedded service registry's names, or nothing if it will
// not parse. Silent, because this runs under a keypress: registry.yaml is
// compiled into the binary, so a failure here is a broken build rather than a
// broken environment, and there is no stream to report it on that would not
// become a completion candidate.
func fleetApps() []string {
	reg, err := registry.Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}

// unusedFlags returns every alias of every flag not already present in typed.
func unusedFlags(typed []string, groups []flagGroup) []string {
	var out []string
	for _, group := range groups {
		if slices.ContainsFunc(group, func(alias string) bool { return slices.Contains(typed, alias) }) {
			continue
		}
		out = append(out, group...)
	}
	return out
}

// previewCandidates completes below `bif preview`.
func previewCandidates(ctx context.Context, typed []string, prefix string, dial func() previewAPI) []string {
	if len(typed) == 0 {
		return []string{"list", "up", "down"}
	}
	switch typed[0] {
	case "up":
		rest := typed[1:]
		// The value of --ttl is a duration bif has no list of; offering flags
		// there would offer them in the one position they cannot go.
		if len(rest) > 0 && rest[len(rest)-1] == "--ttl" {
			return nil
		}
		// The branch itself is not completed: bif has no branch list that does
		// not cost a git or network read, and the shell's own filename
		// completion is not it.
		return unusedFlags(rest, previewUpFlags)
	case "down":
		return previewDownCandidates(ctx, typed[1:], prefix, dial)
	case "list":
		// `bif preview list` takes nothing and refuses leftovers.
		return nil
	default:
		return nil
	}
}

// previewDownCandidates completes below `bif preview down`, and is the only
// position in this file that may touch the network.
//
// The tag is asked for exactly once: after one non-flag token is on the line
// the tag has been given (previewDownCmd takes exactly one), and after that
// only -y/--yes is left to offer. A prefix that starts with "-" is asking for
// a flag, so the call is skipped there too — a network round trip whose every
// candidate is about to be filtered out is a Tab that hangs for nothing.
func previewDownCandidates(ctx context.Context, typed []string, prefix string, dial func() previewAPI) []string {
	flags := unusedFlags(typed, previewDownFlags)
	if strings.HasPrefix(prefix, "-") || slices.ContainsFunc(typed, func(w string) bool { return !strings.HasPrefix(w, "-") }) {
		return flags
	}
	return append(previewTags(ctx, dial), flags...)
}

// previewTags asks bifrost for the live preview tags, the same read `bif
// preview list` makes, and returns nothing at all if anything goes wrong.
//
// Completing tags is worth a network call because they are branch-derived and
// awkward to type; it is worth it only if it can never make Tab feel stuck or
// put something that is not a candidate into the candidate list. So:
//
//   - a deadline of previewTagTimeout, applied here rather than trusted to the
//     client, whose own timeout is 30s;
//   - every failure — deadline, transport, a gcloud login that has expired, a
//     401, a bifrost that 404s the route — returns nil, silently. There is no
//     stream to complain on: completeCmd holds stdout and nothing else, and
//     stdout is the candidate list.
//   - a recover, because "any error at all" includes the client panicking. An
//     operator's Tab is not the place to find out.
func previewTags(ctx context.Context, dial func() previewAPI) (tags []string) {
	defer func() {
		if recover() != nil {
			tags = nil
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, previewTagTimeout)
	defer cancel()

	records, err := dial().List(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Tag)
	}
	return out
}
