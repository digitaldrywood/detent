package explain

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestResolveSnapshotIssueReferenceForms(t *testing.T) {
	t.Parallel()

	issue := telemetry.Issue{
		ID:         "issue-1640",
		Identifier: "digitaldrywood/detent#1640",
		ProjectID:  "detent",
		URL:        "https://github.com/digitaldrywood/detent/issues/1640",
		State:      "Rework",
	}
	snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{issue}}
	tests := []struct {
		name      string
		reference string
	}{
		{name: "ID", reference: issue.ID},
		{name: "canonical identifier", reference: issue.Identifier},
		{name: "URL", reference: issue.URL},
		{name: "bare number", reference: "1640"},
		{name: "hash number", reference: "#1640"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveSnapshotIssue(snapshot, Query{Reference: tt.reference}, SnapshotIssueScope{})
			if err != nil {
				t.Fatalf("ResolveSnapshotIssue() error = %v", err)
			}
			if got.Identity.IssueID != issue.ID || got.Identity.ProjectID != issue.ProjectID || got.Source != "board" {
				t.Fatalf("ResolveSnapshotIssue() = %#v, want board identity", got)
			}
		})
	}
}

func TestResolveSnapshotIssuePrecedenceAndScope(t *testing.T) {
	t.Parallel()

	base := telemetry.Issue{ID: "issue-1", Identifier: "example/repo#1", Number: 1, ProjectID: "detent"}
	board := base
	board.State = "Rework"
	pipeline := base
	pipeline.State = "Todo"
	runtime := base
	runtime.State = "In Progress"
	completed := base
	completed.State = "Done"
	staleRuntime := runtime
	staleRuntime.Identifier = "example/renamed#1"
	staleRuntime.URL = "https://example.com/renamed/issues/1"
	tests := []struct {
		name       string
		snapshot   telemetry.Snapshot
		scope      SnapshotIssueScope
		wantSource string
		wantLane   string
		wantErr    error
	}{
		{
			name: "board precedes pipeline and runtime duplicates",
			snapshot: telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{board},
				Pipeline:    []telemetry.Issue{pipeline},
				Running:     []telemetry.Running{{Issue: runtime}},
			},
			wantSource: "board",
			wantLane:   "Rework",
		},
		{
			name: "stale lower precedence identity does not create ambiguity",
			snapshot: telemetry.Snapshot{
				BoardIssues: []telemetry.Issue{board},
				Running:     []telemetry.Running{{Issue: staleRuntime}},
			},
			wantSource: "board",
			wantLane:   "Rework",
		},
		{
			name:     "completed excluded by default",
			snapshot: telemetry.Snapshot{Completed: []telemetry.Completed{{Issue: completed}}},
			wantErr:  ErrNotFound,
		},
		{
			name:       "completed included explicitly",
			snapshot:   telemetry.Snapshot{Completed: []telemetry.Completed{{Issue: completed}}},
			scope:      SnapshotIssueScope{IncludeCompleted: true},
			wantSource: "completed",
			wantLane:   "Done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveSnapshotIssue(tt.snapshot, Query{ProjectID: "detent", Reference: "#1"}, tt.scope)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveSnapshotIssue() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if got.Source != tt.wantSource || got.Issue.State != tt.wantLane {
				t.Fatalf("ResolveSnapshotIssue() = %#v, want %s/%s", got, tt.wantSource, tt.wantLane)
			}
		})
	}
}

func TestResolveSnapshotIssueRejectsProjectCollision(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{BoardIssues: []telemetry.Issue{
		{ID: "alpha-1", Identifier: "example/alpha#1", Number: 1, ProjectID: "alpha"},
		{ID: "beta-1", Identifier: "example/beta#1", Number: 1, ProjectID: "beta"},
	}}
	_, err := ResolveSnapshotIssue(snapshot, Query{Reference: "#1"}, SnapshotIssueScope{})
	var ambiguous *AmbiguousIdentityError
	if !errors.As(err, &ambiguous) || ambiguous.Field != "project_id" {
		t.Fatalf("ResolveSnapshotIssue() error = %#v, want project ambiguity", err)
	}
}

func TestResolveSnapshotIssueMissingNumberScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issues  []telemetry.Issue
		project string
		wantID  string
		wantErr error
	}{
		{name: "selected project", project: "detent", issues: []telemetry.Issue{
			{ID: "a", Identifier: "owner/a#2337", ProjectID: "detent"},
			{ID: "b", Identifier: "owner/b#2337", ProjectID: "other"},
		}, wantID: "a"},
		{name: "other project only", project: "detent", issues: []telemetry.Issue{
			{ID: "b", Identifier: "owner/b#2337", ProjectID: "other"},
		}, wantErr: ErrNotFound},
		{name: "same project ambiguity", project: "detent", issues: []telemetry.Issue{
			{ID: "a", Identifier: "owner/a#2337", ProjectID: "detent"},
			{ID: "b", Identifier: "owner/b#2337", ProjectID: "detent"},
		}, wantErr: &AmbiguousIdentityError{}},
		{name: "unscoped project ambiguity", issues: []telemetry.Issue{
			{ID: "a", Identifier: "owner/a#2337", ProjectID: "detent"},
			{ID: "b", Identifier: "owner/b#2337", ProjectID: "other"},
		}, wantErr: &AmbiguousIdentityError{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveSnapshotIssue(telemetry.Snapshot{BoardIssues: tt.issues}, Query{ProjectID: tt.project, Reference: "#2337"}, SnapshotIssueScope{})
			var ambiguous *AmbiguousIdentityError
			if errors.As(tt.wantErr, &ambiguous) {
				var target *AmbiguousIdentityError
				if !errors.As(err, &target) {
					t.Fatalf("error = %v, want ambiguity", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got.Identity.IssueID != tt.wantID {
				t.Fatalf("identity = %#v, want %q", got.Identity, tt.wantID)
			}
		})
	}
}

func TestQueryMatchesIssueNumberFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		issue     telemetry.Issue
		reference string
		want      bool
	}{
		{name: "canonical fallback", issue: telemetry.Issue{Identifier: "owner/repo#2337"}, reference: "#2337", want: true},
		{name: "explicit number preserved", issue: telemetry.Issue{Identifier: "local/item", Number: 2337}, reference: "2337", want: true},
		{name: "explicit number takes precedence", issue: telemetry.Issue{Identifier: "owner/repo#2337", Number: 7}, reference: "#2337"},
		{name: "PR number ignored", issue: telemetry.Issue{Identifier: "owner/repo#7", PullRequest: &telemetry.PullRequest{Number: 2337}}, reference: "#2337"},
		{name: "PR URL ignored", issue: telemetry.Issue{URL: "https://github.com/owner/repo/pull/2337"}, reference: "2337"},
		{name: "URL fragment ignored", issue: telemetry.Issue{Identifier: "https://example.com/item#2337"}, reference: "#2337"},
		{name: "node suffix ignored", issue: telemetry.Issue{ID: "node#2337"}, reference: "#2337"},
		{name: "noncanonical suffix ignored", issue: telemetry.Issue{Identifier: "item#2337"}, reference: "#2337"},
		{name: "invalid suffix ignored", issue: telemetry.Issue{Identifier: "owner/repo#2337extra"}, reference: "#2337"},
		{name: "partial number ignored", issue: telemetry.Issue{Identifier: "owner/repo#23370"}, reference: "#2337"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := queryMatchesIssue(Query{Reference: tt.reference}, tt.issue); got != tt.want {
				t.Fatalf("match = %v, want %v", got, tt.want)
			}
		})
	}
}
