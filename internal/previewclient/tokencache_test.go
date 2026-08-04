package previewclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- the harness --------------------------------------------------------

// cacheIn points the token cache at a scratch directory and returns the file
// it will use. Every test in this file runs against XDG_CACHE_HOME, which is
// also the lookup an operator gets — so the path resolution is exercised
// rather than bypassed.
//
// HOME is redirected too, and not for tidiness: it is the fallback. A test
// that set only XDG_CACHE_HOME would, the moment the XDG lookup broke, quietly
// start writing tokens into the developer's real ~/.cache — passing all the
// while, since read and write would agree on the wrong place.
func cacheIn(t *testing.T) (tokenCache, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", t.TempDir())
	return tokenCache{}, filepath.Join(dir, cacheDirName, cacheFileName)
}

// fakeBifrost is a preview API that answers according to the token it is sent.
// It counts requests, because "exactly one retry" is a claim about how many
// times bifrost was asked, not about what came back.
type tokenServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // the Authorization header of each request, in order
}

// newTokenServer answers 200 to accept and 401 to anything else. An empty
// accept means every token is refused.
func newTokenServer(t *testing.T, accept string) *tokenServer {
	t.Helper()
	s := &tokenServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		s.requests = append(s.requests, auth)
		s.mu.Unlock()
		if accept != "" && auth == "Bearer "+accept {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"previews": []any{}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token"}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *tokenServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// clientFor builds a Client with the on-disk cache switched ON — which New
// does and a hand-built Client does not — and a gcloud stand-in that counts
// its calls.
func clientFor(s *tokenServer, gcloudToken string, calls *atomic.Int64) *Client {
	return &Client{
		BaseURL: s.srv.URL,
		HTTP:    s.srv.Client(),
		Token: func(context.Context) (string, error) {
			calls.Add(1)
			return gcloudToken, nil
		},
		cache: &tokenCache{},
	}
}

// ---- the file itself ----------------------------------------------------

// TestCacheModes is the assertion that keeps this change from being a
// downgrade. A bearer token on disk is only acceptable if nothing but its
// owner can read it, and both halves matter: a 0600 file inside a 0755
// directory is still a token another local user can stat their way to.
func TestCacheModes(t *testing.T) {
	c, path := cacheIn(t)
	if err := c.write("a-token"); err != nil {
		t.Fatalf("write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != cacheFileMode {
		t.Errorf("token file mode = %#o, want %#o", got, cacheFileMode)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dir.Mode().Perm(); got != cacheDirMode {
		t.Errorf("cache directory mode = %#o, want %#o", got, cacheDirMode)
	}
}

// TestCacheTightensAnExistingDirectory: MkdirAll leaves an existing
// directory's mode alone, so a ~/.cache/bif that already existed
// world-readable would keep the token in a directory anyone could list.
func TestCacheTightensAnExistingDirectory(t *testing.T) {
	c, path := cacheIn(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}
	if err := c.write("a-token"); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got != cacheDirMode {
		t.Errorf("cache directory mode = %#o, want %#o", got, cacheDirMode)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c, _ := cacheIn(t)
	if err := c.write("a-token"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := c.read()
	if !ok || got != "a-token" {
		t.Errorf("read = %q, %v; want %q, true", got, ok, "a-token")
	}
}

// TestCacheFallsBackToHome covers the other half of the path rule: no
// XDG_CACHE_HOME means ~/.cache, which is where most people's actually is.
func TestCacheFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)

	var c tokenCache
	got, err := c.path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	fallback := filepath.Join(home, ".cache", cacheDirName, cacheFileName)
	if got != fallback {
		t.Errorf("path = %q, want %q", got, fallback)
	}
	if err := c.write("a-token"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(fallback); err != nil {
		t.Errorf("token not written to the HOME fallback: %v", err)
	}
}

// TestCacheRejectsATokenAnyoneCanRead: a token found wider than 0600 has been
// exposed to something, and using it would launder that. It is discarded and
// deleted, and the caller pays a gcloud call to write a fresh one properly.
func TestCacheRejectsATokenAnyoneCanRead(t *testing.T) {
	c, path := cacheIn(t)
	if err := c.write("a-token"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if got, ok := c.read(); ok {
		t.Errorf("read a world-readable token file: %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the exposed token was left on disk (stat err = %v)", err)
	}
}

func TestCacheExpires(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{name: "just inside the TTL", age: tokenTTL - time.Minute, want: true},
		{name: "past the TTL", age: tokenTTL + time.Minute, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, path := cacheIn(t)
			if err := c.write("a-token"); err != nil {
				t.Fatalf("write: %v", err)
			}
			when := time.Now().Add(-tc.age)
			if err := os.Chtimes(path, when, when); err != nil {
				t.Fatalf("chtimes: %v", err)
			}
			if _, ok := c.read(); ok != tc.want {
				t.Errorf("read ok = %v, want %v", ok, tc.want)
			}
			if !tc.want {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("an expired token was left on disk (stat err = %v)", err)
				}
			}
		})
	}
}

// TestCacheReadIsAMissNotAFailure: every unusable state is one answer, so a
// broken cache costs a gcloud call and never a command.
func TestCacheReadIsAMissNotAFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "nothing cached", setup: func(*testing.T, string) {}},
		{
			name: "an empty file",
			setup: func(t *testing.T, path string) {
				mkdirAllOrFail(t, filepath.Dir(path))
				writeFileOrFail(t, path, "")
			},
		},
		{
			name: "whitespace only",
			setup: func(t *testing.T, path string) {
				mkdirAllOrFail(t, filepath.Dir(path))
				writeFileOrFail(t, path, "  \n ")
			},
		},
		{
			name: "a directory where the token should be",
			setup: func(t *testing.T, path string) {
				mkdirAllOrFail(t, path)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, path := cacheIn(t)
			tc.setup(t, path)
			if got, ok := c.read(); ok {
				t.Errorf("read = %q, true; want a miss", got)
			}
		})
	}
}

func mkdirAllOrFail(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFileOrFail(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), cacheFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCacheWriteIsAtomic: `bif preview up`'s poll loop and a Tab press in
// another terminal can hit this file at the same moment. A reader must see the
// whole of one token or the whole of another — never a file mid-truncate,
// which a plain WriteFile would hand it and which would be sent to bifrost as
// a credential.
//
// The tokens are 4KB so a non-atomic write has a window wide enough to be
// caught, and distinct enough that catching it is unambiguous.
func TestCacheWriteIsAtomic(t *testing.T) {
	c, path := cacheIn(t)
	tokens := []string{strings.Repeat("a", 4096), strings.Repeat("b", 4096)}
	if err := c.write(tokens[0]); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var torn atomic.Int64
	var tornLen atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.write(tokens[i%2])
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				got, ok := c.read()
				if ok && got != tokens[0] && got != tokens[1] {
					torn.Add(1)
					tornLen.Store(int64(len(got)))
				}
			}
		}()
	}

	// The readers finish on their own; the writer runs until told.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done

	if n := torn.Load(); n != 0 {
		t.Errorf("%d reads saw a partially written token (one was %d bytes)", n, tornLen.Load())
	}

	// And nothing is left behind: the temp files are renamed into place or
	// removed, never accumulated in a directory the operator never looks at.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != cacheFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache directory holds %v, want just %q", names, cacheFileName)
	}
}

// ---- the client's use of it ---------------------------------------------

// TestTheCacheIsUsedByEveryInvocation is the point of putting this in the
// client rather than in completion: `bif preview list`, `up` and `down` pay
// gcloud once between them, not once each. A second Client stands in for the
// next process, since that is where the memo does not reach.
func TestTheCacheIsUsedByEveryInvocation(t *testing.T) {
	cacheIn(t)
	srv := newTokenServer(t, "the-token")
	var calls atomic.Int64

	for i := 0; i < 3; i++ {
		if _, err := clientFor(srv, "the-token", &calls).List(context.Background()); err != nil {
			t.Fatalf("List %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls across three invocations = %d, want 1", got)
	}
	if got := len(srv.seen()); got != 3 {
		t.Errorf("requests = %d, want 3", got)
	}
}

// TestRotationRetriesOnce is the bug the cache would otherwise introduce: the
// secret is rotated, this machine still holds the old value, and without a
// retry `bif preview` breaks until somebody deletes a file they do not know
// exists.
func TestRotationRetriesOnce(t *testing.T) {
	c, path := cacheIn(t)
	if err := c.write("stale-token"); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	srv := newTokenServer(t, "rotated-token")
	var calls atomic.Int64

	if _, err := clientFor(srv, "rotated-token", &calls).List(context.Background()); err != nil {
		t.Fatalf("List after rotation: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls = %d, want exactly 1", got)
	}
	want := []string{"Bearer stale-token", "Bearer rotated-token"}
	if got := srv.seen(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("requests = %v, want %v", got, want)
	}
	// And the rotated token replaces the stale one, so the NEXT process does
	// not repeat the 401.
	if got, ok := c.read(); !ok || got != "rotated-token" {
		t.Errorf("cached token after rotation = %q, %v; want %q, true", got, ok, "rotated-token")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("cache file missing after the refresh: %v", err)
	}
}

// TestPersistent401DoesNotLoop: one retry, not a retry per failure. A bifrost
// refusing everything must answer in two requests and then report.
func TestPersistent401DoesNotLoop(t *testing.T) {
	c, _ := cacheIn(t)
	if err := c.write("stale-token"); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	srv := newTokenServer(t, "") // refuses everything
	var calls atomic.Int64
	client := clientFor(srv, "no-better-token", &calls)

	_, err := client.List(context.Background())
	if err == nil {
		t.Fatal("List succeeded against a server that refuses every token")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Errorf("error = %v, want an APIError 401", err)
	}
	if got := len(srv.seen()); got != 2 {
		t.Errorf("requests = %d, want exactly 2 (one try, one retry)", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls = %d, want exactly 1", got)
	}

	// The retry is spent for the life of the client, so a second command on
	// the same client costs one request, not two.
	if _, err := client.List(context.Background()); err == nil {
		t.Fatal("second List succeeded unexpectedly")
	}
	if got := len(srv.seen()); got != 3 {
		t.Errorf("requests after a second List = %d, want 3", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls after a second List = %d, want still 1", got)
	}
}

// TestNoRetryWhenTheTokenIsAlreadyFresh: a token gcloud handed over moments
// ago is not stale, so a 401 on it is the CLI's answer, not a reason to ask
// again. This is what keeps every pre-existing 401 path exactly one request
// long.
func TestNoRetryWhenTheTokenIsAlreadyFresh(t *testing.T) {
	cacheIn(t) // empty: nothing cached, so the token comes from gcloud
	srv := newTokenServer(t, "")
	var calls atomic.Int64

	if _, err := clientFor(srv, "fresh-token", &calls).List(context.Background()); err == nil {
		t.Fatal("List succeeded against a server that refuses every token")
	}
	if got := len(srv.seen()); got != 1 {
		t.Errorf("requests = %d, want 1: an uncached token is not stale", got)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls = %d, want 1", got)
	}
}

// TestAFailedRefreshIsNotRetriedEither covers the case the fromCache flag
// alone does not: gcloud itself is broken — an expired login — so the refresh
// never produces a token to mark as fresh, and the stale one stays memoized.
// Without the spent-retry flag, every subsequent request would shell out to
// gcloud again, turning one broken login into a subprocess per API call in
// `preview up`'s poll loop.
func TestAFailedRefreshIsNotRetriedEither(t *testing.T) {
	c, _ := cacheIn(t)
	if err := c.write("stale-token"); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	srv := newTokenServer(t, "") // refuses everything
	var calls atomic.Int64
	client := &Client{
		BaseURL: srv.srv.URL,
		HTTP:    srv.srv.Client(),
		Token: func(context.Context) (string, error) {
			calls.Add(1)
			return "", errors.New("ERROR: (gcloud.secrets.versions.access) reauthentication required")
		},
		cache: &tokenCache{},
	}

	for i := 0; i < 3; i++ {
		if _, err := client.List(context.Background()); err == nil {
			t.Fatalf("List %d succeeded against a server that refuses every token", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("gcloud calls across three commands with a broken login = %d, want 1", got)
	}
}

// TestNoRetryWithoutACache pins the other half of "no behaviour change": a
// hand-built Client — which is every Client in cmd/bif's tests — has no cache,
// so it cannot have a stale token and does not retry.
func TestNoRetryWithoutACache(t *testing.T) {
	cacheIn(t)
	srv := newTokenServer(t, "")
	var calls atomic.Int64
	client := &Client{
		BaseURL: srv.srv.URL,
		HTTP:    srv.srv.Client(),
		Token: func(context.Context) (string, error) {
			calls.Add(1)
			return "a-token", nil
		},
	}

	if _, err := client.List(context.Background()); err == nil {
		t.Fatal("List succeeded against a server that refuses every token")
	}
	if got := len(srv.seen()); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
	// And nothing was written to disk by a client that was not given a cache.
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CACHE_HOME"), cacheDirName)); !os.IsNotExist(err) {
		t.Errorf("a cache-less client touched the cache directory (stat err = %v)", err)
	}
}

// ---- the token stays out of everything ----------------------------------

const sentinel = "SENTINEL-BEARER-TOKEN-VALUE-9f3a"

// TestTokenNeverAppearsInAnError walks every failure this cache can produce
// with a recognisable token in play and checks the one thing that must be true
// of all of them: the token is not in the message. cmd/bif prints these errors
// verbatim, so a token formatted into one is a token on somebody's terminal —
// and, through `bif preview list 2>&1 | tee`, in a file.
func TestTokenNeverAppearsInAnError(t *testing.T) {
	var errs []error

	// A cache directory that cannot be created, because a FILE is sitting
	// where it needs to go: MkdirAll, Chmod, CreateTemp and Rename all fail
	// out of this one setup.
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	writeFileOrFail(t, filepath.Join(dir, cacheDirName), "not a directory")
	var c tokenCache
	errs = append(errs, c.write(sentinel))

	// A cache directory that exists but is read-only, so the temp file cannot
	// be created in it.
	dir2 := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir2)
	mkdirAllOrFail(t, filepath.Join(dir2, cacheDirName))
	if err := os.Chmod(filepath.Join(dir2, cacheDirName), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir2, cacheDirName), cacheDirMode) })
	errs = append(errs, c.write(sentinel))

	// And the client's own failure paths, with the sentinel as the token
	// bifrost rejects.
	srv := newTokenServer(t, "")
	var calls atomic.Int64
	_, listErr := clientFor(srv, sentinel, &calls).List(context.Background())
	errs = append(errs, listErr)
	errs = append(errs, (&APIError{Status: http.StatusUnauthorized, Detail: "bad token"}))
	errs = append(errs, (&TransportError{BaseURL: srv.srv.URL, Err: fmt.Errorf("refused")}))

	saw := false
	for _, err := range errs {
		if err == nil {
			continue
		}
		saw = true
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("an error carries the token: %q", err.Error())
		}
	}
	if !saw {
		t.Fatal("no error was produced, so nothing was checked")
	}
}

// TestPackageHasNowhereToPrint closes the gap an error-message check leaves
// open: a package that can write to a stream can leak the token without any
// error being involved. previewclient answers by never having a stream at all
// — it returns errors and lets cmd/bif decide what to print — and this keeps
// it that way.
func TestPackageHasNowhereToPrint(t *testing.T) {
	banned := map[string]string{
		"os.Stdout":    "stdout is the CLI's output, and for `bif __complete` it is the candidate list",
		"os.Stderr":    "this package returns errors; cmd/bif decides what an operator sees",
		"fmt.Print":    "same",
		"fmt.Printf":   "same",
		"fmt.Println":  "same",
		"fmt.Fprint":   "a stream in this package is a stream the token can reach",
		"fmt.Fprintf":  "same",
		"fmt.Fprintln": "same",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, name, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if path := strings.Trim(imp.Path.Value, `"`); path == "log" || strings.HasSuffix(path, "/log") {
				t.Errorf("%s imports %q: a log line is one more place the token can land", name, path)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if why, bad := banned[pkg.Name+"."+sel.Sel.Name]; bad {
				t.Errorf("%s uses %s.%s: %s", name, pkg.Name, sel.Sel.Name, why)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no files were scanned, so this proves nothing")
	}
}
