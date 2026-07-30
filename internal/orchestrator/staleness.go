package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
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
		Items:        o.stalenessItems(stateLaneEntryIssues(state), state),
		Dispatchable: o.stalenessDispatchableItems(candidates, state, now),
		MergeQueue:   o.stalenessMergeQueueItems(state),
		Completions:  stalenessCompletions(state),
		Decisions:    stalenessDecisions(state.SchedulerDecisions, stalenessDecisionIssueIndex(state, candidates)),
	}, now)
	previous := state.StalenessWarnings
	next := make(map[string]StalenessWarning, len(evaluated))
	for _, warning := range evaluated {
		current, exists := previous[warning.ID]
		if !exists {
			current = StalenessWarning{
				Warning:    warning,
				DetectedAt: now.UTC(),
			}
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      now.UTC(),
				Event:   "fleet_staleness_warning",
				Message: stalenessWarningMessage(warning),
			})
			if o.logger != nil {
				o.logger.Warn("fleet staleness warning", "project_id", warning.ProjectID, "kind", warning.Kind, "issue_id", warning.IssueID, "identifier", warning.Identifier, "lane", warning.Lane, "reason", warning.Reason, "age_seconds", warning.AgeSeconds, "threshold_seconds", warning.ThresholdSeconds, "count", warning.Count, "waiting_on_human", warning.WaitingOnHuman)
			}
		}
		current.Warning = warning
		current.LastObservedAt = now.UTC()
		next[warning.ID] = current
	}
	state.StalenessWarnings = next
	o.deliverStalenessWarnings(ctx, state, now)
}

func (o *Orchestrator) stalenessItems(issues []connector.Issue, state *State) []staleness.Item {
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
		items = append(items, o.stalenessItem(issue, state))
	}
	return items
}

func (o *Orchestrator) stalenessDispatchableItems(candidates []connector.Issue, state *State, now time.Time) []staleness.Item {
	items := make([]staleness.Item, 0, len(candidates))
	seen := make(map[string]struct{})
	planner := o.dispatchPlanner()
	for _, issue := range candidates {
		if !planner.dispatchableIssueDecision(issue, state, false, now, "").dispatchable {
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
		items = append(items, o.stalenessItem(issue, state))
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
		items = append(items, o.stalenessItem(issue, state))
	}
	return items
}

func (o *Orchestrator) stalenessItem(issue connector.Issue, state *State) staleness.Item {
	enteredAt := state.laneEntries[workflowLaneEntryKey(issue)]
	if enteredAt.IsZero() {
		enteredAt = workflowLaneFallbackAt(issue)
	}
	hasRecovery := len(issue.BlockedBy) > 0
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

func stalenessDecisions(decisions []telemetry.SchedulerDecision, issues map[string]connector.Issue) []staleness.Decision {
	out := make([]staleness.Decision, 0, len(decisions))
	for _, decision := range decisions {
		issue, _ := stalenessDecisionIssue(decision, issues)
		out = append(out, staleness.Decision{
			IssueID:      strings.TrimSpace(decision.IssueID),
			Identifier:   strings.TrimSpace(decision.Identifier),
			IssueURL:     strings.TrimSpace(decision.IssueURL),
			CurrentState: strings.TrimSpace(issue.State),
			Closed:       issue.Closed,
			Merged:       pullRequestMerged(issue.PullRequest),
			Reason:       strings.TrimSpace(decision.Reason),
			At:           decision.DecisionAt.UTC(),
		})
	}
	return out
}

func stalenessDecisionIssueIndex(state *State, candidates []connector.Issue) map[string]connector.Issue {
	issues := make(map[string]connector.Issue)
	if state == nil {
		return issues
	}
	for _, completed := range state.Completed {
		indexStalenessDecisionIssue(issues, completed.Issue)
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
		out = append(out, telemetry.StalenessWarning{
			ID:                    value.Warning.ID,
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
