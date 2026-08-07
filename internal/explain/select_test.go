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
		Number:     1640,
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
