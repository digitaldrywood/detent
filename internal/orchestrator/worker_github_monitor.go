package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	workerGitHubMonitorErrorClass        = "worker_github_budget_monitor_unavailable"
	workerGitHubMonitorUnknownCredential = "github-rest:unclassified"
)

type GitHubMonitor = telemetry.GitHubMonitor

type workerGitHubMonitorFailure struct {
	CredentialIdentity string
	Consumer           string
	Operation          string
	Message            string
}

type workerGitHubMonitorWaitMetadata struct {
	CredentialIdentity string    `json:"credential_identity"`
	Consumer           string    `json:"consumer"`
	Operation          string    `json:"operation"`
	DetectedAt         time.Time `json:"detected_at"`
	LastObservedAt     time.Time `json:"last_observed_at"`
	NextProbeAt        time.Time `json:"next_probe_at"`
	LastProbeAt        time.Time `json:"last_probe_at,omitzero"`
	LastProbeResult    string    `json:"last_probe_result,omitempty"`
	LastProbeDetail    string    `json:"last_probe_detail,omitempty"`
	ProbeAttempts      int       `json:"probe_attempts"`
}

func workerGitHubMonitorFailureFromCompletion(event runpkg.Completion, state *State) (workerGitHubMonitorFailure, bool) {
	if !errors.Is(event.Err, runpkg.ErrWorkerGitHubBudgetMonitor) {
		return workerGitHubMonitorFailure{}, false
	}
	failure := workerGitHubMonitorFailure{Message: errorString(event.Err)}
	if monitorErr, ok := runpkg.AsWorkerGitHubBudgetMonitorError(event.Err); ok {
		failure.CredentialIdentity = strings.TrimSpace(monitorErr.CredentialIdentity)
		failure.Consumer = strings.TrimSpace(monitorErr.Consumer)
		failure.Operation = strings.TrimSpace(monitorErr.Operation)
	}
	if failure.CredentialIdentity == "" || failure.Consumer == "" {
		for _, limits := range []*telemetry.RateLimits{event.Result.RateLimits, rateLimitsFromState(state)} {
			budget, ok := workerGitHubMonitorBudget(limits)
			if !ok {
				continue
			}
			if failure.CredentialIdentity == "" {
				failure.CredentialIdentity = strings.TrimSpace(budget.CredentialIdentity)
			}
			if failure.Consumer == "" {
				failure.Consumer = strings.TrimSpace(budget.Consumer)
			}
			break
		}
	}
	if failure.CredentialIdentity == "" {
		failure.CredentialIdentity = workerGitHubMonitorUnknownCredential
	}
	if failure.Consumer != telemetry.RESTConsumerWorker && failure.Consumer != telemetry.RESTConsumerSharedPool {
		failure.Consumer = telemetry.RESTConsumerWorker
	}
	if failure.Operation == "" {
		failure.Operation = "unknown"
	}
	return failure, true
}

func workerGitHubMonitorBudget(limits *telemetry.RateLimits) (telemetry.RESTBudget, bool) {
	if limits == nil {
		return telemetry.RESTBudget{}, false
	}
	for _, budget := range limits.GitHubRESTBudgets {
		consumer := strings.TrimSpace(budget.Consumer)
		if (consumer == telemetry.RESTConsumerWorker || consumer == telemetry.RESTConsumerSharedPool) && strings.TrimSpace(budget.CredentialIdentity) != "" {
			return budget, true
		}
	}
	return telemetry.RESTBudget{}, false
}

func (o *Orchestrator) handleGitHubMonitorCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	failure, unavailable := workerGitHubMonitorFailureFromCompletion(event, state)
	if !unavailable {
		return false
	}
	if event.Result.Tokens != (TokenTotals{}) {
		running.Tokens = event.Result.Tokens
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, running.Tokens)
	if diffStatsPresent(running.DiffStats) {
		state.DiffStats[event.IssueID] = running.DiffStats
	}
	condition := o.registerGitHubMonitor(state, failure, running, event.CompletedAt)
	o.finishForgeAvailabilityProbe(state, event, running)
	if event.Result.TurnStarted {
		o.recoverBackendCapacity(state, running, condition.LastObservedAt)
	} else {
		o.deferBackendCapacityProbe(state, running, condition.LastObservedAt, event.Err)
	}
	o.releaseTerminalAttemptClaim(ctx, state, running.Issue, event.CompletedAt)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		workerGitHubMonitorErrorClass,
		failure.Message,
		"waiting",
		workerGitHubMonitorStatusMessage(condition),
		map[string]any{"worker_github_monitor_wait": workerGitHubMonitorMetadata(condition)},
	)
	if !workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		state.Retry[running.Issue.ID] = Retry{
			Issue:            cloneIssue(running.Issue),
			Attempt:          running.Attempt,
			DueAt:            condition.NextProbeAt,
			Error:            failure.Message,
			WorkerHost:       running.WorkerHost,
			GitHubMonitor:    true,
			GitHubCredential: condition.CredentialIdentity,
		}
	}
	if state.FailureBreaker.Active() && state.FailureBreaker.CanaryIssueID == running.Issue.ID {
		o.deferProjectFailureBreakerCanary(state, running.Issue.ID, event.CompletedAt, condition.NextProbeAt.Sub(event.CompletedAt))
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt.UTC(),
		Event:   workerGitHubMonitorErrorClass,
		Message: "waiting for worker GitHub REST monitor recovery for credential " + condition.CredentialIdentity + " without tripping a failure breaker",
	})
	telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, workerGitHubMonitorErrorClass, "worker GitHub REST budget monitor unavailable", o.runningLifecycleCorrelation(running.Issue, running),
		"credential_identity", condition.CredentialIdentity,
		"consumer", condition.Consumer,
		"operation", condition.Operation,
		"next_probe_at", condition.NextProbeAt,
		"probe_attempts", condition.ProbeAttempts,
		"error", event.Err,
	)
	return true
}

func (o *Orchestrator) registerGitHubMonitor(
	state *State,
	failure workerGitHubMonitorFailure,
	running Running,
	observedAt time.Time,
) GitHubMonitor {
	if observedAt.IsZero() {
		observedAt = o.clockNow()
	}
	observedAt = observedAt.UTC()
	if state.GitHubMonitors == nil {
		state.GitHubMonitors = map[string]GitHubMonitor{}
	}
	key := strings.TrimSpace(failure.CredentialIdentity)
	condition, exists := state.GitHubMonitors[key]
	if !exists {
		condition = GitHubMonitor{
			ProjectID:          strings.TrimSpace(o.cfg.Project.ID),
			CredentialIdentity: key,
			DetectedAt:         observedAt,
		}
	}
	probeFailed := condition.ProbeIssueID == running.Issue.ID
	condition.Consumer = strings.TrimSpace(failure.Consumer)
	condition.Operation = strings.TrimSpace(failure.Operation)
	condition.LastObservedAt = observedAt
	condition.LastError = strings.TrimSpace(failure.Message)
	if probeFailed {
		condition.ProbeIssueID = ""
		condition.LastProbeAt = observedAt
		condition.LastProbeResult = "failed"
		condition.LastProbeDetail = condition.LastError
	}
	if !exists || probeFailed || condition.NextProbeAt.IsZero() {
		condition.NextProbeAt = observedAt.Add(backendCapacityProbeDelayForAttempt(condition.ProbeAttempts)).UTC()
	}
	state.GitHubMonitors[key] = condition
	return condition
}

func workerGitHubMonitorMetadata(condition GitHubMonitor) workerGitHubMonitorWaitMetadata {
	return workerGitHubMonitorWaitMetadata{
		CredentialIdentity: condition.CredentialIdentity,
		Consumer:           condition.Consumer,
		Operation:          condition.Operation,
		DetectedAt:         condition.DetectedAt,
		LastObservedAt:     condition.LastObservedAt,
		NextProbeAt:        condition.NextProbeAt,
		LastProbeAt:        condition.LastProbeAt,
		LastProbeResult:    condition.LastProbeResult,
		LastProbeDetail:    condition.LastProbeDetail,
		ProbeAttempts:      condition.ProbeAttempts,
	}
}

func workerGitHubMonitorStatusMessage(condition GitHubMonitor) string {
	message := "worker GitHub REST budget monitor unavailable for credential " + strings.TrimSpace(condition.CredentialIdentity)
	if !condition.NextProbeAt.IsZero() {
		message += "; next canary at " + condition.NextProbeAt.UTC().Format(time.RFC3339)
	}
	return message
}

func workerGitHubMonitorBlocks(state *State, issueID string, retry Retry, now time.Time) bool {
	if state == nil || len(state.GitHubMonitors) == 0 {
		return false
	}
	if retry.GitHubMonitor {
		condition, ok := state.GitHubMonitors[strings.TrimSpace(retry.GitHubCredential)]
		if !ok || condition.ProbeIssueID == issueID {
			return false
		}
		return condition.ProbeIssueID != "" || now.Before(condition.NextProbeAt)
	}
	for _, condition := range state.GitHubMonitors {
		if condition.ProbeIssueID != issueID {
			return true
		}
	}
	return false
}

func reserveWorkerGitHubMonitorProbe(state *State, issueID string, retry Retry, now time.Time) (string, bool) {
	if state == nil || !retry.GitHubMonitor {
		return "", false
	}
	key := strings.TrimSpace(retry.GitHubCredential)
	condition, ok := state.GitHubMonitors[key]
	if !ok || condition.ProbeIssueID != "" || now.Before(condition.NextProbeAt) {
		return "", false
	}
	condition.ProbeIssueID = issueID
	condition.ProbeAttempts++
	condition.LastProbeAt = now.UTC()
	condition.LastProbeResult = "in_progress"
	condition.LastProbeDetail = "worker GitHub REST monitor canary started"
	condition.NextProbeAt = time.Time{}
	state.GitHubMonitors[key] = condition
	return key, true
}

func releaseWorkerGitHubMonitorProbe(state *State, issueID string, result string, detail string, now time.Time) {
	if state == nil {
		return
	}
	for key, condition := range state.GitHubMonitors {
		if condition.ProbeIssueID != issueID {
			continue
		}
		condition.ProbeIssueID = ""
		condition.LastProbeAt = now.UTC()
		condition.LastProbeResult = strings.TrimSpace(result)
		condition.LastProbeDetail = strings.TrimSpace(detail)
		if condition.NextProbeAt.IsZero() {
			condition.NextProbeAt = now.Add(backendCapacityProbeDelayForAttempt(condition.ProbeAttempts)).UTC()
		}
		state.GitHubMonitors[key] = condition
	}
}

func reservedGitHubCredential(state *State, issueID string) string {
	if state == nil {
		return ""
	}
	for key, condition := range state.GitHubMonitors {
		if condition.ProbeIssueID == issueID {
			return key
		}
	}
	return ""
}

func (o *Orchestrator) recoverWorkerGitHubMonitorFromUpdate(state *State, running Running, limits *telemetry.RateLimits, observedAt time.Time) {
	credential := strings.TrimSpace(running.GitHubCredential)
	if state == nil || credential == "" || limits == nil {
		return
	}
	for _, budget := range limits.GitHubRESTBudgets {
		budgetCredential := strings.TrimSpace(budget.CredentialIdentity)
		if credential != workerGitHubMonitorUnknownCredential && budgetCredential != credential {
			continue
		}
		if credential == workerGitHubMonitorUnknownCredential && budgetCredential == "" {
			continue
		}
		if budget.ObservedAt != nil && !budget.ObservedAt.IsZero() {
			observedAt = budget.ObservedAt.UTC()
		}
		o.completeWorkerGitHubMonitorRecovery(state, credential, observedAt)
		return
	}
}

func (o *Orchestrator) completeWorkerGitHubMonitorRecovery(state *State, credential string, recoveredAt time.Time) {
	credential = strings.TrimSpace(credential)
	condition, ok := state.GitHubMonitors[credential]
	if !ok {
		return
	}
	if recoveredAt.IsZero() {
		recoveredAt = o.clockNow()
	}
	delete(state.GitHubMonitors, credential)
	for issueID, retry := range state.Retry {
		if !retry.GitHubMonitor || strings.TrimSpace(retry.GitHubCredential) != credential {
			continue
		}
		retry.DueAt = recoveredAt.UTC()
		retry.GitHubMonitor = false
		state.Retry[issueID] = retry
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt.UTC(),
		Event:   "worker_github_budget_monitor_recovered",
		Message: "worker GitHub REST budget monitor recovered for credential " + credential + " via canary",
	})
	telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "worker_github_budget_monitor_recovered", "worker GitHub REST budget monitor recovered", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
		"credential_identity", credential,
		"detected_at", condition.DetectedAt,
		"recovered_at", recoveredAt,
		"source", "canary",
	)
}

func (o *Orchestrator) recoverWorkerGitHubMonitorWaits(ctx context.Context, state *State, attempts []store.WorkAttempt, now time.Time) {
	if state == nil || len(attempts) == 0 {
		return
	}
	latest := latestStoreTerminalAttemptsByIssue(attempts)
	waits := make(map[string]workerGitHubMonitorWaitMetadata, len(latest))
	issueIDs := make([]string, 0, len(latest))
	for issueID, attempt := range latest {
		metadata, ok := workerGitHubMonitorWaitMetadataFromAttempt(attempt)
		if !ok {
			continue
		}
		waits[issueID] = metadata
		issueIDs = append(issueIDs, issueID)
	}
	if len(waits) == 0 {
		return
	}
	issuesByID, validated := o.validateGitHubRESTWaitIssues(ctx, issueIDs)
	for issueID, metadata := range waits {
		attempt := latest[issueID]
		issue := githubRESTWaitIssueFromAttempt(attempt)
		if validated {
			var ok bool
			issue, ok = issuesByID[issueID]
			if !ok || !stateIn(issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
				continue
			}
		}
		o.restoreWorkerGitHubMonitorWait(state, issue, attempt, metadata, now)
	}
}

func workerGitHubMonitorWaitMetadataFromAttempt(attempt store.WorkAttempt) (workerGitHubMonitorWaitMetadata, bool) {
	if attempt.TerminalState != store.WorkAttemptTerminalCapacity || strings.TrimSpace(attempt.ErrorClass) != workerGitHubMonitorErrorClass {
		return workerGitHubMonitorWaitMetadata{}, false
	}
	var metadata struct {
		Wait workerGitHubMonitorWaitMetadata `json:"worker_github_monitor_wait"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
		return workerGitHubMonitorWaitMetadata{}, false
	}
	wait := metadata.Wait
	wait.CredentialIdentity = strings.TrimSpace(wait.CredentialIdentity)
	wait.Consumer = strings.TrimSpace(wait.Consumer)
	wait.Operation = strings.TrimSpace(wait.Operation)
	if wait.CredentialIdentity == "" ||
		wait.Consumer != telemetry.RESTConsumerWorker && wait.Consumer != telemetry.RESTConsumerSharedPool ||
		wait.Operation == "" || wait.DetectedAt.IsZero() || wait.LastObservedAt.IsZero() || wait.NextProbeAt.IsZero() || wait.ProbeAttempts < 0 {
		return workerGitHubMonitorWaitMetadata{}, false
	}
	return wait, true
}

func (o *Orchestrator) restoreWorkerGitHubMonitorWait(
	state *State,
	issue connector.Issue,
	attempt store.WorkAttempt,
	metadata workerGitHubMonitorWaitMetadata,
	now time.Time,
) {
	if state.GitHubMonitors == nil {
		state.GitHubMonitors = map[string]GitHubMonitor{}
	}
	nextProbeAt := metadata.NextProbeAt.UTC()
	if nextProbeAt.Before(now) {
		nextProbeAt = now.UTC()
	}
	condition, exists := state.GitHubMonitors[metadata.CredentialIdentity]
	if !exists {
		condition = GitHubMonitor{
			ProjectID:          strings.TrimSpace(o.cfg.Project.ID),
			CredentialIdentity: metadata.CredentialIdentity,
			DetectedAt:         metadata.DetectedAt.UTC(),
		}
	}
	if condition.DetectedAt.IsZero() || metadata.DetectedAt.Before(condition.DetectedAt) {
		condition.DetectedAt = metadata.DetectedAt.UTC()
	}
	if condition.LastObservedAt.IsZero() || metadata.LastObservedAt.After(condition.LastObservedAt) {
		condition.Consumer = metadata.Consumer
		condition.Operation = metadata.Operation
		condition.LastObservedAt = metadata.LastObservedAt.UTC()
		condition.LastError = strings.TrimSpace(attempt.ErrorMessage)
		condition.LastProbeAt = metadata.LastProbeAt.UTC()
		condition.LastProbeResult = metadata.LastProbeResult
		condition.LastProbeDetail = metadata.LastProbeDetail
	}
	if condition.NextProbeAt.IsZero() || nextProbeAt.Before(condition.NextProbeAt) {
		condition.NextProbeAt = nextProbeAt
	}
	condition.ProbeAttempts = max(condition.ProbeAttempts, metadata.ProbeAttempts)
	state.GitHubMonitors[metadata.CredentialIdentity] = condition
	state.Retry[issue.ID] = Retry{
		Issue:            cloneIssue(issue),
		Attempt:          attempt.AttemptNumber,
		DueAt:            nextProbeAt,
		Error:            strings.TrimSpace(attempt.ErrorMessage),
		WorkerHost:       strings.TrimSpace(attempt.WorkerHost),
		GitHubMonitor:    true,
		GitHubCredential: metadata.CredentialIdentity,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now.UTC(),
		Event:   "worker_github_budget_monitor_wait_restored",
		Message: fmt.Sprintf("restored durable worker GitHub REST monitor wait for %s on credential %s", issueLabel(issue), metadata.CredentialIdentity),
	})
}
