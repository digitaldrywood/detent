package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDependencyAuthoritySurvivesRefresh(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, source string
		refs         []connector.BlockedRef
	}{
		{name: "native removal", source: connector.BlockedRefSourceNative},
		{name: "native relation", source: connector.BlockedRefSourceNative, refs: []connector.BlockedRef{{Identifier: "owner/repo#100", Source: connector.BlockedRefSourceNative}}},
		{name: "body fallback", source: connector.BlockedRefSourceProse, refs: []connector.BlockedRef{{Identifier: "owner/repo#100", Source: connector.BlockedRefSourceProse}}},
		{name: "body removal", source: connector.BlockedRefSourceProse},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := connector.Issue{ID: "issue-101", Identifier: "owner/repo#101", State: "Blocked", Description: "Depends on: #100", Comments: []connector.IssueComment{{Body: "Blocked by: #99"}}, BlockedBy: []connector.BlockedRef{{Identifier: "owner/repo#100"}}}
			fresh := connector.Issue{ID: old.ID, Identifier: old.Identifier, State: old.State, DependencySource: tt.source, BlockedBy: tt.refs, DependencyNotes: []string{"owner/repo#99: prose dependency ignored: native relation absent"}}
			tracker := &dependencyAutoUnblockConnector{hydratedIssues: []connector.Issue{fresh}}
			orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
			got, found, err := orch.hydrateDependencyAutoUnblockIssue(t.Context(), old, []string{"blocked"})
			if err != nil || !found {
				t.Fatalf("hydrate = %t, %v", found, err)
			}
			if len(got.BlockedBy) != len(tt.refs) || got.DependencySource != tt.source {
				t.Fatalf("dependency selection = %+v (%s)", got.BlockedBy, got.DependencySource)
			}
			if len(got.DependencyNotes) != 1 || got.DependencyNotes[0] != fresh.DependencyNotes[0] {
				t.Fatalf("notes = %v", got.DependencyNotes)
			}
			// Both dispatch hydration and dependency recovery use this tracker merge.
			merged := issueWithTextDependencyRefs(mergeIssueTrackerFields(old, fresh))
			if len(merged.BlockedBy) != len(tt.refs) {
				t.Fatalf("merge restored stale blockers: %+v", merged.BlockedBy)
			}
		})
	}
}

func TestDependencyTextIgnoresHistoricalComments(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"Blocked by: #100", "## Codex Workpad\nBlocked by: #100"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			issue := issueWithTextDependencyRefs(connector.Issue{Identifier: "owner/repo#101", Comments: []connector.IssueComment{{Body: body}}})
			if len(issue.BlockedBy) != 0 {
				t.Fatalf("historical blockers = %+v", issue.BlockedBy)
			}
		})
	}
}
