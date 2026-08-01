// Package preview derives Kubernetes-safe identifiers for ephemeral preview
// environments and assembles the manifests that back them.
package preview

import "strings"

// maxTagLen caps a branch-derived tag well under Kubernetes/Docker naming
// limits, leaving room for callers to append their own suffixes.
const maxTagLen = 30

// TagForBranch derives a Kubernetes/Docker-safe tag from a git branch name.
// The branch is lowercased; '/', '_', and spaces become '-'; any other
// character outside [a-z0-9-] is dropped; runs of '-' collapse to one;
// leading and trailing '-' are trimmed; and the result is capped at
// maxTagLen characters, trimming a trailing '-' the cut may leave behind.
//
// The mapping is MANY-TO-ONE, and is deliberately left that way. Two
// independent paths collide: truncation
// ("feature/add-user-profile-avatars-v1" and "...-v2" share their first
// maxTagLen characters) and character folding, with no truncation involved at
// all ("feat/foo", "feat_foo" and "feat-foo" all fold to "feat-foo").
// TestTagForBranchIsManyToOne pins both, so the property stays a known
// characteristic of this function rather than folklore.
//
// Do NOT "fix" it by appending a hash of the branch to a truncated tag. That
// closes only the truncation path while leaving folding — which needs no long
// branch name at all — wide open, and it would change the tag of every
// existing long-branch preview, orphaning that preview's namespace and its
// Neon branch from the `bif preview down` that should have reclaimed them. The
// general fix is at the point of use instead: Up refuses to run against a
// namespace another branch already owns (refuseUnusableNamespace and
// ErrTagCollision in orchestrator.go), which catches both paths and every
// other one a future edit here might introduce.
func TagForBranch(branch string) string {
	lower := strings.ToLower(branch)

	var b strings.Builder
	for _, r := range lower {
		switch {
		case r == '/' || r == '_' || r == ' ':
			b.WriteByte('-')
		case r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}

	tag := strings.Trim(collapseDashes(b.String()), "-")
	if len(tag) > maxTagLen {
		tag = tag[:maxTagLen]
	}
	return strings.TrimRight(tag, "-")
}

// collapseDashes replaces runs of consecutive '-' with a single '-'.
func collapseDashes(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
