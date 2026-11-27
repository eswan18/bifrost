package neon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer neon-key" {
			t.Errorf("Authorization = %q, want Bearer neon-key", got)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, mux
}

func TestListBranches(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"branches":[{"id":"br-main-123","name":"main"},{"id":"br-prev-456","name":"preview-hae-cadence"}]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	got, err := c.ListBranches(context.Background(), "proj-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[1].ID != "br-prev-456" || got[1].Name != "preview-hae-cadence" {
		t.Errorf("branches = %+v", got)
	}
}

// TestListBranchesDecodesCreatedAt pins the field the preview orphan sweep's
// age floor is built on. The payload is shaped like a real Neon response —
// created_at is an RFC3339 string, and the object carries fields Branch
// doesn't declare (Neon's branch object has ~25 of them) — so this also proves
// the additive field didn't turn the decode strict.
//
// The second branch has no created_at at all, which is what the sweep must
// never silently mistake for "old": it decodes to the zero time, and the
// assertion says so explicitly rather than leaving it implied.
func TestListBranchesDecodesCreatedAt(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"branches":[
			{"id":"br-prev-456","name":"preview-hae-cadence","created_at":"2026-07-29T09:30:00Z","updated_at":"2026-07-29T09:31:00Z","default":false,"current_state":"ready"},
			{"id":"br-no-ts","name":"preview-undated"}
		]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	got, err := c.ListBranches(context.Background(), "proj-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("branches = %+v, want 2", got)
	}
	want := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	if !got[0].CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, want)
	}
	if !got[1].CreatedAt.IsZero() {
		t.Errorf("CreatedAt for a branch with no created_at = %v, want the zero time", got[1].CreatedAt)
	}
}

func TestCreateBranch(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("POST /projects/proj-abc/branches", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("bad body: %v", err)
		}
		branch, _ := body["branch"].(map[string]any)
		if branch["name"] != "preview-hae-cadence" {
			t.Errorf("branch.name = %v", branch["name"])
		}
		if _, hasParent := branch["parent_id"]; hasParent {
			t.Error("empty parentID must omit parent_id")
		}
		if _, hasEndpoints := body["endpoints"]; !hasEndpoints {
			t.Error("expected endpoints in create request (branch needs a compute to connect to)")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"branch":{"id":"br-new-789","name":"preview-hae-cadence","created_at":"2026-07-29T09:30:00Z"}}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	got, err := c.CreateBranch(context.Background(), "proj-abc", "preview-hae-cadence", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "br-new-789" {
		t.Errorf("branch = %+v", got)
	}
	// createProjectBranch returns the same branch schema listProjectBranches
	// does, created_at included.
	if want := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC); !got.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want)
	}
}

func TestCreateBranchWithParent(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("POST /projects/proj-abc/branches", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("bad body: %v", err)
		}
		branch, _ := body["branch"].(map[string]any)
		if branch["parent_id"] != "br-main-123" {
			t.Errorf("branch.parent_id = %v, want br-main-123", branch["parent_id"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"branch":{"id":"br-new-789","name":"preview-hae-cadence"}}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if _, err := c.CreateBranch(context.Background(), "proj-abc", "preview-hae-cadence", "br-main-123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteBranch(t *testing.T) {
	srv, mux := newTestServer(t)
	called := false
	mux.HandleFunc("DELETE /projects/proj-abc/branches/br-prev-456", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if err := c.DeleteBranch(context.Background(), "proj-abc", "br-prev-456"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("DELETE endpoint not hit")
	}
}

// TestDeleteBranchNoContent covers a live-API delta from the reference brief:
// Neon's deleteProjectBranch operation documents 200 (deleted) AND 204
// ("returned if the branch doesn't exist or has already been deleted") as
// success responses, not just 200.
func TestDeleteBranchNoContent(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("DELETE /projects/proj-abc/branches/br-gone", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if err := c.DeleteBranch(context.Background(), "proj-abc", "br-gone"); err != nil {
		t.Fatalf("unexpected error on 204: %v", err)
	}
}

func TestConnectionURI(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("branch_id") != "br-prev-456" || q.Get("database_name") != "fitnessdb" || q.Get("role_name") != "fitness_owner" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write([]byte(`{"uri":"postgresql://fitness_owner:pw@ep-x.neon.tech/fitnessdb"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	uri, err := c.ConnectionURI(context.Background(), "proj-abc", "br-prev-456", "fitnessdb", "fitness_owner")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uri != "postgresql://fitness_owner:pw@ep-x.neon.tech/fitnessdb" {
		t.Errorf("uri = %q", uri)
	}
}

func TestErrorStatusSurfaces(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	if _, err := c.ListBranches(context.Background(), "proj-abc"); err == nil {
		t.Error("expected error on 429")
	}
}

// TestErrorIncludesBodyExcerpt pins the reason this file exists: Neon puts
// its explanation of a 4xx in the response body (a free-tier branch limit, a
// storage limit, ...), and a bare status code throws that away. Without
// reading the body, the error is indistinguishable from any other failure —
// this is the only signal such a bound was ever hit.
func TestErrorIncludesBodyExcerpt(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("POST /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"branch_limit_exceeded","message":"Your project has reached the maximum number of branches allowed for the free plan"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	_, err := c.CreateBranch(context.Background(), "proj-abc", "preview-hae-cadence", "")
	if err == nil {
		t.Fatal("expected an error on 422")
	}
	if !strings.Contains(err.Error(), "branch_limit_exceeded") || !strings.Contains(err.Error(), "maximum number of branches") {
		t.Errorf("error = %q, want it to carry Neon's own explanation of the 422", err)
	}
}

// TestErrorEmptyBodyHasNoDanglingSeparator covers the degrade-cleanly case: a
// non-2xx response that writes no body at all (a bare status-only failure,
// same as a 429 from a rate limiter with nothing to say) must not leave a
// trailing ": " with nothing after it.
func TestErrorEmptyBodyHasNoDanglingSeparator(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	_, err := c.ListBranches(context.Background(), "proj-abc")
	if err == nil {
		t.Fatal("expected an error on 429")
	}
	const want = "neon GET /projects/proj-abc/branches returned 429"
	if err.Error() != want {
		t.Errorf("error = %q, want %q (no dangling separator on an empty body)", err.Error(), want)
	}
}

// TestErrorBodyExcerptIsCappedAndFlattened covers both halves of the
// sanitization this string needs before it's safe to reach the bifrost/error
// annotation (served over bifrost's JSON API and rendered in the browser): a
// multi-kilobyte body — the shape of an HTML error page from a proxy sitting
// in front of Neon — must not survive whole, and embedded newlines must not
// survive at all.
func TestErrorBodyExcerptIsCappedAndFlattened(t *testing.T) {
	srv, mux := newTestServer(t)
	huge := "line one\nline two\n" + strings.Repeat("x", 5000)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(huge))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	_, err := c.ListBranches(context.Background(), "proj-abc")
	if err == nil {
		t.Fatal("expected an error on 502")
	}
	msg := err.Error()
	if strings.Contains(msg, "\n") {
		t.Errorf("error = %q, want whitespace flattened", msg)
	}
	if len(msg) > 300 {
		t.Errorf("error is %d bytes long, want it capped well under the 5000+-byte raw body", len(msg))
	}
	if !strings.HasSuffix(msg, "...") {
		t.Errorf("error = %q, want a truncation marker showing it was cut", msg)
	}
}

// TestConnectionURIErrorNeverIncludesBody is the one exception to
// TestErrorIncludesBodyExcerpt. ConnectionURI's own success response is a
// credentialed Postgres connection string, so its error path must never
// surface the response body at all — not merely "usually doesn't", since a
// general sanitizer built for CamelCase pod reasons and JSON error objects
// has no reason to recognize a postgres:// URI if one ever appeared here.
// The body below plants one to prove the exclusion is unconditional, not
// coincidental.
func TestConnectionURIErrorNeverIncludesBody(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"branch not found","uri":"postgresql://should_never_appear:pw@ep-x.neon.tech/db"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	_, err := c.ConnectionURI(context.Background(), "proj-abc", "br-missing", "fitnessdb", "fitness_owner")
	if err == nil {
		t.Fatal("expected an error on 404")
	}
	const want = "neon GET /projects/proj-abc/connection_uri?branch_id=br-missing&database_name=fitnessdb&role_name=fitness_owner returned 404"
	if err.Error() != want {
		t.Errorf("error = %q, want %q (no body content at all, credential-shaped or not)", err.Error(), want)
	}
}

// TestPathSegmentsEscaped guards against project/branch IDs containing
// characters that are structurally significant in a URL path (like "/")
// corrupting the path. It deliberately uses a projectID and branchID that
// REQUIRE percent-encoding, so an implementation missing url.PathEscape
// would produce a different (and wrong) wire path than the one asserted
// here — see the fix report for a before/after demonstration.
//
// This uses a bare handler rather than newTestServer's ServeMux, because
// ServeMux pattern matching operates on the decoded path and an encoded
// "/" (%2F) within a single path segment would otherwise be indistinguishable
// from a literal path separator at the routing layer, defeating the point
// of the assertion.
func TestPathSegmentsEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := NewWithBaseURL("neon-key", srv.URL)
	if err := c.DeleteBranch(context.Background(), "proj/abc", "br 1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = "/projects/proj%2Fabc/branches/br%201"
	if gotPath != want {
		t.Errorf("wire path = %q, want %q", gotPath, want)
	}
}

// TestListRoles pins the two things bifrost's role pre-flight depends on:
// the wire path (per-BRANCH, since Neon has no project-level role listing —
// a branch inherits its parent's roles, which is why asking the parent
// answers "will this role exist in the preview?"), and that only names come
// back.
func TestListRoles(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches/br-parent/roles", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"roles":[{"name":"app","password":"pw1","protected":false},{"name":"neondb_owner","password":"pw2","protected":true}]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	roles, err := c.ListRoles(context.Background(), "proj-abc", "br-parent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"app", "neondb_owner"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Errorf("roles[%d] = %q, want %q", i, roles[i], want[i])
		}
	}
}

// TestListRolesErrorNeverIncludesBody is the second instance of the
// exception TestConnectionURIErrorNeverIncludesBody documents, and it is not
// a copy of that test's caution: Neon's published spec marks a Role's
// `password` field — and the whole `roles` array — x-sensitive, so this
// endpoint's body really can carry credentials. The planted password below
// proves the exclusion is unconditional rather than a property of what Neon
// happens to return on an error today.
func TestListRolesErrorNeverIncludesBody(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches/br-missing/roles", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"branch not found","roles":[{"name":"app","password":"should_never_appear"}]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	_, err := c.ListRoles(context.Background(), "proj-abc", "br-missing")
	if err == nil {
		t.Fatal("expected an error on 404")
	}
	const want = "neon GET /projects/proj-abc/branches/br-missing/roles returned 404"
	if err.Error() != want {
		t.Errorf("error = %q, want %q (no body content at all)", err.Error(), want)
	}
}

// TestListBranchesReportsTheDefaultBranch pins that `default` is decoded.
// It is what the migrate-role pre-flight falls back to when an entry names
// no parent — Neon cuts a parentless branch from the default, so that is the
// branch whose roles a preview would inherit. Two branches, only one
// flagged, so a decoder that dropped the field (or defaulted it true) fails.
func TestListBranchesReportsTheDefaultBranch(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/branches", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"branches":[{"id":"br-1","name":"production","default":true},{"id":"br-2","name":"development","default":false}]}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	branches, err := c.ListBranches(context.Background(), "proj-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(branches) != 2 {
		t.Fatalf("branches = %v, want 2", branches)
	}
	if !branches[0].Default {
		t.Errorf("branches[0] (%s) Default = false, want true", branches[0].Name)
	}
	if branches[1].Default {
		t.Errorf("branches[1] (%s) Default = true, want false", branches[1].Name)
	}
}

// TestDirectConnectionURI covers the URI a migration connects through. A
// migration holds session state (an advisory lock, a SET) across statements,
// and Neon's pooled host is PgBouncer in transaction mode, which hands each
// transaction to whichever server connection is free. So the URI must name
// the endpoint's own host, never its "-pooler" one.
//
// The request says so (pooled=false), and the answer is checked anyway: the
// API handed back pooled URIs to requests that did not ask for them, which is
// how haruspex's migration runner came to refuse every preview.
func TestDirectConnectionURI(t *testing.T) {
	cases := []struct {
		name     string
		returned string
		want     string
	}{
		{
			name:     "a pooled host loses its -pooler",
			returned: "postgresql://neondb_owner:npg_abc@ep-ancient-violet-a4xf2xox-pooler.us-east-1.aws.neon.tech/neondb?sslmode=require&channel_binding=require",
			want:     "postgresql://neondb_owner:npg_abc@ep-ancient-violet-a4xf2xox.us-east-1.aws.neon.tech/neondb?sslmode=require&channel_binding=require",
		},
		{
			name:     "a direct host is returned as it came",
			returned: "postgresql://neondb_owner:npg_abc@ep-ancient-violet-a4xf2xox.us-east-1.aws.neon.tech/neondb?sslmode=require",
			want:     "postgresql://neondb_owner:npg_abc@ep-ancient-violet-a4xf2xox.us-east-1.aws.neon.tech/neondb?sslmode=require",
		},
		{
			name:     "a port stays where it was",
			returned: "postgresql://owner:pw@ep-x-pooler.neon.tech:5432/db",
			want:     "postgresql://owner:pw@ep-x.neon.tech:5432/db",
		},
		{
			// Only the host is touched, and the rest comes back byte for
			// byte: an escaped password is not re-encoded, and "-pooler"
			// anywhere but the host's first label means nothing.
			name:     "nothing but the host changes",
			returned: "postgresql://my-pooler:p%40ss-pooler@ep-x-pooler.neon.tech/db-pooler?options=a-pooler",
			want:     "postgresql://my-pooler:p%40ss-pooler@ep-x.neon.tech/db-pooler?options=a-pooler",
		},
		{
			name:     "a later label that ends in -pooler is not the pooler",
			returned: "postgresql://owner:pw@ep-x.my-pooler.example.com/db",
			want:     "postgresql://owner:pw@ep-x.my-pooler.example.com/db",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, mux := newTestServer(t)
			mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("branch_id") != "br-prev-456" || q.Get("database_name") != "neondb" || q.Get("role_name") != "neondb_owner" {
					t.Errorf("query = %v", q)
				}
				if got := q.Get("pooled"); got != "false" {
					t.Errorf("pooled = %q, want it asked for explicitly as false", got)
				}
				_, _ = w.Write([]byte(`{"uri":"` + tc.returned + `"}`))
			})
			c := NewWithBaseURL("neon-key", srv.URL)
			uri, err := c.DirectConnectionURI(context.Background(), "proj-abc", "br-prev-456", "neondb", "neondb_owner")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if uri != tc.want {
				t.Errorf("uri = %q, want %q", uri, tc.want)
			}
		})
	}
}

// TestDirectConnectionURINeverPutsTheURIInAnError: a URI that cannot be read
// as a URL is still a credential. net/url quotes its input in its errors, so
// that error must not be passed along.
func TestDirectConnectionURINeverPutsTheURIInAnError(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uri":"postgresql://owner:hunter2@%zz-pooler/db"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	uri, err := c.DirectConnectionURI(context.Background(), "proj-abc", "br-prev-456", "neondb", "neondb_owner")
	if err == nil {
		t.Fatalf("expected an error for a URI that is not a URL, got %q", uri)
	}
	if uri != "" {
		t.Errorf("uri = %q, want nothing returned alongside an error", uri)
	}
	for _, secret := range []string{"hunter2", "owner", "%zz"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error = %q, which carries %q from the connection string", err.Error(), secret)
		}
	}
}

// TestConnectionURIDoesNotAskAboutPooling: the app's URI is whatever Neon
// gives by default, as it always has been. Only the migrate URI asks.
func TestConnectionURIDoesNotAskAboutPooling(t *testing.T) {
	srv, mux := newTestServer(t)
	mux.HandleFunc("GET /projects/proj-abc/connection_uri", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("pooled") {
			t.Errorf("query = %v, want no pooled parameter", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"uri":"postgresql://app:pw@ep-x-pooler.neon.tech/db"}`))
	})
	c := NewWithBaseURL("neon-key", srv.URL)
	uri, err := c.ConnectionURI(context.Background(), "proj-abc", "br-prev-456", "db", "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uri != "postgresql://app:pw@ep-x-pooler.neon.tech/db" {
		t.Errorf("uri = %q, want it untouched", uri)
	}
}
