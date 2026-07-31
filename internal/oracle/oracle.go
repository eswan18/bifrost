// Package oracle loads the ib.py behaviour fixtures under testdata/oracle.
//
// The fixtures are captured from the real infra/ib.py by
// scripts/capture-oracle.sh. ib.py is the reference implementation: it is
// what has been promoting images to production, so where a Go function and a
// fixture disagree, the fixture describes what production does today and the
// Go side is the one that needs justifying.
//
// This package exists only to be imported from tests; nothing in cmd/ or the
// server should depend on it.
package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Source identifies the ib.py the fixtures were captured from, so a reader
// years from now can tell which version of the oracle they describe.
type Source struct {
	Path                   string `json:"path"`
	RepoHeadSHA            string `json:"repoHeadSha"`
	FileLastCommitSHA      string `json:"fileLastCommitSha"`
	FileUncommittedChanges bool   `json:"fileUncommittedChanges"`
}

// Meta is testdata/oracle/meta.json.
type Meta struct {
	Note   string         `json:"note"`
	Source Source         `json:"source"`
	Python string         `json:"python"`
	Counts map[string]int `json:"counts"`
}

// Dir returns the absolute path of testdata/oracle, resolved relative to this
// source file so any package's tests can find it.
func Dir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("testdata", "oracle")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "oracle")
}

// LoadMeta reads meta.json.
func LoadMeta() (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(Dir(), "meta.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("meta.json: %w", err)
	}
	return m, nil
}

// Load decodes the named fixture into a slice of T and checks the row count
// against meta.json.
//
// The count check is the point: a differential test whose fixture silently
// failed to load would pass against anything, which is worse than no test at
// all. A fixture that lost rows, or a meta.json that was not regenerated with
// it, fails here rather than quietly shrinking the matrix.
func Load[T any](name string) ([]T, error) {
	path := filepath.Join(Dir(), name)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []T
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no cases", name)
	}
	meta, err := LoadMeta()
	if err != nil {
		return nil, err
	}
	want, ok := meta.Counts[name]
	if !ok {
		return nil, fmt.Errorf("%s: not listed in meta.json counts; re-run scripts/capture-oracle.sh", name)
	}
	if len(rows) != want {
		return nil, fmt.Errorf("%s: %d cases, meta.json says %d; re-run scripts/capture-oracle.sh", name, len(rows), want)
	}
	return rows, nil
}

// Str renders a fixture field that ib.py may return as null (Python None) as
// the empty string the Go functions use for the same idea.
func Str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
