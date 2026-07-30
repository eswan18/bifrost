package preview

import (
	"strings"
	"testing"
)

func TestTagForBranch(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		want   string
	}{
		{"already a valid tag", "hae-cadence", "hae-cadence"},
		{"slash and space become dashes", "feat/preview API", "feat-preview-api"},
		{"underscore becomes a dash", "Feat_X", "feat-x"},
		{"leading, trailing, and doubled dashes trimmed", "--weird--", "weird"},
		{"non-ASCII characters dropped", "änderung", "nderung"},
		{"40-char branch capped at 30 with no trailing dash", strings.Repeat("abcdefghij", 4), strings.Repeat("abcdefghij", 3)},
		{"cap landing on a dash trims it", strings.Repeat("x", 29) + "-" + strings.Repeat("y", 10), strings.Repeat("x", 29)},
		{"mixed separators collapse to one dash", "foo/_ bar", "foo-bar"},
		{"all-invalid input yields empty tag", "!!!???", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TagForBranch(tt.branch); got != tt.want {
				t.Errorf("TagForBranch(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}

// TestTagForBranchIsManyToOne pins the property the whole tag-collision guard
// exists for, so it is documented in the test suite rather than passed around
// as folklore: distinct branches can and do derive the same tag.
//
// Both paths are covered because they are independent, and a fix aimed at one
// leaves the other untouched. Truncation needs a long branch name; character
// folding needs nothing but a different separator, and no branch here is
// anywhere near maxTagLen. That second pair is why appending a hash to
// truncated tags is not a fix (see TagForBranch's own note) — it would split
// the first pair and leave the second colliding exactly as before.
//
// The guard that actually resolves the collision lives in
// Orchestrator.Up (refuseUnusableNamespace / ErrTagCollision); see
// TestUpRefusesACollidingTagFromADifferentBranch.
func TestTagForBranchIsManyToOne(t *testing.T) {
	collisions := []struct {
		name string
		a, b string
		want string
	}{
		{
			name: "truncation: two long branches sharing their first 30 characters",
			a:    "feature/add-user-profile-avatars-v1",
			b:    "feature/add-user-profile-avatars-v2",
			want: "feature-add-user-profile-avata",
		},
		{
			name: "character folding: separators fold together, no truncation involved",
			a:    "feat/foo",
			b:    "feat_foo",
			want: "feat-foo",
		},
	}
	for _, c := range collisions {
		t.Run(c.name, func(t *testing.T) {
			if c.a == c.b {
				t.Fatalf("the two branches are identical (%q), so this proves nothing", c.a)
			}
			gotA, gotB := TagForBranch(c.a), TagForBranch(c.b)
			if gotA != c.want || gotB != c.want {
				t.Fatalf("TagForBranch(%q) = %q and TagForBranch(%q) = %q, want both to be %q — "+
					"the collision this documents no longer holds, so re-check "+
					"Up's tag-collision guard still has a collision to guard against",
					c.a, gotA, c.b, gotB, c.want)
			}
			if len(c.want) > maxTagLen {
				t.Errorf("expected tag %q is %d chars, longer than maxTagLen (%d)", c.want, len(c.want), maxTagLen)
			}
		})
	}
}
