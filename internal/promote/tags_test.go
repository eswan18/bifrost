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

func TestImageBase(t *testing.T) {
	tests := []struct {
		image, want string
	}{
		{"reg/foo:abc1234", "reg/foo"},
		{"reg/foo:abc1234-staging", "reg/foo"},
		{"host:5000/repo:tag", "host:5000/repo"},
		{"noTag", "noTag"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := ImageBase(tt.image); got != tt.want {
				t.Errorf("ImageBase(%q) = %q, want %q", tt.image, got, tt.want)
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
		// Migration window: staging already on environment-agnostic (plain)
		// builds while prod still runs a legacy {sha}-prod image. The tag scheme
		// follows the staging artifact, so a plain {sha}-prod must NOT be
		// synthesized (it was never built). Regression: forecasting prod
		// ImagePullBackOff, June 2026.
		{"plain staging, legacy prod", "e521080", "e521080", "2679590-prod", "e521080"},
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
