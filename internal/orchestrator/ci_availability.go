package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	ciUnavailableMinimumChecks       = 2
	ciUnavailableMinimumPullRequests = 2
	ciUnavailableErrorClass          = "ci_unavailable"
)

type CICondition = telemetry.CICondition

type ciUnavailablePullRequest struct {
	checkCount        int
	oldestQueueSecond int64
}

func (o *Orchestrator) syncCIAvailability(state *State, issues []connector.Issue, now time.Time) {
	if state == nil {
		return
	}
	if now.IsZero() {
		now = o.clockNow()
	}
	condition, unavailable := evaluateCIAvailability(issues, now)
	if !unavailable {
		if state.CIUnavailable != nil {
			recordStateEvent(state, telemetry.ActivityEvent{
				At:      now.UTC(),
				Event:   "ci_availability_recovered",
				Message: "CI checks started moving again; CI-gated dispatch resumed",
			})
		}
		state.CIUnavailable = nil
		return
	}
	condition.ProjectID = strings.TrimSpace(o.cfg.Project.ID)
	if state.CIUnavailable != nil && !state.CIUnavailable.DetectedAt.IsZero() {
		condition.DetectedAt = state.CIUnavailable.DetectedAt
	} else {
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now.UTC(),
			Event:   "ci_unavailable",
			Message: ciUnavailableMessage(condition),
		})
		if o.logger != nil {
			o.logger.Warn(
				"CI unavailable",
				"project_id", condition.ProjectID,
				"unstarted_checks", condition.UnstartedCheckCount,
				"pull_requests", condition.PullRequestCount,
				"oldest_queue_seconds", condition.OldestQueueSeconds,
			)
		}
	}
	state.CIUnavailable = &condition
	o.parkCIUnavailableWaiters(state, issues, now)
}

func evaluateCIAvailability(issues []connector.Issue, now time.Time) (CICondition, bool) {
	pullRequests := make(map[string]ciUnavailablePullRequest)
	for _, issue := range issues {
		pullRequest := issue.PullRequest
		if pullRequest == nil {
			continue
		}
		checkCount := max(pullRequest.UnstartedCheckCount, len(pullRequest.UnstartedChecks))
		if checkCount == 0 {
			continue
		}
		key := ciAvailabilityPullRequestKey(issue)
		if key == "" {
			continue
		}
		observation := pullRequests[key]
		observation.checkCount = max(observation.checkCount, checkCount)
		for _, check := range pullRequest.UnstartedChecks {
			observation.oldestQueueSecond = max(observation.oldestQueueSecond, check.QueueSeconds)
		}
		pullRequests[key] = observation
	}

	condition := CICondition{
		PullRequestCount: len(pullRequests),
		DetectedAt:       now.UTC(),
		LastObservedAt:   now.UTC(),
	}
	for _, observation := range pullRequests {
		condition.UnstartedCheckCount += observation.checkCount
		condition.OldestQueueSeconds = max(condition.OldestQueueSeconds, observation.oldestQueueSecond)
	}
	return condition, condition.UnstartedCheckCount >= ciUnavailableMinimumChecks &&
		condition.PullRequestCount >= ciUnavailableMinimumPullRequests
}

func ciAvailabilityPullRequestKey(issue connector.Issue) string {
	pullRequest := issue.PullRequest
	if pullRequest == nil {
		return ""
	}
	if repository, number := strings.TrimSpace(pullRequestRepository(issue)), pullRequestNumber(issue); repository != "" && number > 0 {
		return repository + "#" + strconv.Itoa(number)
	}
	if value := strings.TrimSpace(pullRequest.URL); value != "" {
		return value
	}
	if number := pullRequestNumber(issue); number > 0 {
		return "#" + strconv.Itoa(number)
	}
	return strings.TrimSpace(issue.ID)
}

func activeCIUnavailable(state *State) bool {
	return state != nil && state.CIUnavailable != nil
}

func ciDependentDispatch(issue connector.Issue) bool {
	return mergeWorkerIssue(issue)
}

func ciUnavailableRetry(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	return state.Retry[issueID].CIUnavailable
}

func (o *Orchestrator) parkCIUnavailableWaiters(state *State, issues []connector.Issue, now time.Time) {
	if state == nil {
		return
	}
	fresh := make(map[string]connector.Issue, len(issues))
	for _, issue := range issues {
		if id := strings.TrimSpace(issue.ID); id != "" {
			fresh[id] = issue
		}
	}
	parked := 0
	for issueID, running := range state.Running {
		if issue, ok := fresh[issueID]; ok {
			running.Issue = mergeIssueTrackerFields(running.Issue, issue)
		}
		if !ciUnavailableWaitingRun(running, state) {
			state.Running[issueID] = running
			continue
		}
		parked++
		if running.CIStopRequested || running.stop == nil {
			state.Running[issueID] = running
			continue
		}
		running.CIStopRequested = true
		state.Running[issueID] = running
		running.stop(runpkg.ErrCIUnavailable)
		recordStateEvent(state, telemetry.ActivityEvent{
			At:      now.UTC(),
			Event:   "ci_unavailable_attempt_parking",
			Message: "parking " + issueLabel(running.Issue) + " until CI checks start moving",
		})
	}
	if state.CIUnavailable != nil {
		state.CIUnavailable.ParkedAttemptCount = parked
	}
}

func ciUnavailableWaitingRun(running Running, state *State) bool {
	pullRequest := running.Issue.PullRequest
	if pullRequest == nil || max(pullRequest.UnstartedCheckCount, len(pullRequest.UnstartedChecks)) == 0 {
		return false
	}
	if mergeWorkerIssue(running.Issue) || strings.TrimSpace(running.Mode) == runpkg.RunModeMerge {
		return true
	}
	if strings.TrimSpace(running.Mode) == runpkg.RunModePlan {
		return false
	}
	return running.WorkProductPushed || runningWorkAttemptPhase(running, state) == "waiting_ci"
}

func (o *Orchestrator) handleCIUnavailableCompletion(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
) bool {
	if !errors.Is(event.Err, runpkg.ErrCIUnavailable) {
		return false
	}
	message := "CI unavailable; attempt parked until checks start moving"
	o.completeDurableWorkAttempt(
		ctx,
		state,
		running,
		event.CompletedAt,
		store.WorkAttemptTerminalCapacity,
		ciUnavailableErrorClass,
		message,
		"waiting",
		message,
	)
	if workspaceIssueTerminal(running.Issue, o.cfg.TerminalStates) {
		o.releaseClaim(state, running.Issue.ID)
		return true
	}
	state.Retry[running.Issue.ID] = Retry{
		Issue:         cloneIssue(running.Issue),
		Attempt:       running.Attempt,
		DueAt:         event.CompletedAt,
		Error:         message,
		WorkerHost:    running.WorkerHost,
		CIUnavailable: true,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt.UTC(),
		Event:   "ci_unavailable_attempt_parked",
		Message: "parked " + issueLabel(running.Issue) + " without holding an agent slot",
	})
	return true
}

func ciUnavailableMessage(condition CICondition) string {
	return fmt.Sprintf(
		"CI unavailable: %d checks queued and unstarted across %d PRs; oldest queued %s",
		condition.UnstartedCheckCount,
		condition.PullRequestCount,
		time.Duration(condition.OldestQueueSeconds)*time.Second,
	)
}

func cloneCICondition(condition *CICondition) *CICondition {
	if condition == nil {
		return nil
	}
	cloned := *condition
	return &cloned
}
