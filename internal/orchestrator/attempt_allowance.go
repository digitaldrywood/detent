package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const attemptAllowanceExhaustedReason = "attempt_allowance_exhausted"
const sessionsWithoutMergeAllowance = 3

type attemptAllowance struct {
	Sessions int
	Attempts []store.WorkAttempt
	Triage   *store.WorkAttempt
}

func (a attemptAllowance) exhausted() bool { return a.Sessions >= sessionsWithoutMergeAllowance }

// Unlike progress accounting, every started code/rework attempt consumes the same
// issue allowance. A changed head, lane, diff, or operator acknowledgement is not
// a merge and cannot replenish it.
func countSessionsWithoutMerge(attempts []store.WorkAttempt, mergedAt time.Time) attemptAllowance {
	var result attemptAllowance
	for _, attempt := range attempts {
		if !mergedAt.IsZero() && !attempt.StartedAt.After(mergedAt) {
			continue
		}
		if allowanceInfrastructureAttempt(attempt) {
			continue
		}
		if attempt.WorkerType == runpkg.RunModeTriage {
			if result.Triage == nil || attempt.ID > result.Triage.ID {
				copy := attempt
				result.Triage = &copy
			}
			continue
		}
		switch attempt.WorkerType {
		case "agent", "code", "rework", "implementation":
		default:
			continue
		}
		result.Sessions++
		result.Attempts = append(result.Attempts, attempt)
	}
	return result
}

func allowanceInfrastructureAttempt(attempt store.WorkAttempt) bool {
	if preTurnAttempt(telemetry.WorkAttempt{ErrorClass: attempt.ErrorClass, MetricsJSON: attempt.MetricsJSON, WorkerMetadataJSON: attempt.WorkerMetadataJSON}) {
		return true
	}
	if attempt.TerminalState == store.WorkAttemptTerminalCapacity || attempt.Phase == "completion_deferred" {
		return true
	}
	class := strings.ToLower(strings.TrimSpace(attempt.ErrorClass))
	if strings.HasPrefix(class, "backend_startup_") {
		return true
	}
	switch class {
	case "workspace_preparation", "deliverable_configuration_failure", "tracker_unavailable", "forge_unavailable",
		"service_restart", "lease_expired", "start_state_transition_failed", "backend_capacity", "transient_overload", "backend_protocol_error", "transport_error":
		return true
	}
	var metadata struct {
		Fence struct {
			Excluded bool `json:"excluded_from_worker_outcomes"`
		} `json:"historical_completion_fence"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) == nil && metadata.Fence.Excluded {
		return true
	}
	return false
}

func (o *Orchestrator) issueAttemptAllowance(ctx context.Context, issue connector.Issue) (attemptAllowance, error) {
	if _, operational := operationalCompletionFromIssue(issue); operational {
		return attemptAllowance{}, nil
	}
	if o.workAttempts == nil {
		return attemptAllowance{}, nil
	}
	var mergeEvents []store.WorkflowPhaseEvent
	if reader, ok := o.workflowMetrics.(WorkflowMetricsTimelineReader); ok {
		timeline, err := reader.IssueWorkflowTimeline(ctx, store.IssueIdentity{ProjectID: o.workflowMetricsProjectID(), IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL})
		if err != nil {
			return attemptAllowance{}, err
		}
		mergeEvents = timeline.Events
	}
	mergedAt := lastAllowanceMergeAt(issue, mergeEvents)
	// No recent-history cap: excluded infrastructure attempts must never hide the
	// three chargeable sessions, even after a prolonged instance outage.
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID: o.cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, IssueURL: issue.URL, Limit: math.MaxInt32,
	})
	if err != nil {
		return attemptAllowance{}, err
	}
	active, err := o.workAttempts.ListActiveWorkAttempts(ctx, store.WorkAttemptQuery{ProjectID: o.cfg.Project.ID})
	if err != nil {
		return attemptAllowance{}, err
	}
	for _, attempt := range active {
		if attempt.IssueID == issue.ID && issue.ID != "" || attempt.Identifier == issue.Identifier && issue.Identifier != "" || attempt.IssueURL == issue.URL && issue.URL != "" {
			attempts = append(attempts, attempt)
		}
	}
	return countSessionsWithoutMerge(attempts, mergedAt), nil
}

func lastAllowanceMergeAt(issue connector.Issue, events []store.WorkflowPhaseEvent) time.Time {
	// A repeated observation of an already merged PR is not another merge.
	// Historical events without immutable provider timestamps use their first
	// observation for that PR; current provider evidence refines that boundary.
	merges := map[int64]time.Time{}
	for _, event := range events {
		if event.PhaseType != store.WorkflowPhaseTypeLane || !strings.EqualFold(event.Status, "entered") ||
			(event.Reason != "merge_worker_programmatic_merge" && event.Reason != "pull_request_merged") {
			continue
		}
		var number int64
		if event.PRNumber != nil {
			number = *event.PRNumber
		}
		if metadata, ok := workflowLaneMetadataFromJSON(event.MetadataJSON); ok && metadata.PullRequest != nil {
			number = metadata.PullRequest.Number
		}
		at := workflowLaneTransitionAt(event)
		if previous, found := merges[number]; !found || at.Before(previous) {
			merges[number] = at
		}
	}
	if pr := issue.PullRequest; pr != nil && strings.EqualFold(pr.State, "merged") && pr.MergedAt != nil {
		merges[int64(pr.Number)] = *pr.MergedAt
	}
	var latest time.Time
	for _, at := range merges {
		latest = laterDispatchLoopTime(latest, at)
	}
	return latest
}

func attemptTriageContext(issue connector.Issue, allowance attemptAllowance) (string, error) {
	// Connector hydration supplies PR checks and review-thread links. Preserve
	// prior output and failure summaries as evidence, never as executable policy.
	data, err := json.Marshal(struct {
		Issue                connector.Issue
		SessionsWithoutMerge int
		PriorSessions        []store.WorkAttempt
	}{issue, allowance.Sessions, allowance.Attempts})
	return string(data), err
}

func validAttemptTriageNote(note string) bool {
	sections := []string{"## Why this stalled", "## What is blocking", "## Options"}
	index := -1
	counts := [3]int{}
	for _, line := range strings.Split(strings.TrimSpace(note), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if index+1 < len(sections) && line == sections[index+1] {
			index++
			continue
		}
		if index < 0 || strings.HasPrefix(line, "#") {
			return false
		}
		counts[index]++
		if index > 0 && !strings.HasPrefix(line, "- ") {
			return false
		}
		if index == 1 && (!strings.Contains(line, "](https://") && !strings.Contains(line, "](http://")) {
			return false
		}
	}
	return index == 2 && counts[0] >= 1 && counts[0] <= 3 && counts[1] > 0 && counts[2] >= 2 && counts[2] <= 3
}

func fallbackAttemptTriageNote(issue connector.Issue, detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	return fmt.Sprintf("## Why this stalled\nThree worker sessions started without a merged PR.\nThe single triage pass could not produce a valid explanation: %s\n\n## What is blocking\n- [Issue and prior session evidence](%s) require human diagnosis; automatic work is exhausted.\n\n## Options\n- Inspect the prior sessions and finish the existing PR manually.\n- Narrow the scope into a follow-up issue and close this stalled work.", detail, issue.URL)
}

func (o *Orchestrator) finishAttemptTriage(ctx context.Context, state *State, event runpkg.Completion, running Running) {
	if event.Result.Tokens != (TokenTotals{}) {
		running.Tokens = event.Result.Tokens
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, running.Tokens)
	note := strings.TrimSpace(event.Result.Output)
	if event.Err != nil {
		note = fallbackAttemptTriageNote(running.Issue, event.Err.Error())
	} else if refusal := event.Result.BudgetRefusal; refusal != nil {
		note = fallbackAttemptTriageNote(running.Issue, "budget admission refused ("+refusal.Code+"): "+refusal.Message)
	} else if !validAttemptTriageNote(note) {
		note = fallbackAttemptTriageNote(running.Issue, "invalid or missing output")
	}
	if !o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", "read-only triage completed", map[string]any{"attempt_allowance_triage": note, "attempt_allowance_preserve_lane": running.CompletionLane != ""}) {
		return
	}
	o.releaseCompletedAttemptClaim(ctx, state, running.Issue)
	releaseDispatchRecoveryAdmission(state, running.Issue.ID)
	releaseProjectFailureBreakerCanary(state, running.Issue.ID)
	releaseBackendCapacityProbe(state, running)
	delete(state.Retry, running.Issue.ID)
	if err := o.publishAttemptTriage(ctx, state, running.Issue, store.WorkAttempt{ID: running.WorkAttemptID, WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{"attempt_allowance_triage": note, "attempt_allowance_preserve_lane": running.CompletionLane != ""})}, event.CompletedAt); err != nil && o.logger != nil {
		o.logger.Warn("publish stalled issue triage", "issue_id", running.Issue.ID, "error", err)
	}
}

func (o *Orchestrator) publishAttemptTriage(ctx context.Context, state *State, issue connector.Issue, attempt store.WorkAttempt, now time.Time) error {
	if attempt.Status == store.WorkAttemptStatusActive {
		return nil
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return errors.New("triage publication requires issue comment reads")
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- detent-attempt-triage:%d -->", attempt.ID)
	posted := false
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) {
			posted = true
			break
		}
	}
	var metadata struct {
		Note         string `json:"attempt_allowance_triage"`
		PreserveLane bool   `json:"attempt_allowance_preserve_lane"`
	}
	if err := json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata); err != nil {
		metadata.Note = ""
	}
	// The durable triage consumes the single pass, but its explanation is only
	// historical evidence. Reuse promotion policy against the current PR before
	// publishing it or moving the issue, including publication retries.
	issue.Comments = comments
	if issue.PullRequest != nil || issue.PRNumber != nil {
		hydrator, ok := o.connector.(connector.PullRequestHydrator)
		if !ok {
			return errors.New("triage publication requires live pull request hydration")
		}
		issue, err = hydrator.HydratePullRequest(ctx, issue)
		if err != nil {
			return err
		}
		if issue.PullRequest == nil || pullRequestHydrationUnavailableReason(issue.PullRequest) != "" || issue.PullRequest.HydrationDegradedReason != "" {
			return errors.New("triage pull request evidence unavailable")
		}
		if autoPromotePullRequestMerged(issue.PullRequest) {
			if !metadata.PreserveLane {
				o.reconcileStaleLinkedPullRequestIssues(ctx, state, []connector.Issue{issue}, now)
			}
			return nil
		}
		var hydrated bool
		issue, hydrated = o.hydrateAutoPromoteReviewThreads(ctx, issue)
		if !hydrated {
			return errors.New("triage review thread evidence unavailable")
		}
		cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
		summary := AutoPromoteSummaryFromIssue(issue)
		summary.SecurityAudit = o.securityAuditEvaluation(ctx, issue)
		summary.CompletedFinalState = autoPromoteCompletedFinalState(state, issue.ID)
		summary.AutomatedReviewWaitExpired = autoPromoteReviewWaitExpired(state, issue.ID, cfg, now)
		issue, decision := o.hydrateAutoPromoteWorkpadDecision(ctx, issue, summary, cfg, now)
		if decision.Reason == AutoPromoteReasonSecurityAuditMissing {
			o.startSecurityAuditStage(ctx, issue, now)
			return nil
		}
		if decision.Reason == AutoPromoteReasonSecurityAuditWait {
			return nil
		}
		var validatorReady bool
		decision, validatorReady = o.applyValidatorStage(ctx, state, issue, &summary, decision, cfg, now)
		if !validatorReady {
			return nil
		}
		if !metadata.PreserveLane && decision.Action == AutoPromoteActionPromote {
			target := autoPromoteTargetState(decision.Action, cfg)
			if normalizeState(issue.State) != normalizeState(target) {
				if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, target, now) {
					return errors.New("triage live pull request promotion failed")
				}
			}
			o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
			return nil
		}
	}
	if !posted {
		if !validAttemptTriageNote(metadata.Note) {
			metadata.Note = fallbackAttemptTriageNote(issue, "triage interrupted before its result was persisted")
		}
		if err := o.connector.CreateComment(ctx, issue.ID, metadata.Note+"\n\n"+marker+attemptTriageObservedEvidence(issue, now)); err != nil {
			return err
		}
	}
	if metadata.PreserveLane || normalizeState(issue.State) == normalizeState(autoPromoteSourceState) {
		return nil
	}
	return o.updateIssueState(ctx, state, issue, autoPromoteSourceState, now, attemptAllowanceExhaustedReason)
}

// Observation timestamps qualify the historical worker explanation without
// pretending that the provider's check completion time is available here.
func attemptTriageObservedEvidence(issue connector.Issue, observedAt time.Time) string {
	pr := issue.PullRequest
	if pr == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n### PR evidence observed at %s\nHead: `%s`; mergeable state: `%s`; CI: `%s`; unresolved threads: %d.\nThe worker explanation above predates this observation.\n", observedAt.UTC().Format(time.RFC3339), pr.HeadSHA, pr.MergeableState, pr.CIStatus, len(pr.UnresolvedReviewThreads))
	for _, check := range pr.Checks {
		fmt.Fprintf(&b, "- Check %q (run %d): status=%q, conclusion=%q; observed at %s.\n", check.Name, check.ID, check.Status, check.Conclusion, observedAt.UTC().Format(time.RFC3339))
	}
	return b.String()
}
