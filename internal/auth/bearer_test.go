package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearer(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireBearer("s3cret")(next)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"valid token", "Bearer s3cret", http.StatusNoContent},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic s3cret", http.StatusUnauthorized},
		{"token as prefix", "Bearer s3cret-and-more", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/previews", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

func TestRequireBearerEmptyTokenRejectsEverything(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireBearer("")(next)
	req := httptest.NewRequest("GET", "/api/previews", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty configured token must reject all requests, got %d", rec.Code)
	}
}
