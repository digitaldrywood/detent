package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestBackendCapacityDispatchAllowsOneResetProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	controller := backendCapacityTestController{scope: scope}
	var logs bytes.Buffer
	orch := &Orchestrator{
		capacityController: controller,
		logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(normalizeConfig(Config{}))
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(
		scope,
		backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &resetAt},
		errors.New("usage limit reached"),
	))
	if !ok {
		t.Fatal("capacity error did not unwrap")
	}
	outage := orch.registerBackendOutage(&state, capacityErr, now, false)
	request := runpkg.RunRequest{Issue: connector.Issue{ID: "issue-1", State: "In Progress"}}

	if _, _, paused := orch.backendCapacityDispatch(&state, request, now); !paused {
		t.Fatal("backendCapacityDispatch() paused = false before reset")
	}
	probeAt := now.Add(backendCapacityProbeDelay)
	if !probeAt.Before(outage.ResumeAt) {
		t.Fatalf("probeAt = %s, want before provider resume %s", probeAt, outage.ResumeAt)
	}
	resolvedScope, probeKey, paused := orch.backendCapacityDispatch(&state, request, probeAt)
	if paused || probeKey == "" || !resolvedScope.Matches(scope) {
		t.Fatalf("backendCapacityDispatch() = scope %#v probe %q paused %v, want one early probe", resolvedScope, probeKey, paused)
	}
	orch.markBackendCapacityProbe(&state, probeKey, request.Issue.ID, probeAt)
	if !strings.Contains(logs.String(), "backend capacity probe started") || !stateEventExists(state, "backend_capacity_probe_started") {
		t.Fatalf("probe evidence missing: logs %q events %#v", logs.String(), state.RecentEvents)
	}
	if _, _, paused := orch.backendCapacityDispatch(&state, request, probeAt); !paused {
		t.Fatal("backendCapacityDispatch() paused = false while probe is running")
	}

	orch.recoverBackendCapacity(&state, Running{CapacityScope: scope, CapacityProbe: true}, probeAt.Add(time.Second))
	if len(state.BackendOutages) != 0 || len(state.BackendRecoveries) != 1 {
		t.Fatalf("capacity state after successful probe = outages %#v recoveries %#v", state.BackendOutages, state.BackendRecoveries)
	}
}

func TestHandleRunResultRetriesTransientOverloadWithoutBackendOutage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 20, 34, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{
		OverloadRetryDelay: 45 * time.Second,
		ActiveStates:       []string{"In Progress", "Merging"},
		TerminalStates:     []string{"Done"},
	})
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:                cfg,
		capacityController: backendCapacityTestController{scope: scope},
		logger:             slog.New(slog.NewTextHandler(&logs, nil)),
	}
	state := newState(cfg)
	issue := connector.Issue{ID: "issue-merge-overload", Identifier: "digitaldrywood/detent#1281", State: "Merging"}
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 7, WorkerHost: "worker-a", StartedAt: now.Add(-time.Minute)}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
	overloadErr := backendcapacity.NewError(scope, backendcapacity.Details{
		Type:   backendcapacity.ErrorTypeTransientOverload,
		Kind:   "serverOverloaded",
		Reason: string(backendcapacity.ErrorTypeTransientOverload),
	}, errors.New("selected model is at capacity"))

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Request:      runpkg.RunRequest{Issue: issue, Attempt: 7},
		Err:          overloadErr,
		CompletedAt:  now,
		Retryable:    true,
		RetryAttempt: 7,
		RetryDelay:   45 * time.Second,
	})

	if len(state.BackendOutages) != 0 {
		t.Fatalf("BackendOutages = %#v, want none", state.BackendOutages)
	}
	retry, ok := state.Retry[issue.ID]
	if !ok || retry.Attempt != 7 || !retry.DueAt.Equal(now.Add(45*time.Second)) || retry.Error != "transient_overload" {
		t.Fatalf("Retry[%q] = %#v, want same-attempt transient retry after 45s", issue.ID, retry)
	}
	if _, _, paused := orch.backendCapacityDispatch(&state, runpkg.RunRequest{Issue: connector.Issue{ID: "other-issue"}}, now); paused {
		t.Fatal("transient overload paused dispatch for another issue")
	}
	if len(state.InstantFailures) != 0 || len(state.RepeatedFailures) != 0 {
		t.Fatalf("failure breakers = instant %#v repeated %#v, want no overload strikes", state.InstantFailures, state.RepeatedFailures)
	}
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "reason=transient_overload") {
		t.Fatalf("logs = %q, want INFO transient_overload reason", logs.String())
	}
}

func TestHandleRunResultTracksStartupTimeoutAsCapacityAndBreakerSignal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{
		OverloadRetryDelay: 45 * time.Second,
		ActiveStates:       []string{"In Progress"},
		TerminalStates:     []string{"Done"},
		FailureBreaker: FailureBreakerConfig{
			SameClassLimit: 2,
			Window:         time.Hour,
			Cooldown:       5 * time.Minute,
		},
	})
	attempts := &recordingWorkAttemptStore{}
	orch := &Orchestrator{cfg: cfg, workAttempts: attempts}
	state := newState(cfg)
	issue := connector.Issue{ID: "issue-startup-timeout", State: "In Progress"}
	state.Running[issue.ID] = Running{Issue: issue, Attempt: 3, WorkAttemptID: 42, StartedAt: now.Add(-time.Minute)}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
	timeoutErr := backendcapacity.NewError(scope, backendcapacity.Details{
		Type:   backendcapacity.ErrorTypeTransientOverload,
		Kind:   backendcapacity.StartupTimeoutKind,
		Reason: "backend startup handshake timed out",
	}, context.DeadlineExceeded)

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Err:          timeoutErr,
		CompletedAt:  now,
		RetryAttempt: 3,
		RetryDelay:   45 * time.Second,
	})

	if len(state.BackendOutages) != 0 {
		t.Fatalf("BackendOutages = %#v, want short retry without outage", state.BackendOutages)
	}
	retry := state.Retry[issue.ID]
	if retry.Attempt != 3 || !retry.DueAt.Equal(now.Add(45*time.Second)) || retry.Error != backendcapacity.StartupTimeoutErrorClass {
		t.Fatalf("Retry[%q] = %#v, want startup-capacity retry", issue.ID, retry)
	}
	if len(state.FailureBreaker.Failures[backendcapacity.StartupTimeoutErrorClass]) != 1 || state.FailureBreaker.Active() {
		t.Fatalf("FailureBreaker = %#v, want one preserved systemic signal", state.FailureBreaker)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("work attempt completions = %#v, want one", attempts.completions)
	}
	completion := attempts.completions[0]
	if completion.TerminalState != store.WorkAttemptTerminalTimedOut || completion.ErrorClass != backendcapacity.StartupTimeoutErrorClass || completion.StatusMessage != "retrying after backend startup timeout" {
		t.Fatalf("completion = %#v, want startup timeout telemetry", completion)
	}
}

func TestRegisterBackendOutageRejectsTransientOverload(t *testing.T) {
	t.Parallel()

	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	overloadErr, ok := backendcapacity.As(backendcapacity.NewError(scope, backendcapacity.Details{
		Type: backendcapacity.ErrorTypeTransientOverload,
	}, errors.New("HTTP 529")))
	if !ok {
		t.Fatal("transient overload error did not unwrap")
	}
	state := newState(normalizeConfig(Config{}))
	orch := &Orchestrator{}
	if outage := orch.registerBackendOutage(&state, overloadErr, time.Now(), false); outage != (BackendOutage{}) || len(state.BackendOutages) != 0 {
		t.Fatalf("registerBackendOutage() = %#v, outages %#v, want no outage", outage, state.BackendOutages)
	}
}

func TestHandleRunResultTransientOverloadReleasesCapacityProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 21, 30, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{OverloadRetryDelay: 45 * time.Second, TerminalStates: []string{"Done"}})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	issue := connector.Issue{ID: "issue-overloaded-probe", State: "In Progress"}
	resumeAt := now.Add(-time.Second)
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:        scope,
		ResumeAt:     resumeAt,
		ProbeIssueID: issue.ID,
	}
	state.Running[issue.ID] = Running{
		Issue:         issue,
		Attempt:       3,
		StartedAt:     now.Add(-time.Minute),
		CapacityScope: scope,
		CapacityProbe: true,
	}
	overloadErr := backendcapacity.NewError(scope, backendcapacity.Details{
		Type:   backendcapacity.ErrorTypeTransientOverload,
		Kind:   "serverOverloaded",
		Reason: string(backendcapacity.ErrorTypeTransientOverload),
	}, errors.New("selected model is at capacity"))

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:      issue.ID,
		Err:          overloadErr,
		CompletedAt:  now,
		RetryAttempt: 3,
		RetryDelay:   45 * time.Second,
	})

	outage := state.BackendOutages[scope.Key()]
	if outage.ProbeIssueID != "" || !outage.ResumeAt.Equal(resumeAt) {
		t.Fatalf("outage = %#v, want released probe with unchanged resume time", outage)
	}
	if retry := state.Retry[issue.ID]; retry.Attempt != 3 || !retry.DueAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("Retry[%q] = %#v, want same-attempt overload retry", issue.ID, retry)
	}
}

func TestHandleOperatorStopCompletionDefersCapacityProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 14, 22, 30, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{StopRunTargetState: "Blocked"})
	issue := connector.Issue{ID: "issue-stopped-probe", Identifier: "digitaldrywood/detent#1311", State: "In Progress"}
	connector := &backendCapacityTestConnector{}
	orch := &Orchestrator{
		cfg:            cfg,
		connector:      connector,
		pendingStops:   map[string]*pendingStopRun{},
		completedStops: map[string]StopRunResult{},
		now:            func() time.Time { return now },
	}
	state := newState(cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:         scope,
		ResumeAt:      now.Add(time.Hour),
		ProbeIssueID:  issue.ID,
		ProbeAttempts: 1,
	}
	running := Running{Issue: issue, Attempt: 1, CapacityScope: scope, CapacityProbe: true}
	state.Running[issue.ID] = running
	orch.pendingStops[issue.ID] = &pendingStopRun{result: StopRunResult{
		IssueID:     issue.ID,
		Identifier:  issue.Identifier,
		Attempt:     running.Attempt,
		Destination: "Blocked",
		Outcome:     "pending",
		RequestedAt: now.Add(-time.Second),
	}}

	if handled := orch.handleOperatorStopCompletion(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now}, running); !handled {
		t.Fatal("handleOperatorStopCompletion() handled = false")
	}

	outage := state.BackendOutages[scope.Key()]
	if outage.ProbeIssueID != "" || outage.NextProbeAt.IsZero() || outage.LastProbeResult != "failed" {
		t.Fatalf("outage = %#v, want stopped probe released and deferred", outage)
	}
	if len(connector.updates) != 1 || connector.updates[0] != (backendCapacityTestUpdate{issueID: issue.ID, state: "Blocked"}) {
		t.Fatalf("tracker updates = %#v, want Blocked transition", connector.updates)
	}
}

func TestBackendCapacityStatusProbeRecoversOutageEarly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	controller := backendCapacityTestController{
		scope:     scope,
		status:    runpkg.CapacityStatus{Available: true, Detail: "live provider status reports 20% capacity remaining"},
		hasStatus: true,
	}
	orch := &Orchestrator{capacityStatus: controller}
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:       scope,
		DetectedAt:  now.Add(-time.Hour),
		ResumeAt:    now.Add(3 * time.Hour),
		NextProbeAt: now.Add(5 * time.Minute),
	}
	state.Running["issue-live-status"] = Running{
		Issue:         connector.Issue{ID: "issue-live-status"},
		CapacityScope: scope,
	}

	orch.handleRunUpdate(&state, runUpdate{
		issueID: "issue-live-status",
		usage: runpkg.UsageUpdate{
			LastEventAt: now,
			RateLimits: &telemetry.RateLimits{
				Primary: &telemetry.RateLimitBucket{Limit: 100, Remaining: 20},
			},
		},
	})

	if len(state.BackendOutages) != 0 || len(state.BackendRecoveries) != 1 {
		t.Fatalf("capacity state = outages %#v recoveries %#v, want live-status recovery", state.BackendOutages, state.BackendRecoveries)
	}
	recovery := state.BackendRecoveries[scope.Key()]
	if recovery.Outage.LastProbeResult != "status_available" || !recovery.RecoveredAt.Equal(now) {
		t.Fatalf("recovery = %#v", recovery)
	}
	if !stateEventExists(state, "backend_capacity_recovered") {
		t.Fatalf("events = %#v, want recovery event", state.RecentEvents)
	}
}

func TestBackendCapacityStatusProbeKeepsExhaustedOutage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	controller := backendCapacityTestController{
		scope:     scope,
		status:    runpkg.CapacityStatus{Detail: "live provider status reports 0% capacity remaining"},
		hasStatus: true,
	}
	orch := &Orchestrator{capacityStatus: controller, now: func() time.Time { return now }}
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:       scope,
		DetectedAt:  now.Add(-time.Hour),
		ResumeAt:    now.Add(3 * time.Hour),
		NextProbeAt: now.Add(5 * time.Minute),
	}
	running := Running{CapacityScope: scope}

	orch.recoverBackendCapacityFromStatus(&state, running, &telemetry.RateLimits{
		Primary: &telemetry.RateLimitBucket{Limit: 100},
	}, time.Time{})

	outage, ok := state.BackendOutages[scope.Key()]
	if !ok {
		t.Fatal("exhausted status cleared the outage")
	}
	if outage.LastProbeResult != "status_exhausted" || !outage.LastProbeAt.Equal(now) || outage.LastProbeDetail != controller.status.Detail {
		t.Fatalf("outage = %#v", outage)
	}
	orch.capacityStatus = backendCapacityTestController{}
	orch.recoverBackendCapacityFromStatus(&state, running, &telemetry.RateLimits{}, now)
	orch.capacityStatus = nil
	orch.recoverBackendCapacityFromStatus(&state, running, &telemetry.RateLimits{}, now)
}

func TestClearBackendCapacityClearsScopeAndReleasesRetries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	otherScope := backendcapacity.Scope{BackendID: "claude", BackendKind: "claude_code", Provider: "anthropic"}
	var logs bytes.Buffer
	orch := &Orchestrator{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages[scope.Key()] = BackendOutage{Scope: scope, ResumeAt: now.Add(time.Hour)}
	state.BackendOutages[otherScope.Key()] = BackendOutage{Scope: otherScope, ResumeAt: now.Add(time.Hour)}
	state.Retry["issue-codex"] = Retry{
		Issue:         connector.Issue{ID: "issue-codex"},
		DueAt:         now.Add(time.Hour),
		CapacityScope: scope,
	}

	cleared := orch.clearBackendCapacity(&state, "codex", now)

	if len(cleared) != 1 || !cleared[0].Scope.Matches(scope) {
		t.Fatalf("cleared = %#v", cleared)
	}
	if _, ok := state.BackendOutages[scope.Key()]; ok {
		t.Fatal("codex outage remains after operator clear")
	}
	if _, ok := state.BackendOutages[otherScope.Key()]; !ok {
		t.Fatal("unrelated outage was cleared")
	}
	if !state.Retry["issue-codex"].DueAt.Equal(now) {
		t.Fatalf("retry due = %s, want %s", state.Retry["issue-codex"].DueAt, now)
	}
	if !strings.Contains(logs.String(), "operator cleared backend capacity outage") || !stateEventExists(state, "backend_capacity_operator_cleared") {
		t.Fatalf("operator evidence missing: logs %q events %#v", logs.String(), state.RecentEvents)
	}
}

func stateEventExists(state State, event string) bool {
	for _, candidate := range state.RecentEvents {
		if candidate.Event == event {
			return true
		}
	}
	return false
}

func TestHandleRunResultKeepsOutageWhenCapacityProbeFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:        scope,
		DetectedAt:   now.Add(-44 * time.Minute),
		ResumeAt:     now,
		ProbeIssueID: "issue-capacity-probe",
	}
	state.Running["issue-capacity-probe"] = Running{
		Issue:         connector.Issue{ID: "issue-capacity-probe", State: "In Progress"},
		Attempt:       1,
		StartedAt:     now.Add(-time.Minute),
		CapacityScope: scope,
		CapacityProbe: true,
	}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     "issue-capacity-probe",
		Err:         errors.New("workspace setup failed"),
		CompletedAt: now,
	})

	if _, ok := state.BackendOutages[scope.Key()]; !ok {
		t.Fatal("capacity probe failure cleared the backend outage")
	}
	outage := state.BackendOutages[scope.Key()]
	if outage.ProbeIssueID != "" {
		t.Fatalf("ProbeIssueID = %q, want released probe", outage.ProbeIssueID)
	}
	if !outage.ResumeAt.Equal(now) {
		t.Fatalf("ResumeAt = %s, want preserved provider time %s", outage.ResumeAt, now)
	}
	if want := now.Add(backendCapacityProbeDelayForAttempt(1)); !outage.NextProbeAt.Equal(want) {
		t.Fatalf("NextProbeAt = %s, want %s", outage.NextProbeAt, want)
	}
	if len(state.BackendRecoveries) != 0 {
		t.Fatalf("backend recoveries = %#v, want none", state.BackendRecoveries)
	}
}

func TestBackendCapacityProbeFailureRefreshesProviderWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	freshResetAt := now.Add(2 * time.Hour)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	controller := backendCapacityTestController{scope: scope}
	orch := &Orchestrator{capacityController: controller}
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:           scope,
		DetectedAt:      now.Add(-time.Hour),
		ResumeAt:        now.Add(3 * time.Hour),
		ProbeAttempts:   1,
		ProbeIssueID:    "issue-canary",
		LastProbeAt:     now.Add(-time.Minute),
		LastProbeResult: "in_progress",
	}
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(
		scope,
		backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &freshResetAt},
		errors.New("usage limit reached"),
	))
	if !ok {
		t.Fatal("capacity error did not unwrap")
	}

	outage := orch.registerBackendOutage(&state, capacityErr, now, true)

	if !outage.ResetAt.Equal(freshResetAt) || !outage.ResumeAt.Equal(freshResetAt.Add(backendCapacityResetJitter)) {
		t.Fatalf("fresh provider window = reset %s resume %s", outage.ResetAt, outage.ResumeAt)
	}
	if outage.LastProbeResult != "capacity_exhausted" || outage.ProbeIssueID != "" {
		t.Fatalf("probe result = %#v", outage)
	}
	if want := now.Add(backendCapacityProbeDelayForAttempt(1)); !outage.NextProbeAt.Equal(want) {
		t.Fatalf("NextProbeAt = %s, want %s", outage.NextProbeAt, want)
	}
	request := runpkg.RunRequest{Issue: connector.Issue{ID: "issue-other"}}
	if _, _, paused := orch.backendCapacityDispatch(&state, request, now.Add(backendCapacityProbeDelay)); !paused {
		t.Fatal("full-width dispatch resumed before the next bounded canary")
	}
}

func TestHandleRunResultRecoversOutageWhenCapacityProbeTurnStarts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	cfg := normalizeConfig(Config{})
	orch := &Orchestrator{cfg: cfg}
	state := newState(cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{Scope: scope, ResumeAt: now, ProbeIssueID: "issue-started-probe"}
	state.Running["issue-started-probe"] = Running{
		Issue:         connector.Issue{ID: "issue-started-probe", State: "In Progress"},
		Attempt:       1,
		StartedAt:     now.Add(-time.Minute),
		CapacityScope: scope,
		CapacityProbe: true,
	}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     "issue-started-probe",
		Result:      runpkg.RunResult{TurnStarted: true},
		Err:         errors.New("agent work failed"),
		CompletedAt: now,
	})

	if len(state.BackendOutages) != 0 || len(state.BackendRecoveries) != 1 {
		t.Fatalf("capacity state = outages %#v recoveries %#v, want recovered", state.BackendOutages, state.BackendRecoveries)
	}
}

func TestBackendCapacityDispatchLeavesLocalProviderUnaffected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	state := newState(normalizeConfig(Config{}))
	state.BackendOutages["hosted"] = BackendOutage{
		Scope:    backendcapacity.Scope{BackendID: "codex-hosted", BackendKind: "codex", Provider: "openai"},
		ResumeAt: now.Add(time.Hour),
	}
	orch := &Orchestrator{capacityController: backendCapacityTestController{
		scope: backendcapacity.Scope{BackendID: "codex-local", BackendKind: "codex", Provider: "local_ollama"},
	}}

	_, _, paused := orch.backendCapacityDispatch(&state, runpkg.RunRequest{Issue: connector.Issue{ID: "local"}}, now)
	if paused {
		t.Fatal("local provider dispatch paused by hosted-provider outage")
	}
}

func TestBackendCapacityWithoutResetUsesLowFrequencyProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	if got, want := backendCapacityResumeAt(time.Time{}, now), now.Add(backendCapacityProbeDelay); !got.Equal(want) {
		t.Fatalf("backendCapacityResumeAt() = %s, want %s", got, want)
	}
}

func TestBackendCapacityHelperBoundaries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	if got := backendCapacityStatusMessage(BackendOutage{}); got != "backend agent backend at usage limit" {
		t.Fatalf("backendCapacityStatusMessage() = %q", got)
	}
	outage := BackendOutage{
		Scope:    backendcapacity.Scope{BackendKind: "codex"},
		ResumeAt: now,
	}
	if got := backendCapacityStatusMessage(outage); !strings.Contains(got, "backend codex") || !strings.Contains(got, now.Format(time.RFC3339)) {
		t.Fatalf("backendCapacityStatusMessage() = %q", got)
	}

	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	resetAt := now.Add(time.Hour)
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(
		scope,
		backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &resetAt},
		errors.New("usage limit reached"),
	))
	if !ok {
		t.Fatal("capacity error did not unwrap")
	}
	orch := &Orchestrator{now: func() time.Time { return now }}
	state := State{
		Retry:   map[string]Retry{},
		Claimed: map[string]Claimed{},
	}
	registered := orch.registerBackendOutage(&state, capacityErr, time.Time{}, false)
	if !registered.DetectedAt.Equal(now) || state.BackendOutages[scope.Key()].Kind != "usageLimitExceeded" {
		t.Fatalf("registered outage = %#v", registered)
	}

	running := Running{Issue: connector.Issue{ID: "issue-1"}, Attempt: 2, WorkerHost: "worker"}
	orch.scheduleBackendCapacityRetry(&state, running, registered)
	if state.Claimed[running.Issue.ID].ClaimedAt != registered.LastObservedAt || state.Retry[running.Issue.ID].WorkerHost != "worker" {
		t.Fatalf("scheduled state = claims %#v retries %#v", state.Claimed, state.Retry)
	}
	if !state.Retry[running.Issue.ID].DueAt.Equal(registered.NextProbeAt) {
		t.Fatalf("retry due = %s, want early probe %s", state.Retry[running.Issue.ID].DueAt, registered.NextProbeAt)
	}
	orch.markBackendCapacityProbe(&state, "missing", running.Issue.ID, now)
	orch.recoverBackendCapacity(&state, Running{CapacityScope: backendcapacity.Scope{BackendID: "missing"}}, now)
	if _, _, paused := orch.validatorCapacityDispatch(nil, connector.Issue{}, now); paused {
		t.Fatal("nil validator capacity dispatch paused")
	}
	orch.publishValidatorCapacityEvent(t.Context(), validatorCapacityEvent{})
	orch.handleValidatorCapacityEvent(&state, validatorCapacityEvent{})

	var logs bytes.Buffer
	orch.logger = slog.New(slog.NewTextHandler(&logs, nil))
	orch.handleValidatorCapacityEvent(&state, validatorCapacityEvent{CapacityErr: capacityErr, CompletedAt: now})
	orch.recoverBackendCapacity(&state, Running{CapacityScope: scope, CapacityProbe: true}, now)
	state.BackendOutages[scope.Key()] = registered
	orch.deferBackendCapacityProbe(&state, Running{CapacityScope: scope, CapacityProbe: true}, time.Time{}, errors.New("probe failed"))
	if !strings.Contains(logs.String(), "backend capacity recovered") || !strings.Contains(logs.String(), "probe failed") {
		t.Fatalf("capacity logs = %q", logs.String())
	}

	recovery := BackendRecovery{Outage: registered, RecoveredAt: now}
	if key, _, found := matchingBackendRecovery(map[string]BackendRecovery{scope.Key(): recovery}, scope); !found || key != scope.Key() {
		t.Fatalf("matchingBackendRecovery() = %q, %t", key, found)
	}
	readerless := &Orchestrator{connector: &backendCapacityTestConnector{}}
	if _, _, ok := readerless.classifyBlockedCapacityIssue(t.Context(), &state, connector.Issue{ID: "blocked"}, now); ok {
		t.Fatal("readerless capacity issue classified")
	}

	recoveryState := newState(normalizeConfig(Config{}))
	recoveryIssue := connector.Issue{ID: "recover", State: "Blocked"}
	recoveryState.Blocked[recoveryIssue.ID] = Blocked{Issue: recoveryIssue}
	recoveryOrch := &Orchestrator{connector: &backendCapacityTestConnector{}}
	if !recoveryOrch.applyBackendCapacityBlockedRecovery(t.Context(), &recoveryState, recoveryIssue, BackendOutage{}, recovery, now) {
		t.Fatal("applyBackendCapacityBlockedRecovery() = false")
	}
}

func TestValidatorCapacityPausesWithoutFailureBackoff(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 1, 55, 0, 0, time.UTC)
	resetAt := now.Add(44 * time.Minute)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	validator := &backendCapacityTestValidator{
		requests: make(chan ValidatorRequest, 2),
		err: backendcapacity.NewError(
			scope,
			backendcapacity.Details{Kind: "usageLimitExceeded", ResetAt: &resetAt},
			errors.New("usage limit reached"),
		),
	}
	controller := backendCapacityTestController{scope: scope}
	orch := &Orchestrator{
		cfg:                     normalizeConfig(Config{}),
		validator:               validator,
		validatorCapacity:       controller,
		validatorRuns:           map[string]struct{}{},
		validatorResults:        map[string]validatorStageResult{},
		validatorFailures:       map[string]validatorStageFailure{},
		now:                     func() time.Time { return now },
		validatorCapacityEvents: make(chan validatorCapacityEvent, 1),
		done:                    make(chan struct{}),
	}
	state := newState(orch.cfg)
	issue := connector.Issue{
		ID:    "issue-validator-capacity",
		State: "In Progress",
		PullRequest: &connector.PullRequest{
			HeadSHA: "capacity-head",
		},
	}

	orch.startValidatorStage(t.Context(), &state, issue, now)
	select {
	case <-validator.requests:
	case <-time.After(time.Second):
		t.Fatal("validator did not run")
	}
	var event validatorCapacityEvent
	select {
	case event = <-orch.validatorCapacityEvents:
	case <-time.After(time.Second):
		t.Fatal("validator capacity event was not published")
	}
	orch.handleValidatorCapacityEvent(&state, event)
	if len(orch.validatorFailures) != 0 {
		t.Fatalf("validator failures = %#v, want none", orch.validatorFailures)
	}
	if len(state.BackendOutages) != 1 {
		t.Fatalf("backend outages = %#v, want one", state.BackendOutages)
	}

	orch.startValidatorStage(t.Context(), &state, issue, now.Add(time.Minute))
	select {
	case <-validator.requests:
		t.Fatal("validator ran while backend capacity was paused")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestValidatorTransientOverloadReleasesCapacityProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 21, 0, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	validator := &backendCapacityTestValidator{
		requests: make(chan ValidatorRequest, 1),
		err: backendcapacity.NewError(scope, backendcapacity.Details{
			Type:   backendcapacity.ErrorTypeTransientOverload,
			Kind:   "serverOverloaded",
			Reason: string(backendcapacity.ErrorTypeTransientOverload),
		}, errors.New("selected model is at capacity")),
	}
	cfg := normalizeConfig(Config{OverloadRetryDelay: 45 * time.Second})
	orch := &Orchestrator{
		cfg:                     cfg,
		validator:               validator,
		validatorCapacity:       backendCapacityTestController{scope: scope},
		validatorRuns:           map[string]struct{}{},
		validatorResults:        map[string]validatorStageResult{},
		validatorFailures:       map[string]validatorStageFailure{},
		now:                     func() time.Time { return now },
		validatorCapacityEvents: make(chan validatorCapacityEvent, 1),
		done:                    make(chan struct{}),
	}
	state := newState(cfg)
	issue := connector.Issue{
		ID:    "issue-validator-overload",
		State: "In Progress",
		PullRequest: &connector.PullRequest{
			HeadSHA: "overload-head",
		},
	}
	resumeAt := now
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:    scope,
		ResumeAt: resumeAt,
	}

	orch.startValidatorStage(t.Context(), &state, issue, now)
	select {
	case <-validator.requests:
	case <-time.After(time.Second):
		t.Fatal("validator did not run")
	}
	identity := validatorStageIdentityForIssue(issue)
	deadline := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	t.Cleanup(func() { deadline.Stop() })
	t.Cleanup(ticker.Stop)
	var failure validatorStageFailure
	for failure.NextRetryAt.IsZero() {
		select {
		case <-ticker.C:
			orch.validatorMu.Lock()
			failure = orch.validatorFailures[identity.Key]
			orch.validatorMu.Unlock()
		case <-deadline.C:
			t.Fatal("validator overload retry was not scheduled")
		}
	}
	if failure.Attempt != 0 || !failure.NextRetryAt.Equal(now.Add(45*time.Second)) || failure.Error != "transient_overload" {
		t.Fatalf("validator failure = %#v, want same-attempt retry after 45s", failure)
	}
	var capacityEvent validatorCapacityEvent
	select {
	case capacityEvent = <-orch.validatorCapacityEvents:
	case <-time.After(time.Second):
		t.Fatal("validator overload probe release event was not published")
	}
	orch.handleValidatorCapacityEvent(&state, capacityEvent)
	outage := state.BackendOutages[scope.Key()]
	if outage.ProbeIssueID != "" || !outage.ResumeAt.Equal(resumeAt) {
		t.Fatalf("outage = %#v, want released probe with unchanged resume time", outage)
	}
}

func TestValidatorCapacityProbeFailureKeepsOutage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 2, 39, 0, 0, time.UTC)
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	validator := &backendCapacityTestValidator{
		requests: make(chan ValidatorRequest, 1),
		err:      errors.New("workspace setup failed"),
	}
	controller := backendCapacityTestController{scope: scope}
	orch := &Orchestrator{
		cfg:                     normalizeConfig(Config{}),
		validator:               validator,
		validatorCapacity:       controller,
		validatorRuns:           map[string]struct{}{},
		validatorResults:        map[string]validatorStageResult{},
		validatorFailures:       map[string]validatorStageFailure{},
		now:                     func() time.Time { return now },
		validatorCapacityEvents: make(chan validatorCapacityEvent, 1),
		done:                    make(chan struct{}),
	}
	state := newState(orch.cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{
		Scope:      scope,
		DetectedAt: now.Add(-44 * time.Minute),
		ResumeAt:   now,
	}
	issue := connector.Issue{
		ID:    "issue-validator-probe",
		State: "In Progress",
		PullRequest: &connector.PullRequest{
			HeadSHA: "capacity-probe-head",
		},
	}

	orch.startValidatorStage(t.Context(), &state, issue, now)
	select {
	case <-validator.requests:
	case <-time.After(time.Second):
		t.Fatal("validator did not run")
	}
	orch.validatorWG.Wait()
	select {
	case event := <-orch.validatorCapacityEvents:
		orch.handleValidatorCapacityEvent(&state, event)
	default:
	}

	if _, ok := state.BackendOutages[scope.Key()]; !ok {
		t.Fatal("validator capacity probe failure cleared the backend outage")
	}
	outage := state.BackendOutages[scope.Key()]
	if outage.ProbeIssueID != "" {
		t.Fatalf("ProbeIssueID = %q, want released probe", outage.ProbeIssueID)
	}
	if !outage.ResumeAt.Equal(now) {
		t.Fatalf("ResumeAt = %s, want preserved provider time %s", outage.ResumeAt, now)
	}
	if want := now.Add(backendCapacityProbeDelayForAttempt(1)); !outage.NextProbeAt.Equal(want) {
		t.Fatalf("NextProbeAt = %s, want %s", outage.NextProbeAt, want)
	}
	if len(orch.validatorFailures) != 1 {
		t.Fatalf("validator failures = %#v, want one", orch.validatorFailures)
	}
}

func TestRecoverBackendCapacityBlockedIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		pullRequest *connector.PullRequest
		wantState   string
	}{
		{name: "implementation issue returns to todo", wantState: "Todo"},
		{
			name:        "open pull request returns to rework",
			pullRequest: &connector.PullRequest{Number: 1142, State: "open"},
			wantState:   "Rework",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := connector.Issue{
				ID:          "issue-capacity-blocked",
				Identifier:  "digitaldrywood/detent#1142",
				State:       "Blocked",
				PullRequest: tt.pullRequest,
				Comments: []connector.IssueComment{{
					Body: "Detent stopped retrying this worker after 5 consecutive instant failures with the same backend error. backend_error_body: " +
						`{"error":{"type":"usageLimitExceeded","resetAt":1783666800}}`,
				}},
			}
			tracker := &backendCapacityTestConnector{}
			controller := backendCapacityTestController{scope: backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}}
			orch := &Orchestrator{
				cfg:                normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}}),
				connector:          tracker,
				capacityController: controller,
			}
			state := newState(orch.cfg)
			state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceProjectStatus}

			if transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now); len(transitioned) != 0 {
				t.Fatalf("initial transitioned = %#v, want canary recovery wait", transitioned)
			}
			orch.recoverBackendCapacity(&state, Running{
				CapacityScope: controller.scope,
				CapacityProbe: true,
			}, now.Add(backendCapacityResetJitter))
			transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(backendCapacityResetJitter))
			if _, ok := transitioned[issue.ID]; !ok {
				t.Fatalf("transitioned = %#v, want %s recovered", transitioned, issue.ID)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantState {
				t.Fatalf("updates = %#v, want target %s", tracker.updates, tt.wantState)
			}
			if len(tracker.comments) != 1 || !strings.Contains(tracker.comments[0], "reason: backend_capacity_recovered") {
				t.Fatalf("comments = %#v, want machine-classifiable recovery comment", tracker.comments)
			}
			if _, ok := state.Blocked[issue.ID]; ok {
				t.Fatalf("Blocked[%q] still present after recovery", issue.ID)
			}
		})
	}
}

func TestRecoverBackendCapacityBlockedIssuesIgnoresUnrelatedQuotaComment(t *testing.T) {
	t.Parallel()

	issue := connector.Issue{
		ID:         "issue-unrelated-comment",
		Identifier: "digitaldrywood/detent#1143",
		State:      "Blocked",
		Comments: []connector.IssueComment{{
			Body: "A user mentioned usageLimitExceeded while documenting an unrelated blocker.",
		}},
	}
	tracker := &backendCapacityTestConnector{}
	orch := &Orchestrator{
		cfg:                normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}}),
		connector:          tracker,
		capacityController: backendCapacityTestController{scope: backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}},
	}
	state := newState(orch.cfg)

	transitioned := orch.recoverBackendCapacityBlockedIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
	if len(transitioned) != 0 || len(tracker.updates) != 0 || len(state.BackendOutages) != 0 {
		t.Fatalf("unrelated quota comment triggered recovery: transitioned %#v updates %#v outages %#v", transitioned, tracker.updates, state.BackendOutages)
	}
}

type backendCapacityTestController struct {
	scope     backendcapacity.Scope
	status    runpkg.CapacityStatus
	hasStatus bool
}

func (c backendCapacityTestController) CapacityScope(runpkg.RunRequest) (backendcapacity.Scope, bool) {
	return c.scope, true
}

func (c backendCapacityTestController) ValidatorCapacityScope(runpkg.ValidatorRequest) (backendcapacity.Scope, bool) {
	return c.scope, true
}

func (c backendCapacityTestController) BackendCapacityStatus(
	backendcapacity.Scope,
	*telemetry.RateLimits,
) (runpkg.CapacityStatus, bool) {
	return c.status, c.hasStatus
}

func (c backendCapacityTestController) ClassifyCapacityError(
	_ runpkg.RunRequest,
	err error,
	limits *telemetry.RateLimits,
	now time.Time,
) (*backendcapacity.Error, bool) {
	var fallback *time.Time
	if limits != nil && limits.Primary != nil {
		fallback = limits.Primary.ResetAt
	}
	details, ok := backendcapacity.Classify(err.Error(), fallback, now, backendcapacity.Rules{Kinds: []string{"usageLimitExceeded"}})
	if !ok || !c.scope.Hosted() {
		return nil, false
	}
	capacityErr, ok := backendcapacity.As(backendcapacity.NewError(c.scope, details, err))
	return capacityErr, ok
}

type backendCapacityTestUpdate struct {
	issueID string
	state   string
}

type backendCapacityTestConnector struct {
	updates  []backendCapacityTestUpdate
	comments []string
}

func (c *backendCapacityTestConnector) Name() string {
	return "backend-capacity-test"
}

func (c *backendCapacityTestConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *backendCapacityTestConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.comments = append(c.comments, body)
	return nil
}

func (c *backendCapacityTestConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, backendCapacityTestUpdate{issueID: issueID, state: state})
	return nil
}

func (c *backendCapacityTestConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *backendCapacityTestConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type backendCapacityTestValidator struct {
	requests chan ValidatorRequest
	err      error
}

func (v *backendCapacityTestValidator) Validate(_ context.Context, request ValidatorRequest) (gate.ValidatorResult, error) {
	v.requests <- request
	return gate.ValidatorResult{}, v.err
}
