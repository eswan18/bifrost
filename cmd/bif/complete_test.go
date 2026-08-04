package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eswan18/bifrost/internal/previewapi"
	"github.com/eswan18/bifrost/internal/registry"
)

// ---- the harness --------------------------------------------------------

// stubPreview is a previewAPI that implements List and nothing else. The
// embedded nil interface is the assertion: completion may read the preview
// list and may not create, fetch or delete anything, and a call to any of
// those panics rather than quietly succeeding.
//
// preview_test.go deliberately drives the REAL client against an httptest
// server, because the headers and status-code mapping are invisible from above
// this seam. Completion is the opposite case: what is worth asserting is what
// happens when the call does not come back, which is far easier to stage here
// than on a socket.
type stubPreview struct {
	previewAPI
	list func(ctx context.Context) ([]previewapi.Record, error)
}

func (s stubPreview) List(ctx context.Context) ([]previewapi.Record, error) { return s.list(ctx) }

// dialing wraps a List implementation as the connection run() takes.
func dialing(list func(ctx context.Context) ([]previewapi.Record, error)) func() previewAPI {
	return func() previewAPI { return stubPreview{list: list} }
}

// noNetwork is the connection every completion position except `preview down
// <tag>` must be driven with: reaching for it at all is the failure.
//
// It reports through t rather than panicking, because previewTags recovers
// panics on purpose — a panicking dial would be swallowed by the very code
// under test and the assertion would pass by accident.
func noNetwork(t *testing.T) func() previewAPI {
	t.Helper()
	return func() previewAPI {
		t.Errorf("completion dialled bifrost outside `preview down <tag>`")
		return stubPreview{list: func(context.Context) ([]previewapi.Record, error) { return nil, nil }}
	}
}

// complete runs `bif __complete <words...>` through run, the way the shims do,
// and returns the candidate lines and the exit code.
//
// It asserts the two properties every case shares: nothing reaches stderr, and
// the exit code is 0. The shims splice stdout into the candidate list and the
// shell reads the exit code, so a completion that reports a problem on either
// stream is a completion that has put a diagnostic in front of the operator as
// though it were something they could pick.
func complete(t *testing.T, dial func() previewAPI, words ...string) []string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"__complete"}, words...), strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, dial)
	if code != 0 {
		t.Errorf("__complete %q exited %d, want 0", words, code)
	}
	if stderr.Len() != 0 {
		t.Errorf("__complete %q wrote to stderr: %q", words, stderr.String())
	}
	out := strings.TrimSuffix(stdout.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// fleetNames is the embedded registry's service list, read the same way the
// completion does. Cases below compare against it rather than against a copy
// of registry.yaml, so adding a service to the fleet does not fail this file —
// what is being asserted is which names are dropped, not what the fleet is.
func fleetNames(t *testing.T) []string {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg.Names()
}

func without(names []string, drop ...string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(drop, n) {
			out = append(out, n)
		}
	}
	return out
}

// ---- the offline positions ----------------------------------------------

// TestCompletePositions covers every position that must answer from the
// binary alone. All of them run against noNetwork, so the table doubles as the
// proof that only `preview down <tag>` reaches for bifrost.
func TestCompletePositions(t *testing.T) {
	fleet := fleetNames(t)
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{
			name:  "bare bif offers the visible commands",
			words: []string{""},
			want:  []string{"status", "promote", "preview", "completion"},
		},
		{
			name:  "a prefix narrows the command list",
			words: []string{"pr"},
			want:  []string{"promote", "preview"},
		},
		{
			name:  "status offers the fleet and its flags",
			words: []string{"status", ""},
			want:  append(append([]string{}, fleet...), "-q", "--quiet", "-a", "--attention"),
		},
		{
			name:  "promote offers the fleet and its flag",
			words: []string{"promote", ""},
			want:  append(append([]string{}, fleet...), "-y", "--yes"),
		},
		{
			name:  "a dash narrows to flags",
			words: []string{"promote", "-"},
			want:  []string{"-y", "--yes"},
		},
		{
			name:  "a service prefix narrows the fleet",
			words: []string{"status", "foot"},
			want:  []string{"footstrike-api", "footstrike-dashboard"},
		},
		{
			name:  "preview offers its subcommands",
			words: []string{"preview", ""},
			want:  []string{"list", "up", "down"},
		},
		{
			name:  "preview up offers its flags",
			words: []string{"preview", "up", "my-branch", ""},
			want:  []string{"--ttl", "--no-wait", "--auto-update"},
		},
		{
			name:  "preview up drops a flag already given",
			words: []string{"preview", "up", "my-branch", "--no-wait", ""},
			want:  []string{"--ttl", "--auto-update"},
		},
		{
			name: "nothing completes as --ttl's value",
			// The duration is bif's to accept, not to enumerate; offering
			// flags here would offer them in the one spot they cannot go.
			words: []string{"preview", "up", "my-branch", "--ttl", ""},
			want:  nil,
		},
		{
			name:  "preview list takes nothing",
			words: []string{"preview", "list", ""},
			want:  nil,
		},
		{
			name:  "completion offers the shells it can emit",
			words: []string{"completion", ""},
			want:  []string{"zsh", "bash"},
		},
		{
			name: "an unknown command completes to nothing",
			// Falling back to the fleet here would suggest the typo is a
			// command that takes service names.
			words: []string{"stauts", ""},
			want:  nil,
		},
		{
			name:  "an unknown preview subcommand completes to nothing",
			words: []string{"preview", "lsit", ""},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := complete(t, noNetwork(t), tc.words...)
			if !equal(got, tc.want) {
				t.Errorf("__complete %q = %v, want %v", tc.words, got, tc.want)
			}
		})
	}
}

// TestCompleteOmitsNamesAlreadyOnTheLine is the multi-service half of the
// feature. status and promote both take several names and dedupe them
// (resolveApps), so a name already typed is a candidate that can only produce
// a no-op — and a list that will not shrink as the operator narrows it down.
func TestCompleteOmitsNamesAlreadyOnTheLine(t *testing.T) {
	fleet := fleetNames(t)
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{
			name:  "status drops the one name given",
			words: []string{"status", "bifrost", ""},
			want:  append(without(fleet, "bifrost"), "-q", "--quiet", "-a", "--attention"),
		},
		{
			name:  "promote drops every name given",
			words: []string{"promote", "bifrost", "identity", ""},
			want:  append(without(fleet, "bifrost", "identity"), "-y", "--yes"),
		},
		{
			name: "a name is dropped whichever side of a flag it sits on",
			// takeFlag accepts flags anywhere, so the names are not
			// necessarily contiguous.
			words: []string{"promote", "-y", "bifrost", ""},
			want:  without(fleet, "bifrost"),
		},
		{
			name:  "a dropped name is still dropped under a prefix",
			words: []string{"status", "footstrike-api", "foot"},
			want:  []string{"footstrike-dashboard"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := complete(t, noNetwork(t), tc.words...)
			if !equal(got, tc.want) {
				t.Errorf("__complete %q = %v, want %v", tc.words, got, tc.want)
			}
		})
	}
}

// TestCompleteOmitsFlagsAlreadyGiven checks the same exclusion for flags, and
// checks it per FLAG rather than per spelling: -q and --quiet are one flag,
// and offering the other alias of one already given is offering a token
// takeFlag would strip straight back out.
func TestCompleteOmitsFlagsAlreadyGiven(t *testing.T) {
	fleet := fleetNames(t)
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{
			name:  "the short alias is dropped when the long one is given",
			words: []string{"status", "--quiet", "-"},
			want:  []string{"-a", "--attention"},
		},
		{
			name:  "the long alias is dropped when the short one is given",
			words: []string{"status", "-q", "-"},
			want:  []string{"-a", "--attention"},
		},
		{
			name:  "both flags given leaves only the fleet",
			words: []string{"status", "-q", "-a", ""},
			want:  fleet,
		},
		{
			name:  "promote's flag is dropped once given",
			words: []string{"promote", "-y", "-"},
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := complete(t, noNetwork(t), tc.words...)
			if !equal(got, tc.want) {
				t.Errorf("__complete %q = %v, want %v", tc.words, got, tc.want)
			}
		})
	}
}

// ---- the one networked position -----------------------------------------

func TestCompletePreviewDownTags(t *testing.T) {
	dial := dialing(func(context.Context) ([]previewapi.Record, error) {
		return []previewapi.Record{{Tag: "pr-42-abc"}, {Tag: "fix-login-def"}}, nil
	})
	got := complete(t, dial, "preview", "down", "")
	want := []string{"pr-42-abc", "fix-login-def", "-y", "--yes"}
	if !equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
}

func TestCompletePreviewDownTagsFilterByPrefix(t *testing.T) {
	dial := dialing(func(context.Context) ([]previewapi.Record, error) {
		return []previewapi.Record{{Tag: "pr-42-abc"}, {Tag: "fix-login-def"}}, nil
	})
	got := complete(t, dial, "preview", "down", "pr-")
	if want := []string{"pr-42-abc"}; !equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
}

// TestCompletePreviewDownAsksOnce: previewDownCmd takes exactly one tag, so
// once one is on the line there is nothing left to look up — and no reason to
// spend a network call proving it.
func TestCompletePreviewDownAsksOnce(t *testing.T) {
	got := complete(t, noNetwork(t), "preview", "down", "pr-42-abc", "")
	if want := []string{"-y", "--yes"}; !equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

// TestCompletePreviewDownSkipsTheCallForAFlag: a prefix of "-" is asking for a
// flag, and every tag the call returned would be filtered straight back out. A
// round trip whose result is discarded is a Tab that hangs for nothing.
func TestCompletePreviewDownSkipsTheCallForAFlag(t *testing.T) {
	got := complete(t, noNetwork(t), "preview", "down", "-")
	if want := []string{"-y", "--yes"}; !equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

// TestCompletePreviewDownTimesOut is the assertion that protects the shell.
//
// The stub blocks until its context is done, so a completion that did not
// impose a deadline of its own would wait on it forever — the safety net
// returns a poison candidate instead of hanging the suite, which fails the
// assertion loudly rather than by timing out the test binary.
func TestCompletePreviewDownTimesOut(t *testing.T) {
	restore := previewTagTimeout
	previewTagTimeout = time.Millisecond
	t.Cleanup(func() { previewTagTimeout = restore })

	dial := dialing(func(ctx context.Context) ([]previewapi.Record, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return []previewapi.Record{{Tag: "completion-imposed-no-deadline"}}, nil
		}
	})
	got := complete(t, dial, "preview", "down", "")
	if want := []string{"-y", "--yes"}; !equal(got, want) {
		t.Errorf("candidates after a timeout = %v, want %v", got, want)
	}
}

// TestCompletePreviewDownSwallowsFailures: an expired gcloud login, a 401, a
// bifrost that is down, a route that 404s — all of it is one answer here. The
// error text must not reach stdout, where the shell would offer it as
// something to pick.
func TestCompletePreviewDownSwallowsFailures(t *testing.T) {
	tests := []struct {
		name string
		list func(context.Context) ([]previewapi.Record, error)
	}{
		{
			name: "transport error",
			list: func(context.Context) ([]previewapi.Record, error) {
				return nil, errors.New("dial tcp: connect: connection refused")
			},
		},
		{
			name: "auth failure",
			list: func(context.Context) ([]previewapi.Record, error) {
				return nil, errors.New("ERROR: (gcloud.secrets.versions.access) You do not currently have an active account selected")
			},
		},
		{
			name: "the client panics",
			list: func(context.Context) ([]previewapi.Record, error) {
				panic("a Tab press is not where an operator finds out")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := complete(t, dialing(tc.list), "preview", "down", "")
			if want := []string{"-y", "--yes"}; !equal(got, want) {
				t.Errorf("candidates = %v, want %v", got, want)
			}
		})
	}
}

// ---- hidden, and the shims ----------------------------------------------

// TestCompleteIsHidden pins the one thing `__complete` must never do: appear.
// It is an interface for the shims, an operator has no reason to type it, and
// advertising it would make its argv shape a contract.
func TestCompleteIsHidden(t *testing.T) {
	for _, args := range [][]string{nil, {"stauts"}} {
		var stdout, stderr bytes.Buffer
		run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, noPreview)
		if strings.Contains(stdout.String(), "__complete") {
			t.Errorf("`bif %s` advertises __complete:\n%s", strings.Join(args, " "), stdout.String())
		}
	}

	// completion, by contrast, is a command an operator runs, so it joins the
	// listing the unknown-command path prints.
	var stdout, stderr bytes.Buffer
	run(context.Background(), []string{"stauts"}, strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, noPreview)
	if !strings.Contains(stdout.String(), "Available commands: status, promote, preview, completion") {
		t.Errorf("completion is missing from the command listing:\n%s", stdout.String())
	}

	// And it is not offered as a candidate either, which is the same rule
	// applied where an operator would actually meet it.
	if got := complete(t, noNetwork(t), "__comp"); got != nil {
		t.Errorf("__complete offers itself as a candidate: %v", got)
	}
}

func TestCompletionCmd(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
	}{
		{
			name:     "zsh",
			args:     []string{"completion", "zsh"},
			wantCode: 0,
			wantOut:  []string{"#compdef bif", "bif __complete", "compdef _bif bif"},
		},
		{
			name:     "bash",
			args:     []string{"completion", "bash"},
			wantCode: 0,
			wantOut:  []string{"bif __complete", "complete -F _bif bif"},
		},
		{
			name:     "no shell named",
			args:     []string{"completion"},
			wantCode: 1,
			wantOut:  []string{completionUsage},
		},
		{
			name:     "unknown shell",
			args:     []string{"completion", "fish"},
			wantCode: 1,
			wantOut:  []string{"Unknown shell: fish", completionUsage},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, noPreview)
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

// TestShimsAreValidShell parses the emitted shims with the shells that will
// run them.
//
// Nothing else in this suite would notice a broken one: Go sees two strings,
// and the failure lands in the operator's shell startup — a `source <(bif
// completion zsh)` in a .zshrc that no longer completes, or worse, that
// errors on every new terminal. `-n` is a parse, not a run, so this asserts
// what a Go test can honestly assert about shell it does not execute.
func TestShimsAreValidShell(t *testing.T) {
	tests := []struct {
		shell string
		arg   string
		file  string
	}{
		{shell: "zsh", arg: "zsh", file: "_bif"},
		{shell: "bash", arg: "bash", file: "bif.bash"},
	}
	for _, tc := range tests {
		t.Run(tc.shell, func(t *testing.T) {
			bin, err := osexec.LookPath(tc.shell)
			if err != nil {
				// A skip is the right answer on a developer machine missing a
				// shell, and the wrong one in CI, where it would quietly
				// delete half the gate. The workflow installs both.
				if os.Getenv("CI") != "" {
					t.Fatalf("%s is not installed, and CI must not skip this check", tc.shell)
				}
				t.Skipf("%s is not installed", tc.shell)
			}

			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{"completion", tc.arg}, strings.NewReader(""), &stdout, &stderr, noCluster, unreachableBuilds, noPreview); code != 0 {
				t.Fatalf("bif completion %s exited %d", tc.arg, code)
			}

			path := filepath.Join(t.TempDir(), tc.file)
			if err := os.WriteFile(path, stdout.Bytes(), 0o600); err != nil {
				t.Fatalf("write shim: %v", err)
			}
			cmd := osexec.Command(bin, "-n", path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s -n rejected the emitted shim: %v\n%s", tc.shell, err, out)
			}
		})
	}
}
