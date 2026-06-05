// Package promote contains the pure parsing/decision logic that mirrors ib.py.
package promote

import (
	"regexp"
	"strings"
)

var (
	suffixedSHA = regexp.MustCompile(`^([a-f0-9]+)-(staging|prod)$`)
	plainSHA    = regexp.MustCompile(`^([a-f0-9]{7,})$`)
)

func ExtractTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return "latest"
}

func ExtractSHA(tag string) string {
	if m := suffixedSHA.FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	if m := plainSHA.FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	return ""
}

// NewProdTag returns the tag we'd write to prod when promoting. Matches
// ib.py:220 — if either current tag carries a -staging/-prod suffix, the
// service uses the suffixed naming convention.
func NewProdTag(sha, stagingTag, prodTag string) string {
	if strings.Contains(stagingTag, "-staging") || strings.Contains(prodTag, "-prod") {
		return sha + "-prod"
	}
	return sha
}
