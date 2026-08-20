package telemetry

import "testing"

func TestBoardWorkload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot Snapshot
		project  string
		want     BoardWorkloadCounts
	}{
		{
			name: "separates ready todo dependency waits active lanes and blocked lane",
			snapshot: Snapshot{
				BoardIssues: []Issue{
					{ID: "todo", ProjectID: "detent", State: "Todo"},
					{ID: "waiting", ProjectID: "detent", State: "Todo"},
					{ID: "progress", ProjectID: "detent", State: "In Progress"},
					{ID: "rework", ProjectID: "detent", State: "Rework"},
					{ID: "review", ProjectID: "detent", State: "Human Review"},
					{ID: "merging", ProjectID: "detent", State: "Merging"},
					{ID: "blocked", ProjectID: "detent", State: "Blocked"},
					{ID: "backlog", ProjectID: "detent", State: "Backlog"},
					{ID: "done", ProjectID: "detent", State: "Done"},
				},
				Blocked: []Blocked{{Issue: Issue{ID: "waiting", ProjectID: "detent", State: "Todo"}, Source: BlockedSourceDependency}},
			},
			want: BoardWorkloadCounts{Load: 6, Todo: 1, Active: 4, Waiting: 1, Blocked: 1},
		},
		{
			name: "blocked lane remains blocked even with dependency metadata",
			snapshot: Snapshot{
				BoardIssues: []Issue{{ID: "blocked", State: "Blocked"}},
				Blocked:     []Blocked{{Issue: Issue{ID: "blocked", State: "Blocked"}, Source: BlockedSourceDependency}},
			},
			want: BoardWorkloadCounts{Blocked: 1},
		},
		{
			name: "tracker blocked lane wins over stale dependency runtime state",
			snapshot: Snapshot{
				BoardIssues: []Issue{{ID: "blocked", State: "Blocked"}},
				Blocked:     []Blocked{{Issue: Issue{ID: "blocked", State: "Todo"}, Source: BlockedSourceDependency}},
			},
			want: BoardWorkloadCounts{Blocked: 1},
		},
		{
			name: "resolved refs clear stale dependency classification",
			snapshot: Snapshot{
				BoardIssues: []Issue{{ID: "resolved", State: "Todo"}},
				Blocked: []Blocked{{
					Issue: Issue{
						ID:    "resolved",
						State: "Todo",
						BlockedBy: []BlockedRef{{
							Identifier:   "digitaldrywood/detent#1900",
							State:        "Done",
							TrackerState: "closed",
						}},
					},
					Source:         BlockedSourceProjectStatus,
					RecoveryReason: "dependency_blocker",
				}},
			},
			want: BoardWorkloadCounts{Blocked: 1},
		},
		{
			name: "runtime block wins over stale ready tracker state",
			snapshot: Snapshot{
				BoardIssues: []Issue{{ID: "blocked", State: "Todo"}},
				Blocked:     []Blocked{{Issue: Issue{ID: "blocked", State: "Todo"}, Source: BlockedSourceOwnership}},
			},
			want: BoardWorkloadCounts{Blocked: 1},
		},
		{
			name: "tracker terminal state removes stale blocked runtime state",
			snapshot: Snapshot{
				BoardIssues: []Issue{{ID: "done", State: "Done"}},
				Blocked:     []Blocked{{Issue: Issue{ID: "done", State: "Blocked"}}},
			},
			want: BoardWorkloadCounts{},
		},
		{
			name: "custom tracker states preserve runtime workload state",
			snapshot: Snapshot{
				BoardIssues: []Issue{
					{ID: "running", State: "Research"},
					{ID: "queued", State: "Draft"},
				},
				Running: []Running{{Issue: Issue{ID: "running"}}},
				Queue:   []Queued{{Issue: Issue{ID: "queued"}}},
			},
			want: BoardWorkloadCounts{Load: 2, Todo: 1, Active: 1},
		},
		{
			name: "runtime rows fill absent board details without double counting",
			snapshot: Snapshot{
				Project: Project{ID: "detent"},
				Running: []Running{{Issue: Issue{ID: "active"}}},
				Queue:   []Queued{{Issue: Issue{ID: "todo"}}},
			},
			project: "detent",
			want:    BoardWorkloadCounts{Load: 2, Todo: 1, Active: 1},
		},
		{
			name: "completed sessions retain open tracker states in load",
			snapshot: Snapshot{Completed: []Completed{
				{Issue: Issue{ID: "review", State: "Human Review"}, FinalState: "Human Review"},
				{Issue: Issue{ID: "done", State: "Done"}, FinalState: "Done"},
			}},
			want: BoardWorkloadCounts{Load: 1, Active: 1},
		},
		{
			name: "review lane aliases count as active",
			snapshot: Snapshot{BoardIssues: []Issue{
				{ID: "review", State: "Review"},
				{ID: "in-review", State: "In Review"},
				{ID: "underscore", State: "human_review"},
				{ID: "running", State: "Running"},
			}},
			want: BoardWorkloadCounts{Load: 4, Active: 4},
		},
		{
			name: "project scope excludes other projects",
			snapshot: Snapshot{BoardIssues: []Issue{
				{ID: "detent", ProjectID: "detent", State: "Todo"},
				{ID: "gopher", ProjectID: "gopher-ai", State: "In Progress"},
			}},
			project: "detent",
			want:    BoardWorkloadCounts{Load: 1, Todo: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BoardWorkload(tt.snapshot)
			if tt.project != "" {
				got = BoardWorkloadForProject(tt.snapshot, tt.project)
			}
			if got != tt.want {
				t.Fatalf("BoardWorkload() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
