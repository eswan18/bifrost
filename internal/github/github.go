// Package github looks up branch heads so the preview control plane can
// detect which repos carry a preview branch. Hand-rolled net/http (matching
// internal/auth/oidc.go's conventions) rather than a SDK dependency — one
// endpoint doesn't justify one.
package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const httpTimeout = 10 * time.Second

// maxK8sExtractBytes caps the total size of file content FetchK8s will pull
// out of a tarball, guarding against decompression-bomb-style responses.
const maxK8sExtractBytes = 5 * 1024 * 1024 // 5MB

// ErrNoBranch reports that the repo has no branch by that name (GitHub 404).
var ErrNoBranch = errors.New("branch not found")

type Client interface {
	// BranchSHA returns the head commit SHA of repo's branch.
	// A missing branch is ErrNoBranch; other failures are opaque errors.
	BranchSHA(ctx context.Context, repo, branch string) (string, error)

	// FetchK8s downloads repo's tarball at ref and returns the k8s/ subtree
	// as path→content, with paths relative to k8s/ (e.g.
	// "base/deployment.yaml"). A missing ref is ErrNoBranch; other failures
	// are opaque errors.
	FetchK8s(ctx context.Context, repo, ref string) (map[string][]byte, error)
}

type client struct {
	org     string
	token   string
	baseURL string
	http    *http.Client
}

func New(org, token string) Client {
	return NewWithBaseURL(org, token, "https://api.github.com")
}

// NewWithBaseURL exists as a test seam; production code uses New.
func NewWithBaseURL(org, token, baseURL string) Client {
	return &client{org: org, token: token, baseURL: baseURL, http: &http.Client{Timeout: httpTimeout}}
}

func (c *client) BranchSHA(ctx context.Context, repo, branch string) (string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.baseURL, c.org, url.PathEscape(repo), url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", ErrNoBranch
	default:
		return "", fmt.Errorf("github branch lookup returned %d", resp.StatusCode)
	}
	var body struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Commit.SHA == "" {
		return "", errors.New("github response missing commit sha")
	}
	return body.Commit.SHA, nil
}

// FetchK8s downloads repo's tarball at ref and returns the k8s/ subtree as
// path→content, with paths relative to k8s/.
func (c *client) FetchK8s(ctx context.Context, repo, ref string) (map[string][]byte, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", c.baseURL, c.org, url.PathEscape(repo), url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// This endpoint 302s to codeload.github.com; the default http.Client
	// follows the redirect automatically. Go's net/http strips the
	// Authorization header on cross-host redirects, but that's fine here:
	// codeload serves the tarball from a signed, time-limited URL baked into
	// the redirect target, so no auth header is needed (or sent) for it.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNoBranch
	default:
		return nil, fmt.Errorf("github tarball fetch returned %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("decompressing k8s tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()

	files := make(map[string][]byte)
	budget := int64(maxK8sExtractBytes)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading k8s tarball entry: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// GitHub prepends a "{org}-{repo}-{sha}/" directory to every entry;
		// drop that leading path segment and keep only files under "k8s/".
		_, rel, ok := strings.Cut(hdr.Name, "/")
		if !ok {
			continue
		}
		relToK8s, ok := strings.CutPrefix(rel, "k8s/")
		if !ok {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, budget+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}
		if int64(len(data)) > budget {
			return nil, fmt.Errorf("k8s tarball exceeds %d byte extraction cap", maxK8sExtractBytes)
		}
		budget -= int64(len(data))
		files[relToK8s] = data
	}
	return files, nil
}
