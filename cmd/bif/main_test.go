package main

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// noCluster is the connection for dispatch tests: reaching it at all is the
// failure.
func noCluster() (promoter, error) { return nil, errors.New("dispatch must not connect") }

// noPreview is the preview client for every command that is not `preview`.
// status and promote must never reach it, so calling it is a panic rather than
// an error to check: there is no code path that should recover from this.
func noPreview() previewAPI { panic("status and promote must not touch the preview API") }

// unreachableBuilds is the Cloud Build connection every test that isn't about
// the build column runs with, and it fails — which is the point. No unit test
// may reach Google, and making the default the unreachable case means every
// other status assertion in this package doubles as evidence that a dead Cloud
// Build changes nothing but the build cell: the tables, the verdicts and the
// exit codes below are all recorded against it. The build column's own tests
// supply a working one (see fakeBuilds).
func unreachableBuilds(context.Context) (buildLister, error) {
	return nil, errors.New("no credentials")
}

// unreachableDigests is the Artifact Registry connection every test that isn't
// about the content comparison runs with, and it fails for the same reason
// unreachableBuilds does — with one extra thing to prove. A registry this tool
// cannot read must never SUPPRESS a finding: every stalled-sync assertion in
// this package is recorded against a registry that is not there, so they are
// all also evidence that the fallback is the warning `bif status -a` has always
// printed. The content comparison's own tests supply a working one (see
// fakeDigests).
func unreachableDigests(context.Context) (digestResolver, error) {
	return nil, errors.New("no credentials")
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
	}{
		{
			name:     "no arguments prints usage",
			args:     nil,
			wantCode: 1,
			wantOut:  []string{"bif status <app> -q", "bif promote <app> -y", "Deployment status and promotion helper"},
		},
		{
			name:     "unknown command",
			args:     []string{"stauts"},
			wantCode: 1,
			wantOut:  []string{"Unknown command: stauts", "Available commands: status, promote, preview"},
		},
		{
			name: "promote with no app prints its usage without connecting",
			args: []string{"promote"},
			// Rejected on argv alone, so the cluster is never dialled — the
			// same shape ib.py has, and the reason noCluster can be used here.
			wantCode: 1,
			wantOut:  []string{"Usage: bif promote <app> [<app> ...] [-y/--yes]"},
		},
		{
			name:     "preview with no subcommand prints its usage without connecting",
			args:     []string{"preview"},
			wantCode: 1,
			wantOut:  []string{"Usage: bif preview <list|up|down> ..."},
		},
		{
			// Rejected on argv alone (the equals form of --ttl is not parsed —
			// see parseUpArgs), so nothing is dialled and noPreview's panic
			// stands as proof of it.
			name:     "preview up rejects a bad invocation without connecting",
			args:     []string{"preview", "up", "my-branch", "--ttl=8h"},
			wantCode: 1,
			wantOut:  []string{previewUpUsage},
		},
		{
			name:     "unknown preview subcommand",
			args:     []string{"preview", "lsit"},
			wantCode: 1,
			wantOut:  []string{"Unknown preview subcommand: lsit", "Available subcommands: list, up, down"},
		},
		{
			name:     "usage advertises the ported preview subcommands",
			args:     nil,
			wantCode: 1,
			wantOut:  []string{"bif preview list", "bif preview up <branch>", "bif preview down <tag>"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, unreachableDigests, noPreview)
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d", code, tc.wantCode)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestTakeFlag(t *testing.T) {
	tests := []struct {
		args      []string
		wantRest  []string
		wantFound bool
	}{
		{nil, []string{}, false},
		{[]string{"bifrost"}, []string{"bifrost"}, false},
		{[]string{"-q"}, []string{}, true},
		{[]string{"bifrost", "-q"}, []string{"bifrost"}, true},
		{[]string{"-q", "bifrost"}, []string{"bifrost"}, true},
		{[]string{"--quiet", "bifrost"}, []string{"bifrost"}, true},
		// Every occurrence is removed, matching ib.py's list comprehension.
		{[]string{"-q", "bifrost", "--quiet"}, []string{"bifrost"}, true},
		// A near-miss is left in place, so it lands on the unknown-service
		// path instead of being silently read as --quiet.
		{[]string{"--quite"}, []string{"--quite"}, false},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			rest, found := takeFlag(tc.args, "-q", "--quiet")
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
			if strings.Join(rest, " ") != strings.Join(tc.wantRest, " ") {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

// bifrostClientExempt is the one file in cmd/bif allowed to be a client of
// bifrost: preview.go, which implements `bif preview`. See
// TestNoBifrostServerDependency for why the exemption is a single named file
// rather than a relaxed rule.
const bifrostClientExempt = "preview.go"

// bannedFromStatusAndPromote is what may not appear on the path that has to
// keep working with bifrost down.
//
// The property is about ONE server. `bif status` has always spoken HTTPS to
// the Kubernetes API through client-go, and as of the build column it also
// reads Cloud Build through internal/gcb; neither is the service being
// managed, and neither can make `bif promote bifrost` depend on bifrost being
// up. What may not appear here is a reach for BIFROST — its API, its bearer
// token, its session — under any spelling.
//
// net/http stays on the list, and its entry is the one worth reading twice,
// because it is the only one that is not literally a bifrost package. In THESE
// files a raw net/http import can only mean a hand-rolled client, and the one
// server cmd/bif has ever hand-rolled a client for is bifrost — that is what
// preview.go is. Every legitimate remote read status makes goes through a
// package that owns the protocol: client-go for Kubernetes, internal/gcb for
// Cloud Build. So the ban costs the third-party reads nothing while still
// stopping the thing it was written to stop, and a new third-party client
// arrives the same way internal/gcb did: as a package, visible in the walk
// below, and not by opening a socket in status.go.
var bannedFromStatusAndPromote = map[string]string{
	"net/http": "a raw HTTP client in status/promote can only be a hand-rolled client of bifrost; third-party reads (Kubernetes, Cloud Build) go through their own packages",
	"github.com/eswan18/bifrost/internal/web":           "internal/web is the bifrost server; cmd/bif calls packages, it is not a client of the service it manages",
	"github.com/eswan18/bifrost/internal/auth":          "cmd/bif must never need bifrost's bearer token or session",
	"github.com/eswan18/bifrost/internal/oracle":        "the oracle fixtures are for tests only",
	"github.com/eswan18/bifrost/internal/previewclient": "the preview API client belongs to `bif preview` alone; status and promote must not be able to reach it",
}

// TestNoBifrostServerDependency is the plan's non-negotiable property,
// enforced rather than asserted in prose: `bif promote bifrost` is how bifrost
// is recovered when bifrost is down, so nothing implementing status or promote
// may make this a client of the service it manages.
//
// It used to ban net/http from cmd/bif entirely. `bif preview` is an HTTP
// client by design — bifrost owns the busy set, the write credentials and the
// build tokens — so that exact assertion could not survive, and the honest
// choice was to narrow it rather than delete it. What is enforced now:
//
//   - every non-test file in cmd/bif EXCEPT preview.go obeys the original ban,
//     which is stricter than a hand-written whitelist of status.go/promote.go
//     would be: a new file gets checked automatically, and a helper added to
//     main.go still cannot open a socket.
//   - the client itself lives in its own package, internal/previewclient, and
//     the ban list includes it — so the exemption is one file wide, not "cmd/bif
//     may now do HTTP".
//   - preview.go must actually use the exemption, so it cannot linger as dead
//     permission if `bif preview` is ever restructured.
//   - TestStatusAndPromoteDependenciesStayOffBifrost covers what a file-level
//     import check cannot: a package status or promote depends on growing its
//     own path to the server, below cmd/bif entirely.
//
// What is genuinely given up: the binary now links net/http (it always did
// transitively, through client-go), and Go's file-scoped imports cannot stop
// promote.go from calling a function preview.go declares. The package-level
// boundary is what makes that visible — such a call would have to name a
// preview identifier, in a file this test proves owns the only bifrost reach.
func TestNoBifrostServerDependency(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/bif: %v", err)
	}

	fset := token.NewFileSet()
	scanned := map[string]bool{}
	exemptUsesIt := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned[name] = true
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if name == bifrostClientExempt {
				if path == "github.com/eswan18/bifrost/internal/previewclient" {
					exemptUsesIt = true
				}
				continue
			}
			if why, bad := bannedFromStatusAndPromote[path]; bad {
				t.Errorf("%s imports %q: %s", name, path, why)
			}
		}
	}

	// Without these the test would pass against an empty directory, or against
	// a status.go renamed out of the scan, which is exactly the failure mode a
	// guard like this dies of.
	// complete.go is named because it is the one other file that touches the
	// preview seam: `bif preview down <TAB>` completes live tags. It does so
	// through the previewAPI value run() threads in, never by importing the
	// client, and this is what keeps that true.
	for _, required := range []string{"main.go", "status.go", "promote.go", "complete.go", bifrostClientExempt} {
		if !scanned[required] {
			t.Fatalf("%s was not scanned; cmd/bif's files are %v", required, sortedKeys(scanned))
		}
	}
	if !exemptUsesIt {
		t.Errorf("%s no longer imports internal/previewclient, so its exemption from the ban is dead permission — delete it", bifrostClientExempt)
	}
}

// TestStatusAndPromoteDependenciesStayOffBifrost closes the hole a file-level
// import check leaves open: status and promote could keep clean imports while
// a package they depend on grew its own client of bifrost.
//
// The roots are read from the code rather than listed here, so the test
// follows what status and promote actually import instead of a stale copy of
// it, and the walk is over bifrost's own packages only. That last part is the
// scope, not a gap: client-go speaks HTTPS to Kubernetes and always has, and
// internal/gcb speaks HTTPS to Cloud Build for the build column — talking to a
// third party is what this tool DOES, and what it must not do is depend on the
// server it exists to recover. internal/gcb sits in this closure and passes
// for the reason that matters: it imports nothing of bifrost's, so it cannot
// smuggle the server in, and statusCmd treats its failure as a blank column
// (TestBuildColumnFailuresLeaveStatusIntact) rather than as an error, so it
// cannot make bifrost's availability a precondition either.
//
// internal/auth is banned here as well as file by file: bifrost's session and
// bearer machinery has no business on this path at any depth, and a package
// that pulled it in would be reaching for the server's credentials by
// definition.
func TestStatusAndPromoteDependenciesStayOffBifrost(t *testing.T) {
	const modulePrefix = "github.com/eswan18/bifrost/"
	banned := []string{
		modulePrefix + "internal/web",
		modulePrefix + "internal/previewclient",
		modulePrefix + "internal/auth",
	}

	roots := bifrostImports(t, "main.go", "status.go", "promote.go")
	if len(roots) == 0 {
		t.Fatal("status and promote import no bifrost packages at all; the walk below would prove nothing")
	}

	closure := map[string]bool{}
	var walk func(pkg string)
	walk = func(pkg string) {
		if closure[pkg] {
			return
		}
		closure[pkg] = true
		dir := filepath.Join("..", "..", strings.TrimPrefix(pkg, modulePrefix))
		for _, imp := range bifrostImportsIn(t, dir) {
			walk(imp)
		}
	}
	for _, root := range roots {
		walk(root)
	}

	for _, pkg := range banned {
		if closure[pkg] {
			t.Errorf("%s is in the dependency closure of status and promote: they must work with bifrost down", pkg)
		}
	}
}

// bifrostImports returns the bifrost packages the named files in cmd/bif
// import.
func bifrostImports(t *testing.T, files ...string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if path := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(path, "github.com/eswan18/bifrost/") {
				out = append(out, path)
			}
		}
	}
	return out
}

// bifrostImportsIn returns the bifrost packages every non-test file in dir
// imports.
func bifrostImportsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, imp := range file.Imports {
			if path := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(path, "github.com/eswan18/bifrost/") {
				out = append(out, path)
			}
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
