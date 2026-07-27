package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
