package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildTarGz builds a gzipped tar archive from name→content pairs, matching
// the shape of a GitHub codeload tarball response.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestBranchSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-pat" {
			t.Errorf("Authorization = %q, want Bearer test-pat", got)
		}
		switch r.URL.Path {
		case "/repos/eswan18/footstrike-api/branches/hae-cadence":
			_, _ = w.Write([]byte(`{"name":"hae-cadence","commit":{"sha":"49a402ab12cd"}}`))
		case "/repos/eswan18/footstrike-api/branches/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	sha, err := c.BranchSHA(context.Background(), "footstrike-api", "hae-cadence")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "49a402ab12cd" {
		t.Errorf("sha = %q, want 49a402ab12cd", sha)
	}

	_, err = c.BranchSHA(context.Background(), "footstrike-api", "missing")
	if !errors.Is(err, ErrNoBranch) {
		t.Errorf("expected ErrNoBranch, got %v", err)
	}
}

func TestBranchSHAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)
	_, err := c.BranchSHA(context.Background(), "footstrike-api", "main")
	if err == nil || errors.Is(err, ErrNoBranch) {
		t.Errorf("expected non-ErrNoBranch error, got %v", err)
	}
}

func TestBranchSHAWithSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that the path contains the percent-encoded slash
		if escaped := r.URL.EscapedPath(); escaped != "/repos/eswan18/footstrike-api/branches/feat%2Fx" {
			t.Errorf("EscapedPath = %q, want /repos/eswan18/footstrike-api/branches/feat%%2Fx", escaped)
		}
		_, _ = w.Write([]byte(`{"name":"feat/x","commit":{"sha":"abc123def456"}}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	sha, err := c.BranchSHA(context.Background(), "footstrike-api", "feat/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "abc123def456" {
		t.Errorf("sha = %q, want abc123def456", sha)
	}
}

func TestBranchSHAMissingSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"main","commit":{}}`))
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	_, err := c.BranchSHA(context.Background(), "footstrike-api", "main")
	if err == nil {
		t.Errorf("expected error for missing SHA, got nil")
	}
	if errors.Is(err, ErrNoBranch) {
		t.Errorf("expected non-ErrNoBranch error for missing SHA, got %v", err)
	}
}

func TestFetchK8s(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"repo-sha1234/k8s/base/deployment.yaml": "kind: Deployment\n",
		"repo-sha1234/README.md":                "hello\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-pat" {
			t.Errorf("Authorization = %q, want Bearer test-pat", got)
		}
		if r.URL.Path != "/repos/eswan18/footstrike-api/tarball/hae-cadence" {
			t.Errorf("path = %q, want /repos/eswan18/footstrike-api/tarball/hae-cadence", r.URL.Path)
		}
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	files, err := c.FetchK8s(context.Background(), "footstrike-api", "hae-cadence")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1 (%v)", len(files), files)
	}
	got, ok := files["base/deployment.yaml"]
	if !ok {
		t.Fatalf("missing key base/deployment.yaml, got %v", files)
	}
	if string(got) != "kind: Deployment\n" {
		t.Errorf("content = %q, want %q", got, "kind: Deployment\n")
	}
	if _, ok := files["README.md"]; ok {
		t.Errorf("README.md should not have been extracted, got %v", files)
	}
}

func TestFetchK8sNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	_, err := c.FetchK8s(context.Background(), "footstrike-api", "missing")
	if !errors.Is(err, ErrNoBranch) {
		t.Errorf("expected ErrNoBranch, got %v", err)
	}
}

func TestFetchK8sServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	_, err := c.FetchK8s(context.Background(), "footstrike-api", "main")
	if err == nil || errors.Is(err, ErrNoBranch) {
		t.Errorf("expected non-ErrNoBranch error, got %v", err)
	}
}

func TestFetchK8sRefWithSlash(t *testing.T) {
	tarball := buildTarGz(t, map[string]string{
		"repo-sha1234/k8s/a.yaml": "a: b\n",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if escaped := r.URL.EscapedPath(); escaped != "/repos/eswan18/footstrike-api/tarball/feat%2Fx" {
			t.Errorf("EscapedPath = %q, want /repos/eswan18/footstrike-api/tarball/feat%%2Fx", escaped)
		}
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	if _, err := c.FetchK8s(context.Background(), "footstrike-api", "feat/x"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchK8sExceedsExtractionCap(t *testing.T) {
	big := strings.Repeat("x", 5*1024*1024+1)
	tarball := buildTarGz(t, map[string]string{
		"repo-sha1234/k8s/base/big.yaml": big,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	c := NewWithBaseURL("eswan18", "test-pat", srv.URL)

	_, err := c.FetchK8s(context.Background(), "footstrike-api", "hae-cadence")
	if err == nil {
		t.Fatalf("expected an error for an oversized tarball, got nil")
	}
	if errors.Is(err, ErrNoBranch) {
		t.Errorf("expected non-ErrNoBranch error, got %v", err)
	}
}
