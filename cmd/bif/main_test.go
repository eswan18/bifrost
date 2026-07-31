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
			wantOut:  []string{"Usage: bif promote <app> [-y/--yes]"},
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
			code := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, noCluster, noPreview)
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

// networkExempt is the one file in cmd/bif allowed to reach bifrost:
// preview.go, which implements `bif preview`. See TestNoBifrostServerDependency
// for why the exemption is a single named file rather than a relaxed rule.
const networkExempt = "preview.go"

// bannedFromStatusAndPromote is what may not appear on the path that has to
// work with bifrost down.
var bannedFromStatusAndPromote = map[string]string{
	"net/http": "status and promote read the cluster directly; no HTTP client of bifrost may appear on this path",
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
//   - TestStatusAndPromoteDependenciesStayOffTheNetwork covers what a
//     file-level import check cannot: a package status or promote depends on
//     growing its own path to the server, below cmd/bif entirely.
//
// What is genuinely given up: the binary now links net/http (it always did
// transitively, through client-go), and Go's file-scoped imports cannot stop
// promote.go from calling a function preview.go declares. The package-level
// boundary is what makes that visible — such a call would have to name a
// preview identifier, in a file this test proves owns the only network reach.
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
			if name == networkExempt {
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
	for _, required := range []string{"main.go", "status.go", "promote.go", networkExempt} {
		if !scanned[required] {
			t.Fatalf("%s was not scanned; cmd/bif's files are %v", required, sortedKeys(scanned))
		}
	}
	if !exemptUsesIt {
		t.Errorf("%s no longer imports internal/previewclient, so its exemption from the ban is dead permission — delete it", networkExempt)
	}
}

// TestStatusAndPromoteDependenciesStayOffTheNetwork closes the hole a
// file-level import check leaves open: status and promote could keep clean
// imports while a package they depend on grew its own client of bifrost.
//
// The roots are read from the code rather than listed here, so the test
// follows what status and promote actually import instead of a stale copy of
// it, and the walk is over bifrost's own packages only — client-go speaks HTTP
// to Kubernetes and always has, which is the point, not a violation.
func TestStatusAndPromoteDependenciesStayOffTheNetwork(t *testing.T) {
	const modulePrefix = "github.com/eswan18/bifrost/"
	banned := []string{
		modulePrefix + "internal/web",
		modulePrefix + "internal/previewclient",
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
