//go:build !windows

package workspace

import "testing"

func TestLocalGitPrepareMergeValidatesResolvedHeadGitFailures(t *testing.T) {
	testLocalGitPrepareMergeValidatesResolvedHead(t, []mergeResolvedHeadCase{
		{name: "changed paths Git failure", resolvedGo: "package example\nfunc PR() {}\n", gitFailure: "diff --name-only --no-renames", wantError: "read merge resolution changed paths"},
		{name: "merge base Git failure", resolvedGo: "package example\nfunc PR() {}\n", gitFailure: "merge-base", wantError: "read merge base"},
		{name: "tree listing Git failure", resolvedGo: "package example\nfunc PR() {}\n", gitFailure: "ls-tree -r", wantError: "list merge tree Go files"},
		{name: "blob read Git failure", resolvedGo: "package example\nfunc PR() {}\n", gitFailure: "cat-file blob", wantError: "read merge tree Go source"},
	})
}
