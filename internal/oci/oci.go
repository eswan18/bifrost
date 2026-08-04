// Package oci answers one question about a container registry: what is the
// digest of the IMAGE MANIFEST behind this tag?
//
// It exists because two tags naming the same bytes is a normal, frequent state
// in this fleet and used to read as a stalled deploy. Every image here is built
// by buildx, which pushes an OCI *index* per tag: the image manifest, plus a
// provenance attestation generated fresh for that build. The attestation
// differs every time, so the INDEX digest differs every time — while the image
// manifest under it is identical whenever the build inputs were. bifrost's
// server image builds ./cmd/bifrost, so a commit touching only ./cmd/bif
// produces a new tag whose image is byte-identical to the last one, ArgoCD
// Image Updater correctly does nothing, and `bif status --attention` used to
// report a deploy that had stalled when nothing had.
//
// So the unit this package returns is the image manifest's digest and never the
// index's. Comparing index digests would reproduce exactly the bug this was
// written to fix, which is why selectManifest below is the part with the tests.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Media types this speaks. Both vendors' spellings are listed for both shapes:
// an index (a "manifest list" in Docker's vocabulary) and a single image
// manifest. A registry serves whichever the tag actually holds, and which one
// that is varies by BUILDER in this fleet — buildx pushes indexes,
// `gcr.io/cloud-builders/docker build` pushes bare manifests — so both have to
// be accepted or half the services would fail to resolve.
const (
	ociIndexType       = "application/vnd.oci.image.index.v1+json"
	ociManifestType    = "application/vnd.oci.image.manifest.v1+json"
	dockerListType     = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
)

// acceptHeader is what a manifest GET asks for. Sending it is not optional: a
// registry defaults to the legacy schema-1 manifest for a client that does not
// say what it understands, and the digest of that is not the digest of anything
// we want to compare.
var acceptHeader = strings.Join([]string{
	ociIndexType, ociManifestType, dockerListType, dockerManifestType,
}, ",")

// attestationRefType marks an index entry as build provenance rather than an
// image — the annotation buildx writes on every attestation manifest it pushes.
const (
	attestationAnnotation = "vnd.docker.reference.type"
	attestationRefType    = "attestation-manifest"
)

// unknownPlatform is the other half of how an attestation announces itself:
// architecture and os both "unknown", because provenance is not built for a
// platform. Either signal is enough to exclude an entry, and BOTH are checked
// because they are independent — buildx has emitted attestations with the
// annotation and a plausible-looking platform, and an index written by another
// tool may carry the unknown platform with no annotation at all. An attestation
// mistaken for an image is not a wrong-looking answer; it is a digest that
// changes on every build, which is the bug.
const unknownPlatform = "unknown"

// Platform is the OS/architecture whose image manifest we want out of an index.
type Platform struct {
	OS           string
	Architecture string
}

// DeployedPlatform is what this fleet runs: GKE's nodes are linux/amd64, and
// every image is built on a Cloud Build worker of the same shape. It is the
// selection target rather than a filter that must match — see selectManifest,
// which falls back to an unambiguous single image manifest — because both sides
// of any comparison resolve through the same target, so a fleet that moved to
// arm64 would still compare like with like.
var DeployedPlatform = Platform{OS: "linux", Architecture: "amd64"}

// descriptor is one entry of an index: what it points at, and what it is.
type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Platform    *platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// document is the part of a manifest response this needs: enough to tell an
// index from an image manifest, and an index's entries apart from each other.
type document struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
}

// isIndex reports whether the fetched document is an index rather than an image
// manifest. The media type is authoritative when present; the entry list is the
// fallback, since an image manifest has no `manifests` array at all.
func (d document) isIndex() bool {
	switch d.MediaType {
	case ociIndexType, dockerListType:
		return true
	case ociManifestType, dockerManifestType:
		return false
	}
	return len(d.Manifests) > 0
}

// isImage reports whether an entry points at an image manifest — as opposed to
// an attestation, or a nested index this cannot resolve without another fetch.
func (d descriptor) isImage() bool {
	if d.MediaType != ociManifestType && d.MediaType != dockerManifestType {
		return false
	}
	if d.Annotations[attestationAnnotation] == attestationRefType {
		return false
	}
	if d.Platform != nil && (d.Platform.OS == unknownPlatform || d.Platform.Architecture == unknownPlatform) {
		return false
	}
	return d.Digest != ""
}

func (d descriptor) matches(p Platform) bool {
	return d.Platform != nil && d.Platform.OS == p.OS && d.Platform.Architecture == p.Architecture
}

// selectManifest turns a manifest response into the digest of the image
// manifest inside it.
//
// A bare image manifest is its own answer, and the digest is computed from the
// bytes rather than read from the Docker-Content-Digest header: the digest of a
// manifest IS the hash of its bytes, so computing it needs no trust in a header
// the registry may or may not send.
//
// For an index, every entry that is not an image for a platform is dropped
// first (see isImage), then the deployed platform picks among what is left. A
// lone survivor is accepted whatever platform it claims, because it is
// unambiguous; anything still ambiguous is an ERROR rather than a guess, which
// the caller reads as "could not tell" and falls back to comparing tags.
func selectManifest(body []byte, want Platform) (string, error) {
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parsing manifest: %w", err)
	}
	if !doc.isIndex() {
		sum := sha256.Sum256(body)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}

	var images []descriptor
	for _, m := range doc.Manifests {
		if m.isImage() {
			images = append(images, m)
		}
	}
	var matching []descriptor
	for _, m := range images {
		if m.matches(want) {
			matching = append(matching, m)
		}
	}
	switch {
	case len(matching) == 1:
		return matching[0].Digest, nil
	case len(matching) > 1:
		return "", fmt.Errorf("index has %d image manifests for %s/%s", len(matching), want.OS, want.Architecture)
	case len(images) == 1:
		return images[0].Digest, nil
	case len(images) == 0:
		return "", fmt.Errorf("index has no image manifest (%d entries, all attestations or nested indexes)", len(doc.Manifests))
	default:
		return "", fmt.Errorf("index has %d image manifests and none for %s/%s", len(images), want.OS, want.Architecture)
	}
}

// Resolver reads manifests from a registry over one HTTP client, with one
// token, for the life of a command invocation.
type Resolver struct {
	client   *http.Client
	token    string
	platform Platform
	// scheme is https everywhere but the tests, which point this at an
	// httptest server.
	scheme string
}

// registryScope is what a manifest read needs. Artifact Registry accepts a
// Google OAuth access token directly as a Bearer credential on its Docker API,
// so cloud-platform is the whole of the authentication story here.
const registryScope = "https://www.googleapis.com/auth/cloud-platform"

// New returns a Resolver authenticated with Application Default Credentials —
// the same mechanism internal/gcb reads Cloud Build with (cloudbuild.NewService
// resolves ADC too): workload identity in-cluster, gcloud ADC locally. Nothing
// here shells out to gcloud.
//
// The token is fetched ONCE, here, and reused for every lookup the returned
// Resolver performs. `bif status --attention` resolves several services at once
// and two tags per service, and a token fetch per request would put an
// unnecessary round trip (or, on a metadata server, several) inside a path
// bounded by a short timeout.
func New(ctx context.Context) (*Resolver, error) {
	ts, err := google.DefaultTokenSource(ctx, registryScope)
	if err != nil {
		return nil, err
	}
	return NewWithTokenSource(ctx, ts)
}

// NewWithTokenSource is New with the credential supplied, so a caller (and the
// tests) can prove the one-fetch property against a source that counts.
func NewWithTokenSource(ctx context.Context, ts oauth2.TokenSource) (*Resolver, error) {
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("registry credentials: %w", err)
	}
	return &Resolver{
		client:   &http.Client{},
		token:    tok.AccessToken,
		platform: DeployedPlatform,
		scheme:   "https",
	}, nil
}

// maxManifestBytes bounds the body this will read. Manifests and indexes are a
// few kilobytes; a megabyte is far beyond any of them and keeps a
// misconfigured endpoint from being able to hand a CLI an unbounded read.
const maxManifestBytes = 1 << 20

// ManifestDigest returns the digest of the image manifest behind an image
// reference such as "us-central1-docker.pkg.dev/p/r/bifrost:0cbfc9f".
//
// Every failure — a bad reference, a network error, 401, a missing tag, an
// index of nothing but attestations — is returned as an error and never as an
// empty string that could be compared equal to another empty string. The one
// caller treats any error as "could not tell" and falls back to comparing tags,
// so an error must be unmistakable.
func (r *Resolver) ManifestDigest(ctx context.Context, image string) (string, error) {
	host, repo, ref, err := parseRef(image)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", r.scheme, host, repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", acceptHeader)
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", image, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading %s: registry returned %s", image, resp.Status)
	}
	digest, err := selectManifest(body, r.platform)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", image, err)
	}
	return digest, nil
}

// parseRef splits an image reference into registry host, repository path and
// reference (tag or digest).
//
// The host is the first path element, and it is required to look like one — a
// dot, a port, or localhost — because "bifrost:abc/x" is not a registry and
// silently treating it as one would produce a request to a host that does not
// exist. Every image this sees comes from a pod's container spec in this fleet,
// where they are all fully qualified Artifact Registry references.
func parseRef(image string) (host, repo, ref string, err error) {
	slash := strings.Index(image, "/")
	if slash < 0 {
		return "", "", "", fmt.Errorf("image reference %q has no registry host", image)
	}
	host = image[:slash]
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return "", "", "", fmt.Errorf("image reference %q has no registry host", image)
	}
	rest := image[slash+1:]

	// A digest reference pins the exact manifest already; a tag has to be
	// looked up. "@" wins over ":" because "repo:tag@sha256:..." is legal and
	// the digest is the more specific half.
	if at := strings.Index(rest, "@"); at >= 0 {
		repo, ref = rest[:at], rest[at+1:]
	} else if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		repo, ref = rest[:colon], rest[colon+1:]
	} else {
		repo, ref = rest, "latest"
	}
	if repo == "" || ref == "" {
		return "", "", "", fmt.Errorf("image reference %q has no repository or tag", image)
	}
	return host, repo, ref, nil
}
