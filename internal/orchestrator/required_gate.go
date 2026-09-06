package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) refreshRequiredGateEvidence(ctx context.Context, state *State, issues []connector.Issue) []connector.Issue {
	issues = cloneIssues(issues)
	now := time.Now()
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	state.RequiredGates = make(map[string]telemetry.RequiredGate, len(issues))
	for i, issue := range issues {
		if issue.PullRequest == nil {
			continue
		}
		if normalizeState(issue.State) == normalizeState(normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState) {
			if refreshed, current := o.refreshImplementCompletionIssue(ctx, issue); current {
				issue = refreshed
				issues[i] = refreshed
			}
		}
		summary := AutoPromoteSummaryFromIssue(issue)
		summary.SecurityAudit = o.securityAuditEvaluation(ctx, issue)
		if gate.Effective(cfg.Gate).Validator.Enabled {
			summary.Validator, _, _ = o.validatorStageResult(ctx, issue)
		}
		summary.AutomatedReviewWaitExpired = autoPromoteReviewWaitExpired(state, issue.ID, cfg, now)
		state.RequiredGates[issue.ID] = requiredGateFromSummary(issue, summary, cfg, now)
	}
	return issues
}

func requiredGateFromSummary(issue connector.Issue, summary AutoPromoteSummary, cfg AutoPromoteConfig, now time.Time) telemetry.RequiredGate {
	decision := gate.Evaluate(cfg.Gate, issue.Labels, gateSummary(summary), now, gate.EvaluationOptions{
		QuietDuration:              cfg.QuietDuration,
		AutomatedReviewWaitExpired: summary.AutomatedReviewWaitExpired,
	})
	result := telemetry.RequiredGate{State: "pending", Reason: string(decision.Reason), CIState: summary.CIStatus,
		MergeableState: summary.MergeableState, AuditRunID: summary.SecurityAudit.RunID, AuditReason: summary.SecurityAudit.Reason}
	if issue.PullRequest != nil {
		result.PRNumber = issue.PullRequest.Number
		result.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
		result.BaseSHA = strings.TrimSpace(issue.PullRequest.BaseSHA)
	}
	switch decision.Action {
	case gate.ActionPass:
		result.State = "passed"
	case gate.ActionRework:
		result.State = "failed"
	}
	if gate.Effective(cfg.Gate).SecurityAudit.Enabled && !summary.SecurityAudit.Allowed && summary.SecurityAudit.Reason != securityaudit.ReasonMissing {
		result.State = "failed"
		result.Reason = string(AutoPromoteReasonSecurityAuditFailed)
		if summary.SecurityAudit.Reason == securityaudit.ReasonUnresolvedFindings {
			result.Reason = string(AutoPromoteReasonSecurityAuditFindings)
		}
	}
	if len(summary.FailedChecks) > 0 || autoPromoteMergeConflicts(summary.MergeableState) || len(summary.UnresolvedReviewThreads) > 0 {
		result.State = "failed"
		result.Reason = string(AutoPromoteReasonCINotGreen)
		if autoPromoteMergeConflicts(summary.MergeableState) {
			result.Reason = string(AutoPromoteReasonMergeConflicts)
		} else if len(summary.UnresolvedReviewThreads) > 0 {
			result.Reason = string(AutoPromoteReasonUnresolvedReviewThreads)
		}
	}
	if reworkGateWaitWorkpadBlocked(issue) {
		result.State = "failed"
		result.Reason = string(AutoPromoteReasonWorkpadBlocker)
		_, result.HumanAction = implementProgressBlockedHumanAction(issue)
	}
	if summary.PullRequestHydrationUnavailableReason != "" || summary.PullRequestHydrationDegradedReason != "" {
		result.State = "unavailable"
		result.Reason = string(AutoPromoteReasonPullRequestHydrationUnavailable)
	}
	return result
}
