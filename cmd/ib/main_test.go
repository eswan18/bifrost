package main

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// noCluster is the connection for dispatch tests: reaching it at all is the
// failure.
func noCluster() (podLister, error) { return nil, errors.New("dispatch must not connect") }

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
			wantOut:  []string{"ib status <app> -q", "Deployment status and promotion helper"},
		},
		{
			name:     "unknown command",
			args:     []string{"stauts"},
			wantCode: 1,
			wantOut:  []string{"Unknown command: stauts", "Available commands: status, promote, preview"},
		},
		{
			name:     "promote is reserved, not unknown",
			args:     []string{"promote", "bifrost"},
			wantCode: 1,
			wantOut:  []string{"ib promote is not ported to Go yet"},
		},
		{
			name:     "preview is reserved, not unknown",
			args:     []string{"preview", "list"},
			wantCode: 1,
			wantOut:  []string{"ib preview is not ported to Go yet"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, noCluster)
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

// TestNoBifrostServerDependency is the plan's non-negotiable property,
// enforced rather than asserted in prose: `ib promote bifrost` is how bifrost
// is recovered when bifrost is down, so cmd/ib must not import anything that
// would make it a client of the service it manages.
//
// It checks imports rather than behaviour because that is where the property
// would actually be lost — a future `ib preview` reaching for internal/web's
// record types, or someone "reusing" bifrost's API client for status. Note
// internal/web is banned even though it holds no HTTP client of bifrost's own:
// it is the server, and importing it here would be the first step toward
// status asking the server for its answer.
func TestNoBifrostServerDependency(t *testing.T) {
	banned := map[string]string{
		"net/http": "status and promote read the cluster directly; no HTTP client of bifrost may appear on this path",
		"github.com/eswan18/bifrost/internal/web":    "internal/web is the bifrost server; cmd/ib calls packages, it is not a client of the service it manages",
		"github.com/eswan18/bifrost/internal/auth":   "cmd/ib must never need bifrost's bearer token or session",
		"github.com/eswan18/bifrost/internal/oracle": "the oracle fixtures are for tests only",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/ib: %v", err)
	}

	fset := token.NewFileSet()
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if why, bad := banned[path]; bad {
				t.Errorf("%s imports %q: %s", name, path, why)
			}
		}
	}
	// Without this the test would pass against an empty directory, which is
	// exactly the failure mode a guard like this dies of.
	if files < 2 {
		t.Fatalf("scanned %d non-test files in cmd/ib, expected at least main.go and status.go", files)
	}
}
