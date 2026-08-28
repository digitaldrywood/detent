package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestWorkerGitHubMonitorCompletionDefersBeforeFailureProcessing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC)
	issue := implementProgressIssueWithoutPR()
	issue.ID = "issue-worker-github-monitor"
	issue.Identifier = "digitaldrywood/detent#2028"
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	attempts := &implementProgressAttemptStore{}
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    &implementProgressConnector{},
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue: issue, Attempt: 4, WorkAttemptID: 2028, Mode: runpkg.RunModeImplement,
		StartedAt: now.Add(-25 * time.Minute), DiffStats: DiffStats{Status: "clean"},
		Tokens: TokenTotals{InputTokens: 2_000_000, OutputTokens: 1_000_000, TotalTokens: 3_000_000},
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-25 * time.Minute)}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
	state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "other", At: now.Add(-time.Minute)}}
	instantFailures := map[string]InstantFailure{issue.ID: state.InstantFailures[issue.ID]}
	repeatedFailures := map[string]RepeatedFailure{issue.ID: state.RepeatedFailures[issue.ID]}
	failureBreaker := cloneProjectFailureBreaker(state.FailureBreaker)

	monitorErr := &runpkg.WorkerGitHubBudgetMonitorError{
		CredentialIdentity: "github-rest:shared-worker",
		Consumer:           telemetry.RESTConsumerSharedPool,
		Operation:          "periodic_probe",
		Err:                errors.New("periodic rate-limit probe timed out"),
	}
	supervisor, err := runpkg.NewSupervisor(staticWorkerGitHubMonitorBackend{
		result: runpkg.RunResult{FinalState: runpkg.FinalStateFailed, TurnStarted: true, DiffStats: DiffStats{Status: "clean"}},
		err:    monitorErr,
	}, runpkg.SupervisorConfig{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	completion := supervisor.Run(t.Context(), runpkg.RunRequest{Issue: issue, Attempt: 4, Mode: runpkg.RunModeImplement})
	if completion.Retryable || completion.RetryAttempt != 0 {
		t.Fatalf("runner completion = %#v, want cooperative monitor stop", completion)
	}
	orch.handleRunResult(t.Context(), &state, completion)

	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity {
		t.Fatalf("completions = %#v, want one capacity wait", attempts.completions)
	}
	if attempts.completions[0].ErrorClass != "worker_github_budget_monitor_unavailable" {
		t.Fatalf("error class = %q, want credential-scoped monitor wait", attempts.completions[0].ErrorClass)
	}
	if strings.Contains(attempts.completions[0].WorkerMetadataJSON, "completion_progress") || strings.Contains(attempts.completions[0].ErrorClass, "no_progress") {
		t.Fatalf("completion = %#v, monitor outage was rewritten by progress processing", attempts.completions[0])
	}
	var metrics struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	}
	if err := json.Unmarshal([]byte(attempts.completions[0].MetricsJSON), &metrics); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if metrics.InputTokens != 2_000_000 || metrics.OutputTokens != 1_000_000 || metrics.TotalTokens != 3_000_000 {
		t.Fatalf("metrics = %#v, want actual usage intact", metrics)
	}
	if state.TokenTotals.TotalTokens != 3_000_000 {
		t.Fatalf("aggregate tokens = %#v, want actual attempt usage retained", state.TokenTotals)
	}
	if state.DiffStats[issue.ID].Status != "clean" {
		t.Fatalf("diff stats = %#v, want actual attempt telemetry retained", state.DiffStats[issue.ID])
	}
	condition, ok := state.GitHubMonitors[monitorErr.CredentialIdentity]
	if !ok || condition.Consumer != monitorErr.Consumer || condition.Operation != monitorErr.Operation || !condition.NextProbeAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("monitor condition = %#v, want one shared credential wait", condition)
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.GitHubMonitors) != 1 || snapshot.GitHubMonitors[0].CredentialIdentity != monitorErr.CredentialIdentity {
		t.Fatalf("snapshot monitor conditions = %#v, want visible credential wait", snapshot.GitHubMonitors)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || !retry.DueAt.Equal(condition.NextProbeAt) || !retry.GitHubMonitor || retry.GitHubCredential != monitorErr.CredentialIdentity {
		t.Fatalf("retry = %#v, want same-attempt monitor canary wait", retry)
	}
	if !reflect.DeepEqual(state.InstantFailures, instantFailures) {
		t.Fatalf("instant failures = %#v, want unchanged", state.InstantFailures)
	}
	if !reflect.DeepEqual(state.RepeatedFailures, repeatedFailures) {
		t.Fatalf("repeated failures = %#v, want unchanged", state.RepeatedFailures)
	}
	if !reflect.DeepEqual(state.FailureBreaker, failureBreaker) {
		t.Fatalf("project failure breaker = %#v, want unchanged", state.FailureBreaker)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("issue %q was blocked by machine-recoverable monitor outage", issue.ID)
	}
}

func TestConcurrentWorkerGitHubMonitorCompletionsShareCredentialCondition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 21, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		MaxConcurrentAgents: 7,
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	attempts := &implementProgressAttemptStore{}
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    &implementProgressConnector{},
		workAttempts: attempts,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "other", At: now.Add(-time.Minute)}}
	issues := []connector.Issue{
		{ID: "issue-launch", Identifier: "digitaldrywood/detent#2028", State: "In Progress"},
		{ID: "issue-periodic", Identifier: "digitaldrywood/detent#2029", State: "In Progress"},
	}
	operations := []string{"launch_probe", "periodic_probe"}
	for index, issue := range issues {
		state.Running[issue.ID] = Running{
			Issue: issue, Attempt: index + 2, WorkAttemptID: int64(300 + index), Mode: runpkg.RunModeImplement,
			StartedAt: now.Add(-25 * time.Minute), Tokens: TokenTotals{TotalTokens: 1},
		}
		state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-25 * time.Minute)}
		state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}
		state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
		orch.handleRunResult(t.Context(), &state, runpkg.Completion{
			IssueID: issue.ID, CompletedAt: now.Add(time.Duration(index) * time.Second),
			Request: runpkg.RunRequest{Issue: issue, Attempt: index + 2, Mode: runpkg.RunModeImplement},
			Result: runpkg.RunResult{
				FinalState:  runpkg.FinalStateFailed,
				TurnStarted: true,
				Tokens:      TokenTotals{InputTokens: int64(index+1) * 1_000_000, TotalTokens: int64(index+1) * 1_000_000},
			},
			Err: &runpkg.WorkerGitHubBudgetMonitorError{
				CredentialIdentity: "github-rest:shared-worker",
				Consumer:           telemetry.RESTConsumerSharedPool,
				Operation:          operations[index],
				Err:                errors.New("api.github.com unavailable"),
			},
		})
	}

	if len(state.GitHubMonitors) != 1 {
		t.Fatalf("monitor conditions = %#v, want one credential-scoped condition", state.GitHubMonitors)
	}
	condition := state.GitHubMonitors["github-rest:shared-worker"]
	if !condition.NextProbeAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("NextProbeAt = %s, want first outage backoff preserved", condition.NextProbeAt)
	}
	if len(state.Retry) != len(issues) || len(attempts.completions) != len(issues) {
		t.Fatalf("retries = %#v completions = %#v, want one durable same-attempt wait per worker", state.Retry, attempts.completions)
	}
	for index, issue := range issues {
		retry := state.Retry[issue.ID]
		if retry.Attempt != index+2 || !retry.DueAt.Equal(condition.NextProbeAt) || retry.GitHubCredential != condition.CredentialIdentity {
			t.Fatalf("Retry[%q] = %#v, want shared recovery condition", issue.ID, retry)
		}
		if attempts.completions[index].TerminalState != store.WorkAttemptTerminalCapacity || strings.Contains(attempts.completions[index].WorkerMetadataJSON, "completion_progress") {
			t.Fatalf("completion[%d] = %#v, want capacity without progress rewrite", index, attempts.completions[index])
		}
		if state.InstantFailures[issue.ID].Count != instantFailureThreshold-1 || state.RepeatedFailures[issue.ID].Count != repeatedFailureThreshold-1 {
			t.Fatalf("failure brakes changed for %q: instant=%#v repeated=%#v", issue.ID, state.InstantFailures[issue.ID], state.RepeatedFailures[issue.ID])
		}
	}
	if len(state.FailureBreaker.Failures["existing"]) != 1 || state.FailureBreaker.Active() {
		t.Fatalf("project failure breaker = %#v, want unchanged", state.FailureBreaker)
	}
	if state.TokenTotals.TotalTokens != 3_000_000 {
		t.Fatalf("aggregate tokens = %#v, want both attempts retained", state.TokenTotals)
	}
}

func TestRecoverDurableWorkerGitHubMonitorWaits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 22, 0, 0, 0, time.UTC)
	nextProbeAt := now.Add(3 * time.Minute)
	issues := []connector.Issue{
		{ID: "issue-restart-a", Identifier: "digitaldrywood/detent#2028", State: "In Progress"},
		{ID: "issue-restart-b", Identifier: "digitaldrywood/detent#2029", State: "In Progress"},
	}
	metadata := func(operation string, observedAt time.Time) string {
		return marshalWorkAttemptJSON(map[string]any{
			"worker_github_monitor_wait": workerGitHubMonitorWaitMetadata{
				CredentialIdentity: "github-rest:shared-worker",
				Consumer:           telemetry.RESTConsumerSharedPool,
				Operation:          operation,
				DetectedAt:         now.Add(-2 * time.Minute),
				LastObservedAt:     observedAt,
				NextProbeAt:        nextProbeAt,
				ProbeAttempts:      1,
			},
		})
	}
	attempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{
		{
			ID: 402, ProjectID: "detent", IssueID: issues[1].ID, Identifier: issues[1].Identifier, Lane: issues[1].State,
			AttemptNumber: 3, Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-time.Minute),
			TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubMonitorErrorClass,
			ErrorMessage: "periodic probe unavailable", WorkerMetadataJSON: metadata("periodic_probe", now.Add(-time.Minute)),
		},
		{
			ID: 401, ProjectID: "detent", IssueID: issues[0].ID, Identifier: issues[0].Identifier, Lane: issues[0].State,
			AttemptNumber: 2, Status: store.WorkAttemptStatusTerminal, CompletedAt: now.Add(-90 * time.Second),
			TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubMonitorErrorClass,
			ErrorMessage: "launch probe unavailable", WorkerMetadataJSON: metadata("launch_probe", now.Add(-90*time.Second)),
		},
	}}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done", "Cancelled"}, MaxConcurrentAgents: 2,
	})
	orch := &Orchestrator{
		cfg: cfg, connector: &rateLimitConnector{issuesByID: issues}, workAttempts: attempts,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
	}
	state := newState(cfg)

	orch.recoverDurableWorkAttempts(t.Context(), &state, now)

	if len(state.GitHubMonitors) != 1 || len(state.Retry) != 2 {
		t.Fatalf("conditions = %#v retries = %#v, want one condition and both issue waits", state.GitHubMonitors, state.Retry)
	}
	condition := state.GitHubMonitors["github-rest:shared-worker"]
	if !condition.NextProbeAt.Equal(nextProbeAt) || condition.ProbeAttempts != 1 || condition.Operation != "periodic_probe" {
		t.Fatalf("restored condition = %#v, want latest observation and bounded probe state", condition)
	}
	for _, issue := range issues {
		retry := state.Retry[issue.ID]
		if !retry.GitHubMonitor || retry.GitHubCredential != condition.CredentialIdentity || !retry.DueAt.Equal(nextProbeAt) {
			t.Fatalf("Retry[%q] = %#v, want restored credential wait", issue.ID, retry)
		}
	}
	if terminalAttemptRetryableFailure(telemetryWorkAttempt(attempts.recent[0], now)) {
		t.Fatal("durable worker GitHub monitor wait treated as generic terminal retry")
	}
}

func TestWorkerGitHubMonitorAllowsOneCanaryAndRecoversCredential(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 23, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"},
		TerminalStates: []string{"Done", "Cancelled"}, MaxConcurrentAgents: 2,
	})
	issues := []connector.Issue{
		dispatchTestIssue("issue-canary-a", "In Progress"),
		dispatchTestIssue("issue-canary-b", "In Progress"),
	}
	state := newState(cfg)
	credential := "github-rest:shared-worker"
	state.GitHubMonitors[credential] = GitHubMonitor{
		ProjectID: "detent", CredentialIdentity: credential, Consumer: telemetry.RESTConsumerSharedPool,
		Operation: "periodic_probe", DetectedAt: now.Add(-5 * time.Minute), LastObservedAt: now.Add(-5 * time.Minute), NextProbeAt: now,
	}
	for index, issue := range issues {
		state.Retry[issue.ID] = Retry{
			Issue: issue, Attempt: index + 2, DueAt: now,
			GitHubMonitor: true, GitHubCredential: credential,
		}
	}
	plan := newDispatchPlanner(cfg).plan(&state, issues, now, dispatchPlanHooks{})

	if len(plan.Dispatches) != 1 {
		t.Fatalf("dispatches = %#v, want exactly one credential canary", plan.Dispatches)
	}
	owner := plan.Dispatches[0].IssueID
	condition := state.GitHubMonitors[credential]
	if condition.ProbeIssueID != owner || condition.ProbeAttempts != 1 || condition.LastProbeResult != "in_progress" {
		t.Fatalf("condition = %#v, want single canary owner", condition)
	}
	running := state.Running[owner]
	if running.GitHubCredential != credential {
		t.Fatalf("Running[%q] = %#v, want canary credential ownership", owner, running)
	}
	recoveredAt := now.Add(time.Second)
	orch := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return recoveredAt }}
	orch.handleRunUpdate(&state, runUpdate{
		issueID: owner,
		usage: runpkg.UsageUpdate{
			LastEventAt: recoveredAt,
			RateLimits: &telemetry.RateLimits{GitHubRESTBudgets: []telemetry.RESTBudget{{
				Consumer: telemetry.RESTConsumerSharedPool, CredentialIdentity: credential, Remaining: 4200,
			}}},
		},
	})

	if _, ok := state.GitHubMonitors[credential]; ok {
		t.Fatalf("condition %q remained after successful canary observation", credential)
	}
	for issueID, retry := range state.Retry {
		if retry.GitHubMonitor || !retry.DueAt.Equal(recoveredAt) {
			t.Fatalf("Retry[%q] = %#v, want released immediately after recovery", issueID, retry)
		}
	}
}

func TestWorkerGitHubMonitorDeferredCanaryUsesBoundedBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 23, 10, 0, 0, time.UTC)
	credential := "github-rest:shared-worker"
	issueID := "issue-canary"
	state := newState(normalizeConfig(Config{}))
	state.GitHubMonitors[credential] = GitHubMonitor{
		CredentialIdentity: credential,
		NextProbeAt:        now,
	}
	retry := Retry{GitHubMonitor: true, GitHubCredential: credential}

	if _, reserved := reserveWorkerGitHubMonitorProbe(&state, issueID, retry, now); !reserved {
		t.Fatal("monitor canary was not reserved")
	}
	condition := state.GitHubMonitors[credential]
	if !condition.NextProbeAt.IsZero() || condition.ProbeAttempts != 1 {
		t.Fatalf("reserved condition = %#v, want in-flight probe without a due time", condition)
	}
	releasedAt := now.Add(time.Second)
	releaseWorkerGitHubMonitorProbe(&state, issueID, "deferred", "worker unavailable", releasedAt)
	condition = state.GitHubMonitors[credential]
	wantNextProbeAt := releasedAt.Add(backendCapacityProbeDelayForAttempt(1))
	if condition.ProbeIssueID != "" || !condition.NextProbeAt.Equal(wantNextProbeAt) || condition.LastProbeResult != "deferred" {
		t.Fatalf("released condition = %#v, want bounded canary backoff until %s", condition, wantNextProbeAt)
	}
}

func TestWorkerGitHubMonitorRawSentinelUsesRecoverableFallbackScope(t *testing.T) {
	t.Parallel()

	failure, ok := workerGitHubMonitorFailureFromCompletion(runpkg.Completion{
		Err: errors.Join(errors.New("probe failed"), runpkg.ErrWorkerGitHubBudgetMonitor),
	}, nil)
	if !ok {
		t.Fatal("raw worker GitHub monitor sentinel was not intercepted")
	}
	if failure.CredentialIdentity != workerGitHubMonitorUnknownCredential || failure.Consumer != telemetry.RESTConsumerWorker || failure.Operation != "unknown" {
		t.Fatalf("fallback failure = %#v, want recoverable unclassified scope", failure)
	}
}

func TestWorkerGitHubMonitorCanaryRecoversUnclassifiedCredential(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 23, 15, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, MaxConcurrentAgents: 1})
	state := newState(cfg)
	state.GitHubMonitors[workerGitHubMonitorUnknownCredential] = GitHubMonitor{
		ProjectID: "detent", CredentialIdentity: workerGitHubMonitorUnknownCredential,
		Consumer: telemetry.RESTConsumerWorker, DetectedAt: now.Add(-time.Minute), LastObservedAt: now.Add(-time.Minute), NextProbeAt: now,
	}
	issue := dispatchTestIssue("issue-unclassified", "In Progress")
	state.Running[issue.ID] = Running{Issue: issue, GitHubCredential: workerGitHubMonitorUnknownCredential}
	orch := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orch.handleRunUpdate(&state, runUpdate{
		issueID: issue.ID,
		usage: runpkg.UsageUpdate{
			LastEventAt: now,
			RateLimits: &telemetry.RateLimits{GitHubRESTBudgets: []telemetry.RESTBudget{{
				Consumer: telemetry.RESTConsumerWorker, CredentialIdentity: "github-rest:classified", Remaining: 4200,
			}}},
		},
	})

	if _, ok := state.GitHubMonitors[workerGitHubMonitorUnknownCredential]; ok {
		t.Fatal("unclassified credential condition remained after its canary published a scoped observation")
	}
}

func TestWorkerGitHubMonitorCompletionSurvivesLaneRefreshFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 23, 30, 0, 0, time.UTC)
	issue := connector.Issue{ID: "issue-fence", Identifier: "digitaldrywood/detent#2028", State: "In Progress"}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"},
		TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1,
	})
	attempts := &implementProgressAttemptStore{}
	orch := &Orchestrator{
		cfg: cfg, connector: &rateLimitConnector{fetchByIDErr: errors.New("tracker path unavailable")}, workAttempts: attempts,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 2, WorkAttemptID: 500, Generation: 9, StartedAt: now.Add(-time.Minute)}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID, CompletedAt: now,
		Request: runpkg.RunRequest{Issue: issue, Attempt: 2, WorkAttemptID: 500, Generation: 9},
		Err: &runpkg.WorkerGitHubBudgetMonitorError{
			CredentialIdentity: "github-rest:worker", Consumer: telemetry.RESTConsumerWorker,
			Operation: "periodic_probe", Err: errors.New("DNS unavailable"),
		},
	})

	if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass != workerGitHubMonitorErrorClass {
		t.Fatalf("completions = %#v, want monitor deferral despite completion-fence outage", attempts.completions)
	}
	if len(state.GitHubMonitors) != 1 || len(state.Retry) != 1 {
		t.Fatalf("conditions = %#v retries = %#v, want durable monitor wait", state.GitHubMonitors, state.Retry)
	}
}

func TestWorkerGitHubMonitorWaitMetadataValidation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 28, 23, 45, 0, 0, time.UTC)
	valid := workerGitHubMonitorWaitMetadata{
		CredentialIdentity: "github-rest:worker", Consumer: telemetry.RESTConsumerWorker, Operation: "periodic_probe",
		DetectedAt: now.Add(-time.Minute), LastObservedAt: now.Add(-time.Minute), NextProbeAt: now,
	}
	attempt := func(wait workerGitHubMonitorWaitMetadata) store.WorkAttempt {
		return store.WorkAttempt{
			TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubMonitorErrorClass,
			WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"worker_github_monitor_wait": wait}),
		}
	}
	tests := []struct {
		name    string
		attempt store.WorkAttempt
		want    bool
	}{
		{name: "valid", attempt: attempt(valid), want: true},
		{name: "malformed", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: workerGitHubMonitorErrorClass, WorkerMetadataJSON: `{`}},
		{name: "ordinary failure", attempt: store.WorkAttempt{TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: workerGitHubMonitorErrorClass, WorkerMetadataJSON: attempt(valid).WorkerMetadataJSON}},
		{name: "missing credential", attempt: attempt(workerGitHubMonitorWaitMetadata{Consumer: valid.Consumer, Operation: valid.Operation, DetectedAt: valid.DetectedAt, LastObservedAt: valid.LastObservedAt, NextProbeAt: valid.NextProbeAt})},
		{name: "missing probe time", attempt: attempt(workerGitHubMonitorWaitMetadata{CredentialIdentity: valid.CredentialIdentity, Consumer: valid.Consumer, Operation: valid.Operation, DetectedAt: valid.DetectedAt, LastObservedAt: valid.LastObservedAt})},
		{name: "negative attempts", attempt: attempt(workerGitHubMonitorWaitMetadata{CredentialIdentity: valid.CredentialIdentity, Consumer: valid.Consumer, Operation: valid.Operation, DetectedAt: valid.DetectedAt, LastObservedAt: valid.LastObservedAt, NextProbeAt: valid.NextProbeAt, ProbeAttempts: -1})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := workerGitHubMonitorWaitMetadataFromAttempt(tt.attempt)
			if got != tt.want {
				t.Fatalf("workerGitHubMonitorWaitMetadataFromAttempt() valid = %v, want %v", got, tt.want)
			}
		})
	}
}

type staticWorkerGitHubMonitorBackend struct {
	result runpkg.RunResult
	err    error
}

func (b staticWorkerGitHubMonitorBackend) Run(context.Context, runpkg.RunRequest) (runpkg.RunResult, error) {
	return b.result, b.err
}
