package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	githubRESTCapacityBackendID = "github-rest"
	githubRESTCapacityKind      = "github_rest_rate_limit"
	githubRESTCapacityError     = "github_rest_capacity"

	blockedRecoveryPredicateGitHubRESTBudget = "github_rest_budget_recovered"
	workflowActionGitHubRESTBudgetRecovery   = "github_rest_budget_recovery"
	githubRESTBudgetWaitingReason            = "github_rest_budget_wait"
	githubRESTBudgetObservationPendingReason = "github_rest_budget_observation_pending"
	githubRESTBudgetRearmConsumedReason      = "github_rest_budget_rearm_consumed"
	repeatedFailureCircuitBreakerCause       = "repeated_failure_circuit_breaker"
	githubRESTBudgetProbeRetryInterval       = time.Minute
	legacyGitHubRESTBudgetStageUpdateSkew    = 10 * time.Second
)

var githubRESTCapacityScope = backendcapacity.Scope{
	BackendID:   githubRESTCapacityBackendID,
	BackendKind: "tracker",
	Provider:    "github",
}

type githubRESTBudgetEvidence struct {
	Consumer           string
	CredentialIdentity string
	Remaining          int64
	Limit              int64
	Reserve            int64
	ResetAt            time.Time
	ObservedAt         time.Time
	TargetState        string
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
	running.Issue, _ = o.demoteTerminalAttemptRetry(ctx, state, running.Issue, running.WorkProductPushed, event.CompletedAt)
	o.scheduleBackendCapacityRetry(state, running, outage)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   "github_rest_capacity_completion_deferred",
		Message: githubRESTCapacityStatusMessage(outage),
	})
	return true
}

func isGitHubRESTBudgetHeadroomError(err error) bool {
	return err != nil && (errors.Is(err, runpkg.ErrWorkerGitHubRESTReserved) || isGitHubRESTBudgetHeadroomMessage(err.Error()))
}

func isGitHubRESTBudgetHeadroomMessage(message string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(message)), "worker github rest budget reached reserved headroom")
}

func githubRESTBudgetEvidenceFromError(err error) (githubRESTBudgetEvidence, bool) {
	if !isGitHubRESTBudgetHeadroomError(err) {
		return githubRESTBudgetEvidence{}, false
	}
	return githubRESTBudgetEvidenceFromMessage(err.Error())
}

func githubRESTBudgetEvidenceFromMessage(message string) (githubRESTBudgetEvidence, bool) {
	if !isGitHubRESTBudgetHeadroomMessage(message) {
		return githubRESTBudgetEvidence{}, false
	}
	evidence := githubRESTBudgetEvidence{}
	for _, field := range strings.Fields(message) {
		key, value, ok := strings.Cut(strings.Trim(field, ",;"), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "consumer":
			evidence.Consumer = strings.TrimSpace(value)
		case "credential_identity":
			evidence.CredentialIdentity = strings.TrimSpace(value)
		case "remaining":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				evidence.Remaining = parsed
			}
		case "reserve":
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				evidence.Reserve = parsed
			}
		case "reset_at":
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
				evidence.ResetAt = parsed
			}
		}
	}
	return evidence, evidence.Reserve > 0
}

func (o *Orchestrator) githubRESTBudgetParkEvidence(state *State, err error, targetState string) (githubRESTBudgetEvidence, bool) {
	evidence, ok := githubRESTBudgetEvidenceFromError(err)
	if !ok {
		return githubRESTBudgetEvidence{}, false
	}
	evidence.TargetState = o.repeatedFailureRecoveryTarget(targetState)
	if current, currentOK := currentGitHubRESTBudget(state, evidence); currentOK {
		evidence.Limit = current.Limit
		if evidence.ResetAt.IsZero() {
			evidence.ResetAt = current.ResetAt
		}
		evidence.ObservedAt = current.ObservedAt
		evidence.CredentialIdentity = current.CredentialIdentity
		if current.Consumer != "" {
			evidence.Consumer = current.Consumer
		}
	}
	return evidence, true
}

func applyGitHubRESTBudgetEvidence(metadata *workflowLaneBlockedRecoveryMetadata, evidence githubRESTBudgetEvidence) {
	if metadata == nil {
		return
	}
	metadata.ResourceKind = githubRESTCapacityKind
	metadata.ResourceConsumer = strings.TrimSpace(evidence.Consumer)
	metadata.ResourceCredential = strings.TrimSpace(evidence.CredentialIdentity)
	metadata.ResourceRemaining = evidence.Remaining
	metadata.ResourceLimit = evidence.Limit
	metadata.ResourceReserve = evidence.Reserve
	metadata.ResourceResetAt = githubRESTBudgetTimeValue(evidence.ResetAt)
	metadata.ResourceObservedAt = githubRESTBudgetTimeValue(evidence.ObservedAt)
}

func githubRESTBudgetTimeValue(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func githubRESTBudgetEvidenceFromMetadata(park workflowLaneBlockedRecoveryMetadata) (githubRESTBudgetEvidence, bool) {
	if strings.TrimSpace(park.ResourceKind) != githubRESTCapacityKind || park.ResourceReserve <= 0 {
		return githubRESTBudgetEvidence{}, false
	}
	resetAt := githubRESTBudgetMetadataTime(park.ResourceResetAt)
	observedAt := githubRESTBudgetMetadataTime(park.ResourceObservedAt)
	return githubRESTBudgetEvidence{
		Consumer:           strings.TrimSpace(park.ResourceConsumer),
		CredentialIdentity: strings.TrimSpace(park.ResourceCredential),
		Remaining:          park.ResourceRemaining,
		Limit:              park.ResourceLimit,
		Reserve:            park.ResourceReserve,
		ResetAt:            resetAt,
		ObservedAt:         observedAt,
		TargetState:        strings.TrimSpace(park.TargetState),
	}, true
}

func githubRESTBudgetMetadataTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (o *Orchestrator) repeatedFailureGitHubRESTBudgetEvidence(
	ctx context.Context,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
) (githubRESTBudgetEvidence, bool) {
	if strings.TrimSpace(park.Cause) != repeatedFailureCircuitBreakerCause {
		return githubRESTBudgetEvidence{}, false
	}
	if evidence, ok := githubRESTBudgetEvidenceFromMetadata(park); ok {
		evidence.TargetState = o.repeatedFailureRecoveryTarget(evidence.TargetState)
		return evidence, true
	}
	if o == nil || o.workAttempts == nil {
		return githubRESTBudgetEvidence{}, false
	}
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		Limit:      repeatedFailureThreshold,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("repeated failure REST budget evidence unavailable", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return githubRESTBudgetEvidence{}, false
	}
	if len(attempts) < repeatedFailureThreshold {
		return githubRESTBudgetEvidence{}, false
	}
	var evidence githubRESTBudgetEvidence
	for index, attempt := range attempts[:repeatedFailureThreshold] {
		candidate, ok := githubRESTBudgetEvidenceFromMessage(attempt.ErrorMessage)
		if !ok {
			return githubRESTBudgetEvidence{}, false
		}
		if index == 0 {
			evidence = candidate
			evidence.TargetState = o.repeatedFailureRecoveryTarget(attempt.Lane)
		}
	}
	return evidence, true
}

func (o *Orchestrator) currentLegacyRepeatedFailureGitHubRESTBudgetPark(
	ctx context.Context,
	issue connector.Issue,
) (workflowLaneBlockedRecoveryMetadata, bool) {
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok ||
		normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) ||
		entry.Metadata.BlockedRecovery == nil ||
		issue.StageUpdatedAt == nil ||
		issue.StageUpdatedAt.IsZero() ||
		entry.Event.StartedAt.IsZero() ||
		entry.Event.StartedAt.Before(issue.StageUpdatedAt.Add(-legacyGitHubRESTBudgetStageUpdateSkew)) {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	park := *entry.Metadata.BlockedRecovery
	if !strings.EqualFold(strings.TrimSpace(park.Owner), blockedRecoveryOwnerOrchestrator) ||
		strings.TrimSpace(park.Cause) != repeatedFailureCircuitBreakerCause ||
		strings.TrimSpace(park.Predicate) != blockedRecoveryPredicateFingerprintChange ||
		strings.TrimSpace(park.ResourceKind) != "" {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	evidence, ok := o.repeatedFailureGitHubRESTBudgetEvidence(ctx, issue, park)
	if !ok {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	park.Predicate = blockedRecoveryPredicateGitHubRESTBudget
	park.TargetState = o.repeatedFailureRecoveryTarget(evidence.TargetState)
	applyGitHubRESTBudgetEvidence(&park, evidence)
	return park, true
}

func currentGitHubRESTBudget(state *State, match githubRESTBudgetEvidence) (githubRESTBudgetEvidence, bool) {
	if state == nil || state.RateLimits == nil {
		return githubRESTBudgetEvidence{}, false
	}
	var current githubRESTBudgetEvidence
	for _, budget := range state.RateLimits.GitHubRESTBudgets {
		if budget.Limit <= 0 || budget.ObservedAt == nil {
			continue
		}
		consumer := strings.TrimSpace(budget.Consumer)
		if consumer == "" {
			consumer = telemetry.RESTConsumerOrchestrator
		}
		if match.CredentialIdentity != "" && strings.TrimSpace(budget.CredentialIdentity) != match.CredentialIdentity {
			continue
		}
		if match.Consumer != "" && consumer != match.Consumer {
			continue
		}
		if match.CredentialIdentity == "" && match.Consumer == "" {
			if budget.MinRemainingReserve != match.Reserve || consumer == telemetry.RESTConsumerOrchestrator {
				continue
			}
		}
		if current.ObservedAt.IsZero() || budget.ObservedAt.After(current.ObservedAt) {
			current = githubRESTBudgetEvidence{
				Consumer:           consumer,
				CredentialIdentity: strings.TrimSpace(budget.CredentialIdentity),
				Remaining:          budget.Remaining,
				Limit:              budget.Limit,
				Reserve:            match.Reserve,
				ObservedAt:         budget.ObservedAt.UTC(),
			}
			if budget.ResetAt != nil {
				current.ResetAt = budget.ResetAt.UTC()
			}
		}
	}
	return current, current.Limit > 0
}

func (o *Orchestrator) probeGitHubRESTBudgetPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	evidence githubRESTBudgetEvidence,
	now time.Time,
) {
	if o == nil || o.githubRESTBudgetProber == nil || !evidence.ResetAt.IsZero() && evidence.ResetAt.After(now) {
		return
	}
	key := strings.TrimSpace(evidence.Consumer) + "\x00" + strings.TrimSpace(evidence.CredentialIdentity)
	if o.githubRESTBudgetProbes == nil {
		o.githubRESTBudgetProbes = map[string]time.Time{}
	}
	if next := o.githubRESTBudgetProbes[key]; next.After(now) {
		return
	}
	o.githubRESTBudgetProbes[key] = now.Add(githubRESTBudgetProbeRetryInterval)
	budget, supported, err := o.githubRESTBudgetProber.ProbeGitHubRESTBudget(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("GitHub REST budget recovery probe failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return
	}
	if !supported || !gitHubRESTBudgetMatchesEvidence(budget, evidence) {
		return
	}
	if state.RateLimits == nil {
		state.RateLimits = &telemetry.RateLimits{}
	}
	state.RateLimits.GitHubRESTBudgets = mergeRESTBudgets(state.RateLimits.GitHubRESTBudgets, []telemetry.RESTBudget{budget})
}

func gitHubRESTBudgetMatchesEvidence(budget telemetry.RESTBudget, evidence githubRESTBudgetEvidence) bool {
	consumer := strings.TrimSpace(budget.Consumer)
	if consumer == "" {
		consumer = telemetry.RESTConsumerOrchestrator
	}
	if evidence.Consumer != "" && consumer != evidence.Consumer {
		return false
	}
	if evidence.CredentialIdentity != "" && strings.TrimSpace(budget.CredentialIdentity) != evidence.CredentialIdentity {
		return false
	}
	return evidence.CredentialIdentity != "" || evidence.Consumer != "" || budget.MinRemainingReserve == evidence.Reserve
}

func (o *Orchestrator) repeatedFailureRecoveryTarget(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || normalizeState(candidate) == normalizeState(blockedStatusState) || stateIn(candidate, o.cfg.TerminalStates) {
		return "Todo"
	}
	return candidate
}

func (o *Orchestrator) reconcileRepeatedFailureGitHubRESTBudgetPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	evidence, ok := o.repeatedFailureGitHubRESTBudgetEvidence(ctx, issue, park)
	if !ok {
		return false, false
	}
	park.Predicate = blockedRecoveryPredicateGitHubRESTBudget
	park.TargetState = o.repeatedFailureRecoveryTarget(evidence.TargetState)
	applyGitHubRESTBudgetEvidence(&park, evidence)
	blockedAt := workflowLaneFallbackAt(issue)
	if entry, exists := state.Blocked[strings.TrimSpace(issue.ID)]; exists && !entry.BlockedAt.IsZero() {
		blockedAt = entry.BlockedAt
	}
	current, observed := currentGitHubRESTBudget(state, evidence)
	if observed {
		evidence.Consumer = current.Consumer
		evidence.CredentialIdentity = current.CredentialIdentity
		if evidence.ResetAt.IsZero() {
			evidence.ResetAt = current.ResetAt
		}
		applyGitHubRESTBudgetEvidence(&park, evidence)
	}
	if !observed || current.ObservedAt.IsZero() || !current.ObservedAt.After(blockedAt) || current.ResetAt.IsZero() || !current.ResetAt.After(now) {
		o.probeGitHubRESTBudgetPark(ctx, state, issue, evidence, now)
		current, observed = currentGitHubRESTBudget(state, evidence)
	}
	if !observed || current.ObservedAt.IsZero() || !current.ObservedAt.After(blockedAt) || current.ResetAt.IsZero() || !current.ResetAt.After(now) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", githubRESTBudgetObservationPendingReason, &park, "")
		o.setGitHubRESTBudgetParkSurface(state, issue.ID, githubRESTBudgetStatusMessage("waiting for a fresh observation", evidence))
		return true, false
	}
	applyGitHubRESTBudgetEvidence(&park, current)
	if current.Remaining <= evidence.Reserve {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", githubRESTBudgetWaitingReason, &park, "")
		o.setGitHubRESTBudgetParkSurface(state, issue.ID, githubRESTBudgetStatusMessage("waiting for capacity", current))
		return true, false
	}
	signature := githubRESTBudgetRecoverySignature(current, evidence.Reserve)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionGitHubRESTBudgetRecovery, signature); consumed {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", githubRESTBudgetRearmConsumedReason, &park, signature)
		o.setGitHubRESTBudgetParkSurface(state, issue.ID, githubRESTBudgetStatusMessage("capacity recovered; automatic re-arm already used for this reset window", current))
		return true, false
	}
	targetState := o.repeatedFailureRecoveryTarget(evidence.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionGitHubRESTBudgetRecovery, signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, targetState, now, workflowActionGitHubRESTBudgetRecovery, metadata); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "transition_failed", &park, signature)
		o.setGitHubRESTBudgetParkSurface(state, issue.ID, githubRESTBudgetStatusMessage("capacity recovered; lane transition failed", current))
		return true, false
	}
	if o.connector != nil {
		comment := fmt.Sprintf(
			"GitHub REST capacity recovered. Detent moved %s from %s back to %s.\n\n- consumer: %s\n- credential_identity: %s\n- remaining: %d / %d\n- worker reserve: %d\n- reset_at: %s\n- exhaustion_episode: %s",
			issueLabel(issue), strings.TrimSpace(issue.State), targetState,
			strings.TrimSpace(current.Consumer), strings.TrimSpace(current.CredentialIdentity), current.Remaining, current.Limit, evidence.Reserve,
			current.ResetAt.UTC().Format(time.RFC3339), signature,
		)
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("GitHub REST budget recovery comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	o.logBlockedRecoveryDecision(issue, "transition", "github_rest_budget_recovered", &park, signature)
	return true, true
}

func githubRESTBudgetRecoverySignature(current githubRESTBudgetEvidence, reserve int64) string {
	return fmt.Sprintf(
		"consumer=%s;credential_identity=%s;reset_at=%s;reserve=%d",
		strings.TrimSpace(current.Consumer),
		strings.TrimSpace(current.CredentialIdentity),
		current.ResetAt.UTC().Format(time.RFC3339),
		reserve,
	)
}

func githubRESTBudgetStatusMessage(status string, budget githubRESTBudgetEvidence) string {
	message := fmt.Sprintf("transient GitHub REST budget %s: remaining=%d", strings.TrimSpace(status), budget.Remaining)
	if budget.Limit > 0 {
		message += "/" + strconv.FormatInt(budget.Limit, 10)
	}
	message += " reserve=" + strconv.FormatInt(budget.Reserve, 10)
	if budget.Consumer != "" {
		message += " consumer=" + strings.TrimSpace(budget.Consumer)
	}
	if budget.CredentialIdentity != "" {
		message += " credential_identity=" + strings.TrimSpace(budget.CredentialIdentity)
	}
	if !budget.ResetAt.IsZero() {
		message += " reset_at=" + budget.ResetAt.UTC().Format(time.RFC3339)
	}
	return message
}

func (o *Orchestrator) setGitHubRESTBudgetParkSurface(state *State, issueID string, reason string) {
	if state == nil {
		return
	}
	entry, ok := state.Blocked[strings.TrimSpace(issueID)]
	if !ok {
		return
	}
	entry.Reason = strings.TrimSpace(reason)
	state.Blocked[strings.TrimSpace(issueID)] = entry
}
