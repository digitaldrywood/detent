package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestTransitionCompletedActiveIssuesDirectlyPromotesZeroQuietReview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	reviewedAt := now.Add(-time.Minute)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:                 17,
		URL:                    "https://github.test/digitaldrywood/detent/pull/17",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(result.dispatchCandidates) != 1 || result.dispatchCandidates[0].State != "Merging" {
		t.Fatalf("dispatchCandidates = %#v, want one Merging issue", result.dispatchCandidates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one auto-promote audit comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promoted this issue from In Progress to Merging.",
		"reason: ready",
		"https://github.test/digitaldrywood/detent/pull/17",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing %q", tracker.comments[0].body, fragment)
		}
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Merging" {
		t.Fatalf("Completed issue state = %q, want Merging", got)
	}
	if len(state.RecentEvents) == 0 || !strings.Contains(state.RecentEvents[len(state.RecentEvents)-1].Message, "from In Progress to Merging") {
		t.Fatalf("RecentEvents = %#v, want direct auto-promote audit event", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesKeepsHumanReviewForNonZeroQuietReview(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	reviewedAt := now.Add(-20 * time.Minute)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:                 18,
		URL:                    "https://github.test/digitaldrywood/detent/pull/18",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no auto-promote comment for Human Review transition", tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Human Review" {
		t.Fatalf("Completed issue state = %q, want Human Review", got)
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
