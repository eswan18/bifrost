package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// ---- fixtures ------------------------------------------------------------

// The three tags in the false alarm this package was written for, as the
// registry actually answered for them:
//
//	eb12dfa  index: [61acb248b85e (image), d5d58103a519 (attestation)]
//	9647bc1  index: [61acb248b85e (image), cf1ea3588d56 (attestation)]
//	0cbfc9f  index: [61acb248b85e (image), 59b130d8de1f (attestation)]
//
// One image manifest, shared; one attestation per build, never shared. The
// index digest therefore differs for all three while the image is one image —
// which is why an index digest is not an answer to "is this the same image?".
const (
	sharedImageDigest = "sha256:61acb248b85e0000000000000000000000000000000000000000000000000000"
	attestationA      = "sha256:d5d58103a519000000000000000000000000000000000000000000000000000"
	attestationC      = "sha256:59b130d8de1f000000000000000000000000000000000000000000000000000"
	otherImageDigest  = "sha256:aa11bb22cc33000000000000000000000000000000000000000000000000000"
)

// buildxIndex is what buildx pushes for one tag: the image manifest for the
// platform, plus a provenance attestation.
//
// The attestation entry is deliberately built to LOOK like an image manifest —
// same mediaType, an entry in the same list — because that is what it looks
// like in the real registry. Only its unknown platform and its
// reference-type annotation say otherwise, and every fixture here keeps at
// most one of those two signals so that neither check can be deleted without
// a test noticing.
func buildxIndex(imageDigest, attestationDigest string) string {
	return fmt.Sprintf(`{
	  "schemaVersion": 2,
	  "mediaType": %q,
	  "manifests": [
	    {"mediaType": %q, "digest": %q, "size": 1234,
	     "platform": {"architecture": "amd64", "os": "linux"}},
	    {"mediaType": %q, "digest": %q, "size": 567,
	     "annotations": {"vnd.docker.reference.digest": %q, "vnd.docker.reference.type": "attestation-manifest"},
	     "platform": {"architecture": "unknown", "os": "unknown"}}
	  ]
	}`, ociIndexType, ociManifestType, imageDigest, ociManifestType, attestationDigest, imageDigest)
}

// plainManifest is what `docker build` + `docker push` leaves on a tag: no
// index at all. footstrike-dashboard and every other non-buildx service in the
// fleet is tagged this way.
const plainManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {"mediaType": "application/vnd.docker.container.image.v1+json", "digest": "sha256:cfg", "size": 7},
  "layers": [{"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": "sha256:l1", "size": 99}]
}`

// registryError is what a registry answers a request for a tag that isn't
// there: an error document, in JSON, with a non-200 status.
const registryError = `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`

// ---- fake registry -------------------------------------------------------

// fakeRegistry serves manifests from a "repo:ref" -> body map and records what
// was asked for, including the Accept header — which is not decoration: without
// it a registry answers with a legacy schema-1 manifest whose digest is not
// comparable to anything here.
type fakeRegistry struct {
	bodies map[string]string
	status map[string]int

	mu      sync.Mutex
	gets    []string
	accepts []string
	auths   []string
	// gate, when non-nil, blocks every request until it is closed. Used to
	// prove concurrent resolution.
	gate chan struct{}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /v2/<repo>/manifests/<ref>
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	i := strings.Index(path, "/manifests/")
	if i < 0 {
		http.Error(w, "not a manifest request", http.StatusNotFound)
		return
	}
	key := path[:i] + ":" + path[i+len("/manifests/"):]

	f.mu.Lock()
	f.gets = append(f.gets, key)
	f.accepts = append(f.accepts, r.Header.Get("Accept"))
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}

	// Error bodies are JSON, because a real registry's are — and that is what
	// makes the status check load-bearing rather than incidental. An
	// implementation that ignored the status code would happily hash
	// `{"errors":[...]}` into a perfectly stable digest, and TWO missing tags
	// would then compare EQUAL: a registry outage silently suppressing a real
	// stalled sync, which is the one outcome this must never produce.
	if code, ok := f.status[key]; ok {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, registryError)
		return
	}
	body, ok := f.bodies[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, registryError)
		return
	}
	w.Header().Set("Content-Type", ociIndexType)
	_, _ = io.WriteString(w, body)
}

func (f *fakeRegistry) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gets...)
}

// countingSource hands out one token and counts how often it was asked.
type countingSource struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *countingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &oauth2.Token{AccessToken: "ya29.test"}, nil
}

func (s *countingSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newTestResolver points a Resolver at an httptest server. The image
// references the tests pass carry that server's host, so nothing here has to
// special-case the URL.
func newTestResolver(t *testing.T, reg *fakeRegistry) (*Resolver, *countingSource, string) {
	t.Helper()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	src := &countingSource{}
	r, err := NewWithTokenSource(context.Background(), src)
	if err != nil {
		t.Fatalf("NewWithTokenSource: %v", err)
	}
	r.scheme = "http"
	return r, src, strings.TrimPrefix(srv.URL, "http://")
}

// ---- the bug, as a test --------------------------------------------------

// TestTwoTagsSharingAnImageResolveEqual is the false alarm itself. Two builds
// of commits that changed nothing in the server image produce two tags whose
// INDEXES differ — different provenance attestation each time — over one shared
// image manifest. Resolving to the index digest reports drift where there is
// none, which is the bug; resolving to the image manifest reports the truth.
//
// Both halves are asserted, because only the second one discriminates: a
// resolver that returned the index digest would still return equal digests for
// two requests of the SAME tag.
func TestTwoTagsSharingAnImageResolveEqual(t *testing.T) {
	reg := &fakeRegistry{bodies: map[string]string{
		"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA),
		"p/r/bifrost:0cbfc9f": buildxIndex(sharedImageDigest, attestationC),
	}}
	r, _, host := newTestResolver(t, reg)

	running, err := r.ManifestDigest(context.Background(), host+"/p/r/bifrost:eb12dfa")
	if err != nil {
		t.Fatalf("resolving the running tag: %v", err)
	}
	built, err := r.ManifestDigest(context.Background(), host+"/p/r/bifrost:0cbfc9f")
	if err != nil {
		t.Fatalf("resolving the built tag: %v", err)
	}
	if running != built {
		t.Errorf("two tags over one image manifest resolved differently:\n running %s\n built   %s", running, built)
	}
	if running != sharedImageDigest {
		t.Errorf("digest = %s, want the IMAGE MANIFEST digest %s; an index digest differs per build and reproduces the bug",
			running, sharedImageDigest)
	}
}

// TestDifferentImagesResolveDifferently is the other direction, and it is what
// keeps the fix from being "always say they match": a real content change must
// still come out as a different digest.
func TestDifferentImagesResolveDifferently(t *testing.T) {
	reg := &fakeRegistry{bodies: map[string]string{
		"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA),
		"p/r/bifrost:0cbfc9f": buildxIndex(otherImageDigest, attestationC),
	}}
	r, _, host := newTestResolver(t, reg)

	running, err := r.ManifestDigest(context.Background(), host+"/p/r/bifrost:eb12dfa")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	built, err := r.ManifestDigest(context.Background(), host+"/p/r/bifrost:0cbfc9f")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if running == built {
		t.Errorf("different image manifests resolved to the same digest %s", running)
	}
}

// TestAttestationsAreExcluded pins the exclusion entry by entry, and every
// fixture here is built so that ONE check is the only thing standing between
// the test and a wrong answer.
func TestAttestationsAreExcluded(t *testing.T) {
	entry := func(digest, extra string) string {
		return fmt.Sprintf(`{"mediaType": %q, "digest": %q, "size": 1%s}`, ociManifestType, digest, extra)
	}
	index := func(entries ...string) string {
		return fmt.Sprintf(`{"mediaType": %q, "manifests": [%s]}`, ociIndexType, strings.Join(entries, ","))
	}

	tests := []struct {
		name string
		doc  string
		want string
		why  string
	}{
		{
			name: "unknown platform",
			doc: index(
				entry(sharedImageDigest, `, "platform": {"os": "linux", "architecture": "amd64"}`),
				entry(attestationA, `, "platform": {"os": "unknown", "architecture": "unknown"}`),
			),
			want: sharedImageDigest,
			why:  "an attestation announces itself with an unknown platform and no other signal here",
		},
		{
			// The discriminating fixture for the annotation check: this
			// attestation claims the DEPLOYED platform, so platform filtering
			// alone leaves two candidates and cannot pick.
			name: "annotated as an attestation while claiming a real platform",
			doc: index(
				entry(sharedImageDigest, `, "platform": {"os": "linux", "architecture": "amd64"}`),
				entry(attestationA, `, "platform": {"os": "linux", "architecture": "amd64"}, "annotations": {"vnd.docker.reference.type": "attestation-manifest"}`),
			),
			want: sharedImageDigest,
			why:  "the reference-type annotation is the only thing marking this entry as provenance",
		},
		{
			name: "an index of nothing but attestations",
			doc: index(
				entry(attestationA, `, "platform": {"os": "unknown", "architecture": "unknown"}`),
			),
			want: "",
			why:  "there is no image to compare; an error is the answer, not a digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectManifest([]byte(tc.doc), DeployedPlatform)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("selectManifest = %q, want an error (%s)", got, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectManifest: %v (%s)", err, tc.why)
			}
			if got != tc.want {
				t.Errorf("selectManifest = %s, want %s (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestMultiPlatformIndexPicksTheDeployedPlatform: a genuine multi-arch index
// has several real image manifests, and the one that answers "what is staging
// running?" is the one for the nodes staging runs on.
func TestMultiPlatformIndexPicksTheDeployedPlatform(t *testing.T) {
	doc := fmt.Sprintf(`{"mediaType": %q, "manifests": [
	  {"mediaType": %q, "digest": %q, "platform": {"os": "linux", "architecture": "arm64"}},
	  {"mediaType": %q, "digest": %q, "platform": {"os": "linux", "architecture": "amd64"}},
	  {"mediaType": %q, "digest": %q, "platform": {"os": "unknown", "architecture": "unknown"}}
	]}`, ociIndexType,
		ociManifestType, otherImageDigest,
		ociManifestType, sharedImageDigest,
		ociManifestType, attestationA)

	got, err := selectManifest([]byte(doc), DeployedPlatform)
	if err != nil {
		t.Fatalf("selectManifest: %v", err)
	}
	if got != sharedImageDigest {
		t.Errorf("selectManifest = %s, want the linux/amd64 manifest %s", got, sharedImageDigest)
	}
}

// TestPlainManifestIsHashedFromItsBytes: a tag holding an image manifest
// rather than an index resolves to the hash of the manifest itself, which is
// what its digest IS. Every non-buildx service in the fleet is tagged this way,
// so a resolver that only understood indexes would fail on most of it.
func TestPlainManifestIsHashedFromItsBytes(t *testing.T) {
	reg := &fakeRegistry{bodies: map[string]string{"p/r/dash:abc1234-staging": plainManifest}}
	r, _, host := newTestResolver(t, reg)

	got, err := r.ManifestDigest(context.Background(), host+"/p/r/dash:abc1234-staging")
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	sum := sha256.Sum256([]byte(plainManifest))
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("digest = %s, want %s", got, want)
	}
}

// TestRequestAsksForBothVendorsBothShapes: the Accept header is load-bearing.
// A registry hands a client that does not say what it understands a legacy
// schema-1 manifest, whose digest has nothing to do with the one we compare —
// and this fleet's tags are indexes (buildx) AND bare manifests (docker build),
// in both OCI and Docker spellings.
func TestRequestAsksForBothVendorsBothShapes(t *testing.T) {
	reg := &fakeRegistry{bodies: map[string]string{"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA)}}
	r, _, host := newTestResolver(t, reg)
	if _, err := r.ManifestDigest(context.Background(), host+"/p/r/bifrost:eb12dfa"); err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}

	reg.mu.Lock()
	accept := reg.accepts[0]
	auth := reg.auths[0]
	reg.mu.Unlock()

	for _, want := range []string{ociIndexType, ociManifestType, dockerListType, dockerManifestType} {
		if !strings.Contains(accept, want) {
			t.Errorf("Accept = %q, missing %q", accept, want)
		}
	}
	if auth != "Bearer ya29.test" {
		t.Errorf("Authorization = %q, want the ADC access token as a Bearer credential", auth)
	}
}

// TestTokenIsFetchedOncePerResolver: --attention resolves two tags per stalled
// service across the fleet, and the credential is fetched once for all of them.
func TestTokenIsFetchedOncePerResolver(t *testing.T) {
	reg := &fakeRegistry{bodies: map[string]string{
		"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA),
		"p/r/bifrost:0cbfc9f": buildxIndex(sharedImageDigest, attestationC),
		"p/r/comms:9647bc1":   buildxIndex(otherImageDigest, attestationA),
	}}
	r, src, host := newTestResolver(t, reg)
	for _, ref := range []string{"p/r/bifrost:eb12dfa", "p/r/bifrost:0cbfc9f", "p/r/comms:9647bc1"} {
		if _, err := r.ManifestDigest(context.Background(), host+"/"+ref); err != nil {
			t.Fatalf("ManifestDigest(%s): %v", ref, err)
		}
	}
	if got := src.count(); got != 1 {
		t.Errorf("token fetched %d times for 3 lookups, want 1", got)
	}
	if got := len(reg.asked()); got != 3 {
		t.Errorf("registry saw %d requests, want 3", got)
	}
}

// TestLookupFailuresAreErrors: every way this can fail to KNOW must be an
// error, because the caller reads an error as "fall back to comparing tags and
// warn" and would read a bare digest as truth.
func TestLookupFailuresAreErrors(t *testing.T) {
	reg := &fakeRegistry{
		bodies: map[string]string{
			"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA),
			"p/r/bifrost:garbage": "{not json",
		},
		status: map[string]int{
			"p/r/bifrost:denied": http.StatusUnauthorized,
		},
	}
	r, _, host := newTestResolver(t, reg)

	for _, tc := range []struct {
		name  string
		image string
	}{
		{"a tag that does not exist", host + "/p/r/bifrost:missing"},
		{"the registry refuses the credential", host + "/p/r/bifrost:denied"},
		{"the body is not a manifest", host + "/p/r/bifrost:garbage"},
		{"the reference names no registry host", "bifrost:eb12dfa"},
		{"the host is unreachable", "127.0.0.1:1/p/r/bifrost:eb12dfa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ManifestDigest(context.Background(), tc.image)
			if err == nil {
				t.Fatalf("ManifestDigest = %q, want an error", got)
			}
			if got != "" {
				t.Errorf("ManifestDigest returned %q alongside its error; a failed lookup must yield nothing to compare", got)
			}
		})
	}
}

// TestContextCancellationStopsTheLookup: --attention bounds this whole phase
// with a timeout, and a registry that never answers must return rather than
// hold the command open.
func TestContextCancellationStopsTheLookup(t *testing.T) {
	reg := &fakeRegistry{
		bodies: map[string]string{"p/r/bifrost:eb12dfa": buildxIndex(sharedImageDigest, attestationA)},
		gate:   make(chan struct{}),
	}
	r, _, host := newTestResolver(t, reg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ManifestDigest(ctx, host+"/p/r/bifrost:eb12dfa"); err == nil {
		t.Fatal("ManifestDigest returned no error on a cancelled context")
	}
	close(reg.gate)
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		image           string
		host, repo, ref string
		wantErr         bool
		why             string
	}{
		{image: "us-central1-docker.pkg.dev/p/containers/bifrost:0cbfc9f", host: "us-central1-docker.pkg.dev", repo: "p/containers/bifrost", ref: "0cbfc9f"},
		{image: "us-central1-docker.pkg.dev/p/containers/dash:abc1234-staging", host: "us-central1-docker.pkg.dev", repo: "p/containers/dash", ref: "abc1234-staging"},
		{image: "reg.example.com/x/y", host: "reg.example.com", repo: "x/y", ref: "latest", why: "an untagged reference means latest"},
		{image: "reg.example.com/x/y@sha256:abc", host: "reg.example.com", repo: "x/y", ref: "sha256:abc", why: "a digest pin is already the reference"},
		{image: "localhost:5000/x:t", host: "localhost:5000", repo: "x", ref: "t", why: "a host:port registry is not a tag"},
		{image: "bifrost:0cbfc9f", wantErr: true, why: "no registry host"},
		// Docker would read this as an implicit docker.io reference. Nothing in
		// this fleet is one, and silently treating "bifrost" as a hostname
		// would send the lookup to a host that does not exist — so the first
		// element has to LOOK like a registry or there is no answer to give.
		{image: "bifrost/cmd:0cbfc9f", wantErr: true, why: "the first element is not a registry host"},
		{image: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.image, func(t *testing.T) {
			host, repo, ref, err := parseRef(tc.image)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRef(%q) = %q %q %q, want an error (%s)", tc.image, host, repo, ref, tc.why)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRef(%q): %v", tc.image, err)
			}
			if host != tc.host || repo != tc.repo || ref != tc.ref {
				t.Errorf("parseRef(%q) = %q %q %q, want %q %q %q (%s)", tc.image, host, repo, ref, tc.host, tc.repo, tc.ref, tc.why)
			}
		})
	}
}
