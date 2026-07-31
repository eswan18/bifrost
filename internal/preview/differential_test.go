package preview

import (
	"testing"

	"github.com/eswan18/bifrost/internal/oracle"
)

// TestTagForBranchMatchesOracle asserts TagForBranch agrees with ib.py's
// tag_for_branch for every vector in testdata/oracle/tag_for_branch.json.
//
// ib.py calls its copy "a mirror of bifrost's preview.TagForBranch" and says
// outright that it can drift. Nothing checked that claim until now: the CLI
// uses its tag to look a preview up BEFORE the POST, so drift shows up as a
// wrong create/update verb and a wrong "nothing rebuilt" line, not as an
// error. This is the test that makes the mirror claim true.
//
// The expectations come from running the real ib.py; none is computed by
// calling TagForBranch.
func TestTagForBranchMatchesOracle(t *testing.T) {
	type row struct {
		Branch string `json:"branch"`
		Tag    string `json:"tag"`
	}
	rows, err := oracle.Load[row]("tag_for_branch.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for _, r := range rows {
		t.Run(r.Branch, func(t *testing.T) {
			if got := TagForBranch(r.Branch); got != r.Tag {
				t.Errorf("TagForBranch(%q) = %q, ib.py tag_for_branch = %q", r.Branch, got, r.Tag)
			}
		})
	}
}

// TestTagForBranchOracleCoversFolding guards the matrix itself. The
// many-to-one folding property and the 30-character cap are the two things
// this function is relied on for, and a fixture that stopped exercising them
// would leave a green suite proving nothing. If these vectors are dropped
// from the capture script, this fails.
func TestTagForBranchOracleCoversFolding(t *testing.T) {
	type row struct {
		Branch string `json:"branch"`
		Tag    string `json:"tag"`
	}
	rows, err := oracle.Load[row]("tag_for_branch.json")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	byBranch := map[string]string{}
	longest := 0
	for _, r := range rows {
		byBranch[r.Branch] = r.Tag
		if len(r.Tag) > longest {
			longest = len(r.Tag)
		}
	}
	for _, branch := range []string{"feat/foo", "feat-foo", "feat_foo", "feat foo"} {
		if got, ok := byBranch[branch]; !ok || got != "feat-foo" {
			t.Errorf("fixture no longer pins the folding of %q to feat-foo (got %q, present=%v)", branch, got, ok)
		}
	}
	if _, ok := byBranch[""]; !ok {
		t.Error("fixture no longer covers the empty branch")
	}
	if longest != maxTagLen {
		t.Errorf("fixture's longest tag is %d chars; nothing exercises the %d-character cap", longest, maxTagLen)
	}
}
