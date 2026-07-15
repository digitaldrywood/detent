package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	githubRESTCapacityBackendID = "github-rest"
	githubRESTCapacityKind      = "github_rest_rate_limit"
	githubRESTCapacityError     = "github_rest_capacity"
)

var githubRESTCapacityScope = backendcapacity.Scope{
	BackendID:   githubRESTCapacityBackendID,
	BackendKind: "tracker",
	Provider:    "github",
}

func (o *Orchestrator) syncGitHubRESTCapacityOutage(state *State, now time.Time) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = o.clockNow()
	}
	key, existing, exists := githubRESTCapacityOutage(state.BackendOutages)
	bucket := gitHubRESTBucketFromState(state)
	if bucket == nil {
		return
	}
	if !gitHubRESTCapacityExceeded(bucket, o.cfg.GitHubRESTMinReserve, now) {
		if exists {
			delete(state.BackendOutages, key)
			o.activateDispatchRecovery(
				state,
				dispatchRecoveryGitHubREST,
				existing.Reason,
				now,
				"",
			)
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      now,
				Event:   "github_rest_capacity_recovered",
				Message: "GitHub REST capacity recovered; dispatch resumed",
			})
		}
		return
	}

	if state.BackendOutages == nil {
		state.BackendOutages = map[string]BackendOutage{}
	}
	detectedAt := now
	if exists && !existing.DetectedAt.IsZero() {
		detectedAt = existing.DetectedAt
	}
	resetAt := bucket.ResetAt.UTC()
	outage := BackendOutage{
		Scope:          githubRESTCapacityScope,
		Kind:           githubRESTCapacityKind,
		Reason:         fmt.Sprintf("GitHub REST remaining %d is at or below dispatch floor %d", bucket.Remaining, o.cfg.GitHubRESTMinReserve),
		DetectedAt:     detectedAt,
		LastObservedAt: now,
		ResetAt:        resetAt,
		ResumeAt:       resetAt,
	}
	state.BackendOutages[githubRESTCapacityScope.Key()] = outage
	o.markDispatchRecoveryWait(state, dispatchRecoveryGitHubREST, outage.Reason, outage.ResumeAt, now)
	if !exists {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now,
			Event:   "github_rest_capacity_paused",
			Message: githubRESTCapacityStatusMessage(outage),
		})
	}
}

func gitHubRESTCapacityExceeded(bucket *telemetry.RateLimitBucket, floor int64, now time.Time) bool {
	return bucket != nil && bucket.Limit > 0 && bucket.ResetAt != nil && bucket.ResetAt.After(now) && bucket.Remaining <= floor
}

func githubRESTCapacityOutage(outages map[string]BackendOutage) (string, BackendOutage, bool) {
	for key, outage := range outages {
		if strings.TrimSpace(outage.Kind) == githubRESTCapacityKind {
			return key, outage, true
		}
	}
	return "", BackendOutage{}, false
}

func activeGitHubRESTCapacityOutage(state *State, now time.Time) (BackendOutage, bool) {
	if state == nil {
		return BackendOutage{}, false
	}
	_, outage, ok := githubRESTCapacityOutage(state.BackendOutages)
	return outage, ok && outage.ResumeAt.After(now)
}

func githubRESTCapacityStatusMessage(outage BackendOutage) string {
	message := "GitHub REST dispatch paused"
	if !outage.ResumeAt.IsZero() {
		message += " — resuming at " + outage.ResumeAt.UTC().Format(time.RFC3339)
	}
	return message
}

func (o *Orchestrator) handleGitHubRESTCapacityCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	outage, ok := activeGitHubRESTCapacityOutage(state, event.CompletedAt)
	if !ok {
		return false
	}
	running = o.restoreBackendCapacityIssueState(ctx, state, running, event.CompletedAt)
	errorMessage := githubRESTCapacityStatusMessage(outage)
	if event.Err != nil {
		errorMessage = event.Err.Error()
	}
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		githubRESTCapacityError,
		errorMessage,
		"waiting",
		githubRESTCapacityStatusMessage(outage),
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return true
	}
	o.scheduleBackendCapacityRetry(state, running, outage)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "github_rest_capacity_completion_deferred",
		Message: githubRESTCapacityStatusMessage(outage),
	})
	return true
}
