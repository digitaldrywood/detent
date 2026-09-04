package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	workerGitHubTokenResolutionErrorClass = "worker_github_token_resolution_unavailable"
	workerGitHubTokenResolutionWaitKind   = "worker_github_token_resolution"
)

type workerGitHubTokenResolutionWaitMetadata struct {
	Attempts    int       `json:"attempts"`
	TimeoutMS   int64     `json:"timeout_ms"`
	DetectedAt  time.Time `json:"detected_at"`
	NextRetryAt time.Time `json:"next_retry_at"`
}

func (o *Orchestrator) handleWorkerGitHubTokenResolutionCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	resolutionErr, unavailable := runpkg.AsWorkerGitHubTokenResolutionError(event.Err)
	if !unavailable {
		return false
	}
	detectedAt := event.CompletedAt.UTC()
	delay := o.cfg.OverloadRetryDelay
	if delay <= 0 {
		delay = defaultOverloadRetryDelay
	}
	nextRetryAt := detectedAt.Add(delay)
	o.finishForgeAvailabilityProbe(state, event, running)
	o.deferBackendCapacityProbe(state, running, detectedAt, event.Err)
	releaseWorkerGitHubMonitorProbe(state, event.IssueID, "deferred", errorString(event.Err), detectedAt)
	o.releaseTerminalAttemptClaim(ctx, state, running.Issue, detectedAt)
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	metadata := workerGitHubTokenResolutionWaitMetadata{
		Attempts:    resolutionErr.Attempts,
		TimeoutMS:   resolutionErr.Timeout.Milliseconds(),
		DetectedAt:  detectedAt,
		NextRetryAt: nextRetryAt,
	}
	o.completeDurableWorkAttemptWithMetadata(
		ctx,
		state,
		running,
		detectedAt,
		store.WorkAttemptTerminalCapacity,
		workerGitHubTokenResolutionErrorClass,
		errorString(event.Err),
		"waiting",
		"waiting for worker GitHub token resolution",
		map[string]any{"worker_github_token_resolution_wait": metadata},
	)
	if !workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		state.Retry[running.Issue.ID] = Retry{
			Issue:      cloneIssue(running.Issue),
			Attempt:    running.Attempt,
			DueAt:      nextRetryAt,
			Error:      errorString(event.Err),
			WorkerHost: running.WorkerHost,
			Wait: RetryWait{
				Kind:      workerGitHubTokenResolutionWaitKind,
				StartedAt: detectedAt,
			},
		}
	}
	if state.FailureBreaker.Active() && state.FailureBreaker.CanaryIssueID == running.Issue.ID {
		o.deferProjectFailureBreakerCanary(state, running.Issue.ID, detectedAt, nextRetryAt.Sub(detectedAt))
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      detectedAt,
		Event:   workerGitHubTokenResolutionErrorClass,
		Message: "worker GitHub token resolution unavailable; host credential store may be slow; retrying " + issueLabel(running.Issue) + " at " + nextRetryAt.Format(time.RFC3339),
	})
	telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, workerGitHubTokenResolutionErrorClass, "worker GitHub token resolution unavailable; host credential store may be slow", o.runningLifecycleCorrelation(running.Issue, running),
		"resolution_attempts", resolutionErr.Attempts,
		"resolution_timeout_ms", resolutionErr.Timeout.Milliseconds(),
		"next_retry_at", nextRetryAt,
		"error", event.Err,
	)
	return true
}

func (o *Orchestrator) recoverWorkerGitHubTokenResolutionWaits(ctx context.Context, state *State, attempts []store.WorkAttempt, now time.Time) {
	if state == nil || len(attempts) == 0 {
		return
	}
	latest := latestStoreTerminalAttemptsByIssue(attempts)
	waits := make(map[string]workerGitHubTokenResolutionWaitMetadata, len(latest))
	issueIDs := make([]string, 0, len(latest))
	for issueID, attempt := range latest {
		metadata, ok := workerGitHubTokenResolutionWaitMetadataFromAttempt(attempt)
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
		issue := workerGitHubTokenResolutionIssueFromAttempt(attempt)
		if validated {
			var ok bool
			issue, ok = issuesByID[issueID]
			if !ok || !stateIn(issue.State, o.cfg.ActiveStates) || workspaceIssueTerminal(issue, o.cfg.TerminalStates) {
				continue
			}
		}
		nextRetryAt := metadata.NextRetryAt.UTC()
		if nextRetryAt.Before(now) {
			nextRetryAt = now.UTC()
		}
		state.Retry[issue.ID] = Retry{
			Issue:      cloneIssue(issue),
			Attempt:    attempt.AttemptNumber,
			DueAt:      nextRetryAt,
			Error:      strings.TrimSpace(attempt.ErrorMessage),
			WorkerHost: strings.TrimSpace(attempt.WorkerHost),
			Wait: RetryWait{
				Kind:      workerGitHubTokenResolutionWaitKind,
				StartedAt: metadata.DetectedAt.UTC(),
			},
		}
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now.UTC(),
			Event:   "worker_github_token_resolution_wait_restored",
			Message: "restored durable worker GitHub token resolution wait for " + issueLabel(issue),
		})
	}
}

func workerGitHubTokenResolutionWaitMetadataFromAttempt(attempt store.WorkAttempt) (workerGitHubTokenResolutionWaitMetadata, bool) {
	if attempt.TerminalState != store.WorkAttemptTerminalCapacity || strings.TrimSpace(attempt.ErrorClass) != workerGitHubTokenResolutionErrorClass {
		return workerGitHubTokenResolutionWaitMetadata{}, false
	}
	var metadata struct {
		Wait workerGitHubTokenResolutionWaitMetadata `json:"worker_github_token_resolution_wait"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
		return workerGitHubTokenResolutionWaitMetadata{}, false
	}
	wait := metadata.Wait
	if wait.Attempts <= 0 || wait.TimeoutMS <= 0 || wait.DetectedAt.IsZero() || wait.NextRetryAt.IsZero() {
		return workerGitHubTokenResolutionWaitMetadata{}, false
	}
	return wait, true
}

func workerGitHubTokenResolutionIssueFromAttempt(attempt store.WorkAttempt) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = strings.TrimSpace(attempt.IssueID)
	issue.Identifier = strings.TrimSpace(attempt.Identifier)
	issue.URL = strings.TrimSpace(attempt.IssueURL)
	issue.State = strings.TrimSpace(attempt.Lane)
	issue.PRRepository = strings.TrimSpace(attempt.Repo)
	if attempt.PRNumber != nil && *attempt.PRNumber > 0 {
		number := int(*attempt.PRNumber)
		issue.PRNumber = &number
	}
	return issue
}
