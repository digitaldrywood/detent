package orchestrator

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	mergeSlotConcentrationWindow       = 15 * time.Minute
	mergeSlotConcentrationMinimumCount = 5
	mergeSlotConcentrationPercent      = 80
)

type mergeSlotAcquisition struct {
	issueID string
	at      time.Time
}

func (o *Orchestrator) recordMergeQueueEntries(state *State, issues []connector.Issue, now time.Time, source string) {
	for _, issue := range issuesInStates(issues, []string{autoPromoteMergingState}) {
		o.recordMergeQueueEntered(state, issue, now, source)
	}
}

func (o *Orchestrator) recordMergeQueueEntered(state *State, issue connector.Issue, now time.Time, source string) MergeTiming {
	if state == nil || !mergeWorkerIssue(issue) {
		return MergeTiming{}
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return MergeTiming{}
	}
	if state.MergeTimings == nil {
		state.MergeTimings = map[string]MergeTiming{}
	}
	timing := state.MergeTimings[issueID]
	if !timing.EnteredMergingAt.IsZero() && timing.MergedAt.IsZero() && timing.MergeFailedAt.IsZero() {
		return timing
	}
	if !timing.MergedAt.IsZero() || !timing.MergeFailedAt.IsZero() {
		timing = MergeTiming{}
	}
	timing.EnteredMergingAt = mergeQueueEnteredAt(issue, now)
	timing = timing.withDurations(now)
	state.MergeTimings[issueID] = timing
	o.logMergeTimingInfo("merge_queue_entered", issue, timing, "source", strings.TrimSpace(source))
	return timing
}

func (o *Orchestrator) markMergeWorkerSlotAcquired(state *State, issue connector.Issue, now time.Time) MergeTiming {
	if state == nil || !mergeWorkerIssue(issue) {
		return MergeTiming{}
	}
	timing := o.recordMergeQueueEntered(state, issue, now, "dispatch")
	slotAlreadyAcquired := !timing.MergeWorkerSlotAcquiredAt.IsZero() &&
		timing.MergedAt.IsZero() &&
		timing.MergeFailedAt.IsZero()
	if !slotAlreadyAcquired {
		timing.MergeWorkerSlotAcquiredAt = now.UTC()
	}
	timing.MergedAt = time.Time{}
	timing.MergeFailedAt = time.Time{}
	timing.MergeFailureReason = ""
	timing = timing.withDurations(now)
	state.MergeTimings[strings.TrimSpace(issue.ID)] = timing
	if !slotAlreadyAcquired {
		o.observeMergeSlotConcentration(state, issue, now)
	}
	return timing
}

func (o *Orchestrator) observeMergeSlotConcentration(state *State, issue connector.Issue, now time.Time) {
	if state == nil {
		return
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return
	}
	cutoff := now.Add(-mergeSlotConcentrationWindow)
	recent := state.mergeSlotAcquisitions[:0]
	for _, acquisition := range state.mergeSlotAcquisitions {
		if acquisition.at.Before(cutoff) {
			continue
		}
		recent = append(recent, acquisition)
	}
	recent = append(recent, mergeSlotAcquisition{issueID: issueID, at: now.UTC()})
	state.mergeSlotAcquisitions = recent
	if len(recent) < mergeSlotConcentrationMinimumCount {
		return
	}

	issueCount := 0
	for _, acquisition := range recent {
		if acquisition.issueID == issueID {
			issueCount++
		}
	}
	if issueCount*100 < len(recent)*mergeSlotConcentrationPercent {
		return
	}
	if state.mergeSlotWarnings == nil {
		state.mergeSlotWarnings = map[string]time.Time{}
	}
	for warnedIssueID, warnedAt := range state.mergeSlotWarnings {
		if warnedAt.Before(cutoff) {
			delete(state.mergeSlotWarnings, warnedIssueID)
		}
	}
	if warnedAt := state.mergeSlotWarnings[issueID]; !warnedAt.IsZero() && now.Before(warnedAt.Add(mergeSlotConcentrationWindow)) {
		return
	}
	state.mergeSlotWarnings[issueID] = now.UTC()
	if o.logger != nil {
		o.logger.Warn(
			"merge_worker_slot_concentration",
			mergeWorkerLogAttrs(issue,
				"project_id", strings.TrimSpace(o.cfg.Project.ID),
				"window", mergeSlotConcentrationWindow,
				"issue_acquisitions", issueCount,
				"total_acquisitions", len(recent),
				"share_percent", issueCount*100/len(recent),
			)...,
		)
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "merge_worker_slot_concentration",
		Message: "one issue acquired a disproportionate share of merge slots: " + issueLabel(issue),
	})
}

func (o *Orchestrator) markMergeStarted(state *State, issue connector.Issue, now time.Time) MergeTiming {
	if state == nil || !mergeWorkerIssue(issue) {
		return MergeTiming{}
	}
	timing := o.markMergeWorkerSlotAcquired(state, issue, now)
	timing.MergeStartedAt = now.UTC()
	timing.BaseRefreshStartedAt = timing.MergeStartedAt
	timing.BaseRefreshFinishedAt = time.Time{}
	if timing.CIWaitStartedAt.IsZero() {
		timing.CIWaitStartedAt = timing.MergeStartedAt
	}
	timing.CIWaitFinishedAt = time.Time{}
	timing = timing.withDurations(now)
	state.MergeTimings[strings.TrimSpace(issue.ID)] = timing
	o.logMergeTimingInfo("merge_base_refresh_started", issue, timing)
	o.logMergeTimingInfo("merge_ci_wait_started", issue, timing)
	return timing
}

func (o *Orchestrator) recordMergeCompleted(state *State, issue connector.Issue, at time.Time, finalState string) MergeTiming {
	if !mergeWorkerIssue(issue) {
		return MergeTiming{}
	}
	timing := o.completeMergeTiming(state, issue, at, "", true)
	o.logMergeTimingInfo("merge_base_refresh_finished", issue, timing, "final_state", strings.TrimSpace(finalState))
	o.logMergeTimingInfo("merge_ci_wait_finished", issue, timing, "final_state", strings.TrimSpace(finalState))
	o.logMergeTimingInfo("merge_completed", issue, timing, "final_state", strings.TrimSpace(finalState))
	return timing
}

func (o *Orchestrator) recordMergeFailed(state *State, issue connector.Issue, at time.Time, reason string, err error) MergeTiming {
	if !mergeWorkerIssue(issue) {
		return MergeTiming{}
	}
	timing := MergeTiming{}
	if state != nil {
		timing = state.MergeTimings[strings.TrimSpace(issue.ID)]
	}
	enteredMergingAt := timing.EnteredMergingAt
	if enteredMergingAt.IsZero() {
		enteredMergingAt = mergeQueueEnteredAt(issue, at)
	}
	mergeFailedAt := at.UTC()
	if mergeFailedAt.Before(enteredMergingAt) {
		timingErr := fmt.Errorf(
			"merge_failed_at %s precedes entered_merging_at %s",
			mergeFailedAt.Format(time.RFC3339),
			enteredMergingAt.Format(time.RFC3339),
		)
		if o.logger != nil {
			o.logger.Error(
				"merge_failed_rejected",
				mergeWorkerLogAttrs(issue,
					"reason", strings.TrimSpace(reason),
					"entered_merging_at", enteredMergingAt.Format(time.RFC3339),
					"merge_failed_at", mergeFailedAt.Format(time.RFC3339),
					"error", timingErr,
				)...,
			)
		}
		return timing
	}
	timing = o.completeMergeTiming(state, issue, at, reason, false)
	attrs := []any{"reason", strings.TrimSpace(reason)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logMergeTimingInfo("merge_base_refresh_finished", issue, timing, attrs...)
	o.logMergeTimingInfo("merge_ci_wait_finished", issue, timing, attrs...)
	o.logMergeTimingWarn("merge_failed", issue, timing, attrs...)
	return timing
}

func (o *Orchestrator) completeMergeTiming(state *State, issue connector.Issue, at time.Time, reason string, merged bool) MergeTiming {
	if state == nil || strings.TrimSpace(issue.ID) == "" {
		return MergeTiming{}
	}
	if state.MergeTimings == nil {
		state.MergeTimings = map[string]MergeTiming{}
	}
	timing := state.MergeTimings[strings.TrimSpace(issue.ID)]
	if timing.EnteredMergingAt.IsZero() {
		timing.EnteredMergingAt = mergeQueueEnteredAt(issue, at)
	}
	completedAt := at.UTC()
	if merged {
		timing.MergedAt = completedAt
		timing.MergeFailedAt = time.Time{}
		timing.MergeFailureReason = ""
	} else {
		timing.MergeFailedAt = completedAt
		timing.MergeFailureReason = strings.TrimSpace(reason)
	}
	if timing.BaseRefreshStartedAt.IsZero() && !timing.MergeStartedAt.IsZero() {
		timing.BaseRefreshStartedAt = timing.MergeStartedAt
	}
	if timing.CIWaitStartedAt.IsZero() && !timing.MergeStartedAt.IsZero() {
		timing.CIWaitStartedAt = timing.MergeStartedAt
	}
	if !timing.BaseRefreshStartedAt.IsZero() && timing.BaseRefreshFinishedAt.IsZero() {
		timing.BaseRefreshFinishedAt = completedAt
	}
	if !timing.CIWaitStartedAt.IsZero() && timing.CIWaitFinishedAt.IsZero() {
		timing.CIWaitFinishedAt = completedAt
	}
	timing = timing.withDurations(completedAt)
	state.MergeTimings[strings.TrimSpace(issue.ID)] = timing
	return timing
}

func mergeQueueEnteredAt(issue connector.Issue, fallback time.Time) time.Time {
	for _, candidate := range []*time.Time{issue.StageUpdatedAt, issue.UpdatedAt, issue.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return candidate.UTC()
		}
	}
	return fallback.UTC()
}

func (t MergeTiming) withDurations(now time.Time) MergeTiming {
	now = now.UTC()
	queueEnd := firstNonZeroTime(t.MergeWorkerSlotAcquiredAt, t.MergeStartedAt, t.MergedAt, t.MergeFailedAt, now)
	t.QueueWaitSeconds = durationSeconds(t.EnteredMergingAt, queueEnd)
	activeStart := firstNonZeroTime(t.MergeWorkerSlotAcquiredAt, t.MergeStartedAt)
	activeEnd := firstNonZeroTime(t.MergedAt, t.MergeFailedAt, now)
	t.ActiveMergeDurationSeconds = durationSeconds(activeStart, activeEnd)
	totalEnd := firstNonZeroTime(t.MergedAt, t.MergeFailedAt, now)
	t.TotalMergingSeconds = durationSeconds(t.EnteredMergingAt, totalEnd)
	return t
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value.UTC()
		}
	}
	return time.Time{}
}

func durationSeconds(start time.Time, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return int64(end.Sub(start) / time.Second)
}

func (o *Orchestrator) logMergeTimingInfo(event string, issue connector.Issue, timing MergeTiming, attrs ...any) {
	if o.logger == nil {
		return
	}
	o.logger.Info(event, mergeTimingLogAttrs(issue, timing, attrs...)...)
}

func (o *Orchestrator) logMergeTimingWarn(event string, issue connector.Issue, timing MergeTiming, attrs ...any) {
	if o.logger == nil {
		return
	}
	o.logger.Warn(event, mergeTimingLogAttrs(issue, timing, attrs...)...)
}

func mergeTimingLogAttrs(issue connector.Issue, timing MergeTiming, attrs ...any) []any {
	out := mergeWorkerLogAttrs(issue)
	out = append(out,
		"issue_number", issueNumberFromIdentifier(issue.Identifier),
	)
	out = append(out, mergeTimingAttrs(timing)...)
	return append(out, attrs...)
}

func mergeTimingAttrs(timing MergeTiming) []any {
	out := []any{
		"queue_wait_seconds", timing.QueueWaitSeconds,
		"active_merge_duration_seconds", timing.ActiveMergeDurationSeconds,
		"total_merging_seconds", timing.TotalMergingSeconds,
	}
	out = appendTimeAttr(out, "entered_merging_at", timing.EnteredMergingAt)
	out = appendTimeAttr(out, "merge_worker_slot_acquired_at", timing.MergeWorkerSlotAcquiredAt)
	out = appendTimeAttr(out, "merge_started_at", timing.MergeStartedAt)
	out = appendTimeAttr(out, "base_refresh_started_at", timing.BaseRefreshStartedAt)
	out = appendTimeAttr(out, "base_refresh_finished_at", timing.BaseRefreshFinishedAt)
	out = appendTimeAttr(out, "ci_wait_started_at", timing.CIWaitStartedAt)
	out = appendTimeAttr(out, "ci_wait_finished_at", timing.CIWaitFinishedAt)
	out = appendTimeAttr(out, "merged_at", timing.MergedAt)
	out = appendTimeAttr(out, "merge_failed_at", timing.MergeFailedAt)
	if timing.MergeFailureReason != "" {
		out = append(out, "merge_failure_reason", timing.MergeFailureReason)
	}
	return out
}

func appendTimeAttr(attrs []any, key string, value time.Time) []any {
	if value.IsZero() {
		return attrs
	}
	return append(attrs, key, value.UTC().Format(time.RFC3339))
}

func issueNumberFromIdentifier(identifier string) int {
	index := strings.LastIndex(identifier, "#")
	if index < 0 || index == len(identifier)-1 {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSpace(identifier[index+1:]))
	if err != nil {
		return 0
	}
	return number
}
