package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestCompletedActiveReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issue      connector.Issue
		finalState string
		cfg        AutoPromoteConfig
		want       string
	}{
		{
			name:       "todo completed with open pull request advances to human review when disabled",
			issue:      completionTransitionIssue("Todo", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "in progress completed with open pull request advances to human review when disabled",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "artifact todo completed without pull request advances to configured review",
			issue:      completionTransitionIssue("Todo", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				SourceState: "Review",
				Gate:        gate.Config{Kind: gate.KindArtifact},
			},
			want: "Review",
		},
		{
			name:       "artifact production completed with source gate wait advances to configured review",
			issue:      completionTransitionIssue("Production", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				SourceState: "Review",
				PassState:   "Ready for Pickup",
				ReworkState: "Rework",
				Gate: gate.Config{
					Kind: gate.KindArtifact,
					Artifact: gate.ArtifactConfig{
						StatusField:    "render_status",
						PassStatuses:   []string{"approved", "valid"},
						WaitStatuses:   []string{"queued", "rendering", "pending_review"},
						ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
					},
				},
			},
			want: "Review",
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
			name:       "todo completed without pull request waits when pull request required",
			issue:      completionTransitionIssue("Todo", ""),
			finalState: FinalStateCompleted,
		},
		{
			name:       "zero quiet command gate with source wait skips human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindCommand},
			},
		},
		{
			name:       "command gate with quiet window advances to human review",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				Gate:          gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name:       "zero quiet command gate with review wait advances to human review",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:       true,
				GateWaitState: autoPromoteGateWaitReview,
				Gate:          gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name:       "human review gate keeps human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindHumanReview},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "opt out label keeps human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"requires-human-review"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				OptoutLabel: "requires-human-review",
				Gate:        gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "allowlist miss keeps human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"bug"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:            true,
				AllowedIssueLabels: []string{"release"},
				Gate:               gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "allowlist hit skips human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"release"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:            true,
				AllowedIssueLabels: []string{"release"},
				Gate:               gate.Config{Kind: gate.KindCommand},
			},
		},
	}

	activeStates := normalizedStates([]string{"Todo", "In Progress", "Production", "Rework", "Merging"})
	terminalStates := normalizedStates([]string{"Ready for Pickup", "Done", "Cancelled"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := completedActiveReviewTargetState(
				tt.issue,
				tt.finalState,
				activeStates,
				terminalStates,
				tt.cfg,
			)
			if got != tt.want {
				t.Fatalf("completedActiveReviewTargetState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTransitionCompletedActiveIssuesLeavesAutoPromoteIssueActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:           17,
		URL:              "https://github.test/digitaldrywood/detent/pull/17",
		State:            "OPEN",
		MergeableState:   "unknown",
		CIStatus:         "pending",
		CodexReviewState: "COMMENTED",
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if len(result.transitioned) != 0 {
		t.Fatalf("transitioned = %#v, want none", result.transitioned)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no backend write", tracker.updates)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Completed issue state = %q, want In Progress", got)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want none", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesEscalatesGateWaitTimeout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:         19,
		URL:            "https://github.test/digitaldrywood/detent/pull/19",
		State:          "OPEN",
		MergeableState: "unknown",
		CIStatus:       "pending",
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:         true,
			QuietDuration:   0,
			GateWaitTimeout: 15 * time.Minute,
			Gate:            gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-16 * time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one timeout comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote gate wait timed out",
		"reason: auto_promote_gate_wait_timeout",
		"waited: 16m0s",
		"timeout: 15m0s",
		"https://github.test/digitaldrywood/detent/pull/19",
		"ci_status: pending",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Human Review" {
		t.Fatalf("Completed issue state = %q, want Human Review", got)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "completed_active_gate_wait_timeout" {
		t.Fatalf("RecentEvents = %#v, want timeout event", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesKeepsHumanReviewWhenRequired(t *testing.T) {
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
			Gate:          gate.Config{Kind: gate.KindHumanReview},
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

func TestTransitionCompletedActiveIssuesHandlesFailedReviewUpdate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	trackerErr := errors.New("tracker unavailable")
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		updateErr:   trackerErr,
	}
	cfg := normalizeConfig(Config{
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
		t.Fatalf("transitioned[%q] missing after failed update", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want attempted update %#v", got, want)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Completed issue state = %q, want original In Progress after failed update", got)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none after failed update", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want no transition event after failed update", state.RecentEvents)
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
