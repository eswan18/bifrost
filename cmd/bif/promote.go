package main

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/eswan18/bifrost/internal/kube"
	"github.com/eswan18/bifrost/internal/promote"
)

// promoter is the slice of kube.Client `bif promote` needs: the same pod reads
// status makes, plus the single write this whole CLI performs. Nothing wider —
// promote reads two namespaces and patches one ArgoCD Application, and a seam
// that offered more would be a seam a later command could promote through by
// accident.
type promoter interface {
	podLister
	// PatchAppImage merge-patches <app>-<env>'s kustomize images override.
	PatchAppImage(ctx context.Context, app, env, image string) error
}

// kube.Client has to satisfy promoter, or the fake the tests drive would be
// shaped like an interface the real client doesn't implement.
var _ promoter = (kube.Client)(nil)

const promoteUsage = "Usage: bif promote <app> [-y/--yes]"

// promoteCmd implements `bif promote <app>` and `bif promote <app> -y`,
// ported from ib.py's promote(). The order of the checks is ib.py's, and so
// are the strings: this is the command that changes what runs in production,
// so the way it reports what it is about to do is a contract with the person
// reading it, not a place to improve the wording.
func promoteCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, connect func() (promoter, error)) int {
	args, yes := takeFlag(args, "-y", "--yes")
	if len(args) == 0 {
		outln(stdout, promoteUsage)
		return 1
	}
	apps, ok := loadApps(stderr)
	if !ok {
		return 1
	}
	app := args[0]
	if !validateApp(stdout, apps, app) {
		return 1
	}

	cluster, err := connect()
	if err != nil {
		outf(stderr, "Error: connecting to the cluster: %v\n", err)
		return 1
	}

	staging := deployedImages(ctx, cluster, stderr, app+"-staging")
	prod := deployedImages(ctx, cluster, stderr, app+"-prod")
	return promoteOne(ctx, cluster, app, staging, prod, yes, stdin, stdout)
}

// promoteOne decides, renders, asks, and writes — for one service, from image
// lists already fetched.
//
// The decision is promote.StatusOf's, which is the entire point of the port:
// the tag that reaches production is chosen by the same code the server runs,
// and there is no tag arithmetic in this package. What lives here is ib.py's
// guard sequence around that decision, which StatusOf deliberately does not
// model:
//
//   - Missing pods on either side is an error and an exit 1, where StatusOf
//     merely says Unknown. A promote has a target; status does not need one.
//   - A staging mismatch REFUSES. A prod mismatch WARNS and continues. Both
//     are MidDeploy to StatusOf, and the asymmetry is deliberate in ib.py:
//     staging mid-deploy means the artifact being promoted is not settled and
//     could be the wrong one, while prod mid-deploy just means the last
//     rollout is still landing — re-pinning it is how you correct a bad one.
//
// After the guards there is exactly one image per side, which is the shape
// StatusOf answers for. ib.py reduces to one the same way (`next(iter(...))`
// over a set); the difference is that it takes an arbitrary element of a
// hash-ordered set, while this takes the first of the sorted list normalize
// produced. Only a prod mismatch can make that visible — the arbitrary choice
// feeds the displayed prod tag and the already-in-sync comparison — and Go's
// answer is the deterministic one.
func promoteOne(ctx context.Context, cluster promoter, app string, stagingImages, prodImages []string, yes bool, stdin io.Reader, stdout io.Writer) int {
	staging := normalize(stagingImages)
	prod := normalize(prodImages)

	if len(staging) == 0 {
		outf(stdout, "Error: Could not find staging deployment in %s-staging\n", app)
		return 1
	}
	if len(prod) == 0 {
		outf(stdout, "Error: Could not find prod deployment in %s-prod\n", app)
		return 1
	}
	if len(staging) > 1 {
		outln(stdout, "Error: Staging has an image mismatch (deployment in progress?)")
		writeFoundImages(stdout, staging)
		outln(stdout, "\nWait for the deployment to complete before promoting.")
		return 1
	}
	if len(prod) > 1 {
		outln(stdout, "Warning: Prod has an image mismatch (deployment in progress?)")
		writeFoundImages(stdout, prod)
		outln(stdout)
	}

	stagingImage := staging[0]
	status := promote.StatusOf([]string{stagingImage}, []string{prod[0]})

	outf(stdout, "\n%s promotion check:\n", app)
	outln(stdout, strings.Repeat("-", 50))
	outf(stdout, "  staging: %s\n", status.StagingTag)
	outf(stdout, "  prod:    %s\n", status.ProdTag)

	switch status.State {
	case promote.InSync:
		outf(stdout, "\n✓ Already in sync (both on %s)\n", promote.ExtractSHA(status.StagingTag))
		return 0
	case promote.OutOfSync:
		// The one state with something to promote; fall through.
	default:
		// Unknown, and with one image per side that means one thing: the
		// staging tag carries no SHA, so there is no artifact to name. ib.py
		// exits 0 here — nothing is wrong, there is just nothing to do.
		outf(stdout, "\nWarning: Could not parse staging SHA from '%s'\n", status.StagingTag)
		return 0
	}

	outf(stdout, "\n→ Promote prod to: %s\n", status.NewProdTag)

	if !yes && !confirm(stdin, stdout) {
		outln(stdout, "Aborted.")
		return 0
	}

	// The image reference is built from the staging image, not from the app
	// name: `bif promote footstrike-api` has to write the repository the
	// Deployment actually references. See promote.ReplaceTag.
	newImage := promote.ReplaceTag(stagingImage, status.NewProdTag)

	// ib.py shells out to kubectl and prints the command; this patches through
	// client-go. The line is kept because it says what is about to happen to
	// which object, and an operator who wants to inspect or undo it needs
	// exactly that.
	outf(stdout, "\nRunning: kubectl patch application %s-prod -n %s\n", app, argoNamespace)
	if err := cluster.PatchAppImage(ctx, app, "prod", newImage); err != nil {
		outln(stdout, "\n✗ Promotion failed")
		if msg := strings.TrimSpace(err.Error()); msg != "" {
			outf(stdout, "  %s\n", msg)
		}
		return 1
	}

	outf(stdout, "\n✓ Promoted %s prod to %s\n", app, status.NewProdTag)
	outln(stdout, "  (ArgoCD will sync automatically)")
	return 0
}

// writeFoundImages lists the tags behind an image mismatch.
func writeFoundImages(w io.Writer, images []string) {
	outln(w, "  Images found:")
	for _, img := range images {
		outf(w, "    - %s\n", promote.ExtractTag(img))
	}
}

// confirm asks ib.py's question on ib.py's terms: the prompt goes to stdout
// with no trailing newline, and only a bare "y" — any case, surrounding
// whitespace ignored — proceeds. "yes" does not, which looks unfriendly until
// you remember what the keystroke does; ib.py has always been this strict and
// making a promote easier to trigger is not a portability fix.
//
// A read error, including EOF from a closed or empty stdin, declines. ib.py
// raises EOFError there and dies with a traceback (exit 1); this prints
// "Aborted." and exits 0. Both refuse the write, which is the property that
// matters, and a traceback is not behaviour worth porting.
func confirm(stdin io.Reader, stdout io.Writer) bool {
	outf(stdout, "\nProceed? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line)) == "y"
}
