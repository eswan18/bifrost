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
			w.Write([]byte(`{"name":"hae-cadence","commit":{"sha":"49a402ab12cd"}}`))
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
