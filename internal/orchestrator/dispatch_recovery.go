package orchestrator

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	dispatchRecoveryGitHubREST            = "github_rest"
	dispatchRecoveryPullRequestHydration  = "pull_request_hydration"
	dispatchRecoveryBackendCapacity       = "backend_capacity"
	dispatchRecoveryTrackerUnavailable    = connector.TrackerUnavailableCondition
	dispatchRecoveryProjectFailureBreaker = "project_failure_breaker"
	dispatchRecoveryStatusWaiting         = "waiting"
	dispatchRecoveryStatusRamping         = "ramping"
)

type DispatchRecovery struct {
	Kind       string
	Reason     string
	Status     string
	StartedAt  time.Time
	ResumeAt   time.Time
	Limit      int
	Admissions map[string]bool
}

func dispatchRecoverySnapshots(
	recoveries map[string]DispatchRecovery,
	pool string,
	maxConcurrent int,
) []telemetry.DispatchRecovery {
	rows := make([]telemetry.DispatchRecovery, 0, len(recoveries))
	for _, key := range sortedKeys(recoveries) {
		recovery := recoveries[key]
		progressed := 0
		for _, value := range recovery.Admissions {
			if value {
				progressed++
			}
		}
		rows = append(rows, telemetry.DispatchRecovery{
			Pool:          strings.TrimSpace(pool),
			Kind:          recovery.Kind,
			Reason:        recovery.Reason,
			Status:        recovery.Status,
			StartedAt:     recovery.StartedAt,
			ResumeAt:      recovery.ResumeAt,
			Limit:         recovery.Limit,
			MaxConcurrent: maxConcurrent,
			Admitted:      len(recovery.Admissions),
			Progressed:    progressed,
		})
	}
	return rows
}

func dispatchRecoveriesCapacitySnapshot(
	recoveries map[string]DispatchRecovery,
	pool string,
	maxConcurrent int,
) []map[string]any {
	rows := make([]map[string]any, 0, len(recoveries))
	for _, recovery := range dispatchRecoverySnapshots(recoveries, pool, maxConcurrent) {
		rows = append(rows, map[string]any{
			"pool":           recovery.Pool,
			"kind":           recovery.Kind,
			"reason":         recovery.Reason,
			"status":         recovery.Status,
			"started_at":     recovery.StartedAt,
			"resume_at":      recovery.ResumeAt,
			"limit":          recovery.Limit,
			"max_concurrent": recovery.MaxConcurrent,
			"admitted":       recovery.Admitted,
			"progressed":     recovery.Progressed,
		})
	}
	return rows
}

func cloneDispatchRecoveries(recoveries map[string]DispatchRecovery) map[string]DispatchRecovery {
	cloned := make(map[string]DispatchRecovery, len(recoveries))
	for key, recovery := range recoveries {
		recovery.Admissions = make(map[string]bool, len(recovery.Admissions))
		for issueID, progressed := range recoveries[key].Admissions {
			recovery.Admissions[issueID] = progressed
		}
		cloned[key] = recovery
	}
	return cloned
}

func (o *Orchestrator) markDispatchRecoveryWait(state *State, kind string, reason string, resumeAt time.Time, now time.Time) {
	if state == nil || strings.TrimSpace(kind) == "" {
		return
	}
	if state.DispatchRecoveries == nil {
		state.DispatchRecoveries = map[string]DispatchRecovery{}
	}
	kind = strings.TrimSpace(kind)
	recovery := state.DispatchRecoveries[kind]
	if recovery.StartedAt.IsZero() {
		recovery.StartedAt = now
	}
	recovery.Kind = kind
	recovery.Reason = strings.TrimSpace(reason)
	recovery.Status = dispatchRecoveryStatusWaiting
	recovery.ResumeAt = resumeAt
	recovery.Limit = 0
	recovery.Admissions = nil
	state.DispatchRecoveries[kind] = recovery
}

func (o *Orchestrator) activateDispatchRecovery(
	state *State,
	kind string,
	reason string,
	now time.Time,
	progressedIssueID string,
) {
	if state == nil || strings.TrimSpace(kind) == "" {
		return
	}
	if o.cfg.MaxConcurrentAgents <= 1 {
		delete(state.DispatchRecoveries, strings.TrimSpace(kind))
		return
	}
	if state.DispatchRecoveries == nil {
		state.DispatchRecoveries = map[string]DispatchRecovery{}
	}
	kind = strings.TrimSpace(kind)
	limit := 1
	admissions := map[string]bool{}
	if issueID := strings.TrimSpace(progressedIssueID); issueID != "" {
		admissions[issueID] = true
		limit = min(2, o.cfg.MaxConcurrentAgents)
	}
	state.DispatchRecoveries[kind] = DispatchRecovery{
		Kind:       kind,
		Reason:     strings.TrimSpace(reason),
		Status:     dispatchRecoveryStatusRamping,
		StartedAt:  now,
		ResumeAt:   now,
		Limit:      limit,
		Admissions: admissions,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "dispatch_recovery_ramp_started",
		Message: "dispatch recovery ramp started after " + kind,
	})
}

func (o *Orchestrator) observePullRequestHydrationRecovery(state *State, issues []connector.Issue, now time.Time) {
	if state == nil || len(issues) == 0 {
		return
	}
	blocked := false
	resumeAt := time.Time{}
	reason := "pull request hydration unavailable"
	for _, issue := range issues {
		if !pullRequestHydrationBlocksDispatch(issue) {
			continue
		}
		blocked = true
		if issue.PullRequest == nil {
			continue
		}
		if value := strings.TrimSpace(issue.PullRequest.HydrationUnavailableReason); value != "" {
			reason = value
		} else if value := strings.TrimSpace(issue.PullRequest.HydrationDegradedReason); value != "" {
			reason = value
		}
		if retryAt := issue.PullRequest.HydrationNextRetryAt; retryAt != nil && !retryAt.IsZero() && (resumeAt.IsZero() || retryAt.Before(resumeAt)) {
			resumeAt = *retryAt
		}
	}
	if blocked {
		o.markDispatchRecoveryWait(state, dispatchRecoveryPullRequestHydration, reason, resumeAt, now)
		return
	}
	recovery, ok := state.DispatchRecoveries[dispatchRecoveryPullRequestHydration]
	if !ok || recovery.Status != dispatchRecoveryStatusWaiting {
		return
	}
	o.activateDispatchRecovery(state, recovery.Kind, recovery.Reason, now, "")
}

func dispatchRecoveryBlockReason(state *State, now time.Time) string {
	if state == nil {
		return ""
	}
	for _, key := range sortedKeys(state.DispatchRecoveries) {
		recovery := state.DispatchRecoveries[key]
		if recovery.Status != dispatchRecoveryStatusRamping {
			continue
		}
		if now.Before(recovery.ResumeAt) || len(recovery.Admissions) >= max(recovery.Limit, 1) {
			return recovery.Kind + "_recovery"
		}
	}
	return ""
}

func dispatchRecoveryPollInterval(state *State, now time.Time, interval time.Duration) time.Duration {
	if state == nil {
		return interval
	}
	for _, recovery := range state.DispatchRecoveries {
		if recovery.ResumeAt.IsZero() || !recovery.ResumeAt.After(now) {
			continue
		}
		untilResume := recovery.ResumeAt.Sub(now)
		if interval <= 0 || untilResume < interval {
			interval = untilResume
		}
	}
	return interval
}

func tryReserveDispatchRecovery(state *State, issueID string, now time.Time) (bool, bool, string) {
	if reason := dispatchRecoveryBlockReason(state, now); reason != "" {
		return false, false, reason
	}
	issueID = strings.TrimSpace(issueID)
	reserved := false
	for _, key := range sortedKeys(state.DispatchRecoveries) {
		recovery := state.DispatchRecoveries[key]
		if recovery.Status != dispatchRecoveryStatusRamping {
			continue
		}
		if recovery.Admissions == nil {
			recovery.Admissions = map[string]bool{}
		}
		recovery.Admissions[issueID] = false
		state.DispatchRecoveries[key] = recovery
		reserved = true
	}
	return reserved, true, ""
}

func releaseDispatchRecoveryAdmission(state *State, issueID string) {
	if state == nil {
		return
	}
	issueID = strings.TrimSpace(issueID)
	for key, recovery := range state.DispatchRecoveries {
		delete(recovery.Admissions, issueID)
		state.DispatchRecoveries[key] = recovery
	}
}

func (o *Orchestrator) advanceDispatchRecovery(state *State, issueID string, at time.Time) {
	if state == nil {
		return
	}
	issueID = strings.TrimSpace(issueID)
	for _, key := range sortedKeys(state.DispatchRecoveries) {
		recovery := state.DispatchRecoveries[key]
		progressed, ok := recovery.Admissions[issueID]
		if recovery.Status != dispatchRecoveryStatusRamping || !ok || progressed {
			continue
		}
		recovery.Admissions[issueID] = true
		recovery.Limit++
		if recovery.Limit >= o.cfg.MaxConcurrentAgents {
			delete(state.DispatchRecoveries, key)
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      at,
				Event:   "dispatch_recovery_ramp_completed",
				Message: "configured concurrency restored after " + recovery.Kind,
			})
			continue
		}
		state.DispatchRecoveries[key] = recovery
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      at,
			Event:   "dispatch_recovery_ramp_advanced",
			Message: "dispatch recovery limit increased after " + recovery.Kind,
		})
	}
}

func (o *Orchestrator) backoffDispatchRecovery(state *State, issueID string, failedAt time.Time, delay time.Duration) {
	if state == nil {
		return
	}
	if delay <= 0 {
		delay = defaultOverloadRetryDelay
	}
	if failedAt.IsZero() {
		failedAt = o.clockNow()
	}
	issueID = strings.TrimSpace(issueID)
	for key, recovery := range state.DispatchRecoveries {
		progressed, ok := recovery.Admissions[issueID]
		if recovery.Status != dispatchRecoveryStatusRamping || !ok {
			continue
		}
		if progressed {
			delete(recovery.Admissions, issueID)
			state.DispatchRecoveries[key] = recovery
			continue
		}
		recovery.Limit = 1
		recovery.ResumeAt = failedAt.Add(delay)
		recovery.Admissions = map[string]bool{}
		state.DispatchRecoveries[key] = recovery
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      failedAt,
			Event:   "dispatch_recovery_canary_deferred",
			Message: "dispatch recovery canary failed before progress; retrying after " + recovery.Kind,
		})
	}
}
