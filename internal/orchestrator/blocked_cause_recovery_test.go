package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// Exercise both observations synchronously: the run loop may publish either
// snapshot before a reader observes the park.

func TestBreakerRecoveryMetadataRemembersPreviousLane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cause string
	}{
		{name: "instant failure", cause: instantFailureCircuitBreakerCause},
		{name: "repeated failure", cause: repeatedFailureCircuitBreakerCause},
		{name: "token ceiling", cause: "token_ceiling_circuit_breaker"},
		{name: "terminal attempt retry", cause: terminalAttemptRetryLimitCause},
		{name: "workspace preparation retry", cause: workspacePreparationRetryLimitCause},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := dependencyAutoUnblockIssue("issue-"+strings.ReplaceAll(tt.name, " ", "-"), "In Progress")
			orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
			metadata := orch.newBlockedRecoveryMetadata(
				t.Context(),
				issue,
				RunModeImplement,
				tt.cause,
				blockedRecoveryPredicateFingerprintChange,
				"Todo",
				DiffStats{},
			)

			if metadata.BlockedRecovery.Predicate != blockedRecoveryPredicateBreakerCooldown {
				t.Fatalf("predicate = %q, want breaker_cooldown_elapsed", metadata.BlockedRecovery.Predicate)
			}
			if metadata.BlockedRecovery.TargetState != issue.State {
				t.Fatalf("target state = %q, want previous lane %q", metadata.BlockedRecovery.TargetState, issue.State)
			}
		})
	}
}

func TestBreakerParkCauseIncludesRetryCycleLimits(t *testing.T) {
	t.Parallel()

	for _, cause := range []string{instantFailureCircuitBreakerCause, terminalAttemptRetryLimitCause, workspacePreparationRetryLimitCause} {
		if !breakerParkCause(cause) {
			t.Fatalf("breakerParkCause(%q) = false, want true", cause)
		}
	}
}

type staticGitHubRESTBudgetProber struct {
	budget telemetry.RESTBudget
	calls  int
}

func (p *staticGitHubRESTBudgetProber) ProbeGitHubRESTBudget(context.Context, connector.Issue) (telemetry.RESTBudget, bool, error) {
	p.calls++
	return p.budget, true, nil
}

func TestBlockedCauseFingerprintTracksNormalizedLabels(t *testing.T) {
	t.Parallel()

	original := blockedCauseSignals{Labels: blockedCauseLabels([]string{"bug", "detent:in-progress"}, "detent:")}
	reordered := blockedCauseSignals{Labels: blockedCauseLabels([]string{" DETENT:BLOCKED ", "Bug", "bug"}, "detent:")}
	withOverride := blockedCauseSignals{Labels: blockedCauseLabels([]string{"bug", "detent:blocked", "agent:max-tokens"}, "detent:")}

	if blockedCauseFingerprint(noProgressLimitReason, original) != blockedCauseFingerprint(noProgressLimitReason, reordered) {
		t.Fatal("status label transition changed the cause fingerprint")
	}
	if blockedCauseFingerprint(noProgressLimitReason, original) == blockedCauseFingerprint(noProgressLimitReason, withOverride) {
		t.Fatal("recovery-affecting label did not change the cause fingerprint")
	}
	if blockedCauseFingerprint(noProgressLimitReason, original) == blockedCauseFingerprint(spendProgressReason, original) {
		t.Fatal("failure cause did not change the cause fingerprint")
	}
}

func TestBlockedCauseFingerprintIgnoresUnrelatedBaseForBreakerParks(t *testing.T) {
	t.Parallel()

	parked := blockedCauseSignals{ConfigFingerprint: "config-same", BaseFingerprint: "base-before", Health: "ready"}
	current := parked
	current.BaseFingerprint = "base-after-unrelated-merge"

	for _, cause := range []string{
		noProgressLimitReason,
		dispatchLoopDetectedReason,
		spendProgressReason,
		repeatedFailureCircuitBreakerCause,
		"token_ceiling_circuit_breaker",
	} {
		if blockedCauseFingerprint(cause, parked) != blockedCauseFingerprint(cause, current) {
			t.Fatalf("breaker cause %q changed after unrelated base movement", cause)
		}
	}
	if blockedCauseFingerprint(strandedUnpushedWorkReason, parked) == blockedCauseFingerprint(strandedUnpushedWorkReason, current) {
		t.Fatal("non-breaker recovery fingerprint ignored base movement")
	}
}

func TestBlockedCauseFingerprintIgnoresAttemptMutatedSignals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current blockedCauseSignals
	}{
		{
			name: "workspace churn",
			current: blockedCauseSignals{
				ConfigFingerprint:    "config-same",
				WorkspaceFingerprint: "workspace-after-failure",
				WorkspaceStatus:      "dirty",
				WorkspacePresent:     true,
				WorkspaceFiles:       1,
				UnpushedCommits:      1,
				Health:               "ready",
			},
		},
		{
			name: "workpad churn",
			current: blockedCauseSignals{
				ConfigFingerprint: "config-same",
				Health:            "ready",
				Workpad: &workpad.Signal{
					Source: workpad.SourceStructured,
					Status: workpad.StatusInProgress,
					Fields: map[string]string{"attempt": "5"},
				},
			},
		},
	}

	parked := blockedCauseSignals{
		ConfigFingerprint:    "config-same",
		WorkspaceFingerprint: "workspace-before-failure",
		WorkspaceStatus:      "clean",
		WorkspacePresent:     true,
		Health:               "ready",
		Workpad: &workpad.Signal{
			Source: workpad.SourceStructured,
			Status: workpad.StatusInProgress,
			Fields: map[string]string{"attempt": "4"},
		},
	}
	want := blockedCauseFingerprint("instant_failure_circuit_breaker", parked)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := blockedCauseFingerprint("instant_failure_circuit_breaker", tt.current); got != want {
				t.Fatalf("fingerprint = %q, want %q", got, want)
			}
		})
	}
}

func blockedLaneEntryCount(events []store.WorkflowPhaseEvent, issueID string) int {
	count := 0
	for _, event := range events {
		if event.IssueID == issueID && event.PhaseType == store.WorkflowPhaseTypeLane &&
			normalizeState(event.PhaseName) == normalizeState(blockedStatusState) &&
			strings.EqualFold(strings.TrimSpace(event.Status), "entered") {
			count++
		}
	}
	return count
}

func workflowTimelineHasCurrentFingerprint(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	fingerprint string,
) bool {
	t.Helper()
	timeline, err := metrics.IssueWorkflowTimeline(t.Context(), store.IssueIdentity{ProjectID: defaultWorkflowMetricsProjectID, IssueID: issue.ID})
	if err != nil {
		t.Fatalf("IssueWorkflowTimeline() error = %v", err)
	}
	for _, event := range timeline.Events {
		metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON)
		if ok && metadata.BlockedRecovery != nil &&
			metadata.BlockedRecovery.CauseFingerprintVersion == blockedCauseFingerprintVersion &&
			metadata.BlockedRecovery.CauseFingerprint == fingerprint {
			return true
		}
	}
	return false
}

func TestBlockedRecoveryMetadataSeparatesIntentFromReachability(t *testing.T) {
	t.Parallel()

	issue := dependencyAutoUnblockIssue("issue-resume-intent", blockedStatusState)
	orch := blockedCauseTestOrchestrator(&dependencyAutoUnblockConnector{})
	orch.recoveryInspector = staticBlockedRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{
		WorkspacePresent: true,
		WorkspaceFiles:   1,
		WorkspaceStatus:  "present",
		Health:           "ready",
	}}
	metadata := orch.newBlockedRecoveryMetadata(
		t.Context(),
		issue,
		RunModeImplement,
		noProgressLimitReason,
		blockedRecoveryPredicateFingerprintChange,
		"Todo",
		DiffStats{},
	)
	raw := workflowLaneMetadataJSON(issue, metadata)
	if !strings.Contains(raw, `"intent_resumable":true`) {
		t.Fatalf("metadata = %s", raw)
	}
	if !strings.Contains(raw, `"cause_fingerprint_version":3`) {
		t.Fatalf("metadata = %s", raw)
	}
	if strings.Contains(raw, `"resumable":true`) {
		t.Fatalf("metadata retains ambiguous resumable field: %s", raw)
	}
}

func blockedReadyPullRequestIssue() connector.Issue {
	prNumber := 1776
	issue := dependencyAutoUnblockIssue("issue-ready-pr", blockedStatusState)
	issue.PRNumber = &prNumber
	issue.PRRepository = "digitaldrywood/detent"
	issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusComplete}
	issue.PullRequest = &connector.PullRequest{
		Number:          prNumber,
		URL:             "https://github.com/digitaldrywood/detent/pull/1776",
		State:           "OPEN",
		MergeableState:  "clean",
		HeadSHA:         "ready-head",
		BaseSHA:         "ready-base",
		DiffFingerprint: "ready-diff",
		CIStatus:        "success",
		CheckRunCount:   1,
	}
	return issue
}

type blockedReadyPullRequestLookupConnector struct {
	remoteHead       string
	remoteErr        error
	remoteCalls      int
	remoteRepository string
	remoteBranch     string
	*dependencyAutoUnblockConnector
	pullRequest  connector.PullRequest
	found        bool
	err          error
	repository   string
	branch       string
	headSHA      string
	lookupCalls  int
	hydrateCalls int
	created      *connector.PullRequest
	createErr    error
	createCalls  int
}

func (c *blockedReadyPullRequestLookupConnector) LookupPullRequestByHead(_ context.Context, repository string, branch string, headSHA string) (connector.PullRequest, bool, error) {
	c.lookupCalls++
	c.repository = repository
	c.branch = branch
	c.headSHA = headSHA
	return c.pullRequest, c.found, c.err
}

func (c *blockedReadyPullRequestLookupConnector) CreateDraftPullRequest(context.Context, string, string, string, string) (connector.PullRequest, error) {
	c.createCalls++
	if c.createErr != nil {
		return connector.PullRequest{}, c.createErr
	}
	if c.created == nil {
		return connector.PullRequest{}, errors.New("draft pull request unavailable")
	}
	return *c.created, nil
}

func (c *blockedReadyPullRequestLookupConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrateCalls++
	return issue, nil
}

func blockedCauseTestOrchestrator(tracker *dependencyAutoUnblockConnector) *Orchestrator {
	orch := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{})
	orch.cfg.BlockedRecovery.Enabled = false
	orch.cfg.StatusLabelPrefix = "detent:"
	return orch
}

func recordBlockedCausePark(
	t *testing.T,
	metrics *autoPromoteWorkflowMetricsRecorder,
	issue connector.Issue,
	at time.Time,
	metadata workflowLaneMetadata,
) {
	t.Helper()
	if _, err := metrics.RecordWorkflowPhaseEvent(context.Background(), store.WorkflowPhaseEvent{
		ProjectID:    defaultWorkflowMetricsProjectID,
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		IssueURL:     issue.URL,
		PhaseType:    store.WorkflowPhaseTypeLane,
		PhaseName:    blockedStatusState,
		Reason:       noProgressLimitReason,
		Status:       "entered",
		StartedAt:    at,
		MetadataJSON: workflowLaneMetadataJSON(issue, metadata),
	}); err != nil {
		t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
	}
}

type staticBlockedRecoveryInspector struct {
	snapshot runpkg.BlockedRecoverySnapshot
}

func (i staticBlockedRecoveryInspector) BlockedRecoverySnapshot(context.Context, runpkg.RunRequest) runpkg.BlockedRecoverySnapshot {
	return i.snapshot
}

func (c *blockedReadyPullRequestLookupConnector) LookupBranchHead(_ context.Context, repository, branch string) (string, error) {
	c.remoteCalls++
	c.remoteRepository, c.remoteBranch = repository, branch
	return c.remoteHead, c.remoteErr
}
