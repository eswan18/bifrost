// Command bif is the deploy CLI for the fleet: see what's running in staging
// versus prod, and promote staging to prod.
//
// `status` and `promote` are deliberately NOT clients of bifrost. `bif promote
// bifrost` is how bifrost gets recovered when bifrost is down — it went down on
// a spot-node preemption during the preview-environments work, and this is the
// path back. So they read the cluster directly through client-go, and
// internal/registry compiles the service list into the binary, so even the
// fleet list needs no network. Nothing on those paths may grow a dependency on
// bifrost's HTTP API, its bearer token, or its availability.
//
// `bif preview` is the one exception, and is an HTTP client by design: the
// server owns preview orchestration — the busy set, the cluster write
// credentials, the Neon and Cloud Build tokens — so there is nothing there for
// the CLI to do locally. It is confined to preview.go and
// internal/previewclient so the exception stays an exception; see
// main_test.go's TestNoBifrostServerDependency, which enforces the boundary
// file by file.
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
	"slices"
	"strings"

	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/registry"
)

// usage mirrors ib.py's module docstring, which is what it prints when run
// with no arguments.
const usage = `Deployment status and promotion helper for GKE services.

Usage:
    bif status               # Show status for all services
    bif status <app>         # Show current images for staging and prod
    bif status -q            # List out-of-sync services (* = mid-deploy)
    bif status <app> -q      # Exit 0 if in sync, 1 if not (minimal output)
    bif promote <app>        # Compare staging vs prod, offer to promote
    bif promote <app> -y     # Promote without confirmation
    bif preview list                      # Table of preview environments, TTL remaining
    bif preview up <branch>               # Create/update, show live progress, print URLs
    bif preview up <branch> --ttl 8h      # Same, but auto-expire after 8h
    bif preview up <branch> --no-wait     # Fire and return the tag
    bif preview up <branch> --auto-update # Same, but redeploy when the branch moves
    bif preview down <tag>                # Tear down (confirm unless -y/--yes)`

// argoNamespace is where the ArgoCD Applications live. `bif status` never
// touches them — it reads pods — but kube.New wants it, and `bif promote` will.
const argoNamespace = "argocd"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, dialCluster, dialPreview))
}

// run is main with the process edges passed in — argv, the input and output
// streams, the exit status as a return value, and the two connections as
// functions — so tests can drive whole commands end to end, including
// promote's confirmation prompt.
//
// The cluster and the preview API are separate parameters rather than one
// bundle because they are separate privileges: everything reachable through
// `connect` works with bifrost down, and nothing reachable through
// `connectPreview` does. Only the preview branch below is handed the second
// one.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, connect func() (promoter, error), connectPreview func() previewAPI) int {
	if len(args) == 0 {
		outln(stdout, usage)
		return 1
	}

	switch cmd := args[0]; cmd {
	case "status":
		// status gets the read-only view of the connection, not the one
		// promote uses. See readOnly.
		return statusCmd(ctx, args[1:], stdout, stderr, readOnly(connect))
	case "promote":
		return promoteCmd(ctx, args[1:], stdin, stdout, stderr, connect)
	case "preview":
		return previewCmd(ctx, args[1:], stdin, stdout, stderr, connectPreview)
	default:
		outf(stdout, "Unknown command: %s\n", cmd)
		outln(stdout, "Available commands: status, promote, preview")
		return 1
	}
}

// readOnly narrows a cluster connection down to the pod reads `bif status`
// makes. The wider seam exists for exactly one caller — promote, which patches
// an ArgoCD Application — and this keeps it out of reach of the other: nothing
// on the status path can grow a write without changing this line first.
func readOnly(connect func() (promoter, error)) func() (podLister, error) {
	return func() (podLister, error) { return connect() }
}

// dialCluster opens a direct connection to the Kubernetes API — in-cluster
// config when there is one, otherwise ~/.kube/config. Note what is absent: no
// bifrost URL, no bearer token, no HTTP client for the service this tool
// manages.
func dialCluster() (promoter, error) {
	c, err := kube.New(argoNamespace)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// loadApps returns the fleet from the embedded registry, reporting a load
// failure on stderr. go:embed means this needs no network, which is what lets
// it retire ib.py's hand-maintained SERVICES without costing the
// works-when-bifrost-is-down property.
func loadApps(stderr io.Writer) ([]string, bool) {
	reg, err := registry.Load()
	if err != nil {
		outf(stderr, "Error: loading the service registry: %v\n", err)
		return nil, false
	}
	return reg.Names(), true
}

// validateApp mirrors ib.py's validate_app, including printing to stdout. Both
// status and promote call it before connecting, so a typo'd service name fails
// the same way whether or not the cluster is reachable — and, for promote,
// before anything could possibly be written.
func validateApp(stdout io.Writer, apps []string, app string) bool {
	if slices.Contains(apps, app) {
		return true
	}
	outf(stdout, "Unknown service: %s\n", app)
	outf(stdout, "Known services: %s\n", strings.Join(apps, ", "))
	return false
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
