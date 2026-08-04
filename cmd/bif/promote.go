package main

import (
	"bufio"
	"context"
	"fmt"
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

const promoteUsage = "Usage: bif promote <app> [<app> ...] [-y/--yes]"

// promoteCmd implements `bif promote <app> [<app> ...]`, ported from ib.py's
// promote(). The order of the checks is ib.py's, and so are the strings: this
// is the command that changes what runs in production, so the way it reports
// what it is about to do is a contract with the person reading it, not a place
// to improve the wording.
//
// One name and several names are two renderings of one decision, not two
// commands. Every service is planned (planPromotion) before any is rendered,
// asked about, or written, which is what lets the several-name form show one
// combined plan and ask ONE question. The one-name form's output is then
// exactly what it always was, byte for byte — it is the branch below that
// renders it, and TestPromoteMatchesOracle still compares it against ib.py's
// captured stdout.
func promoteCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, connect func() (promoter, error)) int {
	args, yes := takeFlag(args, "-y", "--yes")
	if len(args) == 0 {
		outln(stdout, promoteUsage)
		return 1
	}
	known, ok := loadApps(stderr)
	if !ok {
		return 1
	}
	apps, ok := resolveApps(stdout, known, args)
	if !ok {
		return 1
	}

	cluster, err := connect()
	if err != nil {
		outf(stderr, "Error: connecting to the cluster: %v\n", err)
		return 1
	}

	plans := make([]promotePlan, 0, len(apps))
	for _, app := range apps {
		staging := deployedImages(ctx, cluster, stderr, app+"-staging")
		prod := deployedImages(ctx, cluster, stderr, app+"-prod")
		plans = append(plans, planPromotion(app, staging, prod))
	}

	if len(plans) == 1 {
		return promoteOne(ctx, cluster, plans[0], yes, stdin, stdout)
	}
	return promoteMany(ctx, cluster, plans, yes, stdin, stdout)
}

// promoteOutcome is what planPromotion decided for one service. It is the
// whole of ib.py's guard sequence, enumerated: every branch promoteOne used to
// take inline is one of these, so the several-name form can report the same
// decisions in a different shape without a second copy of the reasoning.
type promoteOutcome int

const (
	// outcomeNoStaging / outcomeNoProd: a promote has a TARGET, and one side of
	// it is running nothing. StatusOf merely says Unknown; ib.py errors, and so
	// does this.
	outcomeNoStaging promoteOutcome = iota
	outcomeNoProd
	// outcomeStagingMismatch REFUSES. See promotePlan.prodMismatch for the
	// other half of the asymmetry.
	outcomeStagingMismatch
	outcomeInSync
	// outcomeNoSHA: the staging tag carries no SHA, so there is no artifact to
	// name. ib.py exits 0 here — nothing is wrong, there is just nothing to do.
	outcomeNoSHA
	outcomePromote
)

// refused reports whether this outcome is a promotion the operator ASKED FOR
// and did not get. That is the exit-code question, and it is not the same as
// "nothing was written": an in-sync service and a staging tag with no SHA also
// write nothing, and both exit 0 because there was nothing to do. A refusal is
// different — the operator named a service expecting prod to move, and it did
// not — so it exits 1, in the one-name form (where it always has) and in the
// several-name form alike.
func (o promoteOutcome) refused() bool {
	switch o {
	case outcomeNoStaging, outcomeNoProd, outcomeStagingMismatch:
		return true
	}
	return false
}

// promotePlan is one service's decision, made from image lists already fetched
// and before anything is printed or written.
//
// The decision is promote.StatusOf's, which is the entire point of the port:
// the tag that reaches production is chosen by the same code the server runs,
// and there is no tag arithmetic in this package. What lives here is ib.py's
// guard sequence around that decision, which StatusOf deliberately does not
// model — see promoteOutcome, and prodMismatch below.
type promotePlan struct {
	app string
	// staging and prod are normalize'd: deduped and sorted.
	staging []string
	prod    []string
	// prodMismatch is the other half of the asymmetry. A staging mismatch
	// REFUSES (outcomeStagingMismatch); a prod mismatch WARNS and continues.
	// Both are MidDeploy to StatusOf, and the difference is deliberate in
	// ib.py: staging mid-deploy means the artifact being promoted is not
	// settled and could be the wrong one, while prod mid-deploy just means the
	// last rollout is still landing — re-pinning it is how you correct a bad
	// one.
	prodMismatch bool
	outcome      promoteOutcome
	// status is meaningless unless the guards passed, which is every outcome
	// except the two missing-deployment ones and the staging mismatch.
	status promote.Status
	// newImage is what gets written, and only outcomePromote has one. It is
	// built from the staging IMAGE, not from the app name: `bif promote
	// footstrike-api` has to write the repository the Deployment actually
	// references, which is still fitness-api. See promote.ReplaceTag.
	newImage string
}

// planPromotion runs ib.py's guards, in ib.py's order, and decides. It prints
// nothing and writes nothing — that is the point of the split, and it is what
// lets promoteMany show every service's decision before asking about any of
// them.
//
// After the guards there is exactly one image per side, which is the shape
// StatusOf answers for. ib.py reduces to one the same way (`next(iter(...))`
// over a set); the difference is that it takes an arbitrary element of a
// hash-ordered set, while this takes the first of the sorted list normalize
// produced. Only a prod mismatch can make that visible — the arbitrary choice
// feeds the displayed prod tag and the already-in-sync comparison — and Go's
// answer is the deterministic one.
func planPromotion(app string, stagingImages, prodImages []string) promotePlan {
	p := promotePlan{app: app, staging: normalize(stagingImages), prod: normalize(prodImages)}
	switch {
	case len(p.staging) == 0:
		p.outcome = outcomeNoStaging
		return p
	case len(p.prod) == 0:
		p.outcome = outcomeNoProd
		return p
	case len(p.staging) > 1:
		p.outcome = outcomeStagingMismatch
		return p
	}
	p.prodMismatch = len(p.prod) > 1

	stagingImage := p.staging[0]
	p.status = promote.StatusOf([]string{stagingImage}, []string{p.prod[0]})
	switch p.status.State {
	case promote.InSync:
		p.outcome = outcomeInSync
	case promote.OutOfSync:
		p.outcome = outcomePromote
		p.newImage = promote.ReplaceTag(stagingImage, p.status.NewProdTag)
	default:
		p.outcome = outcomeNoSHA
	}
	return p
}

// promoteOne renders, asks, and writes for a single named service. Its output
// is ib.py's, byte for byte, and it is pinned that way by
// TestPromoteMatchesOracle: the several-name form got a new rendering precisely
// so that this one did not have to change.
func promoteOne(ctx context.Context, cluster promoter, p promotePlan, yes bool, stdin io.Reader, stdout io.Writer) int {
	renderPromoteCheck(stdout, p)
	if p.outcome != outcomePromote {
		if p.outcome.refused() {
			return 1
		}
		return 0
	}
	if !yes && !confirm(stdin, stdout, "\nProceed? [y/N] ") {
		outln(stdout, "Aborted.")
		return 0
	}
	if !executePromotion(ctx, cluster, stdout, p) {
		return 1
	}
	return 0
}

// renderPromoteCheck is ib.py's single-service report: the guard messages, the
// two-line table under a rule, and the verdict. Every string and every blank
// line here is captured in testdata/oracle/promote_decision.json.
func renderPromoteCheck(w io.Writer, p promotePlan) {
	switch p.outcome {
	case outcomeNoStaging:
		outf(w, "Error: Could not find staging deployment in %s-staging\n", p.app)
		return
	case outcomeNoProd:
		outf(w, "Error: Could not find prod deployment in %s-prod\n", p.app)
		return
	case outcomeStagingMismatch:
		outln(w, "Error: Staging has an image mismatch (deployment in progress?)")
		writeFoundImages(w, p.staging)
		outln(w, "\nWait for the deployment to complete before promoting.")
		return
	}
	if p.prodMismatch {
		outln(w, "Warning: Prod has an image mismatch (deployment in progress?)")
		writeFoundImages(w, p.prod)
		outln(w)
	}

	outf(w, "\n%s promotion check:\n", p.app)
	outln(w, strings.Repeat("-", 50))
	outf(w, "  staging: %s\n", p.status.StagingTag)
	outf(w, "  prod:    %s\n", p.status.ProdTag)

	switch p.outcome {
	case outcomeInSync:
		outf(w, "\n✓ Already in sync (both on %s)\n", promote.ExtractSHA(p.status.StagingTag))
	case outcomeNoSHA:
		outf(w, "\nWarning: Could not parse staging SHA from '%s'\n", p.status.StagingTag)
	case outcomePromote:
		outf(w, "\n→ Promote prod to: %s\n", p.status.NewProdTag)
	}
}

// executePromotion performs the one write this CLI makes, and reports it.
// Returns whether prod actually moved.
func executePromotion(ctx context.Context, cluster promoter, w io.Writer, p promotePlan) bool {
	// ib.py shells out to kubectl and prints the command; this patches through
	// client-go. The line is kept because it says what is about to happen to
	// which object, and an operator who wants to inspect or undo it needs
	// exactly that.
	outf(w, "\nRunning: kubectl patch application %s-prod -n %s\n", p.app, argoNamespace)
	if err := cluster.PatchAppImage(ctx, p.app, "prod", p.newImage); err != nil {
		outln(w, "\n✗ Promotion failed")
		if msg := strings.TrimSpace(err.Error()); msg != "" {
			outf(w, "  %s\n", msg)
		}
		return false
	}
	outf(w, "\n✓ Promoted %s prod to %s\n", p.app, p.status.NewProdTag)
	outln(w, "  (ArgoCD will sync automatically)")
	return true
}

// ---- `bif promote <app> <app> ...` --------------------------------------

// promoteMany is the several-name form: ONE combined plan, ONE prompt, then
// each write in turn.
//
// One prompt is the whole design. Asking per service would make the answer to
// the first question change what the second question is about — and the
// operator who typed three names is deciding about three services at once, so
// they are shown all three and asked once. The plan table above the prompt is
// what makes that safe: nothing is written before the reader has seen every
// tag that is about to move, and every service that will be skipped and why.
//
// Failure is per service and does NOT abort the rest. A promote of three
// services is three independent writes; the second one failing tells you
// nothing about the third, and stopping would leave the operator to work out
// which of their names had been acted on. So each is attempted, each reports
// itself, and the summary at the end names what happened to all of them.
//
// Exit code: 0 only if everything attempted worked and nothing was refused.
// See promoteOutcome.refused — a service skipped because it is already in sync
// is not a failure, and a service that refused because staging is mid-rollout
// is, even though neither wrote anything.
func promoteMany(ctx context.Context, cluster promoter, plans []promotePlan, yes bool, stdin io.Reader, stdout io.Writer) int {
	renderPromotePlans(stdout, plans)

	var todo []promotePlan
	var refused, skipped []string
	for _, p := range plans {
		switch {
		case p.outcome == outcomePromote:
			todo = append(todo, p)
		case p.outcome.refused():
			refused = append(refused, p.app)
		default:
			skipped = append(skipped, p.app)
		}
	}

	if len(todo) == 0 {
		// No prompt: there is no question to ask. The table above already said
		// what each named service is doing instead.
		outln(stdout, "\nNothing to promote.")
		if len(refused) > 0 {
			writeSummary(stdout, nil, nil, refused, skipped)
			return 1
		}
		return 0
	}

	if !yes && !confirm(stdin, stdout, fmt.Sprintf("\nProceed with %s? [y/N] ", count(len(todo), "promotion"))) {
		outln(stdout, "Aborted.")
		// Declining is the operator's own choice and is not a failure — but a
		// refusal earlier in the run still is, and it did not stop being one
		// because the rest were declined.
		if len(refused) > 0 {
			return 1
		}
		return 0
	}

	var promoted, failed []string
	for _, p := range todo {
		if executePromotion(ctx, cluster, stdout, p) {
			promoted = append(promoted, p.app)
		} else {
			failed = append(failed, p.app)
		}
	}

	writeSummary(stdout, promoted, failed, refused, skipped)
	if len(failed) > 0 || len(refused) > 0 {
		return 1
	}
	return 0
}

// renderPromotePlans is the several-name form's report: one line per named
// service, aligned, saying either what will move or why it will not.
//
// A prod mismatch is the one thing that does not fit on that line — it is a
// warning ABOUT a service that is still going to be promoted, and it carries a
// list of images. It goes above the table, naming its service, for the same
// reason the single-service form puts it before the check: the reader has to
// see it before they see the promotion it qualifies.
func renderPromotePlans(w io.Writer, plans []promotePlan) {
	for _, p := range plans {
		if p.prodMismatch {
			outf(w, "\nWarning: Prod has an image mismatch for %s (deployment in progress?)\n", p.app)
			writeFoundImages(w, p.prod)
		}
	}

	width := 0
	for _, p := range plans {
		width = max(width, len(p.app))
	}
	outln(w)
	for _, p := range plans {
		outf(w, "%-*s  %s\n", width, p.app, planLine(p))
	}
}

// planLine is one service's row in the combined plan. Every non-promoting row
// ends in "skipping", so the eye can sort the table into the two groups the
// prompt is about without reading the reasons.
func planLine(p promotePlan) string {
	switch p.outcome {
	case outcomePromote:
		return fmt.Sprintf("%s -> prod  (prod: %s)", p.status.NewProdTag, p.status.ProdTag)
	case outcomeInSync:
		return "already in sync, skipping"
	case outcomeStagingMismatch:
		return "staging has an image mismatch (deployment in progress?), skipping"
	case outcomeNoStaging:
		return fmt.Sprintf("no deployment found in %s-staging, skipping", p.app)
	case outcomeNoProd:
		return fmt.Sprintf("no deployment found in %s-prod, skipping", p.app)
	default:
		return fmt.Sprintf("could not parse staging SHA from '%s', skipping", p.status.StagingTag)
	}
}

// writeSummary closes a several-name run by naming every service in each
// group. It exists because the per-service output above it scrolls: after
// three promotions, two of which failed, the operator needs one line that says
// which — and it has to NAME them, because a count would send them back up
// through the output they were trying to summarize.
//
// Empty groups are omitted rather than printed as zeroes: "0 failed" invites
// the reader to check whether it says 0 or 8.
func writeSummary(w io.Writer, promoted, failed, refused, skipped []string) {
	var parts []string
	for _, g := range []struct {
		label string
		apps  []string
	}{
		{"promoted", promoted},
		{"failed", failed},
		{"refused", refused},
		{"skipped", skipped},
	} {
		if len(g.apps) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s (%s)", len(g.apps), g.label, strings.Join(g.apps, ", ")))
		}
	}
	if len(parts) == 0 {
		return
	}
	outf(w, "\nSummary: %s\n", strings.Join(parts, ", "))
}

// count renders "1 promotion" / "2 promotions". The prompt is the last thing
// read before an irreversible write, so it is worth the four lines not to ask
// about "1 promotions".
func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
//
// The prompt is a parameter because `bif preview down` asks a different
// question ("Tear down preview <tag>? [y/N] ") on identical terms. The two
// irreversible commands in this CLI accept the same keystroke, and that stays
// true by construction rather than by two copies agreeing.
func confirm(stdin io.Reader, stdout io.Writer, prompt string) bool {
	outf(stdout, "%s", prompt)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line)) == "y"
}
