package previewclient

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The on-disk copy of the preview API bearer token.
//
// It exists because reading the token is not cheap: GcloudToken shells out to
// gcloud, which costs 450-780ms of Python startup before a single byte goes to
// bifrost. Every `bif preview` invocation paid that, and it is what made
// completing `bif preview down <TAB>` impossible inside a deadline a keypress
// can afford — 400ms, which no gcloud run has ever fitted in.
//
// What this trades away is that a copy of a bearer token now sits on a disk,
// so the file is 0600 inside a 0700 directory and is discarded unread if it is
// ever found wider than that. What it does NOT trade away is correctness when
// the secret rotates: see Client.discardStaleToken, which is why the TTL below
// is a staleness bound rather than a correctness one.

const (
	// tokenTTL is how long a token is served from disk before gcloud is asked
	// again.
	//
	// It is NOT what makes rotation safe — a 401 is, and the retry path
	// handles that within one request, whatever this value is. What it bounds
	// is how long a copy of a secret that has been revoked rather than
	// rotated can linger on a disk, and how stale the cache can get for
	// somebody who never makes a request that would notice.
	//
	// So it is chosen from the other side: 12 hours is a working day plus
	// slack, which means an operator pays gcloud's half-second once in the
	// morning instead of on every command, and no copy of the token outlives
	// the day it was fetched in. Shorter buys very little — the 401 path
	// already owns the case that matters — and puts the half-second back in
	// the middle of the afternoon.
	tokenTTL = 12 * time.Hour

	cacheDirName  = "bif"
	cacheFileName = "preview-token"

	// A bearer token readable by other local users is the one way this cache
	// is worse than shelling out every time, so the modes are checked on the
	// way out as well as set on the way in.
	cacheDirMode  os.FileMode = 0o700
	cacheFileMode os.FileMode = 0o600
)

// tokenCache is the token file. It holds no state: the path is resolved per
// call from the environment, so a cache is never stale with respect to a HOME
// that changed underneath it, and a test can point it somewhere harmless.
type tokenCache struct{}

// path is $XDG_CACHE_HOME/bif/preview-token, falling back to
// ~/.cache/bif/preview-token — the ordinary place for a regenerable file that
// is not configuration and not data.
func (tokenCache) path() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the token cache: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, cacheDirName, cacheFileName), nil
}

// read returns the cached token, and whether there was one worth using.
//
// Missing, expired, empty, unreadable, or readable by anyone but its owner are
// all one answer: a miss. Silently — this is a latency optimisation, and a
// broken cache must cost a gcloud call, never a command and never a line of
// output. The two disqualifying cases delete the file rather than leave it:
// one is a secret sitting somewhere it should not be, and the other is a
// secret nobody is going to use again.
func (c tokenCache) read() (string, bool) {
	path, err := c.path()
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	if info.Mode().Perm()&0o077 != 0 {
		c.purge()
		return "", false
	}
	if time.Since(info.ModTime()) > tokenTTL {
		c.purge()
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", false
	}
	return token, true
}

// write stores the token for next time.
//
// Two properties, both load-bearing:
//
//   - 0600 in a 0700 directory, with the directory's mode SET rather than
//     merely requested: MkdirAll leaves an existing directory alone, so a
//     pre-existing world-readable ~/.cache/bif would otherwise keep its mode.
//   - atomic. The token goes to a temp file in the same directory and is
//     renamed into place, so a concurrent reader — `bif preview up`'s poll
//     loop against a Tab press in another terminal — sees the whole of one
//     token or the whole of the other, never a truncated file being filled in.
//
// Every error below names a PATH and never the token. That is the rule for
// this whole file: the value is written to one place and read from one place,
// and nothing formats it into anything a person or a log will see.
func (c tokenCache) write(token string) error {
	path, err := c.path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, cacheDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, cacheDirMode); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, cacheFileName+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	if err := tmp.Chmod(cacheFileMode); err != nil {
		cleanup()
		return fmt.Errorf("securing %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.WriteString(token); err != nil {
		cleanup()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("renaming into %s: %w", path, err)
	}
	return nil
}

// purge deletes the cached token. Failure is not reported and not actionable:
// the caller is already on its way to gcloud for a fresh one.
func (c tokenCache) purge() {
	if path, err := c.path(); err == nil {
		_ = os.Remove(path)
	}
}
