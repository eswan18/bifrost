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
