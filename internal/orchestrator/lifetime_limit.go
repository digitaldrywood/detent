package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	lifetimeLimitReason                    = "lifetime_limit"
	lifetimeLimitBlockedReasonPrefix       = "lifetime issue limit: "
	blockedRecoveryPredicateLifetimeLimit  = "cooldown_or_override" // Retained for persisted parks; time no longer releases them.
	lifetimeLimitEvidenceUnavailableReason = "lifetime_limit_evidence_unavailable"
)

type LifetimeUsageStore interface {
	IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error)
}

type lifetimeLimitDecision struct {
	Usage           store.TokenSpend
	ProjectHistory  store.ProjectLifetimeUsage
	SessionLimit    int64
	TokenLimit      int64
	SessionsReached bool
	TokensReached   bool
}

func (d lifetimeLimitDecision) reached() bool {
	return d.SessionsReached || d.TokensReached
}

func evaluateLifetimeLimit(usage store.TokenSpend, sessionLimit int64, tokenLimit int64) lifetimeLimitDecision {
	return lifetimeLimitDecision{
		Usage:           usage,
		SessionLimit:    sessionLimit,
		TokenLimit:      tokenLimit,
		SessionsReached: sessionLimit > 0 && usage.Sessions >= sessionLimit,
		TokensReached:   tokenLimit > 0 && usage.TotalTokens >= tokenLimit,
	}
}

func (o *Orchestrator) enforceLifetimeLimits(ctx context.Context, state *State, issues []connector.Issue, now time.Time) {
	if o == nil || state == nil || o.lifetimeUsage == nil || !o.lifetimeLimitsEnabled() {
		return
	}
	state.ensureInitialized(o.cfg)
	var projectHistory store.ProjectLifetimeUsage
	projectHistoryLoaded := false
	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" || !stateIn(issue.State, o.cfg.ActiveStates) {
			continue
		}
		if _, running := state.Running[issue.ID]; running {
			continue
		}
		if _, blocked := state.Blocked[issue.ID]; blocked {
			continue
		}
		usage, err := o.issueLifetimeUsage(ctx, issue)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("lifetime issue usage lookup failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
			}
			continue
		}
		decision := evaluateLifetimeLimit(usage, o.cfg.LifetimeSessionLimit, o.cfg.LifetimeTokenLimit)
		if !decision.reached() || o.lifetimeLimitOverride(issue) {
			continue
		}
		if !projectHistoryLoaded {
			projectHistory = o.projectLifetimeUsage(ctx)
			projectHistoryLoaded = true
		}
		decision.ProjectHistory = projectHistory
		o.parkLifetimeLimit(ctx, state, issue, decision, now)
	}
}

func (o *Orchestrator) projectLifetimeUsage(ctx context.Context) store.ProjectLifetimeUsage {
	historyStore, ok := o.lifetimeUsage.(store.ProjectLifetimeUsageStore)
	if !ok {
		return store.ProjectLifetimeUsage{}
	}
	history, err := historyStore.ProjectLifetimeUsage(ctx, o.workflowMetricsProjectID())
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("project lifetime usage lookup failed", "project_id", o.workflowMetricsProjectID(), "error", err)
		}
		return store.ProjectLifetimeUsage{}
	}
	return history
}

func (o *Orchestrator) lifetimeLimitsEnabled() bool {
	return o.cfg.LifetimeSessionLimit > 0 || o.cfg.LifetimeTokenLimit > 0
}

func (o *Orchestrator) issueLifetimeUsage(ctx context.Context, issue connector.Issue) (store.TokenSpend, error) {
	return o.lifetimeUsage.IssueTokenSpend(ctx, store.IssueIdentity{
		ProjectID:  o.workflowMetricsProjectID(),
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		IssueURL:   issue.URL,
	})
}

func (o *Orchestrator) lifetimeLimitOverride(issue connector.Issue) bool {
	want := strings.ToLower(strings.TrimSpace(o.cfg.LifetimeLimitOverrideLabel))
	if want == "" {
		return false
	}
	for _, label := range issue.Labels {
		if strings.ToLower(strings.TrimSpace(label)) == want {
			return true
		}
	}
	return false
}

func lifetimeLimitUsageSignature(usage store.TokenSpend) string {
	return "sessions=" + strconv.FormatInt(usage.Sessions, 10) + ";tokens=" + strconv.FormatInt(usage.TotalTokens, 10)
}

func (o *Orchestrator) parkLifetimeLimit(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	decision lifetimeLimitDecision,
	now time.Time,
) {
	runMode := o.dispatchMode(ctx, state, issue)
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		runMode,
		lifetimeLimitReason,
		blockedRecoveryPredicateLifetimeLimit,
		issue.State,
		DiffStats{},
	)
	metadata.BlockedRecovery.LifetimeSessions = decision.Usage.Sessions
	metadata.BlockedRecovery.LifetimeTokens = decision.Usage.TotalTokens
	metadata.BlockedRecovery.LifetimeSessionLimit = decision.SessionLimit
	metadata.BlockedRecovery.LifetimeTokenLimit = decision.TokenLimit

	transitioned := false
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, now, lifetimeLimitReason, metadata); err != nil {
		if o.logger != nil {
			o.logger.Error("lifetime issue limit state transition failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	} else {
		transitioned = true
		issue.State = blockedStatusState
	}
	if transitioned && o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, o.lifetimeLimitComment(issue, decision)); err != nil && o.logger != nil {
			o.logger.Warn("lifetime issue limit comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.Warn("lifetime issue limit claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	state.Blocked[issue.ID] = Blocked{
		Issue:                   cloneIssue(issue),
		Reason:                  lifetimeLimitBlockedReason(decision),
		RecoveryAction:          "hold",
		RecoveryReason:          lifetimeLimitReason,
		RecoveryTarget:          metadata.BlockedRecovery.TargetState,
		RecoveryRemedy:          lifetimeLimitRecoveryRemedy(o.cfg.LifetimeLimitOverrideLabel),
		RecoveryReachability:    blockedRecoveryReachability("hold"),
		RecoveryIntentResumable: true,
		BlockedAt:               now,
		Source:                  BlockedSourceProjectStatus,
		Recovery:                metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "lifetime_issue_limit_reached",
		Message: "parked " + issueLabel(issue) + " after " + lifetimeLimitBlockedReason(decision),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(
			o.logger,
			slog.LevelWarn,
			telemetry.LifecycleSafetyControl,
			"lifetime_issue_limit_reached",
			"lifetime issue limit reached",
			o.issueLifecycleCorrelation(issue),
			"sessions", decision.Usage.Sessions,
			"session_limit", decision.SessionLimit,
			"total_tokens", decision.Usage.TotalTokens,
			"token_limit", decision.TokenLimit,
		)
	}
}

func lifetimeLimitBlockedReason(decision lifetimeLimitDecision) string {
	parts := make([]string, 0, 2)
	if decision.SessionsReached {
		parts = append(parts, fmt.Sprintf("sessions %d/%d", decision.Usage.Sessions, decision.SessionLimit))
	}
	if decision.TokensReached {
		parts = append(parts, fmt.Sprintf("tokens %d/%d", decision.Usage.TotalTokens, decision.TokenLimit))
	}
	return lifetimeLimitBlockedReasonPrefix + strings.Join(parts, ", ")
}

func (o *Orchestrator) lifetimeLimitComment(issue connector.Issue, decision lifetimeLimitDecision) string {
	var b strings.Builder
	b.WriteString("Detent parked this issue because its cumulative agent usage reached a configured lifetime limit.\n\n")
	b.WriteString("- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- lifetime_sessions: ")
	b.WriteString(strconv.FormatInt(decision.Usage.Sessions, 10))
	b.WriteString(" / ")
	b.WriteString(formatLifetimeLimit(decision.SessionLimit))
	b.WriteString("\n- lifetime_tokens: ")
	b.WriteString(strconv.FormatInt(decision.Usage.TotalTokens, 10))
	b.WriteString(" / ")
	b.WriteString(formatLifetimeLimit(decision.TokenLimit))
	if decision.ProjectHistory.CompletedIssues > 0 {
		b.WriteString("\n- project_session_context: ")
		b.WriteString(strconv.FormatInt(decision.Usage.Sessions, 10))
		b.WriteString(" sessions; project p95 is ")
		b.WriteString(strconv.FormatInt(decision.ProjectHistory.P95Sessions, 10))
		b.WriteString(" across ")
		b.WriteString(strconv.FormatInt(decision.ProjectHistory.CompletedIssues, 10))
		b.WriteString(" completed issues")
		b.WriteString("\n- project_token_context: ")
		b.WriteString(strconv.FormatInt(decision.Usage.TotalTokens, 10))
		b.WriteString(" tokens; project p95 is ")
		b.WriteString(strconv.FormatInt(decision.ProjectHistory.P95Tokens, 10))
		b.WriteString(" across ")
		b.WriteString(strconv.FormatInt(decision.ProjectHistory.CompletedIssues, 10))
		b.WriteString(" completed issues")
	}
	b.WriteString("\n\nThis park is held until the override label is applied or the configured limits no longer apply.")
	if label := strings.TrimSpace(o.cfg.LifetimeLimitOverrideLabel); label != "" {
		b.WriteString(" Apply the `")
		b.WriteString(label)
		b.WriteString("` label to deliberately bypass this lifetime limit for the issue.")
	}
	return b.String()
}

func formatLifetimeLimit(limit int64) string {
	if limit <= 0 {
		return "disabled"
	}
	return strconv.FormatInt(limit, 10)
}

func lifetimeLimitRecoveryRemedy(label string) string {
	remedy := "Raise the configured lifetime limits above current usage."
	if label = strings.TrimSpace(label); label != "" {
		remedy += " Apply the " + label + " label for an explicit bypass."
	}
	return remedy
}

func (o *Orchestrator) reconcileLifetimeLimitPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	if strings.TrimSpace(park.Cause) != lifetimeLimitReason || strings.TrimSpace(park.Predicate) != blockedRecoveryPredicateLifetimeLimit {
		return false, false
	}
	if o.lifetimeUsage == nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", lifetimeLimitEvidenceUnavailableReason, &park, "")
		return true, false
	}
	usage, err := o.issueLifetimeUsage(ctx, issue)
	if err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", lifetimeLimitEvidenceUnavailableReason, &park, "")
		if o.logger != nil {
			o.logger.Warn("lifetime issue recovery usage lookup failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return true, false
	}
	decision := evaluateLifetimeLimit(usage, o.cfg.LifetimeSessionLimit, o.cfg.LifetimeTokenLimit)
	override := o.lifetimeLimitOverride(issue)
	if !override && decision.reached() {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", lifetimeLimitReason, &park, lifetimeLimitUsageSignature(usage))
		return true, false
	}

	targetState := strings.TrimSpace(park.TargetState)
	if targetState == "" {
		targetState = "Todo"
	}
	signature := lifetimeLimitUsageSignature(usage)
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, targetState, now, "lifetime_limit_recovered", workflowLaneMetadata{}); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "transition_failed", &park, signature)
		return true, false
	}
	if o.connector != nil {
		reason := "the configured lifetime limit no longer applies"
		if override {
			reason = "the configured override label is present"
		}
		comment := fmt.Sprintf("Lifetime usage park cleared for %s because %s. Moved the issue to `%s`.", issueLabel(issue), reason, targetState)
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("lifetime issue recovery comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	o.recordBlockedRecoveryDecision(ctx, state, issue, "transition", "lifetime_limit_recovered", &park, signature)
	o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
	return true, true
}
