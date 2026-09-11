package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func InitializeIssueLane(ctx context.Context, cfg Config, deps Dependencies, issue connector.Issue, target string) error {
	if deps.LaneLedger == nil || deps.Connector == nil {
		return errors.New("issue lane initialization requires a durable ledger and tracker")
	}
	owner := Orchestrator{cfg: normalizeConfig(cfg), connector: deps.Connector, laneLedger: deps.LaneLedger, laneCoordination: deps.LaneCoordination, workflowMetrics: deps.WorkflowMetrics, logger: deps.Logger}
	return owner.updateIssueState(ctx, nil, issue, target, time.Now().UTC(), "state_transition")
}

func (o *Orchestrator) finishObservedLaneRun(ctx context.Context, state *State, running Running, event runpkg.Completion) {
	o.logWorkerLifecycle(running.Issue, "worker_"+workerOutcome(event.Err, event.Result.FinalState), "attempt", running.Attempt, "final_state", event.Result.FinalState)
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	running.Tokens = tokens
	releaseWorkerGitHubMonitorProbe(state, event.IssueID, "deferred", "worker completed after lane transition", event.CompletedAt)
	if event.Err == nil || event.Result.TurnStarted || running.TurnCount > 0 {
		o.recoverBackendCapacity(state, running, event.CompletedAt)
	} else {
		o.deferBackendCapacityProbe(state, running, event.CompletedAt, event.Err)
	}
	releaseDispatchRecoveryAdmission(state, event.IssueID)
	releaseProjectFailureBreakerCanary(state, event.IssueID)
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
		state.DiffStats[event.IssueID] = running.DiffStats
	}
	if o.handlePreTurnFailure(ctx, state, event, running) {
		return
	}
	if event.Err == nil && stateIn(running.Issue.State, o.cfg.TerminalStates) {
		o.completeTerminalRunning(ctx, state, event.IssueID, running, terminalCompletedAt(running.Issue, o.cfg.TerminalStates, event.CompletedAt), tokens)
		return
	}
	errorClass := ""
	if issueConfigurationFailure(event.Err, "", "") {
		errorClass = "issue_configuration"
	}
	terminal := terminalStateForRun(event.Err, event.Result.FinalState)
	cleanliness := o.evaluateCompletionCleanliness(ctx, running, running.Issue, running.DiffStats)
	metadata := completionCleanlinessMetadata(cleanliness)
	if terminal == store.WorkAttemptTerminalSuccess && (!cleanliness.Attempted || cleanliness.Outcome == completionCleanlinessAccepted) {
		resetWorkerFailureBreakers(state, event.IssueID)
	}
	if !o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, terminal, errorClass, errorString(event.Err), "completed", string(terminal), metadata) && o.workAttempts != nil && running.WorkAttemptID > 0 {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, errors.New("persist completed work attempt failed"))
		return
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, terminal, event.Err, "", errorString(event.Err))
	if terminal == store.WorkAttemptTerminalSuccess || stateIn(running.Issue.State, o.cfg.TerminalStates) {
		finalState := event.Result.FinalState
		if stateIn(running.Issue.State, o.cfg.TerminalStates) {
			finalState = running.Issue.State
		}
		state.Completed[event.IssueID] = Completed{Issue: cloneIssue(running.Issue), SessionID: running.SessionID, StartedAt: running.StartedAt, CompletedAt: event.CompletedAt, FinalState: finalState, Tokens: tokens, RuntimeIdentity: running.RuntimeIdentity}
	}
	o.finishAcceptedCompletionLaneRun(ctx, state, running, event.CompletedAt)
}

func (o *Orchestrator) writeTrackerLane(ctx context.Context, issueID, target string, metadata workflowLaneMetadata) error {
	if metadata.StateFieldID > 0 {
		setter, ok := o.connector.(connector.IssueFieldSetter)
		if !ok {
			return connector.ErrNotImplemented
		}
		return setter.SetIssueField(ctx, issueID, metadata.StateFieldID, metadata.StateFieldValue)
	}
	return o.connector.UpdateIssueState(ctx, issueID, target)
}

func (o *Orchestrator) prepareLaneWrite(ctx context.Context, issue connector.Issue, target, reason string, at time.Time) (coordination.LaneWrite, error) {
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	if o.laneLedger == nil {
		if ledger, ok := o.workflowMetrics.(store.LaneLedgerStore); ok {
			o.laneLedger = ledger
		}
	}
	write := coordination.LaneWrite{Issue: issue.ID, From: issue.State, To: target, Reason: reason, WrittenAt: at}
	if o.laneLedger != nil {
		var err error
		write, err = o.laneLedger.PrepareLaneWrite(ctx, o.workflowMetricsProjectID(), write)
		if err != nil {
			return coordination.LaneWrite{}, err
		}
	} else {
		write.InstanceIdentity = "local"
		write.FenceToken = o.laneWrites[issue.ID].FenceToken + 1
	}
	if o.laneWrites == nil {
		o.laneWrites = make(map[string]coordination.LaneWrite)
		o.laneWriteResults = make(map[string]string)
	}
	o.laneWrites[issue.ID] = write
	o.laneWriteResults[issue.ID] = "prepared"
	if err := coordination.PublishLaneWrite(ctx, o.laneCoordination, o.workflowMetricsProjectID(), write); err != nil {
		return coordination.LaneWrite{}, errors.Join(err, o.resolveLaneWrite(ctx, write, "failed"))
	}
	return write, nil
}

func (o *Orchestrator) resolveLaneWrite(ctx context.Context, write coordination.LaneWrite, result string) error {
	o.laneWriteResults[write.Issue] = result
	if o.laneLedger == nil {
		return nil
	}
	if err := o.laneLedger.ResolveLaneWrite(context.WithoutCancel(ctx), write.FenceToken, result); err != nil {
		return err
	}
	if result != "applied" || o.laneWriteOrigin == "" {
		return nil
	}
	recorder, ok := o.laneLedger.(laneWriteActionRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordLaneWriteAction(context.WithoutCancel(ctx), write.FenceToken, o.laneWriteOrigin, o.laneWriteAction)
}

func (o *Orchestrator) observeLane(ctx context.Context, state *State, issue connector.Issue, at time.Time) (store.LaneObservation, provenance.Attribution, error) {
	unlock := o.lockLaneWrites()
	defer unlock()
	if o.laneLedger == nil {
		if ledger, ok := o.workflowMetrics.(store.LaneLedgerStore); ok {
			o.laneLedger = ledger
		}
	}
	if at.IsZero() {
		at = o.clockNow().UTC()
	}
	identity := store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID}
	previous, known := o.laneObservations[issue.ID]
	if !known && o.laneLedger != nil {
		var err error
		previous, err = o.laneLedger.LaneObservation(ctx, identity)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.LaneObservation{}, provenance.Attribution{}, err
		}
		known = err == nil
	}
	if !known {
		timeline, _ := o.issueWorkflowTimeline(ctx, issue)
		if event, found := latestLedgerBaseline(timeline.Events); found {
			previous = store.LaneObservation{State: event.PhaseName, EnteredAt: workflowLaneTransitionAt(event)}
			known = true
		}
	}
	if issue.StageUpdatedAt == nil && !known {
		transition := o.trackerIssueStateTransition(ctx, issue)
		if !transition.EnteredAt.IsZero() {
			issue.StageUpdatedAt = &transition.EnteredAt
		}
	}
	if known && previous.Origin != "" && normalizeState(previous.State) == normalizeState(issue.State) && (issue.StageUpdatedAt == nil || !issue.StageUpdatedAt.After(previous.EnteredAt)) {
		return previous, laneLedgerAttribution(provenance.Origin(previous.Origin)), nil
	}
	same := known && normalizeState(previous.State) == normalizeState(issue.State) && (issue.StageUpdatedAt == nil || !issue.StageUpdatedAt.After(previous.EnteredAt))
	transitionAfter := time.Time{}
	if known && !same {
		transitionAfter = previous.EnteredAt
	}
	write := o.laneWrites[issue.ID]
	result := o.laneWriteResults[issue.ID]
	if o.laneLedger != nil {
		var err error
		write, result, err = o.laneLedger.LatestLaneWrite(ctx, identity)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return store.LaneObservation{}, provenance.Attribution{}, err
		}
	}
	local := write.FenceToken > 0 && result != "blocked" && result != "failed" &&
		normalizeState(write.To) == normalizeState(issue.State) && !write.WrittenAt.After(at) &&
		(transitionAfter.IsZero() || write.WrittenAt.After(transitionAfter))
	peer := false
	if !local {
		var err error
		peer, err = coordination.MatchPeerLaneWrite(ctx, o.laneCoordination, identity.ProjectID, write.InstanceIdentity, issue.ID, issue.State, transitionAfter, at)
		if err != nil && o.logger != nil {
			o.logger.Warn("peer lane lookup unavailable; using local ledger", "issue_id", issue.ID, "error", err)
		}
	}
	attribution := provenance.AttributionFromSource(provenance.SourceDetentInstance, provenance.Actor{})
	if !local && !peer {
		attribution = provenance.Prepare(provenance.Attribution{Origin: provenance.OriginHuman})
	}
	enteredAt := at
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		enteredAt = issue.StageUpdatedAt.UTC()
	} else if local {
		enteredAt = write.WrittenAt
	}
	if same {
		enteredAt = previous.EnteredAt
	}
	observation := store.LaneObservation{State: issue.State, EnteredAt: enteredAt, Origin: string(attribution.Origin)}
	if err := o.saveLaneObservation(ctx, issue.ID, observation); err != nil {
		return store.LaneObservation{}, provenance.Attribution{}, err
	}
	if known && !same && !local && !peer {
		before := cloneIssue(issue)
		before.State = previous.State
		if normalizeState(before.State) == normalizeState(issue.State) {
			before.State = ""
		}
		o.recordLaneTransition(ctx, before, issue.State, enteredAt, "operator_move", workflowLaneMetadata{Provenance: attribution})
		o.handleOperatorMove(state, OperatorMoveRequest{IssueID: issue.ID, Identifier: issue.Identifier, FromState: previous.State, ToState: strings.TrimSpace(issue.State)}, enteredAt)
	}
	if !same && normalizeState(issue.State) == normalizeState(autoPromoteReworkState) {
		observed := cloneIssue(issue)
		observed.State = ""
		o.captureReworkLesson(observed, enteredAt, "tracker_state_observed")
	}
	return observation, attribution, nil
}

func (o *Orchestrator) lockLaneWrites() func() {
	o.laneWriteMu.Lock()
	if o.laneLedger == nil {
		if ledger, ok := o.workflowMetrics.(store.LaneLedgerStore); ok {
			o.laneLedger = ledger
		}
	}
	if o.laneLedger == nil {
		return o.laneWriteMu.Unlock
	}
	lock := o.laneLedger.LaneWriteLock()
	lock.Lock()
	return func() {
		lock.Unlock()
		o.laneWriteMu.Unlock()
	}
}

func laneLedgerAttribution(origin provenance.Origin) provenance.Attribution {
	if origin == provenance.OriginDetent {
		return provenance.AttributionFromSource(provenance.SourceDetentInstance, provenance.Actor{})
	}
	return provenance.Prepare(provenance.Attribution{Origin: origin})
}

func (o *Orchestrator) saveLaneObservation(ctx context.Context, issueID string, observation store.LaneObservation) error {
	if o.laneLedger != nil {
		identity := store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issueID}
		if err := o.laneLedger.SaveLaneObservation(ctx, identity, observation); err != nil {
			return fmt.Errorf("persist observed lane: %w", err)
		}
	}
	if o.laneObservations == nil {
		o.laneObservations = make(map[string]store.LaneObservation)
	}
	o.laneObservations[issueID] = observation
	return nil
}

func latestLedgerBaseline(events []store.WorkflowPhaseEvent) (store.WorkflowPhaseEvent, bool) {
	var latest store.WorkflowPhaseEvent
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || event.Status != "entered" || event.StartedAt.IsZero() {
			continue
		}
		if event.StartedAt.After(latest.StartedAt) || (event.StartedAt.Equal(latest.StartedAt) && event.ID > latest.ID) {
			latest = event
		}
	}
	return latest, !latest.StartedAt.IsZero()
}
