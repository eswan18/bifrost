// Package github looks up branch heads so the preview control plane can
// detect which repos carry a preview branch. Hand-rolled net/http (matching
// internal/auth/oidc.go's conventions) rather than a SDK dependency — one
// endpoint doesn't justify one.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const httpTimeout = 10 * time.Second

// ErrNoBranch reports that the repo has no branch by that name (GitHub 404).
var ErrNoBranch = errors.New("branch not found")

type Client interface {
	// BranchSHA returns the head commit SHA of repo's branch.
	// A missing branch is ErrNoBranch; other failures are opaque errors.
	BranchSHA(ctx context.Context, repo, branch string) (string, error)
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
	url := fmt.Sprintf("%s/repos/%s/%s/branches/%s", c.baseURL, c.org, repo, branch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
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
