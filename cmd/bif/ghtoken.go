package main

import (
	"context"
	"errors"
	// Aliased because this package's tests already own the name `exec` (the
	// helper that runs one whole bif invocation).
	osexec "os/exec"
	"strings"
	"time"
)

// ghTokenTimeout bounds `gh auth token`. It reads a keychain or a config file
// and normally answers in tens of milliseconds; a gh that is waiting on
// something (a locked keychain prompt, a broken credential helper) must not
// spend the whole lookup budget before a single request is made.
const ghTokenTimeout = 2 * time.Second

// discoverGitHubToken finds a GitHub token for the CLI: the first non-empty of
// GITHUB_TOKEN, GH_TOKEN, then whatever gh says. Empty when there is none,
// which is a working answer — see commitlog.New.
//
// The environment wins over gh so that a token set deliberately for this
// invocation is the one used, and so the common CI shape never spawns a
// process. GITHUB_TOKEN before GH_TOKEN matches the name the bifrost server's
// own config reads.
//
// getenv and gh are parameters so the order can be tested without touching the
// real environment or running a real gh. The token is returned and never
// logged; nothing in this CLI prints one.
//
// This lives in cmd/bif, not in internal/commitlog, because it is the CLI's own
// answer to "whose token?": the server has its configured per-owner tokens
// and must never read an operator's environment or run gh. The package takes a
// token; where one comes from is each caller's business.
func discoverGitHubToken(ctx context.Context, getenv func(string) string, gh func(context.Context) (string, error)) string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
	}
	if gh == nil {
		return ""
	}
	// Any failure is "no token": gh not installed, not logged in, too slow.
	// The requests are then made anonymously and either work (public repo) or
	// produce the one warning the caller prints.
	token, err := gh(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// ghAuthToken asks the GitHub CLI for the logged-in user's token. It is the
// production value of discoverGitHubToken's gh parameter.
//
// LookPath first, so a machine without gh costs a PATH scan and not a failed
// exec. Stderr is discarded: gh's "not logged in" advice belongs to someone
// who ran gh, not to someone who ran `bif promote`.
func ghAuthToken(ctx context.Context) (string, error) {
	path, err := osexec.LookPath("gh")
	if err != nil {
		return "", errors.New("gh is not on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()
	out, err := osexec.CommandContext(ctx, path, "auth", "token").Output()
	if err != nil {
		return "", errors.New("gh auth token failed")
	}
	return strings.TrimSpace(string(out)), nil
}
