package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestForgeUnavailableCompletionBypassesAllFailureBreakers(t *testing.T) {
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
		Message:        "HTTP 503: unavailable",
	}
	err := forgeavailability.NewError(
		forgeavailability.Scope{Host: "github.com", Operation: deliverableErr.Operation},
		forgeavailability.ClassServer,
		deliverableErr,
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
	if persisted.ForgeWait.Host != "github.com" || persisted.ForgeWait.Branch != "detent/1871" || !persisted.ForgeWait.WorkProductPushed {
		t.Fatalf("persisted forge wait = %#v, want scoped pushed branch", persisted.ForgeWait)
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

func TestMissingDeliverableCredentialsParkIssueWithoutRetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	tracker := &dependencyAutoUnblockConnector{}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework"},
		ObservedStates: []string{"Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	issue := connector.Issue{
		ID:         "issue-missing-credential",
		Identifier: "digitaldrywood/client-portals#132",
		State:      "In Progress",
	}
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, StartedAt: now.Add(-time.Minute)}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Issue: issue, Attempt: 1, Mode: runpkg.RunModeImplement},
		Result:  runpkg.RunResult{FinalState: runpkg.FinalStateFailed, TurnStarted: true},
		Err: &runpkg.DeliverableCommandError{
			OperationClass: "push",
			Operation:      "git push",
			Status:         "failed",
			Message:        "To get started with GitHub CLI, run: gh auth login; alternatively populate GH_TOKEN",
		},
		CompletedAt:  now,
		Retryable:    true,
		RetryAttempt: 2,
		RetryDelay:   time.Minute,
	})

	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after missing credentials", issue.ID)
	}
	blocked, ok := state.Blocked[issue.ID]
	if !ok || blocked.Issue.State != blockedStatusState {
		t.Fatalf("Blocked[%q] = %#v, want Blocked lane", issue.ID, blocked)
	}
	if blocked.Recovery == nil || blocked.Recovery.Owner != blockedRecoveryOwnerHuman {
		t.Fatalf("Blocked[%q].Recovery = %#v, want human-owned recovery", issue.ID, blocked.Recovery)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != blockedStatusState {
		t.Fatalf("state updates = %#v, want one Blocked transition", tracker.updates)
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
