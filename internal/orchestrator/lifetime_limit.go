package orchestrator

import (
	"context"
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
