package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestReviewPlanIssuesUsesPersistedReworkSignature(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		issue            connector.Issue
		persistedIssue   connector.Issue
		wantUpdates      int
		wantComments     int
		wantTransitioned bool
	}{
		{
			name:             "first review routes to rework",
			issue:            planReviewPullRequestIssue("issue-plan-first", "review-head"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "same review commit skips after restart",
			issue:          planReviewPullRequestIssue("issue-plan-same", "review-head"),
			persistedIssue: planReviewPullRequestIssue("issue-plan-same", "review-head"),
		},
		{
			name:             "changed review commit re-arms rework",
			issue:            planReviewPullRequestIssue("issue-plan-reset", "review-new"),
			persistedIssue:   planReviewPullRequestIssue("issue-plan-reset", "review-old"),
			wantUpdates:      1,
			wantComments:     1,
			wantTransitioned: true,
		},
		{
			name:           "same comment artifact skips after restart",
			issue:          planReviewCommentIssue("issue-plan-comment", "comment-1"),
			persistedIssue: planReviewCommentIssue("issue-plan-comment", "comment-1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &dependencyAutoUnblockConnector{stateIssues: []connector.Issue{tt.issue}}
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch := planReviewTestOrchestrator(tracker, metrics)
			if tt.persistedIssue.ID != "" {
				recordPlanReviewReworkSignatureEvent(t, metrics, tt.persistedIssue, time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC))
			}
			state := newState(orch.cfg)

			transitioned := orch.reviewPlanIssues(context.Background(), &state, []connector.Issue{tt.issue}, time.Date(2026, 7, 8, 15, 1, 0, 0, time.UTC))

			if got := len(tracker.updates); got != tt.wantUpdates {
				t.Fatalf("updates = %#v, want %d update(s)", tracker.updates, tt.wantUpdates)
			}
			if got := len(tracker.comments); got != tt.wantComments {
				t.Fatalf("comments = %#v, want %d comment(s)", tracker.comments, tt.wantComments)
			}
			_, didTransition := transitioned[tt.issue.ID]
			if didTransition != tt.wantTransitioned {
				t.Fatalf("transitioned[%q] = %v, want %v", tt.issue.ID, didTransition, tt.wantTransitioned)
			}
			if tt.wantUpdates > 0 {
				assertWorkflowActionSignature(t, metrics, tt.issue, workflowActionPlanReviewRework, planReviewEvaluationFromIssue(tt.issue).Signature)
			}
		})
	}
}

func TestDispatchModeUsesPlanReviewTimelineProvenance(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, supersedingLane, want   string
		persisted, fallback, disabled bool
	}{
		{name: "ordinary rework", want: runpkg.RunModeImplement},
		{name: "plan review rework", persisted: true, want: runpkg.RunModePlan},
		{name: "in memory fallback", fallback: true, want: runpkg.RunModePlan},
		{name: "stale plan provenance", persisted: true, supersedingLane: gate.DefaultPlanStop, want: runpkg.RunModeImplement},
		{name: "superseded rework", persisted: true, supersedingLane: autoPromoteReworkState, want: runpkg.RunModeImplement},
		{name: "persisted event overrides fallback", fallback: true, supersedingLane: autoPromoteReworkState, want: runpkg.RunModeImplement},
		{name: "planning disabled", persisted: true, fallback: true, disabled: true, want: runpkg.RunModeImplement},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
			issue := planReviewCommentIssue("issue-plan-dispatch", "comment-1")
			issue.State = autoPromoteReworkState
			issue.Comments = append(issue.Comments, connector.IssueComment{Body: "Plan review routed this issue from Plan Review to Rework."})
			metrics := &autoPromoteWorkflowMetricsRecorder{}
			orch := planReviewTestOrchestrator(&dependencyAutoUnblockConnector{}, metrics)
			orch.cfg.Plan.Enabled = !tt.disabled
			state := newState(orch.cfg)
			if tt.fallback {
				state.planRework[issue.ID] = struct{}{}
			}
			if tt.persisted {
				recordPlanReviewReworkSignatureEvent(t, metrics, issue, at)
			}
			if tt.supersedingLane != "" {
				if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
					ProjectID: defaultWorkflowMetricsProjectID, IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL,
					PhaseType: store.WorkflowPhaseTypeLane, PhaseName: tt.supersedingLane,
					Reason: "plan_worker_completed", Status: "entered", StartedAt: at.Add(2 * time.Minute), MetadataJSON: "{}",
				}); err != nil {
					t.Fatal(err)
				}
			}
			if got := orch.dispatchMode(t.Context(), &state, issue); got != tt.want {
				t.Fatalf("dispatchMode = %q, want %q", got, tt.want)
			}
			state.BoardIssues = []connector.Issue{issue}
			state.Pipeline = []connector.Issue{issue}
			state.Running[issue.ID] = Running{Issue: issue}
			state.Retry[issue.ID] = Retry{Issue: issue}
			state.Blocked[issue.ID] = Blocked{Issue: issue}
			state.Completed[issue.ID] = Completed{Issue: issue}
			orch.publishState(t.Context(), &state)
			published := orch.publishedState().clone()
			orch.startCompletion(t.Context(), &state)
			for _, observed := range []State{published, orch.publishedState()} {
				snapshot := observed.Snapshot(at)
				for _, row := range []telemetry.Issue{snapshot.BoardIssues[0], snapshot.Pipeline[0], snapshot.Running[0].Issue, snapshot.Queue[0].Issue, snapshot.Blocked[0].Issue, snapshot.Completed[0].Issue} {
					if row.DispatchMode != tt.want {
						t.Fatalf("snapshot dispatch mode = %q, want %q", row.DispatchMode, tt.want)
					}
				}
			}
		})
	}
}

func TestPlanReviewContainsReviewSeverityUsesExplicitBadges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		severity string
		want     bool
	}{
		{
			name:     "bracketed p1 anywhere",
			body:     "Automated review found [P1] missing validation.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "line anchored colon",
			body:     "P1: Missing rollback path.",
			severity: "P1",
			want:     true,
		},
		{
			name:     "list prefixed p2 badge",
			body:     "- P2 BADGE naming concern.",
			severity: "P2",
			want:     true,
		},
		{
			name:     "narrative p1 negative",
			body:     "No P1 issues found - approved.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "mid sentence p1 fix",
			body:     "The P1 fix from last week is already present.",
			severity: "P1",
			want:     false,
		},
		{
			name:     "p2 finding prose no longer matches",
			body:     "P2 finding count is zero.",
			severity: "P2",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := containsReviewSeverity(tt.body, tt.severity)
			if got != tt.want {
				t.Fatalf("containsReviewSeverity(%q, %q) = %t, want %t", tt.body, tt.severity, got, tt.want)
			}
		})
	}
}

func TestPlanReviewDecisionLogsReviewStateDisagreement(t *testing.T) {
	t.Parallel()

	var logs strings.Builder
	orch := &Orchestrator{
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	issue := connector.Issue{
		ID:         "issue-1072",
		Identifier: "digitaldrywood/detent#1072",
		PullRequest: &connector.PullRequest{
			CodexReviewAPIState:     "APPROVED",
			CodexReviewBodySeverity: "P1",
		},
	}

	orch.logPlanReviewDecision(issue, gate.Decision{
		Action: gate.ActionRework,
		Reason: gate.ReasonP1Findings,
	}, "Rework")

	for _, fragment := range []string{"review_api_state=APPROVED", "review_body_severity=P1"} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func planReviewTestOrchestrator(
	tracker *dependencyAutoUnblockConnector,
	metrics *autoPromoteWorkflowMetricsRecorder,
) *Orchestrator {
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		Plan: gate.PlanConfig{
			Enabled: true,
			Review:  gate.PlanReviewAutomated,
			Stop:    gate.DefaultPlanStop,
		},
		ActiveStates:               []string{"Todo", "In Progress", "Rework"},
		TerminalStates:             []string{"Done", "Cancelled"},
		ContinuationRetryDelay:     time.Second,
		FailureRetryBaseDelay:      time.Second,
		GitHubGraphQLWarnRemaining: 500,
	})
	return &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		workflowMetrics: metrics,
	}
}

func planReviewPullRequestIssue(id string, reviewCommit string) connector.Issue {
	issue := dependencyAutoUnblockIssue(id, gate.DefaultPlanStop)
	prNumber := 524
	issue.PRNumber = &prNumber
	issue.PullRequest = &connector.PullRequest{
		Number:                     prNumber,
		State:                      "OPEN",
		URL:                        "https://github.test/digitaldrywood/detent/pull/524",
		HeadSHA:                    "head-sha",
		CodexReviewState:           "P1",
		LatestCodexReviewCommitSHA: reviewCommit,
		CodexReviewFindings: []connector.PullRequestFinding{{
			Body: "Plan omits acceptance criteria.",
			URL:  "https://github.test/comment/plan-review",
		}},
	}
	return issue
}

func planReviewCommentIssue(id string, commentID string) connector.Issue {
	issue := dependencyAutoUnblockIssue(id, gate.DefaultPlanStop)
	issue.Comments = []connector.IssueComment{{
		ID:   commentID,
		Body: "## Detent Plan Review\n\n- state: P1\n\n### Findings\n\n- Plan omits acceptance criteria.",
		URL:  "https://github.test/comment/" + commentID,
	}}
	return issue
}

func recordPlanReviewReworkSignatureEvent(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
) {
	t.Helper()

	signature := planReviewEvaluationFromIssue(issue).Signature
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionPlanReviewRework, signature)
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    autoPromoteReworkState,
		Reason:       "plan_review_decision",
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}
