package orchestrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
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
		if _, queued := state.Retry[issue.ID]; queued {
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
	if err := o.retryMachineFailure(ctx, state, issue, nextAttempt(state.Retry[issue.ID].Attempt), now, lifetimeLimitReason, ""); err != nil {
		if o.logger != nil {
			o.logger.Warn("machine retry routing failed", "issue_id", issue.ID, "error", err)
		}
		return
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
