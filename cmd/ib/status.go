package main

import (
	"context"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
	"github.com/eswan18/bifrost/internal/registry"
)

// podLister is the slice of kube.Client that `ib status` needs: pods, and
// nothing else. Narrow on purpose — status is a read, and a wider seam here
// would invite a later command to reach for a write through it.
type podLister interface {
	ListPods(ctx context.Context, namespace string) ([]kube.PodInfo, error)
}

// kube.Client has to satisfy podLister, or the fake the tests drive would be
// shaped like an interface the real client doesn't implement.
var _ podLister = (kube.Client)(nil)

// verdict is the tri-state ib.py's status() returns: True (in sync), False
// (out of sync), None (indeterminate). Keeping all three is the point —
// inSync and indeterminate both exit 0, so collapsing them would look
// harmless right up until something scripted the difference.
type verdict int

const (
	indeterminate verdict = iota
	inSync
	outOfSync
)

// exitCode maps the verdicts of a status run to ib's exit status, mirroring
// ib.py's main(): only a definite "out of sync" is a failure, and the mapping
// is the same whether or not -q was passed. Indeterminate — mid-deploy, no
// pods, an unparseable staging tag — exits 0, so a script asking "is there
// anything to promote?" gets "no" rather than an error when the answer isn't
// knowable yet. For the whole-fleet form, one out-of-sync service is enough to
// exit 1, and the others still print.
func exitCode(verdicts []verdict) int {
	if slices.Contains(verdicts, outOfSync) {
		return 1
	}
	return 0
}

// statusCmd implements all four forms of `ib status`.
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer, connect func() (podLister, error)) int {
	args, quiet := takeFlag(args, "-q", "--quiet")

	// The service list comes from the embedded registry, not a constant. That
	// is what retires ib.py's hand-maintained SERVICES, and it costs nothing
	// against the offline requirement: registry.yaml is go:embed'd, so this is
	// a parse of bytes already inside the binary.
	reg, err := registry.Load()
	if err != nil {
		outf(stderr, "Error: loading the service registry: %v\n", err)
		return 1
	}
	apps := reg.Names()

	if len(args) > 0 {
		// Validated before connecting, so a typo'd service name fails the same
		// way whether or not the cluster is reachable. Mirrors ib.py's
		// validate_app, including printing to stdout.
		app := args[0]
		if !slices.Contains(apps, app) {
			outf(stdout, "Unknown service: %s\n", app)
			outf(stdout, "Known services: %s\n", strings.Join(apps, ", "))
			return 1
		}
		apps = []string{app}
	}

	cluster, err := connect()
	if err != nil {
		outf(stderr, "Error: connecting to the cluster: %v\n", err)
		return 1
	}

	verdicts := make([]verdict, 0, len(apps))
	for _, app := range apps {
		staging := deployedImages(ctx, cluster, stderr, app+"-staging")
		prod := deployedImages(ctx, cluster, stderr, app+"-prod")
		verdicts = append(verdicts, statusOne(stdout, app, staging, prod, quiet))
	}
	return exitCode(verdicts)
}

// deployedImages returns the images running in a namespace. A List failure is
// reported and then read as "no pods": ib.py does the same thing (its kubectl
// helper prints the error and exits, and get_deployed_images catches that exit
// and returns an empty set), so a namespace that doesn't exist yet reads as
// indeterminate instead of aborting a whole-fleet status.
func deployedImages(ctx context.Context, cluster podLister, stderr io.Writer, ns string) []string {
	pods, err := cluster.ListPods(ctx, ns)
	if err != nil {
		outf(stderr, "Error: listing pods in %s: %v\n", ns, err)
		return nil
	}
	return kube.Images(pods)
}

// statusOne renders one service and returns its verdict.
//
// The verdict comes from promote.StatusOf — the same decision logic the server
// runs, which is the whole reason for this port. The table does NOT: it is
// rendered straight from the image lists, exactly as ib.py renders it.
//
// That split is deliberate, and it is how this resolves Task 1's second
// finding (promote.Status zeroes StagingTag/ProdTag when either side is
// mid-deploy or has no pods, where ib.py still displays the other side's tag).
// The alternative was widening promote.Status, and it is the wrong shape: what
// the mid-deploy display needs is every tag, so Status would have to carry
// []string per environment — its own input handed back — and gain fields whose
// meaning changed with State. cmd/ib already holds the image lists, having
// just fetched them. So promote decides, and cmd/ib displays, and no verdict
// moved to make the output match.
func statusOne(w io.Writer, app string, stagingImages, prodImages []string, quiet bool) verdict {
	staging := normalize(stagingImages)
	prod := normalize(prodImages)
	status := promote.StatusOf(staging, prod)

	if quiet {
		// Quiet mode prints only the names a script cares about: bare for out
		// of sync, "*"-suffixed for mid-deploy. In sync and indeterminate
		// print nothing at all.
		switch status.State {
		case promote.MidDeploy:
			outf(w, "%s*\n", app)
		case promote.OutOfSync:
			outln(w, app)
		}
		return verdictOf(status.State)
	}

	outf(w, "\n%s deployment status:\n", app)
	outln(w, strings.Repeat("-", 50))
	writeImages(w, "staging", staging)
	writeImages(w, "prod", prod)

	switch status.State {
	case promote.MidDeploy:
		// Which environment is rolling is a property of the image lists, and
		// staging is checked first because ib.py checks it first — with both
		// mid-deploy, staging is the one named.
		if len(staging) > 1 {
			outln(w, "\n⚠ Staging has an image mismatch (deployment in progress?)")
		} else {
			outln(w, "\n⚠ Prod has an image mismatch (deployment in progress?)")
		}
	case promote.InSync:
		outln(w, "\n✓ In sync")
	case promote.OutOfSync:
		outln(w, "\n✗ Out of sync")
		outf(w, "  To promote: ib promote %s\n", app)
		outf(w, "  This will deploy %s to prod\n", status.NewProdTag)
	default:
		// Indeterminate: the table is the whole answer, and ib.py still ends
		// with a blank line here.
		outln(w)
	}
	return verdictOf(status.State)
}

func verdictOf(state promote.State) verdict {
	switch state {
	case promote.InSync:
		return inSync
	case promote.OutOfSync:
		return outOfSync
	default:
		return indeterminate
	}
}

// writeImages renders one environment's line(s). ib.py pads the labels to a
// fixed width ("  staging: x" / "  prod:    x") but drops the padding when it
// has several images to list under a header, so both forms are reproduced.
func writeImages(w io.Writer, label string, images []string) {
	switch len(images) {
	case 0:
		outf(w, "  %-8s %s\n", label+":", "(no pods found)")
	case 1:
		outf(w, "  %-8s %s\n", label+":", promote.ExtractTag(images[0]))
	default:
		outf(w, "  %s:\n", label)
		for _, img := range images {
			outf(w, "    - %s\n", promote.ExtractTag(img))
		}
	}
}

// normalize dedupes and sorts image refs. ib.py keeps them in a set and prints
// sorted(...), so both the mid-deploy count and the listing order follow from
// that — note it sorts full image refs, not the extracted tags. kube.Images
// already dedupes; doing it again here means statusOne renders identically
// from any caller's list, including the oracle fixtures'.
func normalize(images []string) []string {
	if len(images) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(images))
	out := make([]string, 0, len(images))
	for _, img := range images {
		if _, dup := seen[img]; dup {
			continue
		}
		seen[img] = struct{}{}
		out = append(out, img)
	}
	sort.Strings(out)
	return out
}
