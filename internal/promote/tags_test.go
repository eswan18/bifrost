package promote

import "testing"

func TestExtractTag(t *testing.T) {
	tests := []struct {
		image, want string
	}{
		{"us-central1-docker.pkg.dev/proj/containers/foo:abc123", "abc123"},
		{"us-central1-docker.pkg.dev/proj/containers/foo:abc123-staging", "abc123-staging"},
		{"foo", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := ExtractTag(tt.image); got != tt.want {
				t.Errorf("ExtractTag(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestExtractSHA(t *testing.T) {
	tests := []struct {
		tag, want string
	}{
		{"abc1234", "abc1234"},
		{"abc1234-staging", "abc1234"},
		{"abc1234-prod", "abc1234"},
		{"latest", ""},
		{"v1.2.3", ""},
		{"deadbeef0123", "deadbeef0123"},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			if got := ExtractSHA(tt.tag); got != tt.want {
				t.Errorf("ExtractSHA(%q) = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}
}

func TestNewProdTag(t *testing.T) {
	tests := []struct {
		name, sha, stagingTag, prodTag, want string
	}{
		{"plain", "abc1234", "abc1234", "def5678", "abc1234"},
		{"suffixed via staging", "abc1234", "abc1234-staging", "def5678-prod", "abc1234-prod"},
		{"suffixed via prod only", "abc1234", "abc1234", "def5678-prod", "abc1234-prod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewProdTag(tt.sha, tt.stagingTag, tt.prodTag); got != tt.want {
				t.Errorf("NewProdTag(%q,%q,%q) = %q, want %q",
					tt.sha, tt.stagingTag, tt.prodTag, got, tt.want)
			}
		})
	}
}
