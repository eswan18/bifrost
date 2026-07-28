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
