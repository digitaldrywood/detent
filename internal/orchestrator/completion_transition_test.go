package orchestrator

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestCompletedActiveReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		issue              connector.Issue
		finalState         string
		reviewState        string
		requirePullRequest bool
		want               string
	}{
		{
			name:               "todo completed with open pull request advances to human review",
			issue:              completionTransitionIssue("Todo", "OPEN"),
			finalState:         FinalStateCompleted,
			requirePullRequest: true,
			want:               autoPromoteSourceState,
		},
		{
			name:               "in progress completed with open pull request advances to human review",
			issue:              completionTransitionIssue("In Progress", "OPEN"),
			finalState:         FinalStateCompleted,
			requirePullRequest: true,
			want:               autoPromoteSourceState,
		},
		{
			name:               "artifact todo completed without pull request advances to configured review",
			issue:              completionTransitionIssue("Todo", ""),
			finalState:         FinalStateCompleted,
			reviewState:        "Review",
			requirePullRequest: false,
			want:               "Review",
		},
		{
			name:               "rework completed with open pull request waits for dispatch",
			issue:              completionTransitionIssue("Rework", "OPEN"),
			finalState:         FinalStateCompleted,
			requirePullRequest: true,
		},
		{
			name:               "merging completed with open pull request waits for merge lifecycle",
			issue:              completionTransitionIssue("Merging", "OPEN"),
			finalState:         FinalStateCompleted,
			requirePullRequest: true,
		},
		{
			name:               "todo completed without pull request waits when pull request required",
			issue:              completionTransitionIssue("Todo", ""),
			finalState:         FinalStateCompleted,
			requirePullRequest: true,
		},
	}

	activeStates := normalizedStates([]string{"Todo", "In Progress", "Rework", "Merging"})
	terminalStates := normalizedStates([]string{"Done", "Cancelled"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reviewState := tt.reviewState
			if reviewState == "" {
				reviewState = autoPromoteSourceState
			}
			got := completedActiveReviewTargetState(
				tt.issue,
				tt.finalState,
				activeStates,
				terminalStates,
				reviewState,
				tt.requirePullRequest,
			)
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
