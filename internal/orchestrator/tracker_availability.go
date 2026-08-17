package orchestrator

import (
	"context"
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
	trackerUnavailableMinimumObservations = 2
	trackerUnavailableErrorClass          = connector.TrackerUnavailableCondition
)

type TrackerCondition = telemetry.TrackerCondition

type trackerAvailabilityEvidence struct {
	Scope        connector.TrackerAvailabilityScope
	Source       telemetry.RefreshSourceName
	Class        string
	Count        int
	FirstFailure time.Time
	LastFailure  time.Time
	LastError    string
}

func (o *Orchestrator) observeTrackerReadFailure(state *State, source telemetry.RefreshSourceName, err error, observedAt time.Time) bool {
	availabilityErr, ok := connector.AsTrackerAvailability(err)
	if !ok {
		o.recordTrackerReachableFailure(state, source, observedAt)
		return false
	}
	if state == nil {
		return true
	}
	if observedAt.IsZero() {
		observedAt = o.clockNow()
	}
	observedAt = observedAt.UTC()
	if state.trackerEvidence == nil {
		state.trackerEvidence = map[string]trackerAvailabilityEvidence{}
	}
	key := availabilityErr.Scope.Key(availabilityErr.Class)
	evidence := state.trackerEvidence[key]
	if evidence.Count == 0 {
		evidence = trackerAvailabilityEvidence{
			Scope:        availabilityErr.Scope.Normalize(),
			Source:       source,
			Class:        strings.TrimSpace(availabilityErr.Class),
			FirstFailure: observedAt,
		}
	}
	evidence.Count++
	evidence.LastFailure = observedAt
	evidence.LastError = strings.TrimSpace(err.Error())
	state.trackerEvidence[key] = evidence

	if state.TrackerUnavailable != nil {
		conditionKey := connector.TrackerAvailabilityScope{
			Connector:          state.TrackerUnavailable.Connector,
			Endpoint:           state.TrackerUnavailable.Endpoint,
			Operation:          state.TrackerUnavailable.Operation,
			CredentialIdentity: state.TrackerUnavailable.CredentialIdentity,
		}.Key(state.TrackerUnavailable.ErrorClass)
		if key != conditionKey {
			return true
		}
		condition := *state.TrackerUnavailable
		condition.LastObservedAt = observedAt
		condition.LastError = evidence.LastError
		state.TrackerUnavailable = &condition
		return true
	}
	if evidence.Count < trackerUnavailableMinimumObservations {
		return true
	}

	condition := TrackerCondition{
		ProjectID:          strings.TrimSpace(o.cfg.Project.ID),
		Connector:          trackerConnectorName(o.connector, evidence.Scope.Connector),
		ConnectorInstance:  trackerConnectorInstance(o.cfg.Project.ID, o.connector, evidence.Scope.Connector),
		Endpoint:           evidence.Scope.Endpoint,
		Operation:          evidence.Scope.Operation,
		ErrorClass:         evidence.Class,
		CredentialIdentity: evidence.Scope.CredentialIdentity,
		RefreshSource:      evidence.Source,
		DetectedAt:         evidence.FirstFailure,
		LastObservedAt:     observedAt,
		NextProbeAt:        observedAt.Add(backendCapacityProbeDelayForAttempt(0)),
		LastError:          evidence.LastError,
	}
	state.TrackerUnavailable = &condition
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      observedAt,
		Event:   connector.TrackerUnavailableCondition,
		Message: trackerUnavailableStatusMessage(condition),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelWarn, telemetry.LifecycleSafetyControl, connector.TrackerUnavailableCondition, "tracker unavailable", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
			"connector", condition.Connector,
			"connector_instance", condition.ConnectorInstance,
			"endpoint", condition.Endpoint,
			"operation", condition.Operation,
			"error_class", condition.ErrorClass,
			"credential_identity", condition.CredentialIdentity,
			"next_probe_at", condition.NextProbeAt,
			"error", err,
		)
	}
	return true
}

func trackerConnectorName(candidate connector.Connector, fallback string) string {
	if candidate != nil {
		if name := strings.TrimSpace(candidate.Name()); name != "" {
			return name
		}
	}
	return strings.TrimSpace(fallback)
}

func trackerConnectorInstance(projectID string, candidate connector.Connector, fallback string) string {
	return strings.TrimSpace(projectID) + ":" + trackerConnectorName(candidate, fallback)
}

func (o *Orchestrator) recordTrackerReadSuccess(state *State, source telemetry.RefreshSourceName, observedAt time.Time) {
	if state == nil {
		return
	}
	clearTrackerAvailabilityEvidence(state, source)
	if state.TrackerUnavailable == nil || (state.TrackerUnavailable.RefreshSource != "" && state.TrackerUnavailable.RefreshSource != source) {
		return
	}
	o.completeTrackerAvailabilityRecovery(state, observedAt, "canary")
}

func (o *Orchestrator) recordTrackerReachableFailure(state *State, source telemetry.RefreshSourceName, observedAt time.Time) {
	if state == nil {
		return
	}
	clearTrackerAvailabilityEvidence(state, source)
	if state.TrackerUnavailable == nil || (state.TrackerUnavailable.RefreshSource != "" && state.TrackerUnavailable.RefreshSource != source) {
		return
	}
	o.completeTrackerAvailabilityRecovery(state, observedAt, "reachable_response")
}

func clearTrackerAvailabilityEvidence(state *State, source telemetry.RefreshSourceName) {
	for key, evidence := range state.trackerEvidence {
		if source == "" || evidence.Source == source {
			delete(state.trackerEvidence, key)
		}
	}
}

func activeTrackerUnavailable(state *State) bool {
	return state != nil && state.TrackerUnavailable != nil
}

func trackerDependentDispatch(issue connector.Issue) bool {
	return strings.TrimSpace(issue.ID) != ""
}

func trackerUnavailableRetry(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	return state.Retry[issueID].TrackerUnavailable
}

func (o *Orchestrator) trackerAvailabilityPaused(ctx context.Context, state *State, now time.Time) bool {
	if state == nil || state.TrackerUnavailable == nil {
		return false
	}
	condition := *state.TrackerUnavailable
	if !condition.NextProbeAt.IsZero() && now.Before(condition.NextProbeAt) {
		markRefreshError(state, trackerUnavailableStatusMessage(condition), now)
		return true
	}
	return !o.probeTrackerAvailability(ctx, state, now)
}

func (o *Orchestrator) probeTrackerAvailability(ctx context.Context, state *State, now time.Time) bool {
	if state == nil || state.TrackerUnavailable == nil {
		return true
	}
	condition := *state.TrackerUnavailable
	condition.ProbeAttempts++
	condition.LastProbeAt = now.UTC()
	condition.LastProbeResult = "in_progress"
	condition.LastProbeDetail = "canary tracker read started"
	condition.NextProbeAt = time.Time{}
	state.TrackerUnavailable = &condition

	var err error
	switch condition.RefreshSource {
	case telemetry.RefreshSourceDrift:
		reader, ok := o.connector.(connector.StatusDriftReader)
		if ok {
			var drift connector.StatusDrift
			drift, err = reader.FetchStatusDrift(ctx)
			if err == nil {
				state.StatusDrift = cloneStatusDrift(drift)
			}
		} else {
			_, err = o.fetchCandidateIssuesForTick(ctx, state)
		}
	case telemetry.RefreshSourceStatuses:
		states := o.observedStatusFetchStatesForTick(state)
		if len(states) > 0 {
			_, err = o.fetchObservedIssuesByStates(ctx, states)
		} else {
			_, err = o.fetchCandidateIssuesForTick(ctx, state)
		}
	default:
		_, err = o.fetchCandidateIssuesForTick(ctx, state)
	}
	if err == nil {
		recordRefreshSourceSuccess(state, trackerConditionRefreshSource(condition), now)
		o.completeTrackerAvailabilityRecovery(state, now, "canary")
		return true
	}

	source := trackerConditionRefreshSource(condition)
	recordRefreshSourceFailure(state, source, err, now)
	markRefreshError(state, "tracker canary failed: "+err.Error(), now)
	availabilityErr, unavailable := connector.AsTrackerAvailability(err)
	if !unavailable {
		o.completeTrackerAvailabilityRecovery(state, now, "reachable_response")
		return false
	}
	condition = *state.TrackerUnavailable
	condition.LastObservedAt = now.UTC()
	condition.LastProbeAt = now.UTC()
	condition.LastProbeResult = "failed"
	condition.LastProbeDetail = strings.TrimSpace(err.Error())
	condition.LastError = strings.TrimSpace(err.Error())
	condition.Endpoint = availabilityErr.Scope.Endpoint
	condition.Operation = availabilityErr.Scope.Operation
	condition.ErrorClass = availabilityErr.Class
	condition.NextProbeAt = backendCapacityBoundedProbeAt(time.Time{}, now.Add(backendCapacityProbeDelayForAttempt(condition.ProbeAttempts)), now)
	state.TrackerUnavailable = &condition
	return false
}

func trackerConditionRefreshSource(condition TrackerCondition) telemetry.RefreshSourceName {
	if condition.RefreshSource != "" {
		return condition.RefreshSource
	}
	return telemetry.RefreshSourceCandidates
}

func (o *Orchestrator) completeTrackerAvailabilityRecovery(state *State, recoveredAt time.Time, source string) {
	if state == nil || state.TrackerUnavailable == nil {
		return
	}
	if recoveredAt.IsZero() {
		recoveredAt = o.clockNow()
	}
	condition := *state.TrackerUnavailable
	state.TrackerUnavailable = nil
	clear(state.trackerEvidence)
	releaseTrackerUnavailableRetries(state, recoveredAt)
	o.activateDispatchRecovery(state, dispatchRecoveryTrackerUnavailable, trackerUnavailableStatusMessage(condition), recoveredAt, "")
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      recoveredAt.UTC(),
		Event:   "tracker_availability_recovered",
		Message: "tracker " + condition.Connector + " recovered via " + source,
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelInfo, telemetry.LifecycleSafetyControl, "tracker_availability_recovered", "tracker availability recovered", telemetry.LifecycleCorrelation{ProjectID: o.cfg.Project.ID},
			"connector", condition.Connector,
			"connector_instance", condition.ConnectorInstance,
			"detected_at", condition.DetectedAt,
			"recovered_at", recoveredAt,
			"source", source,
		)
	}
}

func releaseTrackerUnavailableRetries(state *State, releasedAt time.Time) {
	for issueID, retry := range state.Retry {
		if !retry.TrackerUnavailable {
			continue
		}
		retry.DueAt = releasedAt
		retry.TrackerUnavailable = false
		state.Retry[issueID] = retry
	}
}

func (o *Orchestrator) handleTrackerUnavailableCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	availabilityErr, ok := connector.AsTrackerAvailability(event.Err)
	if !ok || availabilityErr == nil {
		return false
	}
	o.observeTrackerReadFailure(state, "", event.Err, event.CompletedAt)
	message := strings.TrimSpace(event.Err.Error())
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		trackerUnavailableErrorClass,
		message,
		"waiting",
		message,
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return true
	}
	dueAt := event.CompletedAt.Add(max(event.RetryDelay, o.cfg.PollInterval))
	if state.TrackerUnavailable != nil && !state.TrackerUnavailable.NextProbeAt.IsZero() {
		dueAt = state.TrackerUnavailable.NextProbeAt
	}
	if dueAt.Before(event.CompletedAt) {
		dueAt = event.CompletedAt
	}
	if state.FailureBreaker.Active() && state.FailureBreaker.CanaryIssueID == running.Issue.ID {
		o.deferProjectFailureBreakerCanary(state, running.Issue.ID, event.CompletedAt, dueAt.Sub(event.CompletedAt))
	}
	state.Retry[running.Issue.ID] = Retry{
		Issue:              cloneIssue(running.Issue),
		Attempt:            running.Attempt,
		DueAt:              dueAt,
		Error:              message,
		WorkerHost:         running.WorkerHost,
		TrackerUnavailable: true,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt.UTC(),
		Event:   "tracker_unavailable_attempt_waiting",
		Message: "waiting to retry " + issueLabel(running.Issue) + " without tripping a failure breaker",
	})
	return true
}

func trackerUnavailableStatusMessage(condition TrackerCondition) string {
	tracker := strings.TrimSpace(condition.Connector)
	if tracker == "" {
		tracker = "configured"
	}
	message := fmt.Sprintf("tracker %s unavailable (%s/%s)", tracker, connector.TrackerUnavailableCondition, condition.ErrorClass)
	if !condition.NextProbeAt.IsZero() {
		message += "; next canary at " + condition.NextProbeAt.UTC().Format(time.RFC3339)
	}
	return message
}

func cloneTrackerCondition(condition *TrackerCondition) *TrackerCondition {
	if condition == nil {
		return nil
	}
	cloned := *condition
	return &cloned
}

func (o *Orchestrator) clearTrackerAvailability(state *State, clearedAt time.Time) []TrackerCondition {
	if state == nil || state.TrackerUnavailable == nil {
		return nil
	}
	if clearedAt.IsZero() {
		clearedAt = o.clockNow()
	}
	condition := *state.TrackerUnavailable
	condition.LastProbeAt = clearedAt.UTC()
	condition.LastProbeResult = "operator_cleared"
	condition.LastProbeDetail = "operator cleared the recorded condition"
	condition.NextProbeAt = time.Time{}
	state.TrackerUnavailable = nil
	clear(state.trackerEvidence)
	releaseTrackerUnavailableRetries(state, clearedAt)
	o.activateDispatchRecovery(state, dispatchRecoveryTrackerUnavailable, trackerUnavailableStatusMessage(condition), clearedAt, "")
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      clearedAt.UTC(),
		Event:   "tracker_availability_operator_cleared",
		Message: "operator cleared tracker " + condition.Connector + " availability condition",
	})
	return []TrackerCondition{condition}
}
