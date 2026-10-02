package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const attemptAllowanceExhaustedReason = "attempt_allowance_exhausted"

func allowanceInfrastructureAttempt(attempt store.WorkAttempt) bool {
	if preTurnAttempt(telemetry.WorkAttempt{ErrorClass: attempt.ErrorClass, MetricsJSON: attempt.MetricsJSON, WorkerMetadataJSON: attempt.WorkerMetadataJSON}) {
		return true
	}
	if attempt.TerminalState == store.WorkAttemptTerminalAbandoned || attempt.TerminalState == store.WorkAttemptTerminalCapacity || attempt.Phase == "completion_deferred" {
		return true
	}
	class := strings.ToLower(strings.TrimSpace(attempt.ErrorClass))
	if class == workAttemptErrorRunner {
		class = runnerWorkAttemptErrorClass(errors.New(attempt.ErrorMessage))
	}
	if strings.HasPrefix(class, "backend_startup_") {
		return true
	}
	switch class {
	case "workspace_preparation", "deliverable_configuration_failure", "tracker_unavailable", "forge_unavailable",
		"service_restart", "lease_expired", "start_state_transition_failed", "backend_capacity", "transient_overload", "backend_protocol_error", "transport_error":
		return true
	}
	var metadata struct {
		Cancellation    *runpkg.CancellationCause   `json:"cancellation"`
		BlockerEvidence []telemetry.BlockerEvidence `json:"blocker_evidence"`
		Fence           struct {
			Excluded bool `json:"excluded_from_worker_outcomes"`
		} `json:"historical_completion_fence"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) == nil {
		if metadata.Fence.Excluded || attempt.TerminalState == store.WorkAttemptTerminalCancelled && metadata.Cancellation.IsAvailabilityInterruption() {
			return true
		}
		for _, evidence := range metadata.BlockerEvidence {
			if evidence.Owner == workpad.BlockerOwnerInstance {
				return true
			}
		}
	}
	return false
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
		issue.Comments = comments
		if resolved, ok := o.resolveMergedCompletionPullRequest(ctx, issue); ok {
			issue = resolved
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
		// CI that is still running belongs to the gate, not the exhausted
		// source-worker allowance. Re-evaluate this durable triage on the next tick.
		if decision.Reason == AutoPromoteReasonCINotGreen && attemptTriageCIPending(issue.PullRequest) {
			return nil
		}
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
	targetState := o.nonReviewDecisionTargetState(issue, normalizeAutoPromoteConfig(o.cfg.AutoPromote).reviewTargetState())
	if metadata.PreserveLane || normalizeState(issue.State) == normalizeState(targetState) {
		return nil
	}
	return o.updateIssueState(ctx, state, issue, targetState, now, attemptAllowanceExhaustedReason)
}

func attemptTriageCIPending(pr *connector.PullRequest) bool {
	if pr == nil || !currentHeadCIStatusPending(pr.CIStatus) {
		return false
	}
	for _, check := range pr.RequiredCheckFailures {
		if !attemptTriageCheckRunning(check) {
			return false
		}
	}
	if len(pr.RunningChecks) > 0 {
		return true
	}
	for _, check := range pr.Checks {
		if attemptTriageCheckRunning(check) {
			return true
		}
	}
	return false
}

func attemptTriageCheckRunning(check connector.PullRequestCheck) bool {
	if strings.EqualFold(strings.TrimSpace(check.Conclusion), "missing") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(check.Status)) {
	case "pending", "queued", "waiting", "in_progress", "in progress", "requested":
		return true
	default:
		return false
	}
}

// Observation timestamps qualify the historical worker explanation without
// pretending that the provider's check completion time is available here.
func attemptTriageObservedEvidence(issue connector.Issue, observedAt time.Time) string {
	pr := issue.PullRequest
	if pr == nil {
		return ""
	}
	ciEvidence := pr.CIStatus
	for _, check := range pr.Checks {
		if strings.EqualFold(strings.TrimSpace(check.Conclusion), "skipped") {
			ciEvidence = "not fully verified (checks skipped; aggregate: " + pr.CIStatus + ")"
			break
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n### PR evidence observed at %s\nHead: `%s`; mergeable state: `%s`; CI: `%s`; unresolved threads: %d.\nThe worker explanation above predates this observation.\n", observedAt.UTC().Format(time.RFC3339), pr.HeadSHA, pr.MergeableState, ciEvidence, len(pr.UnresolvedReviewThreads))
	for _, check := range pr.Checks {
		fmt.Fprintf(&b, "- Check %q (run %d): status=%q, conclusion=%q; observed at %s.\n", check.Name, check.ID, check.Status, check.Conclusion, observedAt.UTC().Format(time.RFC3339))
	}
	return b.String()
}
