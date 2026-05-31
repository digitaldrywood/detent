package orchestrator

import (
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/symphony-go/internal/config"
	"github.com/digitaldrywood/symphony-go/internal/connector"
)

func TestSortIssuesForDispatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		dispatchPriority []string
		issues           []connector.Issue
		want             []string
	}{
		{
			name:             "sorts by state dispatch rank before priority and age",
			dispatchPriority: []string{"Merging", "Rework"},
			issues: []connector.Issue{
				rankingIssue("todo-old-urgent", "Todo", 1, now.Add(-4*time.Hour)),
				rankingIssue("rework-new-low", "Rework", 4, now.Add(-time.Hour)),
				rankingIssue("merging-new-low", "Merging", 4, now.Add(-30*time.Minute)),
			},
			want: []string{"merging-new-low", "rework-new-low", "todo-old-urgent"},
		},
		{
			name:             "sorts by priority within the same state rank",
			dispatchPriority: []string{"Todo"},
			issues: []connector.Issue{
				rankingIssue("todo-medium", "Todo", 3, now.Add(-3*time.Hour)),
				rankingIssue("todo-none", "Todo", 0, now.Add(-4*time.Hour)),
				rankingIssue("todo-urgent", "Todo", 1, now.Add(-time.Hour)),
				rankingIssue("todo-high", "Todo", 2, now.Add(-2*time.Hour)),
			},
			want: []string{"todo-urgent", "todo-high", "todo-medium", "todo-none"},
		},
		{
			name:             "sorts older issues first within the same state rank and priority",
			dispatchPriority: []string{"Todo"},
			issues: []connector.Issue{
				rankingIssue("todo-new", "Todo", 2, now.Add(-time.Hour)),
				rankingIssue("todo-old", "Todo", 2, now.Add(-3*time.Hour)),
				rankingIssue("todo-middle", "Todo", 2, now.Add(-2*time.Hour)),
			},
			want: []string{"todo-old", "todo-middle", "todo-new"},
		},
		{
			name:             "normalizes state ranks and sorts unranked states last",
			dispatchPriority: []string{" Merging ", "Rework"},
			issues: []connector.Issue{
				rankingIssue("todo-old-urgent", "Todo", 1, now.Add(-4*time.Hour)),
				rankingIssue("rework-high", " rework ", 2, now.Add(-time.Hour)),
				rankingIssue("merging-low", "merging", 4, now.Add(-30*time.Minute)),
				rankingIssue("in-progress-high", "In Progress", 2, now.Add(-3*time.Hour)),
			},
			want: []string{"merging-low", "rework-high", "todo-old-urgent", "in-progress-high"},
		},
		{
			name: "uses deterministic identifier order after state rank priority and age",
			issues: []connector.Issue{
				rankingIssue("issue-c", "Todo", 2, now),
				rankingIssue("issue-a", "Todo", 2, now),
				rankingIssue("issue-b", "Todo", 2, now),
			},
			want: []string{"issue-a", "issue-b", "issue-c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issues := cloneRankingIssues(tt.issues)
			sortIssuesForDispatch(issues, tt.dispatchPriority)

			if got := rankingIssueIDs(issues); !equalStrings(got, tt.want) {
				t.Fatalf("sortIssuesForDispatch() ids = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConfigFromWorkflowIncludesDispatchPriorityByState(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Default()
	workflow.Agent.DispatchPriorityByState = []string{"Merging", "Rework"}

	cfg := ConfigFromWorkflow(workflow)
	workflow.Agent.DispatchPriorityByState[0] = "Todo"

	want := []string{"Merging", "Rework"}
	if !equalStrings(cfg.DispatchPriorityByState, want) {
		t.Fatalf("ConfigFromWorkflow().DispatchPriorityByState = %#v, want %#v", cfg.DispatchPriorityByState, want)
	}
}

func rankingIssue(id string, state string, priority int, createdAt time.Time) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = id
	issue.State = state
	issue.CreatedAt = &createdAt
	if priority > 0 {
		issue.Priority = &priority
	}
	return issue
}

func cloneRankingIssues(issues []connector.Issue) []connector.Issue {
	cloned := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		cloned[i] = cloneIssue(issue)
	}
	return cloned
}

func rankingIssueIDs(issues []connector.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
