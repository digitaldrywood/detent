package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/provenance"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	stalenessDeliveryRetryBase = 5 * time.Minute
	stalenessDeliveryRetryMax  = time.Hour
	stalenessDeliveriesPerTick = 1
)

func (o *Orchestrator) refreshStalenessWarnings(ctx context.Context, state *State, candidates []connector.Issue, now time.Time) {
	if o == nil || state == nil {
		return
	}
	if !o.cfg.Staleness.Enabled {
		state.StalenessWarnings = map[string]StalenessWarning{}
		return
	}
	evaluated := staleness.Evaluate(o.cfg.Staleness, staleness.Input{
		ProjectID:    strings.TrimSpace(o.cfg.Project.ID),
		Items:        o.stalenessItems(ctx, stateLaneEntryIssues(state), state, now),
		Dispatchable: o.stalenessDispatchableItems(candidates, state, now),
		MergeQueue:   o.stalenessMergeQueueItems(state),
		Completions:  stalenessCompletions(state),
		Decisions:    stalenessDecisions(state.SchedulerDecisions, stalenessDecisionIssueIndex(state, candidates), state.laneEntries),
	}, now)
	persisted := o.stalenessWarningStates(ctx, state)
	previous := state.StalenessWarnings
	next := make(map[string]StalenessWarning, len(evaluated))
	for _, warning := range evaluated {
		warningState := persisted[warning.ID]
		if warningState.AcknowledgedAt != nil {
			continue
		}
		current, exists := previous[warning.ID]
		visible := true
		if !exists {
			current = StalenessWarning{
				Warning:    warning,
				Visible:    visible,
				DetectedAt: now.UTC(),
			}
			if visible {
				recordStateEvent(state, telemetry.ActivityEvent{
					At:      now.UTC(),
					Event:   "fleet_observability_condition",
					Message: stalenessWarningMessage(warning),
				})
				if o.logger != nil {
					o.logger.Debug("fleet observability condition", "class", warning.Class, "project_id", warning.ProjectID, "kind", warning.Kind, "issue_id", warning.IssueID, "identifier", warning.Identifier, "lane", warning.Lane, "reason", warning.Reason, "age_seconds", warning.AgeSeconds, "threshold_seconds", warning.ThresholdSeconds, "count", warning.Count, "waiting_on_human", warning.WaitingOnHuman)
				}
			}
		}
		current.Warning = warning
		current.Visible = visible
		current.LastObservedAt = now.UTC()
		next[warning.ID] = current
	}
	state.StalenessWarnings = next
	o.deliverStalenessWarnings(ctx, state, now)
}

func (o *Orchestrator) stalenessWarningStates(ctx context.Context, state *State) map[string]store.StalenessWarningState {
	if state.stalenessReminders == nil {
		state.stalenessReminders = map[string]time.Time{}
	}
	persisted := make(map[string]store.StalenessWarningState)
	projectID := strings.TrimSpace(o.cfg.Project.ID)
	if o.stalenessWarningStore != nil {
		states, err := o.stalenessWarningStore.ListStalenessWarningStates(ctx, projectID)
		if err != nil {
			if o.logger != nil {
				o.logger.Error("list staleness warning states", "project_id", projectID, "error", err)
			}
		} else {
			for _, warningState := range states {
				persisted[warningState.WarningID] = warningState
				if warningState.RemindedAt != nil {
					state.stalenessReminders[warningState.WarningID] = warningState.RemindedAt.UTC()
				}
			}
		}
	}
	return persisted
}

func (o *Orchestrator) recordStalenessWarningReminder(ctx context.Context, state *State, warningID string, at time.Time) {
	state.stalenessReminders[warningID] = at.UTC()
	if o.stalenessWarningStore == nil {
		return
	}
	projectID := strings.TrimSpace(o.cfg.Project.ID)
	if err := o.stalenessWarningStore.RecordStalenessWarningReminder(ctx, projectID, warningID, at); err != nil && o.logger != nil {
		o.logger.Error("record staleness warning reminder", "project_id", projectID, "warning_id", warningID, "error", err)
	}
}

func (o *Orchestrator) stalenessItems(ctx context.Context, issues []connector.Issue, state *State, now time.Time) []staleness.Item {
	items := make([]staleness.Item, 0, len(issues))
	seen := make(map[string]struct{})
	for _, issue := range issues {
		key := workflowLaneEntryKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, o.stalenessItem(ctx, issue, state, now))
	}
	return items
}

func (o *Orchestrator) stalenessDispatchableItems(candidates []connector.Issue, state *State, now time.Time) []staleness.Item {
	items := make([]staleness.Item, 0, len(candidates))
	seen := make(map[string]struct{})
	planner := o.dispatchPlanner()
	for _, issue := range candidates {
		decision := planner.dispatchableIssueDecision(issue, state, false, now, "")
		if !decision.dispatchable && decision.reason != dispatchSkipRateWindowBackpressure {
			continue
		}
		key := workflowIssueIdentityKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, o.stalenessBasicItem(issue, state))
	}
	return items
}

func (o *Orchestrator) stalenessMergeQueueItems(state *State) []staleness.Item {
	issues := append(cloneIssues(state.BoardIssues), state.Pipeline...)
	items := make([]staleness.Item, 0)
	seen := make(map[string]struct{})
	for _, issue := range issues {
		if normalizeState(issue.State) != normalizeState(autoPromoteMergingState) {
			continue
		}
		key := workflowIssueIdentityKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, o.stalenessBasicItem(issue, state))
	}
	return items
}

func (o *Orchestrator) stalenessItem(ctx context.Context, issue connector.Issue, state *State, now time.Time) staleness.Item {
	item := o.stalenessBasicItem(issue, state)
	timeline, _ := o.issueWorkflowTimeline(ctx, issue)
	parkCause, recordedPark := recordedStalenessPark(issue, state, timeline.Events)
	parkCauseKey, parkCauseStale, parkCauseDetail, parkCauseSince := staleRecordedParkCause(issue, state, item.EnteredAt, parkCause, o.cfg.TerminalStates, now)
	item.RecordedPark = recordedPark
	item.ParkCauseKey = parkCauseKey
	item.ParkCauseStale = parkCauseStale
	item.ParkCauseDetail = parkCauseDetail
	item.ParkCauseSince = parkCauseSince
	item.LaneVisits = stalenessLaneVisits(timeline.Events)
	return item
}

func (o *Orchestrator) stalenessBasicItem(issue connector.Issue, state *State) staleness.Item {
	enteredAt := state.laneEntries[workflowLaneEntryKey(issue)]
	if enteredAt.IsZero() {
		enteredAt = workflowLaneFallbackAt(issue)
	}
	hasRecovery := blockedRefsUnresolved(issue.BlockedBy, o.cfg.TerminalStates)
	if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok {
		hasRecovery = hasRecovery || blocked.Recovery != nil || strings.TrimSpace(blocked.RecoveryReason) != ""
	}
	return staleness.Item{
		ID:                   strings.TrimSpace(issue.ID),
		Identifier:           strings.TrimSpace(issue.Identifier),
		URL:                  strings.TrimSpace(issue.URL),
		Title:                strings.TrimSpace(issue.Title),
		State:                strings.TrimSpace(issue.State),
		EnteredAt:            enteredAt.UTC(),
		WaitingOnHuman:       artifactGateWaitStatusBlocksDispatch(issue, o.cfg.AutoPromote.Gate),
		HasRecoveryPredicate: hasRecovery,
	}
}

func recordedStalenessPark(issue connector.Issue, state *State, events []store.WorkflowPhaseEvent) (string, bool) {
	if normalizeState(issue.State) != normalizeState("Blocked") {
		return "", false
	}
	var blocked *Blocked
	if current, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blockedEntryMatchesCurrent(issue, current.BlockedAt) {
		blocked = &current
	}
	latest, _ := latestCurrentLaneEntry(events, issue.State)
	if !latest.StartedAt.IsZero() && !blockedEntryMatchesCurrent(issue, latest.StartedAt) {
		latest = store.WorkflowPhaseEvent{}
	}
	metadata, _ := workflowLaneMetadataFromJSON(latest.MetadataJSON)
	for _, cause := range []string{
		strings.TrimSpace(issue.BlockerReason),
		blockedReason(blocked),
		blockedRecoveryCause(blocked),
		strings.TrimSpace(latest.Reason),
		blockedRecoveryMetadataCause(metadata.BlockedRecovery),
	} {
		if stickyBlockReason(cause) {
			return cause, true
		}
	}
	if blocked != nil && blocked.Source == BlockedSourceOperatorStop {
		if cause := firstNonBlank(blocked.StopReason, blocked.Reason); cause != "" {
			return cause, true
		}
	}
	if blocked != nil && blocked.Recovery != nil {
		owner := strings.ToLower(strings.TrimSpace(blocked.Recovery.Owner))
		if owner == blockedRecoveryOwnerHuman || owner == blockedRecoveryOwnerOperator {
			if cause := blockedRecoveryMetadataCause(blocked.Recovery); cause != "" {
				return cause, true
			}
		}
	}
	if metadata.BlockedRecovery != nil && strings.EqualFold(strings.TrimSpace(metadata.BlockedRecovery.Owner), blockedRecoveryOwnerHuman) {
		if cause := blockedRecoveryMetadataCause(metadata.BlockedRecovery); cause != "" {
			return cause, true
		}
	}
	attribution := state.laneProvenance[workflowLaneEntryKey(issue)]
	if provenance.NormalizeOrigin(attribution.Origin) != provenance.OriginHuman {
		return "", false
	}
	cause := firstNonBlank(strings.TrimSpace(issue.BlockerReason), blockedReason(blocked), blockedStopReason(blocked), workpadParkCause(issue))
	return cause, cause != ""
}

func blockedReason(blocked *Blocked) string {
	if blocked == nil {
		return ""
	}
	return strings.TrimSpace(blocked.Reason)
}

func blockedStopReason(blocked *Blocked) string {
	if blocked == nil {
		return ""
	}
	return strings.TrimSpace(blocked.StopReason)
}

func blockedRecoveryCause(blocked *Blocked) string {
	if blocked == nil || blocked.Recovery == nil {
		return ""
	}
	return blockedRecoveryMetadataCause(blocked.Recovery)
}

func blockedRecoveryMetadataCause(recovery *workflowLaneBlockedRecoveryMetadata) string {
	if recovery == nil {
		return ""
	}
	return firstNonBlank(strings.TrimSpace(recovery.Cause), strings.TrimSpace(recovery.HoldReason))
}

func workpadParkCause(issue connector.Issue) string {
	signal := issue.WorkpadSignal
	if signal == nil || strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return ""
	}
	if cause := strings.TrimSpace(signal.HumanAction); cause != "" {
		return cause
	}
	for _, blocker := range signal.Blockers {
		if cause := strings.TrimSpace(blocker.Reason); cause != "" {
			return cause
		}
	}
	return ""
}

func staleRecordedParkCause(issue connector.Issue, state *State, enteredAt time.Time, cause string, terminalStates []string, now time.Time) (string, bool, string, time.Time) {
	keyParts := []string{strings.TrimSpace(cause)}
	since := enteredAt.UTC()
	if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok {
		if !blocked.BlockedAt.IsZero() {
			since = blocked.BlockedAt.UTC()
		}
		for _, evidence := range blocked.BlockerEvidence {
			status := strings.ToLower(strings.TrimSpace(evidence.Status))
			expired := evidence.ExpiresAt != nil && !evidence.ExpiresAt.IsZero() && !evidence.ExpiresAt.After(now)
			if status != blockerEvidenceStatusCleared && status != blockerEvidenceStatusUnverifiable && !evidence.Unverifiable && !expired {
				continue
			}
			recordedAtKey := ""
			if evidence.RecordedAt != nil {
				recordedAtKey = stalenessEventTimestampKey(*evidence.RecordedAt)
			}
			expiresAtKey := ""
			if evidence.ExpiresAt != nil {
				expiresAtKey = stalenessEventTimestampKey(*evidence.ExpiresAt)
			}
			keyParts = append(keyParts, evidence.Type, evidence.Reference, evidence.Reason, status, recordedAtKey, expiresAtKey)
			if expired {
				since = evidence.ExpiresAt.UTC()
			} else if evidence.RecordedAt != nil && !evidence.RecordedAt.IsZero() {
				since = evidence.RecordedAt.UTC()
			}
			fallback := "the recorded park evidence is no longer verifiable"
			if expired {
				fallback = "the recorded park evidence expired"
			}
			detail := firstNonBlank(strings.TrimSpace(evidence.Detail), strings.TrimSpace(evidence.Reason), fallback)
			return strings.Join(keyParts, "\x00"), true, detail, since
		}
	}
	if len(issue.BlockedBy) > 0 && !blockedRefsUnresolved(issue.BlockedBy, terminalStates) {
		for _, ref := range issue.BlockedBy {
			keyParts = append(keyParts, ref.Identifier, ref.ID, ref.State, ref.TrackerState)
		}
		return strings.Join(keyParts, "\x00"), true, "the recorded dependencies no longer block this item", since
	}
	return strings.Join(keyParts, "\x00"), false, "", since
}

func stalenessLaneVisits(events []store.WorkflowPhaseEvent) []staleness.LaneVisit {
	visits := make([]staleness.LaneVisit, 0)
	seen := make(map[string]struct{})
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(strings.TrimSpace(event.Status), "exited") || event.StartedAt.IsZero() || event.FinishedAt.IsZero() || !event.FinishedAt.After(event.StartedAt) {
			continue
		}
		key := normalizeState(event.PhaseName) + "\x00" + stalenessEventTimestampKey(event.StartedAt) + "\x00" + stalenessEventTimestampKey(event.FinishedAt)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		visits = append(visits, staleness.LaneVisit{State: strings.TrimSpace(event.PhaseName), EnteredAt: event.StartedAt.UTC(), ExitedAt: event.FinishedAt.UTC()})
	}
	return visits
}

func stalenessEventTimestampKey(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func stalenessCompletions(state *State) []staleness.Completion {
	completions := make([]staleness.Completion, 0, len(state.WorkAttempts)+len(state.Completed)+len(state.MergeTimings))
	for _, attempt := range state.WorkAttempts {
		if attempt.TerminalState != string(store.WorkAttemptTerminalSuccess) || attempt.CompletedAt == nil || attempt.CompletedAt.IsZero() {
			continue
		}
		completions = append(completions, staleness.Completion{
			At:     attempt.CompletedAt.UTC(),
			Merged: strings.EqualFold(strings.TrimSpace(attempt.WorkerType), "merge"),
		})
	}
	for _, completed := range state.Completed {
		if completed.CompletedAt.IsZero() {
			continue
		}
		completions = append(completions, staleness.Completion{
			At:     completed.CompletedAt.UTC(),
			Merged: !completed.MergeTiming.MergedAt.IsZero(),
		})
	}
	for _, timing := range state.MergeTimings {
		if timing.MergedAt.IsZero() {
			continue
		}
		completions = append(completions, staleness.Completion{At: timing.MergedAt.UTC(), Merged: true})
	}
	return completions
}

func stalenessDecisions(decisions []telemetry.SchedulerDecision, issues map[string]connector.Issue, laneEntries map[string]time.Time) []staleness.Decision {
	out := make([]staleness.Decision, 0, len(decisions))
	for _, decision := range decisions {
		issue, _ := stalenessDecisionIssue(decision, issues)
		laneEnteredAt := laneEntries[workflowLaneEntryKey(issue)]
		out = append(out, staleness.Decision{
			IssueID:       strings.TrimSpace(decision.IssueID),
			Identifier:    strings.TrimSpace(decision.Identifier),
			IssueURL:      strings.TrimSpace(decision.IssueURL),
			CurrentState:  strings.TrimSpace(issue.State),
			Closed:        issue.Closed,
			Merged:        pullRequestMerged(issue.PullRequest),
			Result:        strings.TrimSpace(decision.Result),
			Reason:        strings.TrimSpace(decision.Reason),
			Detail:        strings.TrimSpace(decision.WaitReason),
			At:            decision.DecisionAt.UTC(),
			LaneEnteredAt: laneEnteredAt.UTC(),
		})
	}
	return out
}

func stalenessDecisionIssueIndex(state *State, candidates []connector.Issue) map[string]connector.Issue {
	issues := make(map[string]connector.Issue)
	if state == nil {
		return issues
	}
	for _, blocked := range state.Blocked {
		indexStalenessDecisionIssue(issues, blocked.Issue)
	}
	for _, retry := range state.Retry {
		indexStalenessDecisionIssue(issues, retry.Issue)
	}
	for _, running := range state.Running {
		indexStalenessDecisionIssue(issues, running.Issue)
	}
	for _, issue := range state.Pipeline {
		indexStalenessDecisionIssue(issues, issue)
	}
	for _, issue := range state.BoardIssues {
		indexStalenessDecisionIssue(issues, issue)
	}
	for _, issue := range candidates {
		indexStalenessDecisionIssue(issues, issue)
	}
	return issues
}

func indexStalenessDecisionIssue(issues map[string]connector.Issue, issue connector.Issue) {
	for _, key := range []string{
		stalenessDecisionIssueIDKey(issue.ID),
		stalenessDecisionIdentifierKey(issue.Identifier),
		stalenessDecisionURLKey(issue.URL),
	} {
		if key != "" {
			issues[key] = issue
		}
	}
}

func stalenessDecisionIssue(decision telemetry.SchedulerDecision, issues map[string]connector.Issue) (connector.Issue, bool) {
	for _, key := range []string{
		stalenessDecisionIssueIDKey(decision.IssueID),
		stalenessDecisionIdentifierKey(decision.Identifier),
		stalenessDecisionURLKey(decision.IssueURL),
	} {
		if issue, ok := issues[key]; key != "" && ok {
			return issue, true
		}
	}
	return connector.Issue{}, false
}

func stalenessDecisionIssueIDKey(value string) string {
	return stalenessDecisionIssueKey("id:", value)
}

func stalenessDecisionIdentifierKey(value string) string {
	return stalenessDecisionIssueKey("identifier:", value)
}

func stalenessDecisionURLKey(value string) string {
	return stalenessDecisionIssueKey("url:", value)
}

func stalenessDecisionIssueKey(prefix string, value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return prefix + value
	}
	return ""
}

func (o *Orchestrator) deliverStalenessWarnings(ctx context.Context, state *State, now time.Time) {
	if o.newStalenessNotifier == nil || strings.TrimSpace(o.cfg.StalenessDelivery.WebhookURL) == "" {
		return
	}
	notifier, err := o.newStalenessNotifier(o.cfg.StalenessDelivery)
	if err != nil {
		o.recordStalenessNotifierError(state, now, err)
		return
	}
	if notifier == nil {
		return
	}
	ids := sortedKeys(state.StalenessWarnings)
	attempted := 0
	for _, id := range ids {
		current := state.StalenessWarnings[id]
		if observability.Normalize(current.Warning.Class, observability.Staleness(current.Warning.WaitingOnHuman)) != observability.ClassFault {
			continue
		}
		if !current.DeliveredAt.IsZero() || !stalenessDeliveryDue(current, now) {
			continue
		}
		if attempted >= stalenessDeliveriesPerTick {
			break
		}
		attempted++
		current.DeliveryAttempts++
		current.LastDeliveryAttemptAt = now.UTC()
		err := notifier.Notify(ctx, current.Warning)
		if err != nil {
			current.DeliveryError = err.Error()
			if o.logger != nil {
				o.logger.Warn("deliver fleet staleness warning failed", "warning_id", id, "project_id", current.Warning.ProjectID, "error", err)
			}
		} else {
			current.DeliveredAt = now.UTC()
			current.DeliveryError = ""
			if current.Warning.WaitingOnHuman {
				o.recordStalenessWarningReminder(ctx, state, current.Warning.ID, now)
			}
		}
		state.StalenessWarnings[id] = current
	}
}

func (o *Orchestrator) recordStalenessNotifierError(state *State, now time.Time, err error) {
	for id, current := range state.StalenessWarnings {
		if !current.DeliveredAt.IsZero() || !stalenessDeliveryDue(current, now) {
			continue
		}
		current.DeliveryAttempts++
		current.LastDeliveryAttemptAt = now.UTC()
		current.DeliveryError = err.Error()
		state.StalenessWarnings[id] = current
	}
}

func stalenessDeliveryDue(warning StalenessWarning, now time.Time) bool {
	if warning.LastDeliveryAttemptAt.IsZero() {
		return true
	}
	delay := stalenessDeliveryRetryBase
	for attempt := 1; attempt < warning.DeliveryAttempts && delay < stalenessDeliveryRetryMax; attempt++ {
		delay *= 2
	}
	if delay > stalenessDeliveryRetryMax {
		delay = stalenessDeliveryRetryMax
	}
	return !now.Before(warning.LastDeliveryAttemptAt.Add(delay))
}

func stalenessWarningMessage(warning staleness.Warning) string {
	target := strings.TrimSpace(warning.Identifier)
	if target == "" {
		target = strings.TrimSpace(warning.IssueID)
	}
	if target == "" {
		target = strings.TrimSpace(warning.ProjectID)
	}
	return fmt.Sprintf("%s: %s (%s)", target, warning.Detail, warning.Reason)
}

func stalenessWarningSnapshots(values map[string]StalenessWarning) []telemetry.StalenessWarning {
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]telemetry.StalenessWarning, 0, len(ids))
	for _, id := range ids {
		value := values[id]
		if !value.Visible {
			continue
		}
		out = append(out, telemetry.StalenessWarning{
			ID:                    value.Warning.ID,
			Class:                 observability.Normalize(value.Warning.Class, observability.Staleness(value.Warning.WaitingOnHuman)),
			ProjectID:             value.Warning.ProjectID,
			Kind:                  value.Warning.Kind,
			IssueID:               value.Warning.IssueID,
			Identifier:            value.Warning.Identifier,
			IssueURL:              value.Warning.IssueURL,
			Title:                 value.Warning.Title,
			Lane:                  value.Warning.Lane,
			Reason:                value.Warning.Reason,
			Detail:                value.Warning.Detail,
			Since:                 value.Warning.Since,
			DetectedAt:            value.DetectedAt,
			LastObservedAt:        value.LastObservedAt,
			AgeSeconds:            value.Warning.AgeSeconds,
			ThresholdSeconds:      value.Warning.ThresholdSeconds,
			Count:                 value.Warning.Count,
			WaitingOnHuman:        value.Warning.WaitingOnHuman,
			HasRecoveryPredicate:  value.Warning.HasRecoveryPredicate,
			DeliveredAt:           timePointer(value.DeliveredAt),
			DeliveryAttempts:      value.DeliveryAttempts,
			LastDeliveryAttemptAt: timePointer(value.LastDeliveryAttemptAt),
			DeliveryError:         value.DeliveryError,
		})
	}
	return out
}
