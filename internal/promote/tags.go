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

// ImageBase returns the registry/repository portion of a full image ref,
// dropping the ":tag" suffix. Only the last colon separates the tag, so a
// registry host:port (e.g. "host:5000/repo:tag" → "host:5000/repo") is
// preserved. A ref with no tag is returned unchanged.
func ImageBase(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[:i]
	}
	return image
}

// ReplaceTag returns image with its tag replaced by tag — the step that turns
// the tag a promote decided on into the image reference written to the ArgoCD
// Application.
//
// It lives here, next to ImageBase, because the repository half of that
// reference is a decision and not a formatting detail: it comes from the
// artifact actually running, never from the app's name. ib.py builds the same
// string as REGISTRY + "/" + app + ":" + tag and so cannot express an image
// whose repository is named anything else — see ImageBase and
// differential_test.go's TestImageBaseMatchesOracle. Both the server and
// cmd/bif call this, so neither can drift back toward the app name.
func ReplaceTag(image, tag string) string {
	return ImageBase(image) + ":" + tag
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
