package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCompletedActiveReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issue      connector.Issue
		finalState string
		want       string
	}{
		{
			name:       "todo completed with open pull request advances to human review",
			issue:      completionTransitionIssue("Todo", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "in progress completed with open pull request advances to human review",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "rework completed with open pull request waits for dispatch",
			issue:      completionTransitionIssue("Rework", "OPEN"),
			finalState: FinalStateCompleted,
		},
		{
			name:       "merging completed with open pull request waits for merge lifecycle",
			issue:      completionTransitionIssue("Merging", "OPEN"),
			finalState: FinalStateCompleted,
		},
		{
			name:       "todo completed without pull request waits",
			issue:      completionTransitionIssue("Todo", ""),
			finalState: FinalStateCompleted,
		},
	}

	activeStates := normalizedStates([]string{"Todo", "In Progress", "Rework", "Merging"})
	terminalStates := normalizedStates([]string{"Done", "Cancelled"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := completedActiveReviewTargetState(tt.issue, tt.finalState, activeStates, terminalStates)
			if got != tt.want {
				t.Fatalf("completedActiveReviewTargetState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func completionTransitionIssue(state string, pullRequestState string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-1"
	issue.Identifier = "digitaldrywood/detent#1"
	issue.Title = "Transition completion"
	issue.State = state
	if pullRequestState != "" {
		issue.PullRequest = &connector.PullRequest{State: pullRequestState}
	}
	return issue
}
