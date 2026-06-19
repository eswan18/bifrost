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

// NewProdTag returns the tag we'd write to prod when promoting. Mirrors
// ib.py's new_prod_tag_for. The tag scheme follows the artifact being
// promoted — the staging image — NOT the current prod image. A service can
// migrate to environment-agnostic builds (plain {sha} + latest, no suffix)
// while prod still runs a legacy {sha}-prod image; keying off the stale prod
// tag in that window synthesizes a {sha}-prod reference that was never built,
// causing ImagePullBackOff (forecasting prod outage, June 2026). prodTag is
// intentionally unused.
func NewProdTag(sha, stagingTag, prodTag string) string {
	if strings.Contains(stagingTag, "-staging") {
		return sha + "-prod"
	}
	return sha
}
