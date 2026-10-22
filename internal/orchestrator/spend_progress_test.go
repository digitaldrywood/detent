package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func spendProgressIssueWithPR(headSHA string, mergeableState string, ciStatus string) connector.Issue {
	number := 214
	return connector.Issue{
		PRNumber: &number,
		PullRequest: &connector.PullRequest{
			Number:         214,
			HeadSHA:        headSHA,
			MergeableState: mergeableState,
			CIStatus:       ciStatus,
		},
	}
}

func TestRefreshSpendProgressIssue(t *testing.T) {
	t.Parallel()

	baseIssue := connector.Issue{ID: "issue-1276", Identifier: "digitaldrywood/detent#1276"}
	linkedIssue := spendProgressIssueWithPR("head", "dirty", "failure")
	linkedIssue.ID = baseIssue.ID
	linkedIssue.Identifier = baseIssue.Identifier
	linkedIssue.PRRepository = "digitaldrywood/detent"
	degradedIssue := cloneIssue(linkedIssue)
	degradedIssue.PullRequest.HydrationDegradedReason = connector.PullRequestHydrationReasonStaleCachedPullData
	fallbackTracker := &implementProgressConnector{refreshed: linkedIssue, hydrated: linkedIssue}

	tests := []struct {
		name        string
		orch        *Orchestrator
		issue       connector.Issue
		wantWarning string
		wantPR      bool
		wantHead    string
	}{
		{name: "missing connector", orch: &Orchestrator{}, issue: baseIssue, wantWarning: "refresh unavailable"},
		{name: "reference refresh failure", orch: &Orchestrator{connector: &implementProgressConnector{referenceErr: errors.New("github unavailable")}}, issue: baseIssue, wantWarning: "reference refresh failed"},
		{name: "fallback refresh failure", orch: &Orchestrator{connector: connectorOnly{Connector: &implementProgressConnector{refreshErr: errors.New("github unavailable")}}}, issue: baseIssue, wantWarning: "evidence refresh failed"},
		{name: "refresh confirms no PR", orch: &Orchestrator{connector: &implementProgressConnector{refreshed: baseIssue}}, issue: baseIssue},
		{
			name: "fallback refresh discovers and hydrates PR",
			orch: &Orchestrator{connector: spendProgressHydratingConnector{
				Connector: fallbackTracker,
				hydrator:  fallbackTracker,
			}},
			issue:    baseIssue,
			wantPR:   true,
			wantHead: "head",
		},
		{
			name: "closing reference discovers and hydrates PR",
			orch: &Orchestrator{connector: &implementProgressConnector{
				refreshed:  baseIssue,
				referenced: linkedIssue,
				hydrated:   linkedIssue,
			}},
			issue:    baseIssue,
			wantPR:   true,
			wantHead: "head",
		},
		{
			name:        "linked PR without hydrator",
			orch:        &Orchestrator{connector: connectorOnly{Connector: &implementProgressConnector{}}},
			issue:       linkedIssue,
			wantWarning: "hydrator unavailable",
			wantPR:      true,
			wantHead:    "head",
		},
		{
			name:        "hydration failure",
			orch:        &Orchestrator{connector: &implementProgressConnector{hydrateErr: errors.New("github unavailable")}},
			issue:       linkedIssue,
			wantWarning: "hydration failed",
			wantPR:      true,
			wantHead:    "head",
		},
		{
			name:        "degraded hydration",
			orch:        &Orchestrator{connector: &implementProgressConnector{hydrated: degradedIssue}},
			issue:       linkedIssue,
			wantWarning: connector.PullRequestHydrationReasonStaleCachedPullData,
			wantPR:      true,
			wantHead:    "head",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue, warning := tt.orch.refreshSpendProgressIssue(t.Context(), tt.issue)
			if !strings.Contains(warning, tt.wantWarning) {
				t.Fatalf("warning = %q, want containing %q", warning, tt.wantWarning)
			}
			if got := issue.PullRequest != nil; got != tt.wantPR {
				t.Fatalf("PR present = %t, want %t", got, tt.wantPR)
			}
			if tt.wantHead != "" && issue.PullRequest.HeadSHA != tt.wantHead {
				t.Fatalf("head = %q, want %q", issue.PullRequest.HeadSHA, tt.wantHead)
			}
			if tt.wantPR && issue.PRRepository == "" {
				t.Fatal("PRRepository is empty")
			}
		})
	}
}

type connectorOnly struct {
	connector.Connector
}

type spendProgressHydratingConnector struct {
	connector.Connector
	hydrator connector.PullRequestHydrator
}

func (c spendProgressHydratingConnector) HydratePullRequest(ctx context.Context, issue connector.Issue) (connector.Issue, error) {
	return c.hydrator.HydratePullRequest(ctx, issue)
}

func TestHandleRunResultAcceptsPRAdvanceBeforeWorkerError(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC)
	createdAt := base.Add(-time.Hour)
	runningIssue := spendProgressIssueWithPR("old-head", "dirty", "failure")
	runningIssue.ID = "issue-1276"
	runningIssue.Identifier = "digitaldrywood/detent#1276"
	runningIssue.State = "In Progress"
	runningIssue.CreatedAt = &createdAt
	hydratedIssue := cloneIssue(runningIssue)
	hydratedIssue.PullRequest.HeadSHA = "new-head"
	tracker := &implementProgressConnector{hydrated: hydratedIssue}
	attempts := &implementProgressAttemptStore{history: []store.WorkAttempt{{
		CompletedAt: base.Add(-20 * time.Minute),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			"spend_since_progress_breaker": map[string]any{
				"limit_usd":      5,
				"pr_fingerprint": map[string]any{"number": 214, "head_sha": "old-head", "mergeable_state": "dirty", "ci_status": "failure"},
			},
		}),
	}}}
	cfg := normalizeConfig(Config{
		Project:                 scheduler.ProjectCandidate{ID: "detent"},
		BillingMode:             "metered",
		NoProgressSpendLimitUSD: 5,
		ActiveStates:            []string{"In Progress"},
		ObservedStates:          []string{"Blocked"},
		TerminalStates:          []string{"Done"},
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
	}
	state := newState(cfg)
	running := Running{TurnCount: 1,
		Issue:               runningIssue,
		Attempt:             2,
		WorkAttemptID:       42,
		Mode:                runpkg.RunModeImplement,
		StartedAt:           base.Add(-5 * time.Minute),
		DispatchSourceState: "Rework",
		DispatchTargetState: "In Progress",
	}
	state.Running[runningIssue.ID] = running
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: running.StartedAt}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: base,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Err:         errors.New("session token ceiling exceeded"),
	})

	if _, blocked := state.Blocked[runningIssue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after PR head advanced", runningIssue.ID)
	}
	if tracker.hydrations != 2 {
		t.Fatalf("hydrations = %d, want 2", tracker.hydrations)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("completions = %#v, want one", attempts.completions)
	}
	retry, ok := state.Retry[runningIssue.ID]
	if !ok || retry.Issue.PullRequest == nil || retry.Issue.PullRequest.HeadSHA != "new-head" || retry.Issue.State != "In Progress" {
		t.Fatalf("retry lost current PR evidence: %+v", retry)
	}
}

func TestHandleRunResultDiscoversWorkerOpenedPRBeforeWorkerError(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	createdAt := base.Add(-8 * 24 * time.Hour)
	runningIssue := implementProgressIssueWithoutPR()
	runningIssue.CreatedAt = &createdAt
	linkedIssue := spendProgressIssueWithPR("new-head", "dirty", "pending")
	linkedIssue.ID = runningIssue.ID
	linkedIssue.Identifier = runningIssue.Identifier
	linkedIssue.PRRepository = "digitaldrywood/detent"
	linkedIssue.Title = runningIssue.Title
	linkedIssue.State = runningIssue.State
	linkedIssue.URL = runningIssue.URL
	linkedIssue.CreatedAt = runningIssue.CreatedAt
	tracker := &implementProgressConnector{
		refreshed:  runningIssue,
		referenced: linkedIssue,
		hydrated:   linkedIssue,
	}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:              scheduler.ProjectCandidate{ID: "detent"},
		BillingMode:          "subscription",
		NoProgressTokenLimit: 25_000_000,
		ActiveStates:         []string{"In Progress"},
		ObservedStates:       []string{"Blocked"},
		TerminalStates:       []string{"Done"},
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
	}
	state := newState(cfg)
	running := Running{TurnCount: 1,
		Issue:               runningIssue,
		Attempt:             4,
		WorkAttemptID:       42,
		Mode:                runpkg.RunModeImplement,
		StartedAt:           base.Add(-10 * time.Minute),
		DispatchSourceState: "Rework",
		DispatchTargetState: "In Progress",
	}
	state.Running[runningIssue.ID] = running
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: running.StartedAt}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: base,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Err:         errors.New("session token ceiling exceeded"),
	})

	if _, blocked := state.Blocked[runningIssue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after linked PR discovery", runningIssue.ID)
	}
	if tracker.referenceRefreshes != 1 || tracker.hydrations != 2 {
		t.Fatalf("reference refreshes = %d, hydrations = %d, want 2 each", tracker.referenceRefreshes, tracker.hydrations)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("completions = %#v, want one", attempts.completions)
	}
	retry, ok := state.Retry[runningIssue.ID]
	if !ok || retry.Issue.PullRequest == nil || retry.Issue.PullRequest.HeadSHA != "new-head" || retry.Issue.State != "In Progress" {
		t.Fatalf("retry lost current PR evidence: %+v", retry)
	}
}
