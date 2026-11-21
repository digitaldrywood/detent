package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const reworkBreakerStageUpdateSkew = time.Second

const (
	blockedRecoveryReasonMergeConflict        = "merge_conflict"
	blockedRecoveryReasonStaleBase            = "stale_base"
	blockedRecoveryReasonMissingCurrentHeadCI = "missing_current_head_ci"
)

type BlockedRecoveryConfig struct {
	Enabled      bool
	SourceStates []string
	TargetState  string
	ReasonCodes  []string
}

type BlockedRecoveryAction string

const (
	BlockedRecoveryActionNone   BlockedRecoveryAction = ""
	BlockedRecoveryActionRework BlockedRecoveryAction = "rework"
)

type BlockedRecoveryReason string

const (
	BlockedRecoveryReasonNotBlocked           BlockedRecoveryReason = "not_blocked"
	BlockedRecoveryReasonHumanBlocker         BlockedRecoveryReason = "human_blocker"
	BlockedRecoveryReasonDependencyBlocker    BlockedRecoveryReason = "dependency_blocker"
	BlockedRecoveryReasonMissingPullRequest   BlockedRecoveryReason = "missing_pull_request"
	BlockedRecoveryReasonPullRequestNotOpen   BlockedRecoveryReason = "pull_request_not_open"
	BlockedRecoveryReasonNoRecoverableSignal  BlockedRecoveryReason = "no_recoverable_signal"
	BlockedRecoveryReasonMergeConflicts       BlockedRecoveryReason = "merge_conflicts"
	BlockedRecoveryReasonStaleBase            BlockedRecoveryReason = "stale_base"
	BlockedRecoveryReasonMissingCurrentHeadCI BlockedRecoveryReason = "missing_current_head_ci"
)

type BlockedRecoveryDecision struct {
	Action      BlockedRecoveryAction
	Reason      BlockedRecoveryReason
	TargetState string
	Detail      string
}

func EvaluateBlockedRecovery(issue connector.Issue) BlockedRecoveryDecision {
	return evaluateBlockedRecovery(issue, normalizeBlockedRecoveryConfig(BlockedRecoveryConfig{
		Enabled:      true,
		SourceStates: []string{blockedStatusState},
		TargetState:  autoPromoteReworkState,
	}), []string{"Done", "Cancelled", "Canceled", "Closed"})
}

func EvaluateBlockedRecoveryWithConfig(issue connector.Issue, cfg BlockedRecoveryConfig) BlockedRecoveryDecision {
	decision, _, ok := evaluateStructuredBlockedRecovery(issue, cfg)
	if !ok {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNoRecoverableSignal, "")
	}
	return decision
}

func evaluateBlockedRecovery(issue connector.Issue, cfg BlockedRecoveryConfig, terminalStates []string) BlockedRecoveryDecision {
	issue = issueWithoutSelfDependencies(issue)
	cfg = normalizeBlockedRecoveryConfig(cfg)
	if !stateIn(issue.State, cfg.SourceStates) {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNotBlocked, "")
	}
	if blockedRecoveryHumanOnly(issue) {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonHumanBlocker, "blocked reason requires a human")
	}
	if blockedRefsUnresolved(issue.BlockedBy, terminalStates) {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonDependencyBlocker, "dependency blockers must clear first")
	}
	if issue.PullRequest == nil {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonMissingPullRequest, "")
	}
	pr := issue.PullRequest
	if normalizePullRequestState(pr.State) != "open" {
		return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonPullRequestNotOpen, "")
	}

	if connector.PullRequestConflicts(pr.MergeableState) {
		return blockedRecoveryDecisionWithTarget(BlockedRecoveryActionRework, BlockedRecoveryReasonMergeConflicts, cfg.TargetState, "linked PR has merge conflicts")
	}
	switch strings.ToLower(strings.TrimSpace(pr.MergeableState)) {
	case "behind":
		return blockedRecoveryDecisionWithTarget(BlockedRecoveryActionRework, BlockedRecoveryReasonStaleBase, cfg.TargetState, "linked PR branch is behind the base branch")
	}

	if blockedRecoveryNoCurrentHeadCI(pr) {
		return blockedRecoveryDecisionWithTarget(BlockedRecoveryActionRework, BlockedRecoveryReasonMissingCurrentHeadCI, cfg.TargetState, "latest PR head has no CI signal")
	}
	return blockedRecoveryDecision(BlockedRecoveryActionNone, BlockedRecoveryReasonNoRecoverableSignal, "")
}

func blockedRecoveryDecision(action BlockedRecoveryAction, reason BlockedRecoveryReason, detail string) BlockedRecoveryDecision {
	return blockedRecoveryDecisionWithTarget(action, reason, autoPromoteReworkState, detail)
}

func blockedRecoveryDecisionWithTarget(
	action BlockedRecoveryAction,
	reason BlockedRecoveryReason,
	targetState string,
	detail string,
) BlockedRecoveryDecision {
	decision := BlockedRecoveryDecision{
		Action: action,
		Reason: reason,
		Detail: strings.TrimSpace(detail),
	}
	if action == BlockedRecoveryActionRework {
		decision.TargetState = strings.TrimSpace(defaultString(targetState, autoPromoteReworkState))
	}
	return decision
}

func normalizeBlockedRecoveryConfig(cfg BlockedRecoveryConfig) BlockedRecoveryConfig {
	cfg.SourceStates = normalizedStates(defaultStringSlice(cfg.SourceStates, []string{blockedStatusState}))
	cfg.TargetState = strings.TrimSpace(defaultString(cfg.TargetState, autoPromoteReworkState))
	cfg.ReasonCodes = normalizeBlockedRecoveryReasonCodes(defaultStringSlice(cfg.ReasonCodes, []string{
		blockedRecoveryReasonMergeConflict,
		blockedRecoveryReasonStaleBase,
		blockedRecoveryReasonMissingCurrentHeadCI,
	}))
	return cfg
}

func normalizeBlockedRecoveryReasonCodes(reasonCodes []string) []string {
	normalized := make([]string, 0, len(reasonCodes))
	seen := map[string]struct{}{}
	for _, reasonCode := range reasonCodes {
		reasonCode = normalizeBlockedRecoveryReasonCode(reasonCode)
		if reasonCode == "" {
			continue
		}
		if _, ok := seen[reasonCode]; ok {
			continue
		}
		seen[reasonCode] = struct{}{}
		normalized = append(normalized, reasonCode)
	}
	return normalized
}

func normalizeBlockedRecoveryReasonCode(reasonCode string) string {
	reasonCode = strings.ToLower(strings.TrimSpace(reasonCode))
	reasonCode = strings.ReplaceAll(reasonCode, "-", "_")
	reasonCode = strings.ReplaceAll(reasonCode, " ", "_")
	if reasonCode == "merge_conflicts" {
		return blockedRecoveryReasonMergeConflict
	}
	return reasonCode
}

func (o *Orchestrator) recoverBlockedIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	transitioned := map[string]struct{}{}
	for _, issue := range issues {
		if issue.ID != "" && o.recoverCauseBlockedIssue(ctx, state, issue, now) {
			transitioned[issue.ID] = struct{}{}
		}
	}
	return transitioned
}

func blockedRecoveryReasonAllowed(cfg BlockedRecoveryConfig, reasonCode string) bool {
	reasonCode = normalizeBlockedRecoveryReasonCode(reasonCode)
	for _, allowed := range cfg.ReasonCodes {
		if normalizeBlockedRecoveryReasonCode(allowed) == reasonCode {
			return true
		}
	}
	return false
}

func blockedRecoveryConditionMatches(reasonCode string, reason BlockedRecoveryReason) bool {
	switch normalizeBlockedRecoveryReasonCode(reasonCode) {
	case blockedRecoveryReasonMergeConflict:
		return reason == BlockedRecoveryReasonMergeConflicts
	case blockedRecoveryReasonStaleBase:
		return reason == BlockedRecoveryReasonStaleBase
	case blockedRecoveryReasonMissingCurrentHeadCI:
		return reason == BlockedRecoveryReasonMissingCurrentHeadCI
	default:
		return false
	}
}

func evaluateStructuredBlockedRecovery(
	issue connector.Issue,
	cfg BlockedRecoveryConfig,
) (BlockedRecoveryDecision, string, bool) {
	cfg = normalizeBlockedRecoveryConfig(cfg)
	signal := issue.WorkpadSignal
	if signal == nil ||
		signal.Invalid != nil ||
		signal.Source != workpad.SourceStructured ||
		strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return BlockedRecoveryDecision{}, "", false
	}
	reasonCode := normalizeBlockedRecoveryReasonCode(signal.ReasonCode)
	if !blockedRecoveryReasonAllowed(cfg, reasonCode) {
		return BlockedRecoveryDecision{}, "", false
	}
	decision := evaluateBlockedRecovery(issue, cfg, []string{"Done", "Cancelled", "Canceled", "Closed"})
	if decision.Action != BlockedRecoveryActionRework ||
		!blockedRecoveryConditionMatches(reasonCode, decision.Reason) {
		return BlockedRecoveryDecision{}, "", false
	}
	return decision, reasonCode, true
}

func blockedRefsUnresolved(refs []connector.BlockedRef, terminalStates []string) bool {
	for _, ref := range refs {
		switch strings.ToLower(strings.TrimSpace(ref.TrackerState)) {
		case connector.BlockedRefTrackerStateClosed:
			continue
		case connector.BlockedRefTrackerStateOpen:
			return true
		}
		if !stateIn(ref.State, terminalStates) {
			return true
		}
	}
	return false
}

func blockedRecoveryHumanOnly(issue connector.Issue) bool {
	return BlockedRecoveryHumanHoldReason(issue, "") == "human_action" || issue.WorkpadSignal != nil && issue.WorkpadSignal.ReasonCode == "permission_wait"
}

func blockedRecoveryNoCurrentHeadCI(pr *connector.PullRequest) bool {
	if pr == nil || strings.TrimSpace(pr.HeadSHA) == "" || strings.TrimSpace(pr.CIStatus) != "" {
		return false
	}
	return pr.CheckRunCount == 0 && pr.StatusContextCount == 0
}
