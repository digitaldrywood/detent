package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestApprovalDeniedDeliverableUsesInstanceForgeWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		ForgeHost:           "github.com",
		PollInterval:        time.Minute,
		ActiveStates:        []string{"In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 1,
			Window:         time.Hour,
			Cooldown:       time.Hour,
		},
	})
	tracker := &backendCapacityTestConnector{}
	attempts := &recordingWorkAttemptStore{}
	orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := dispatchTestIssue("issue-forge-wait", "In Progress")
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1871"
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 4, WorkAttemptID: 42, StartedAt: now.Add(-time.Minute)}
	state.InstantFailures[issue.ID] = InstantFailure{Issue: issue, Count: instantFailureThreshold - 1}
	state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: repeatedFailureThreshold - 1}
	state.FailureBreaker.Failures["existing"] = []ProjectFailure{{IssueID: "other", At: now.Add(-time.Minute)}}
	deliverableErr := &runpkg.DeliverableCommandError{
		OperationClass: "pull_request",
		Operation:      "codex_apps/github.create_pull_request",
		Arguments:      `{"head":"detent/1871"}`,
		Status:         "failed",
		Message:        "tool approval declined",
		ApprovalDenied: true,
	}
	err := forgeavailability.NewError(
		forgeavailability.Scope{Host: "github.com", Operation: deliverableErr.Operation},
		forgeavailability.ClassTransport,
		&runpkg.DeliverableRecoveryError{Branch: "detent/1871", Err: deliverableErr},
	)

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Request:      runpkg.RunRequest{Issue: issue, Attempt: 4},
		Result:       runpkg.RunResult{PullRequestHeadPushed: true, WorkspaceBranch: "detent/1871"},
		Err:          err,
		CompletedAt:  now,
		RetryAttempt: 5,
		RetryDelay:   time.Minute,
	})

	if got := state.InstantFailures[issue.ID].Count; got != instantFailureThreshold-1 {
		t.Fatalf("instant failure count = %d, want unchanged", got)
	}
	if got := state.RepeatedFailures[issue.ID].Count; got != repeatedFailureThreshold-1 {
		t.Fatalf("repeated failure count = %d, want unchanged", got)
	}
	if got := len(state.FailureBreaker.Failures["existing"]); got != 1 || state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want existing evidence unchanged and inactive", state.FailureBreaker)
	}
	if _, blocked := state.Blocked[issue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after forge wait", issue.ID)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 4 || !retry.ForgeUnavailable || retry.ForgeRetry == nil {
		t.Fatalf("Retry[%q] = %#v, want same-attempt typed forge wait", issue.ID, retry)
	}
	if !retry.ForgeRetry.WorkProductPushed || retry.ForgeRetry.Branch != "detent/1871" {
		t.Fatalf("ForgeRetry = %#v, want pushed branch preserved", retry.ForgeRetry)
	}
	if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
		t.Fatalf("tracker mutations = states %#v comments %#v, want none", tracker.updates, tracker.comments)
	}
	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity {
		t.Fatalf("work attempt completions = %#v, want durable capacity wait", attempts.completions)
	}
	var persisted struct {
		ForgeWait forgeWaitMetadata `json:"forge_wait"`
	}
	if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &persisted); err != nil {
		t.Fatalf("decode forge wait metadata: %v", err)
	}
	if persisted.ForgeWait.Host != "github.com" || persisted.ForgeWait.Branch != "detent/1871" || !persisted.ForgeWait.WorkProductPushed || persisted.ForgeWait.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
		t.Fatalf("persisted forge wait = %#v, want scoped pushed branch", persisted.ForgeWait)
	}

	completion := attempts.completions[0]
	restartedAttempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{{
		ID: 42, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL,
		WorkerHost: "worker-a", Lane: issue.State, AttemptNumber: 4, Status: store.WorkAttemptStatusTerminal,
		CompletedAt: completion.CompletedAt, TerminalState: completion.TerminalState, ErrorClass: completion.ErrorClass,
		ErrorMessage: completion.ErrorMessage, WorkerMetadataJSON: completion.WorkerMetadataJSON,
	}}}
	restartedOrch := Orchestrator{
		cfg:          cfg,
		connector:    &forgeWaitRecoveryConnector{issues: []connector.Issue{issue}},
		workAttempts: restartedAttempts,
		now:          func() time.Time { return now.Add(time.Second) },
	}
	restartedState := newState(cfg)
	restartedOrch.recoverDurableWorkAttempts(context.Background(), &restartedState, now.Add(time.Second))
	condition, ok := restartedState.ForgeUnavailable["github.com"]
	if !ok || condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
		t.Fatalf("restarted forge condition = %#v, want durable credential wait", restartedState.ForgeUnavailable)
	}
	restartedRetry, ok := restartedState.Retry[issue.ID]
	if !ok || !restartedRetry.ForgeUnavailable || restartedRetry.ForgeRetry == nil || restartedRetry.ForgeRetry.Branch != "detent/1871" || !restartedRetry.ForgeRetry.WorkProductPushed {
		t.Fatalf("restarted Retry[%q] = %#v, want restored same-attempt pushed branch", issue.ID, restartedRetry)
	}
}

func TestRecoverDurableForgeAvailabilityWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	waiting := dispatchTestIssue("durable-forge-wait", "In Progress")
	waiting.URL = "https://github.com/digitaldrywood/detent/issues/1871"
	completed := dispatchTestIssue("completed-after-wait", "In Progress")
	metadata := func(branch string) string {
		return marshalWorkAttemptJSON(map[string]any{
			"forge_wait": forgeWaitMetadata{
				Host:              "github.com",
				Operation:         "git push",
				Branch:            branch,
				WorkProductPushed: false,
				ErrorClass:        forgeavailability.ClassTransport,
				DetectedAt:        now.Add(-2 * time.Minute),
				NextProbeAt:       now.Add(time.Minute),
			},
		})
	}
	attempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{
		{
			ID: 3, ProjectID: "detent", IssueID: waiting.ID, Identifier: waiting.Identifier, IssueURL: waiting.URL,
			WorkerHost: "worker-a", Lane: waiting.State, AttemptNumber: 4, Status: store.WorkAttemptStatusTerminal,
			CompletedAt: now.Add(-time.Minute), TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: forgeUnavailableErrorClass,
			ErrorMessage: "forge github.com unavailable", WorkerMetadataJSON: metadata("detent/1871"),
		},
		{
			ID: 2, ProjectID: "detent", IssueID: completed.ID, Identifier: completed.Identifier,
			Lane: completed.State, AttemptNumber: 2, Status: store.WorkAttemptStatusTerminal,
			CompletedAt: now.Add(-30 * time.Second), TerminalState: store.WorkAttemptTerminalSuccess, WorkerMetadataJSON: `{}`,
		},
		{
			ID: 1, ProjectID: "detent", IssueID: completed.ID, Identifier: completed.Identifier,
			Lane: completed.State, AttemptNumber: 1, Status: store.WorkAttemptStatusTerminal,
			CompletedAt: now.Add(-2 * time.Minute), TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: forgeUnavailableErrorClass,
			WorkerMetadataJSON: metadata("detent/old"),
		},
	}}
	connectorBackend := &forgeWaitRecoveryConnector{issues: []connector.Issue{waiting, completed}}
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		ForgeHost:           "github.com",
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	})
	orch := Orchestrator{cfg: cfg, connector: connectorBackend, workAttempts: attempts, now: func() time.Time { return now }}
	state := newState(cfg)

	orch.recoverDurableWorkAttempts(context.Background(), &state, now)

	condition, ok := state.ForgeUnavailable["github.com"]
	if !ok || !condition.NextProbeAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("ForgeUnavailable = %#v, want restored host condition", state.ForgeUnavailable)
	}
	retry, ok := state.Retry[waiting.ID]
	if !ok || !retry.ForgeUnavailable || retry.Attempt != 4 || retry.ForgeRetry == nil || retry.ForgeRetry.Branch != "detent/1871" {
		t.Fatalf("Retry[%q] = %#v, want restored same-attempt write canary", waiting.ID, retry)
	}
	if _, ok := state.Retry[completed.ID]; ok {
		t.Fatalf("Retry[%q] restored despite newer successful attempt", completed.ID)
	}
	if terminalAttemptRetryableFailure(telemetryWorkAttempt(attempts.recent[0], now)) {
		t.Fatal("durable forge wait treated as a generic terminal retry")
	}
}

func TestRecoverDurableWorkspaceGitReadWait(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 15, 36, 0, 0, time.UTC)
	issue := dispatchTestIssue("workspace-git-read-wait", "In Progress")
	metadata := forgeWaitMetadata{
		Host: "github.com", Operation: "git ls-remote", ErrorClass: forgeavailability.ClassTransport,
		DetectedAt: now, NextProbeAt: now.Add(time.Minute),
	}
	attempts := &recordingWorkAttemptStore{recent: []store.WorkAttempt{{
		ID: 1, ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier,
		Lane: issue.State, AttemptNumber: 3, Status: store.WorkAttemptStatusTerminal,
		CompletedAt: now, TerminalState: store.WorkAttemptTerminalCapacity,
		ErrorClass: forgeUnavailableErrorClass, ErrorMessage: "git@github.com: Permission denied (publickey)",
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"forge_wait": metadata}),
	}}}
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", ActiveStates: []string{"In Progress"}})
	orch := Orchestrator{cfg: cfg, connector: &forgeWaitRecoveryConnector{issues: []connector.Issue{issue}}, workAttempts: attempts, now: func() time.Time { return now }}
	state := newState(cfg)
	orch.recoverDurableWorkAttempts(t.Context(), &state, now)

	condition, ok := state.ForgeUnavailable["github.com"]
	if !ok || condition.Operation != "git ls-remote" || !condition.NextProbeAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("restored forge condition = %#v, want ls-remote wait", state.ForgeUnavailable)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || !retry.ForgeUnavailable || retry.Attempt != 3 || retry.ForgeRetry == nil || retry.ForgeRetry.Operation != "git ls-remote" || !retry.DueAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("restored retry = %#v, want same-attempt ls-remote backoff", retry)
	}
}

func TestForgeWaitMetadataRequiresStructuredAvailabilityEvidence(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	valid := forgeWaitMetadata{
		Host: "github.com", Operation: "git push", ErrorClass: forgeavailability.ClassServer,
		DetectedAt: now.Add(-time.Minute), NextProbeAt: now,
	}
	tests := []struct {
		name    string
		attempt store.WorkAttempt
		want    bool
	}{
		{name: "valid", attempt: forgeWaitAttempt(valid), want: true},
		{name: "workspace ls-remote read", attempt: forgeWaitAttempt(forgeWaitMetadata{Host: "github.com", Operation: "git ls-remote", ErrorClass: forgeavailability.ClassTransport}), want: true},
		{name: "workspace fetch read", attempt: forgeWaitAttempt(forgeWaitMetadata{Host: "github.com", Operation: "git fetch", ErrorClass: forgeavailability.ClassTransport}), want: true},
		{name: "malformed JSON", attempt: store.WorkAttempt{Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity, ErrorClass: forgeUnavailableErrorClass, WorkerMetadataJSON: `{`}},
		{name: "missing host", attempt: forgeWaitAttempt(forgeWaitMetadata{Operation: "git push", ErrorClass: forgeavailability.ClassServer})},
		{name: "tracker read operation", attempt: forgeWaitAttempt(forgeWaitMetadata{Host: "github.com", Operation: "search issues", ErrorClass: forgeavailability.ClassServer})},
		{name: "unknown class", attempt: forgeWaitAttempt(forgeWaitMetadata{Host: "github.com", Operation: "git push", ErrorClass: "auth"})},
		{name: "ordinary failure", attempt: store.WorkAttempt{Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: forgeUnavailableErrorClass, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"forge_wait": valid})}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := forgeWaitMetadataFromAttempt(tt.attempt)
			if got != tt.want {
				t.Fatalf("forgeWaitMetadataFromAttempt() valid = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecoverForgeWaitPreservesDeferralWhenTrackerValidationFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	issue := dispatchTestIssue("tracker-validation-failed", "In Progress")
	attempt := forgeWaitAttempt(forgeWaitMetadata{
		Host: "github.com", Operation: "git push", Branch: "detent/1871",
		ErrorClass: forgeavailability.ClassServer, DetectedAt: now.Add(-time.Minute), NextProbeAt: now,
	})
	attempt.ID = 1
	attempt.IssueID = issue.ID
	attempt.Identifier = issue.Identifier
	attempt.Lane = issue.State
	attempt.AttemptNumber = 3
	attempt.CompletedAt = now.Add(-time.Minute)
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 1,
	})
	orch := Orchestrator{cfg: cfg, connector: &forgeWaitRecoveryConnector{err: errors.New("tracker unavailable")}}
	state := newState(cfg)

	orch.recoverForgeAvailabilityWaits(context.Background(), &state, []store.WorkAttempt{attempt}, now)

	if _, ok := state.ForgeUnavailable["github.com"]; !ok {
		t.Fatal("forge condition not restored after tracker validation failure")
	}
	if retry, ok := state.Retry[issue.ID]; !ok || !retry.ForgeUnavailable {
		t.Fatalf("Retry[%q] = %#v, want continued durable deferral", issue.ID, retry)
	}
}

func forgeWaitAttempt(metadata forgeWaitMetadata) store.WorkAttempt {
	return store.WorkAttempt{
		Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity,
		ErrorClass: forgeUnavailableErrorClass, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"forge_wait": metadata}),
	}
}

func TestForgeAvailabilityDispatchIsScopedToNextWriteAndHost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		ForgeHost:           "github.com",
		ActiveStates:        []string{"In Progress", "Merging"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 2,
	})
	state := newState(cfg)
	state.ForgeUnavailable["github.com"] = ForgeCondition{Host: "github.com", NextProbeAt: now.Add(time.Minute)}
	planner := newDispatchPlanner(cfg)

	implementation := dispatchTestIssue("implementation", "In Progress")
	implementation.URL = "https://github.com/digitaldrywood/detent/issues/1871"
	if decision := planner.dispatchableIssueDecision(implementation, &state, false, now, ""); !decision.dispatchable {
		t.Fatalf("implementation decision = %#v, want non-write work to proceed", decision)
	}

	merge := dispatchTestIssueWithPullRequest("merge", "Merging", "OPEN")
	if decision := planner.dispatchableIssueDecision(merge, &state, false, now, ""); decision.dispatchable || decision.reason != dispatchSkipForgeUnavailable {
		t.Fatalf("merge decision = %#v, want forge wait", decision)
	}

	otherHostRetry := Retry{
		Issue:            implementation,
		Attempt:          2,
		DueAt:            now,
		ForgeUnavailable: true,
		ForgeHost:        "gitlab.example.com",
	}
	state.Retry[implementation.ID] = otherHostRetry
	if _, allowed, reason := planner.retryAction(&state, implementation, otherHostRetry, now); !allowed || reason != "" {
		t.Fatalf("other-host retry = allowed %v reason %q, want independent host", allowed, reason)
	}
}

func TestWorkerGitHubCredentialAvailabilityBlocksProjectAcrossHosts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		conditionHost string
		issueURL      string
	}{
		{name: "GitHub credential blocks Linear issue", conditionHost: "github.com", issueURL: "https://linear.app/detent/issue/DET-2548"},
		{name: "API host credential blocks GitHub issue", conditionHost: "api.github.com", issueURL: "https://github.com/digitaldrywood/detent/issues/2548"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com"})
			state := newState(cfg)
			state.ForgeUnavailable[tt.conditionHost] = ForgeCondition{
				Host:        tt.conditionHost,
				ErrorClass:  forgeavailability.ClassWorkerGitHubCredentialUnavailable,
				NextProbeAt: now.Add(time.Minute),
			}
			issue := dispatchTestIssue("cross-host", "In Progress")
			issue.URL = tt.issueURL

			if !forgeAvailabilityBlocks(&state, issue, Retry{}, cfg.ForgeHost, now) {
				t.Fatal("forgeAvailabilityBlocks() = false, want project credential pause")
			}

			retry := Retry{Issue: issue, ForgeUnavailable: true, ForgeHost: tt.conditionHost}
			condition := state.ForgeUnavailable[tt.conditionHost]
			condition.NextProbeAt = now
			state.ForgeUnavailable[tt.conditionHost] = condition
			if forgeAvailabilityBlocks(&state, issue, retry, cfg.ForgeHost, now) {
				t.Fatal("forgeAvailabilityBlocks() = true for due credential canary, want retry allowed")
			}
		})
	}
}

func TestForgeAvailabilityProbeClearsOnlyForgeCondition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", MaxConcurrentAgents: 1})
	orch := Orchestrator{cfg: cfg, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := dispatchTestIssue("forge-probe", "In Progress")
	state.TrackerUnavailable = &TrackerCondition{Connector: "github", DetectedAt: now.Add(-time.Minute)}
	state.ForgeUnavailable["github.com"] = ForgeCondition{
		Host:         "github.com",
		ProbeIssueID: issue.ID,
		DetectedAt:   now.Add(-time.Minute),
	}
	state.Retry[issue.ID] = Retry{Issue: issue, DueAt: now.Add(time.Hour), ForgeUnavailable: true, ForgeHost: "github.com"}

	orch.finishForgeAvailabilityProbe(&state, runpkg.Completion{
		IssueID:     issue.ID,
		Result:      runpkg.RunResult{ForgeWriteCompleted: true},
		CompletedAt: now,
	}, Running{Issue: issue, ForgeProbeHost: "github.com"})

	if len(state.ForgeUnavailable) != 0 {
		t.Fatalf("ForgeUnavailable = %#v, want cleared", state.ForgeUnavailable)
	}
	if state.TrackerUnavailable == nil {
		t.Fatal("TrackerUnavailable cleared with independent forge condition")
	}
	retry := state.Retry[issue.ID]
	if retry.ForgeUnavailable || !retry.DueAt.Equal(now) {
		t.Fatalf("Retry[%q] = %#v, want released", issue.ID, retry)
	}
}

func TestRejectedPushRemainsWorkFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		Project:             scheduler.ProjectCandidate{ID: "detent"},
		ForgeHost:           "github.com",
		ActiveStates:        []string{"In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	})
	orch := Orchestrator{cfg: cfg, connector: &backendCapacityTestConnector{}, now: func() time.Time { return now }}
	state := newState(cfg)
	issue := dispatchTestIssue("rejected-push", "In Progress")
	issue.URL = "https://github.com/digitaldrywood/detent/issues/1871"
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, StartedAt: now.Add(-time.Second)}
	err := &runpkg.DeliverableCommandError{
		OperationClass: "push",
		Operation:      "git push",
		Status:         "failed",
		Message:        "[rejected] feature -> feature (non-fast-forward)",
	}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Request:      runpkg.RunRequest{Issue: issue, Attempt: 1},
		Result:       runpkg.RunResult{TurnStarted: true},
		Err:          err,
		CompletedAt:  now,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	if len(state.ForgeUnavailable) != 0 {
		t.Fatalf("ForgeUnavailable = %#v, want no outage for rejected push", state.ForgeUnavailable)
	}
	if retry := state.Retry[issue.ID]; retry.ForgeUnavailable {
		t.Fatalf("Retry[%q] = %#v, want ordinary work failure", issue.ID, retry)
	}
	if state.RepeatedFailures[issue.ID].Count != 1 || state.InstantFailures[issue.ID].Count != 1 {
		t.Fatalf("failure counts = repeated %#v instant %#v, want one real strike", state.RepeatedFailures, state.InstantFailures)
	}
}

func TestForgeWriteRejectionClearsProbeButStillReturnsFailure(t *testing.T) {
	t.Parallel()

	err := &runpkg.DeliverableCommandError{OperationClass: "push", Operation: "git push", Message: "HTTP 403: forbidden"}
	if !forgeWriteReachedRemote(err) {
		t.Fatal("forgeWriteReachedRemote() = false, want reachable rejection to clear a canary")
	}
	if _, unavailable := forgeavailability.As(err); unavailable || errors.Is(err, forgeavailability.ErrUnavailable) {
		t.Fatalf("error = %v, want ordinary failure", err)
	}
}

func TestCredentialForgeProbeRequiresSuccessfulWrite(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", MaxConcurrentAgents: 1})
	orch := Orchestrator{cfg: cfg, now: func() time.Time { return now }}
	issue := dispatchTestIssue("credential-probe", "In Progress")
	state := newState(cfg)
	state.ForgeUnavailable["github.com"] = ForgeCondition{
		Host:         "github.com",
		ErrorClass:   forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		ProbeIssueID: issue.ID,
		DetectedAt:   now.Add(-time.Minute),
	}
	state.Retry[issue.ID] = Retry{Issue: issue, ForgeUnavailable: true, ForgeHost: "github.com"}

	orch.finishForgeAvailabilityProbe(&state, runpkg.Completion{
		IssueID: issue.ID,
		Err: &runpkg.DeliverableCommandError{
			OperationClass: "push",
			Operation:      "git push",
			Message:        "HTTP 403: forbidden",
		},
		CompletedAt: now,
	}, Running{Issue: issue, ForgeProbeHost: "github.com"})

	condition, ok := state.ForgeUnavailable["github.com"]
	if !ok {
		t.Fatal("credential forge condition cleared after rejected write")
	}
	if condition.ProbeIssueID != "" || condition.NextProbeAt.IsZero() || condition.LastProbeResult != "inconclusive" {
		t.Fatalf("credential condition = %#v, want released probe scheduled for retry", condition)
	}
	if !state.Retry[issue.ID].ForgeUnavailable {
		t.Fatalf("Retry[%q] = %#v, want credential wait retained", issue.ID, state.Retry[issue.ID])
	}
}

func TestClassifyWorkerGitHubCredentialUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantClass string
	}{
		{
			name: "credential signature",
			err: &runpkg.DeliverableCommandError{
				OperationClass: "pull_request", Operation: "gh pr create", Message: "run gh auth login",
			},
			wantClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		},
		{
			name: "typed connector approval denial",
			err: forgeavailability.NewError(
				forgeavailability.Scope{Host: "github.com", Operation: "codex_apps/github.create_pull_request"},
				forgeavailability.ClassTransport,
				&runpkg.DeliverableCommandError{
					OperationClass: "pull_request", Operation: "codex_apps/github.create_pull_request",
					Message: "tool approval declined", ApprovalDenied: true,
				},
			),
			wantClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		},
		{
			name: "final message connector write question",
			err: &runpkg.DeliverableCommandError{
				OperationClass: "pull_request", Operation: "create_pull_request",
				Message: "Could you enable GitHub connector write access or open the PR manually?", ApprovalDenied: true,
			},
			wantClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		},
		{
			name: "final message manual PR support request",
			err: &runpkg.DeliverableCommandError{
				OperationClass: "pull_request", Operation: "create_pull_request",
				Message: "Can I open the PR manually?", ApprovalDenied: true,
			},
			wantClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		},
		{
			name: "unrelated executable failure",
			err: &runpkg.DeliverableCommandError{
				OperationClass: "pull_request", Operation: "gh pr create", Message: "exec: gh: executable file not found",
			},
		},
	}

	o := Orchestrator{cfg: normalizeConfig(Config{ForgeHost: "github.com"})}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := o.classifyWorkerGitHubCredentialUnavailable(tt.err, Running{})
			availabilityErr, classified := forgeavailability.As(got)
			if tt.wantClass == "" {
				if classified {
					t.Fatalf("classified error = %v, want ordinary deliverable failure", got)
				}
				return
			}
			if !classified || availabilityErr.Class != tt.wantClass {
				t.Fatalf("classified error = %#v, want class %q", availabilityErr, tt.wantClass)
			}
		})
	}
}

func TestWorkerGitHubCredentialUnavailablePausesProjectWithoutParkingIssue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
	}{
		{name: "GitHub CLI credential unavailable", message: "To get started with GitHub CLI, run: gh auth login; alternatively populate GH_TOKEN"},
		{name: "connector write approval denied", message: "MCP tool call requires approval, but approval policy is never"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
			tracker := &dependencyAutoUnblockConnector{}
			cfg := normalizeConfig(Config{
				Project:             scheduler.ProjectCandidate{ID: "detent"},
				ForgeHost:           "github.com",
				ActiveStates:        []string{"Todo", "In Progress", "Rework"},
				ObservedStates:      []string{"Blocked"},
				TerminalStates:      []string{"Done", "Cancelled"},
				MaxConcurrentAgents: 2,
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, now: func() time.Time { return now }}
			state := newState(cfg)
			issue := connector.Issue{
				ID:         "issue-missing-credential",
				Identifier: "digitaldrywood/client-portals#132",
				URL:        "https://github.com/digitaldrywood/client-portals/issues/132",
				State:      "In Progress",
			}
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}

			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID,
				Request: runpkg.RunRequest{Issue: issue, Attempt: 1, Mode: runpkg.RunModeImplement},
				Result:  runpkg.RunResult{FinalState: runpkg.FinalStateFailed, TurnStarted: true},
				Err: &runpkg.DeliverableCommandError{
					OperationClass: "pull_request",
					Operation:      "codex_apps/github.create_pull_request",
					Status:         "failed",
					Message:        tt.message,
				},
				CompletedAt:  now,
				Retryable:    true,
				RetryAttempt: 2,
				RetryDelay:   time.Minute,
			})

			if len(state.Blocked) != 0 || len(tracker.updates) != 0 {
				t.Fatalf("blocked = %#v updates = %#v, want unchanged issue lane", state.Blocked, tracker.updates)
			}
			condition, ok := state.ForgeUnavailable["github.com"]
			if !ok || condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
				t.Fatalf("ForgeUnavailable = %#v, want named worker credential pause", state.ForgeUnavailable)
			}
			retry, ok := state.Retry[issue.ID]
			if !ok || !retry.ForgeUnavailable || retry.Issue.State != issue.State {
				t.Fatalf("Retry[%q] = %#v, want same-lane write canary", issue.ID, retry)
			}
			other := dispatchTestIssue("issue-other", "In Progress")
			other.URL = "https://github.com/digitaldrywood/client-portals/issues/133"
			if decision := newDispatchPlanner(cfg).dispatchableIssueDecision(other, &state, false, now, ""); decision.dispatchable || decision.reason != dispatchSkipForgeUnavailable {
				t.Fatalf("project dispatch decision = %#v, want credential pause", decision)
			}

			condition.ProbeIssueID = issue.ID
			state.ForgeUnavailable["github.com"] = condition
			state.Running[issue.ID] = Running{Issue: issue, ForgeProbeHost: "github.com"}
			recoveredAt := now.Add(time.Minute)
			orch.finishForgeAvailabilityProbe(&state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: recoveredAt,
				Result: runpkg.RunResult{ForgeWriteCompleted: true},
			}, state.Running[issue.ID])
			if len(state.ForgeUnavailable) != 0 || state.Retry[issue.ID].ForgeUnavailable {
				t.Fatalf("credential pause did not clear after successful write: condition=%#v retry=%#v", state.ForgeUnavailable, state.Retry[issue.ID])
			}
		})
	}
}

type forgeWaitRecoveryConnector struct {
	backendCapacityTestConnector
	issues []connector.Issue
	err    error
}

func (c *forgeWaitRecoveryConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return append([]connector.Issue(nil), c.issues...), c.err
}

func TestCredentialWaitSurvivesOverlappingFailures(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{
		{forgeavailability.ClassWorkerGitHubCredentialUnavailable, forgeavailability.ClassTransport},
		{forgeavailability.ClassTransport, forgeavailability.ClassWorkerGitHubCredentialUnavailable},
	} {
		t.Run(order[0], func(t *testing.T) {
			now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com"})
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			restarted := newState(cfg)
			for index, class := range order {
				issue := dispatchTestIssue(class, "In Progress")
				condition := orch.registerForgeUnavailable(&state, forgeavailability.NewError(
					forgeavailability.Scope{Host: "github.com", Operation: "git push"}, class, errors.New(class)), Running{Issue: issue}, now.Add(time.Duration(index)*time.Second))
				orch.restoreForgeAvailabilityWait(&restarted, issue, store.WorkAttempt{CompletedAt: now}, forgeWaitMetadata{
					Host: "github.com", Operation: "git push", ErrorClass: class, DetectedAt: now,
				}, now)
				if index == 1 && condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
					t.Fatalf("overlapping failure downgraded credential pause: %#v", condition)
				}
			}
			other := dispatchTestIssue("other", "In Progress")
			other.URL = "https://linear.app/team/issue/other"
			for _, candidate := range []*State{&state, &restarted} {
				if !forgeAvailabilityBlocks(candidate, other, Retry{}, "github.com", now) {
					t.Fatal("ordinary cross-host work escaped credential pause")
				}
			}
		})
	}
}

func TestCredentialCanaryDurableRecovery(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		err            error
		writeCompleted bool
	}{
		{name: "inconclusive error", err: errors.New("worker stopped before writing")},
		{name: "inconclusive success"},
		{name: "token resolution", err: &runpkg.WorkerGitHubTokenResolutionError{}},
		{name: "budget monitor", err: &runpkg.WorkerGitHubBudgetMonitorError{}},
		{name: "successful write", writeCompleted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
			backend := openWorkAttemptRecoveryStore(t, t.Context())
			issue := dispatchTestIssue("credential-probe", "In Progress")
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", ActiveStates: []string{"In Progress"}, MaxConcurrentAgents: 1})
			orch := Orchestrator{cfg: cfg, workAttempts: backend, connector: &forgeWaitRecoveryConnector{issues: []connector.Issue{issue}}, now: func() time.Time { return now }}
			state := newState(cfg)
			for index := range 2 {
				id, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "detent", IssueID: issue.ID, WorkerType: "agent", Lane: issue.State, StartedAt: now.Add(time.Duration(index) * time.Minute)})
				if err != nil {
					t.Fatal(err)
				}
				running := Running{Issue: issue, WorkAttemptID: id, Attempt: index + 1, StartedAt: now}
				if index == 0 {
					orch.handleForgeUnavailableCompletion(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now,
						Err: forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: "git push"}, forgeavailability.ClassWorkerGitHubCredentialUnavailable, errors.New("gh auth login")),
					}, running)
					continue
				}
				running.ForgeProbeHost = "github.com"
				condition := state.ForgeUnavailable["github.com"]
				condition.ProbeIssueID = issue.ID
				state.ForgeUnavailable["github.com"] = condition
				state.Running[issue.ID] = running
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID,
					Request: runpkg.RunRequest{Issue: issue, Attempt: index + 1}, CompletedAt: now.Add(time.Minute), Err: tt.err,
					Result: runpkg.RunResult{TurnStarted: true, FinalState: FinalStateCompleted, ForgeWriteCompleted: tt.writeCompleted},
				})
			}
			for _, candidate := range []*State{&state, nil} {
				if candidate == nil {
					restarted := newState(cfg)
					orch.recoverDurableWorkAttempts(t.Context(), &restarted, now.Add(2*time.Minute))
					candidate = &restarted
				}
				_, paused := candidate.ForgeUnavailable["github.com"]
				if paused == tt.writeCompleted {
					t.Fatalf("paused = %v, write completed = %v", paused, tt.writeCompleted)
				}
				if !tt.writeCompleted && !candidate.Retry[issue.ID].ForgeUnavailable {
					t.Fatal("credential canary retry lost")
				}
			}
		})
	}
}

func TestCredentialConditionOutlivesOriginatingIssue(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Done", "Blocked", "missing"} {
		t.Run(lane, func(t *testing.T) {
			now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
			issue := dispatchTestIssue("origin", lane)
			tracker := &forgeWaitRecoveryConnector{issues: []connector.Issue{issue}}
			if lane == "missing" {
				tracker.issues = nil
			}
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}})
			orch := Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.recoverForgeAvailabilityWaits(t.Context(), &state, []store.WorkAttempt{{
				ID: 1, IssueID: issue.ID, Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalCapacity,
				CompletedAt: now, ErrorClass: forgeUnavailableErrorClass,
				WorkerMetadataJSON: `{"forge_wait":{"host":"github.com","operation":"git push","error_class":"worker_github_credential_unavailable"}}`,
			}}, now)
			if _, ok := state.ForgeUnavailable["github.com"]; !ok {
				t.Fatal("issue state erased project credential condition")
			}
			if len(state.Retry) != 0 {
				t.Fatalf("inactive issue must not be retried: %#v", state.Retry)
			}
			replacement := dispatchTestIssue("replacement", "In Progress")
			planner := newDispatchPlanner(cfg)
			action, dispatchable, reason := planner.dispatchAction(&state, replacement, now)
			if !dispatchable {
				t.Fatalf("replacement canary refused: %s", reason)
			}
			planner.markDispatched(&state, action, now)
			if state.Running[replacement.ID].ForgeProbeHost != "github.com" {
				t.Fatal("replacement did not reserve existing canary")
			}
			if !forgeAvailabilityBlocks(&state, dispatchTestIssue("other", "In Progress"), Retry{}, "github.com", now) {
				t.Fatal("multiple ordinary workers admitted as canary")
			}

		})
	}
}

func TestCredentialClearSchedulesCanaryWithoutUnpausing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	orch := Orchestrator{cfg: normalizeConfig(Config{})}
	state := newState(orch.cfg)
	issue := dispatchTestIssue("probe", "In Progress")
	state.ForgeUnavailable["github.com"] = ForgeCondition{Host: "github.com", ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, NextProbeAt: now.Add(time.Hour)}
	state.Retry[issue.ID] = Retry{Issue: issue, ForgeUnavailable: true, ForgeHost: "github.com", DueAt: now.Add(time.Hour)}
	if cleared := orch.clearForgeAvailability(&state, "github.com", now); len(cleared) != 0 {
		t.Fatalf("cleared = %#v without write proof", cleared)
	}
	condition, ok := state.ForgeUnavailable["github.com"]
	if !ok || !condition.NextProbeAt.Equal(now) || !state.Retry[issue.ID].DueAt.Equal(now) {
		t.Fatal("clear did not retain condition and schedule canary now")
	}
	if forgeAvailabilityBlocks(&state, issue, state.Retry[issue.ID], "github.com", now) {
		t.Fatal("due canary blocked")
	}
	if !forgeAvailabilityBlocks(&state, dispatchTestIssue("other", "In Progress"), Retry{}, "github.com", now) {
		t.Fatal("ordinary dispatch unpaused without write proof")
	}
}

func TestObservedLaneCredentialCanaryCompletion(t *testing.T) {
	t.Parallel()
	for _, writeCompleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "inconclusive", true: "successful write"}[writeCompleted], func(t *testing.T) {
			now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
			issue := dispatchTestIssue("probe", "Done")
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}})
			tracker := &backendCapacityTestConnector{}
			orch := Orchestrator{cfg: cfg, connector: tracker, workAttempts: &recordingWorkAttemptStore{}}
			state := newState(cfg)
			state.ForgeUnavailable["github.com"] = ForgeCondition{Host: "github.com", Operation: "git push", ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, ProbeIssueID: issue.ID}
			state.Running[issue.ID] = Running{Issue: issue, WorkAttemptID: 1, CompletionLane: "Done", ForgeProbeHost: "github.com"}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Result: runpkg.RunResult{ForgeWriteCompleted: writeCompleted, FinalState: FinalStateCompleted, TurnStarted: true}})
			condition, paused := state.ForgeUnavailable["github.com"]
			if paused == writeCompleted || condition.ProbeIssueID != "" {
				t.Fatalf("condition = %#v, paused = %v, write completed = %v", condition, paused, writeCompleted)
			}
			if len(tracker.updates) != 0 || len(state.Retry) != 0 {
				t.Fatalf("observed lane changed or retried: updates=%#v retries=%#v", tracker.updates, state.Retry)
			}
		})
	}
}

func TestCredentialCanaryRecoversOverlappingHosts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name          string
		firstHost     string
		operatorRetry bool
		staggered     bool
	}{
		{name: "github first", firstHost: "github.com"},
		{name: "api first", firstHost: "api.github.com"},
		{name: "operator retry", firstHost: "github.com", operatorRetry: true},
		{name: "other host not due", firstHost: "api.github.com", staggered: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 14, 5, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", MaxConcurrentAgents: 3})
			orch := Orchestrator{cfg: cfg}
			planner := newDispatchPlanner(cfg)
			state := newState(cfg)
			otherHost := "api.github.com"
			if tt.firstHost == otherHost {
				otherHost = "github.com"
			}
			hosts := []string{tt.firstHost, otherHost}
			for _, host := range hosts {
				issue := dispatchTestIssue(host, "In Progress")
				issue.URL = "https://github.com/digitaldrywood/detent/issues/2548"
				due := now
				if tt.operatorRetry || tt.staggered && host == otherHost {
					due = now.Add(time.Hour)
				}
				state.ForgeUnavailable[host] = ForgeCondition{Host: host, ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, NextProbeAt: due}
				state.Retry[issue.ID] = Retry{Issue: issue, ForgeUnavailable: true, ForgeHost: host, DueAt: due}
			}
			ordinary := dispatchTestIssue("ordinary", "In Progress")
			if tt.operatorRetry {
				orch.clearForgeAvailability(&state, tt.firstHost, now)
			}
			for index, host := range hosts {
				if !forgeAvailabilityBlocks(&state, ordinary, Retry{}, cfg.ForgeHost, now) {
					t.Fatal("ordinary work admitted before all hosts recovered")
				}
				retry := state.Retry[host]
				if now.Before(retry.DueAt) {
					now = retry.DueAt
				}
				action, allowed, reason := planner.retryAction(&state, retry.Issue, retry, now)
				if !allowed {
					t.Fatalf("%s credential canary blocked: %s", host, reason)
				}
				planner.markDispatched(&state, action, now)
				running := state.Running[retry.Issue.ID]
				if running.ForgeProbeHost != host {
					t.Fatalf("reserved host = %q, want %q", running.ForgeProbeHost, host)
				}
				if index == 0 {
					other := state.Retry[otherHost]
					if !forgeAvailabilityBlocks(&state, other.Issue, other, cfg.ForgeHost, now.Add(2*time.Hour)) {
						t.Fatal("second credential canary admitted while first is running")
					}
				}
				if !forgeAvailabilityBlocks(&state, ordinary, Retry{}, cfg.ForgeHost, now) {
					t.Fatal("ordinary work admitted during credential canary")
				}
				orch.finishForgeAvailabilityProbe(&state, runpkg.Completion{CompletedAt: now, Result: runpkg.RunResult{ForgeWriteCompleted: true}}, running)
				delete(state.Running, retry.Issue.ID)
				if _, active := state.ForgeUnavailable[host]; active {
					t.Fatalf("successful write left %s paused", host)
				}
			}
			if forgeAvailabilityBlocks(&state, ordinary, Retry{}, cfg.ForgeHost, now) {
				t.Fatal("project remains paused after both successful writes")
			}
		})
	}
}

func TestCredentialCanaryReplacementOverlappingHosts(t *testing.T) {
	t.Parallel()
	for _, replacementFirst := range []bool{false, true} {
		name := "retained retry first"
		if replacementFirst {
			name = "replacement first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 14, 5, 0, 0, 0, time.UTC)
			cfg := normalizeConfig(Config{ForgeHost: "github.com", MaxConcurrentAgents: 3})
			planner := newDispatchPlanner(cfg)
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			for _, host := range []string{"github.com", "api.github.com"} {
				state.ForgeUnavailable[host] = ForgeCondition{Host: host, ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, NextProbeAt: now}
			}
			retained := dispatchTestIssue("retained", "In Progress")
			retained.URL = "https://github.com/digitaldrywood/detent/issues/2548"
			state.Retry[retained.ID] = Retry{Issue: retained, ForgeUnavailable: true, ForgeHost: "github.com", DueAt: now}
			replacement := dispatchTestIssue("replacement", "In Progress")
			replacement.URL = retained.URL
			issues := []connector.Issue{retained, replacement}
			if replacementFirst {
				issues[0], issues[1] = issues[1], issues[0]
			}
			for index, issue := range issues {
				var action dispatchAction
				var allowed bool
				var reason string
				if issue.ID == retained.ID {
					action, allowed, reason = planner.retryAction(&state, issue, state.Retry[issue.ID], now)
				} else {
					action, allowed, reason = planner.dispatchAction(&state, issue, now)
				}
				if !allowed {
					t.Fatalf("%s canary refused: %s", issue.ID, reason)
				}
				planner.markDispatched(&state, action, now)
				probes := 0
				for _, condition := range state.ForgeUnavailable {
					if condition.ProbeIssueID != "" {
						probes++
					}
				}
				if probes != 1 {
					t.Fatalf("reserved %d probes, want one project credential canary", probes)
				}
				if index == 0 {
					other := issues[1]
					if !forgeAvailabilityBlocks(&state, other, state.Retry[other.ID], cfg.ForgeHost, now) {
						t.Fatal("other worker admitted while credential canary runs")
					}
				}
				orch.finishForgeAvailabilityProbe(&state, runpkg.Completion{CompletedAt: now, Result: runpkg.RunResult{ForgeWriteCompleted: true}}, state.Running[issue.ID])
				delete(state.Running, issue.ID)
			}
			if len(state.ForgeUnavailable) != 0 {
				t.Fatal("successful canaries left project paused")
			}
		})
	}
}

func TestCredentialCanaryCrossHostFailureReleasesReservation(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"In Progress", "Done"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 14, 5, 0, 0, 0, time.UTC)
			issue := dispatchTestIssue("probe", lane)
			cfg := normalizeConfig(Config{Project: scheduler.ProjectCandidate{ID: "detent"}, ForgeHost: "github.com", ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 3})
			orch := Orchestrator{cfg: cfg, connector: &backendCapacityTestConnector{}, workAttempts: &recordingWorkAttemptStore{}}
			state := newState(cfg)
			state.ForgeUnavailable["github.com"] = ForgeCondition{Host: "github.com", Operation: "git push", ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, ProbeIssueID: issue.ID}
			running := Running{Issue: issue, WorkAttemptID: 1, ForgeProbeHost: "github.com"}
			if lane == "Done" {
				running.CompletionLane = lane
			}
			state.Running[issue.ID] = running
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Err:    forgeavailability.NewError(forgeavailability.Scope{Host: "api.github.com", Operation: "create_pull_request"}, forgeavailability.ClassWorkerGitHubCredentialUnavailable, errors.New("bad credentials")),
				Result: runpkg.RunResult{FinalState: FinalStateCompleted, TurnStarted: true},
			})
			if len(state.ForgeUnavailable) != 2 {
				t.Fatal("failed write erased an unresolved host condition")
			}
			for host, condition := range state.ForgeUnavailable {
				if condition.ProbeIssueID != "" {
					t.Fatalf("completed canary left %s reserved by %q", host, condition.ProbeIssueID)
				}
				if !condition.NextProbeAt.After(now) {
					t.Fatalf("failed probe did not back off %s", host)
				}
			}
			planner := newDispatchPlanner(cfg)
			var action dispatchAction
			var allowed bool
			var reason string
			if lane == "Done" {
				if len(state.Retry) != 0 {
					t.Fatal("terminal issue retained a retry")
				}
				action, allowed, reason = planner.dispatchAction(&state, dispatchTestIssue("replacement", "In Progress"), now.Add(time.Hour))
			} else {
				retry := state.Retry[issue.ID]
				action, allowed, reason = planner.retryAction(&state, issue, retry, retry.DueAt)
			}
			if !allowed {
				t.Fatalf("next credential canary refused: %s", reason)
			}
			planner.markDispatched(&state, action, now.Add(time.Hour))
			probes := 0
			for _, condition := range state.ForgeUnavailable {
				if condition.ProbeIssueID != "" {
					probes++
				}
			}
			if probes != 1 {
				t.Fatalf("reserved %d probes, want one", probes)
			}
		})
	}
}

func TestCredentialCanaryExcludesMergeWorker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 6, 30, 0, 0, time.UTC)
	for _, lane := range []string{"Merging", "In Progress", "Rework"} {
		t.Run(lane, func(t *testing.T) {
			state := newState(normalizeConfig(Config{}))
			issue := dispatchTestIssue("canary", lane)
			state.ForgeUnavailable["github.com"] = ForgeCondition{Host: "github.com", ErrorClass: forgeavailability.ClassWorkerGitHubCredentialUnavailable, NextProbeAt: now}
			state.Retry["merge"] = Retry{Issue: dispatchTestIssue("merge", "Merging"), ForgeUnavailable: true, ForgeHost: "github.com"}
			if got := forgeAvailabilityBlocks(&state, issue, Retry{}, "github.com", now); got != (lane == "Merging") {
				t.Fatalf("blocked = %v for %s", got, lane)
			}
		})
	}
}

func TestCompoundPushCLIAuthCompletion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ operation, completionLane string }{
		{"git push", ""},
		{"post-push command", ""},
		{"git push", "Rework"},
	} {
		t.Run(tt.operation+"/"+tt.completionLane, func(t *testing.T) {
			now := time.Date(2026, 9, 15, 5, 58, 15, 0, time.UTC)
			cfg := normalizeConfig(Config{ForgeHost: "github.com"})
			issue := dispatchTestIssue("2731", "In Progress")
			issue.URL = "https://detent.dev/issues/2731"
			attempts := &implementProgressAttemptStore{}
			orch := Orchestrator{cfg: cfg, connector: &implementProgressConnector{}, workAttempts: attempts, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 4, WorkAttemptID: 2731, CompletionLane: tt.completionLane, Mode: runpkg.RunModeImplement, StartedAt: now.Add(-time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now,
				Result: runpkg.RunResult{PullRequestHeadPushed: true, ForgeWriteCompleted: true, TurnStarted: true},
				Err:    &runpkg.DeliverableCommandError{OperationClass: "push", Operation: tt.operation, Message: "32bbf98..9773c9e HEAD -> detent/example\nlist pull request labels: exit status 4: To get started with GitHub CLI, please run: gh auth login"},
			})
			if len(state.ForgeUnavailable) != 0 || len(state.Blocked) != 0 {
				t.Fatalf("unexpected pause or park: %#v / %#v", state.ForgeUnavailable, state.Blocked)
			}
			if state.Retry[issue.ID].Attempt != 4 {
				t.Fatalf("attempt charged: %#v", state.Retry[issue.ID])
			}
			if len(attempts.completions) != 1 || attempts.completions[0].ErrorClass != workerGitHubTokenResolutionErrorClass {
				t.Fatalf("completion = %#v", attempts.completions)
			}
			completed := attempts.completions[0]
			restored := newState(cfg)
			orch.connector = &rateLimitConnector{issuesByID: []connector.Issue{issue}}
			orch.recoverWorkerGitHubTokenResolutionWaits(t.Context(), &restored, []store.WorkAttempt{{
				IssueID: issue.ID, Identifier: issue.Identifier, Lane: issue.State, AttemptNumber: 4, Status: store.WorkAttemptStatusTerminal,
				TerminalState: completed.TerminalState, ErrorClass: completed.ErrorClass,
				WorkerMetadataJSON: completed.WorkerMetadataJSON,
			}}, now.Add(time.Second))
			retry, ok := restored.Retry[issue.ID]
			if !ok || retry.Attempt != 4 || !retry.DueAt.Equal(state.Retry[issue.ID].DueAt) || !retry.DueAt.After(now.Add(time.Second)) {
				t.Fatalf("restored retry = %#v, want original durable wait %#v", retry, state.Retry[issue.ID])
			}
		})
	}
}
