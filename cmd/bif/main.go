// Command bif is the deploy CLI for the fleet: see what's running in staging
// versus prod, and promote staging to prod.
//
// It is deliberately NOT a client of bifrost. `bif promote bifrost` is how
// bifrost gets recovered when bifrost is down — it went down on a spot-node
// preemption during the preview-environments work, and this is the path back.
// So `status` and `promote` read the cluster directly through client-go, and
// internal/registry compiles the service list into the binary, so even the
// fleet list needs no network. Nothing on those paths may grow a dependency on
// bifrost's HTTP API, its bearer token, or its availability. (`bif preview`,
// when it lands, is a different case: the server owns preview orchestration,
// so that one is an HTTP client by design.)
//
// Ported from infra/ib.py, which stays the reference implementation until the
// cutover. Argument handling is hand-rolled to match it: this is a
// three-command tool, and a CLI framework would be more machinery than the
// thing it drives.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/eswan18/bifrost/internal/kube"
)

// usage mirrors ib.py's module docstring, which is what it prints when run
// with no arguments.
const usage = `Deployment status and promotion helper for GKE services.

Usage:
    bif status               # Show status for all services
    bif status <app>         # Show current images for staging and prod
    bif status -q            # List out-of-sync services (* = mid-deploy)
    bif status <app> -q      # Exit 0 if in sync, 1 if not (minimal output)

Not ported yet — run these with the Python CLI:
    ib promote <app> [-y/--yes]
    ib preview <list|up|down> ...`

// argoNamespace is where the ArgoCD Applications live. `bif status` never
// touches them — it reads pods — but kube.New wants it, and `bif promote` will.
const argoNamespace = "argocd"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, dialCluster))
}

// run is main with the process edges passed in — argv, the output streams, the
// exit status as a return value, and the cluster connection as a function — so
// tests can drive whole commands end to end.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, connect func() (podLister, error)) int {
	if len(args) == 0 {
		outln(stdout, usage)
		return 1
	}

	switch cmd := args[0]; cmd {
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr, connect)
	case "promote", "preview":
		// Named rather than folded into the unknown-command case: the
		// operator typed a real bif command, and "unknown command" would send
		// them looking for a typo instead of at the other binary.
		outf(stdout, "bif %s is not ported to Go yet — use `ib %s` (infra/ib.py) for it.\n", cmd, cmd)
		return 1
	default:
		outf(stdout, "Unknown command: %s\n", cmd)
		outln(stdout, "Available commands: status, promote, preview")
		return 1
	}
}

// dialCluster opens a direct connection to the Kubernetes API — in-cluster
// config when there is one, otherwise ~/.kube/config. Note what is absent: no
// bifrost URL, no bearer token, no HTTP client for the service this tool
// manages.
func dialCluster() (podLister, error) {
	c, err := kube.New(argoNamespace)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// outf and outln write to a command's output stream and deliberately discard
// the write error. A CLI's stdout write fails for one reason worth naming —
// EPIPE, from `bif status -q | head -1` — and the right answer to a closed pipe
// is to finish quietly, not to report a problem with the cluster. Discarding
// it once here beats an unread error at every call site.
func outf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func outln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// takeFlag removes every occurrence of the given aliases from args and reports
// whether any was present. This reproduces ib.py's `"-q" in args`-then-filter
// idiom, which accepts the flag anywhere in the argument list — `bif status -q
// bifrost` and `bif status bifrost -q` are the same command, and scripts rely
// on both.
func takeFlag(args []string, aliases ...string) ([]string, bool) {
	found := false
	out := make([]string, 0, len(args))
	for _, arg := range args {
		matched := false
		for _, alias := range aliases {
			if arg == alias {
				matched = true
				break
			}
		}
		if matched {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}
