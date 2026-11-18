//go:build !windows

package workspace

import "testing"

func TestLocalGitPrepareMergeAbortsConflictingRebaseGitFailures(t *testing.T) {
	testLocalGitPrepareMergeAbortsConflictingRebase(t, []mergeConflictCase{
		{name: "subject lookup is best effort", commits: 1, failSubjects: true}})
}
