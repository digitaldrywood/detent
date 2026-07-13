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
			name:       "artifact rework completed without pull request advances to configured review",
			issue:      completionTransitionIssue("Rework", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				SourceState: "Review",
				PassState:   "Ready for Pickup",
				ReworkState: "Rework",
				Gate:        artifactCompletionTestGate(),
			},
			want: "Review",
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
			name:       "command gate with quiet window and source wait skips human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				Gate:          gate.Config{Kind: gate.KindCommand},
			},
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

func TestActiveArtifactGateWaitReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue connector.Issue
		want  string
	}{
		{
			name:  "production pending review moves to review",
			issue: artifactCompletionTransitionIssue("Production", "pending_review"),
			want:  "Review",
		},
		{
			name:  "rework rendering moves to review",
			issue: artifactCompletionTransitionIssue("Rework", "rendering"),
			want:  "Review",
		},
		{
			name:  "todo queued does not move to review",
			issue: artifactCompletionTransitionIssue("Todo", "queued"),
		},
		{
			name:  "production pass status does not move to review",
			issue: artifactCompletionTransitionIssue("Production", "approved"),
		},
	}

	activeStates := normalizedStates([]string{"Todo", "Production", "Rework"})
	terminalStates := normalizedStates([]string{"Ready for Pickup", "Done", "Cancelled"})
	cfg := AutoPromoteConfig{
		Enabled:     true,
		SourceState: "Review",
		PassState:   "Ready for Pickup",
		ReworkState: "Rework",
		Gate:        artifactCompletionTestGate(),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := activeArtifactGateWaitReviewTargetState(tt.issue, activeStates, terminalStates, cfg)
			if got != tt.want {
				t.Fatalf("activeArtifactGateWaitReviewTargetState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAutoPromoteActiveGatePendingIssueIncludesCompletedArtifact(t *testing.T) {
	t.Parallel()

	issue := artifactCompletionTransitionIssue("Production", "pending_review")
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			SourceState:   "Review",
			PassState:     "Ready for Pickup",
			ReworkState:   "Rework",
			GateWaitState: autoPromoteGateWaitSource,
			Gate:          artifactCompletionTestGate(),
		},
	})
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	if !autoPromoteActiveGatePendingIssue(issue, &state, cfg, cfg.AutoPromote) {
		t.Fatal("completed artifact gate wait was not recognized without a pull request")
	}
}

func TestTransitionActiveArtifactGateWaitIssuesReconcilesToReviewAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 15, 35, 0, 0, time.UTC)
	issue := artifactCompletionTransitionIssue("Production", "pending_review")
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate:        artifactCompletionTestGate(),
		},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	result := orch.transitionActiveArtifactGateWaitIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after restart reconciliation", issue.ID)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "artifact_gate_wait_review_reconciliation" {
		t.Fatalf("RecentEvents = %#v, want artifact gate wait reconciliation event", state.RecentEvents)
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

func TestTransitionCompletedActiveIssuesHandlesArtifactReworkNoop(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 13, 0, 0, 0, time.UTC)
	issue := artifactCompletionTransitionIssue("Rework", "recut")
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate:        artifactCompletionTestGate(),
		},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Review"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Rework" {
		t.Fatalf("Completed issue state = %q, want Rework", got)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want none", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesRoutesInvalidWorkpadStatusToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 14, 10, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:                 20,
		URL:                    "https://github.test/digitaldrywood/detent/pull/20",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: timePointer(now.Add(-20 * time.Minute)),
	}
	issue.Comments = []connector.IssueComment{{
		Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
		URL:  "https://github.test/comment/completed-invalid-workpad",
	}}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			GateWaitState: autoPromoteGateWaitReview,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
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

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Rework" {
		t.Fatalf("Completed issue state = %q, want Rework", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one rework comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote routed this issue from In Progress to Rework",
		"reason: workpad_status_invalid",
		`status "human-review"`,
		"in_progress, blocked, complete",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
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

func artifactCompletionTransitionIssue(state string, status string) connector.Issue {
	issue := completionTransitionIssue(state, "")
	issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
	issue.Fields = map[string]string{"render_status": status}
	return issue
}

func artifactCompletionTestGate() gate.Config {
	return gate.Config{
		Kind: gate.KindArtifact,
		Artifact: gate.ArtifactConfig{
			StatusField:    "render_status",
			PassStatuses:   []string{"approved", "valid"},
			WaitStatuses:   []string{"queued", "rendering", "pending_review"},
			ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
		},
	}
}
