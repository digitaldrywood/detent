package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	blockedRecoveryOwnerOrchestrator           = "orchestrator"
	blockedRecoveryOwnerHuman                  = "human"
	blockedRecoveryOwnerOperator               = "operator"
	blockedRecoveryPredicateFingerprintChange  = "fingerprint_changed"
	blockedRecoveryPredicateOncePerFingerprint = "once_per_fingerprint"
	blockedRecoveryPredicateManaged            = "managed"
	workflowActionCauseBlockedRecovery         = "cause_blocked_recovery"
	workflowActionBlockedReadyPRReconciliation = "blocked_ready_pr_reconciliation"
)

type blockedCauseSignals struct {
	ConfigFingerprint    string            `json:"config_fingerprint,omitempty"`
	ToolingFingerprint   string            `json:"tooling_fingerprint,omitempty"`
	BaseFingerprint      string            `json:"base_fingerprint,omitempty"`
	WorkspaceHeadSHA     string            `json:"workspace_head_sha,omitempty"`
	WorkspaceFingerprint string            `json:"workspace_fingerprint,omitempty"`
	WorkspaceStatus      string            `json:"workspace_status,omitempty"`
	WorkspacePresent     bool              `json:"workspace_present,omitempty"`
	WorkspaceFiles       int               `json:"workspace_files,omitempty"`
	UnpushedCommits      int               `json:"unpushed_commits,omitempty"`
	Health               string            `json:"health,omitempty"`
	Description          string            `json:"description,omitempty"`
	ModelOverride        string            `json:"model_override,omitempty"`
	Labels               []string          `json:"labels,omitempty"`
	Workpad              *workpad.Signal   `json:"workpad,omitempty"`
	Fields               map[string]string `json:"fields,omitempty"`
	PRNumber             int               `json:"pr_number,omitempty"`
	PRHeadSHA            string            `json:"pr_head_sha,omitempty"`
	PRBaseSHA            string            `json:"pr_base_sha,omitempty"`
	PRDiffFingerprint    string            `json:"pr_diff_fingerprint,omitempty"`
}

func (o *Orchestrator) newBlockedRecoveryMetadata(
	ctx context.Context,
	issue connector.Issue,
	runMode string,
	cause string,
	predicate string,
	targetState string,
	fallback DiffStats,
) workflowLaneMetadata {
	signals := o.blockedCauseSignals(ctx, issue, runMode, targetState, fallback)
	targetState = blockedCauseTargetState(issue, signals, targetState)
	return workflowLaneMetadata{
		BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
			Owner:            blockedRecoveryOwnerOrchestrator,
			Cause:            strings.TrimSpace(cause),
			Predicate:        strings.TrimSpace(predicate),
			CauseFingerprint: blockedCauseFingerprint(signals),
			TargetState:      targetState,
			RunMode:          strings.TrimSpace(runMode),
			IntentResumable:  blockedCauseResumable(issue, signals),
		},
	}
}

func (o *Orchestrator) blockedCauseSignals(
	ctx context.Context,
	issue connector.Issue,
	runMode string,
	targetState string,
	fallback DiffStats,
) blockedCauseSignals {
	inspectionIssue := cloneIssue(issue)
	if strings.TrimSpace(targetState) != "" {
		inspectionIssue.State = targetState
	}
	snapshot := runpkg.BlockedRecoverySnapshot{Health: "inspection_unavailable", WorkspaceStatus: "unavailable"}
	if o != nil && o.recoveryInspector != nil {
		snapshot = o.recoveryInspector.BlockedRecoverySnapshot(ctx, runpkg.RunRequest{
			Issue:           inspectionIssue,
			Mode:            runMode,
			SelectorContext: o.selectorContext(),
		})
	}
	if !snapshot.WorkspacePresent && diffStatsPresent(fallback) {
		snapshot.WorkspacePresent = true
		snapshot.WorkspaceFingerprint = strings.TrimSpace(fallback.Fingerprint)
		if snapshot.WorkspaceFingerprint == "" {
			snapshot.WorkspaceFingerprint = blockedCauseHash(fmt.Sprintf(
				"files=%d;added=%d;removed=%d;unpushed=%d;status=%s",
				fallback.FilesChanged,
				fallback.AddedLines,
				fallback.RemovedLines,
				fallback.UnpushedCommits,
				strings.TrimSpace(fallback.Status),
			))
		}
		snapshot.UnpushedCommits = fallback.UnpushedCommits
		snapshot.WorkspaceFiles = fallback.FilesChanged
		snapshot.WorkspaceStatus = "present"
	}
	signals := blockedCauseSignals{
		ConfigFingerprint:    strings.TrimSpace(snapshot.ConfigFingerprint),
		ToolingFingerprint:   strings.TrimSpace(snapshot.ToolingFingerprint),
		BaseFingerprint:      strings.TrimSpace(snapshot.BaseFingerprint),
		WorkspaceHeadSHA:     strings.TrimSpace(snapshot.HeadSHA),
		WorkspaceFingerprint: strings.TrimSpace(snapshot.WorkspaceFingerprint),
		WorkspaceStatus:      strings.TrimSpace(snapshot.WorkspaceStatus),
		WorkspacePresent:     snapshot.WorkspacePresent,
		WorkspaceFiles:       snapshot.WorkspaceFiles,
		UnpushedCommits:      snapshot.UnpushedCommits,
		Health:               strings.TrimSpace(snapshot.Health),
		Description:          strings.TrimSpace(issue.Description),
		ModelOverride:        strings.TrimSpace(issue.ModelOverride),
		Labels:               blockedCauseLabels(issue.Labels),
		Workpad:              blockedCauseWorkpadSignal(issue.WorkpadSignal),
		Fields:               blockedCauseFields(issue.Fields),
	}
	if issue.PRNumber != nil {
		signals.PRNumber = *issue.PRNumber
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			signals.PRNumber = issue.PullRequest.Number
		}
		signals.PRHeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
		signals.PRBaseSHA = strings.TrimSpace(issue.PullRequest.BaseSHA)
		signals.PRDiffFingerprint = strings.TrimSpace(issue.PullRequest.DiffFingerprint)
	}
	return signals
}

func blockedCauseWorkpadSignal(signal *workpad.Signal) *workpad.Signal {
	if signal == nil {
		return nil
	}
	cloned := *signal
	cloned.CommentURL = ""
	cloned.Blockers = append([]workpad.Blocker(nil), signal.Blockers...)
	cloned.Fields = blockedCauseFields(signal.Fields)
	if signal.Invalid != nil {
		invalid := *signal.Invalid
		cloned.Invalid = &invalid
	}
	return &cloned
}

func blockedCauseLabels(labels []string) []string {
	normalized := normalizeLabels(labels)
	slices.Sort(normalized)
	return normalized
}

func blockedCauseFields(fields map[string]string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(fields))
	for key, value := range fields {
		if normalizeState(key) == "status" {
			continue
		}
		cloned[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if len(cloned) == 0 {
		return nil
	}
	return cloned
}

func blockedCauseFingerprint(signals blockedCauseSignals) string {
	data, err := json.Marshal(signals)
	if err != nil {
		return blockedCauseHash(err.Error())
	}
	return blockedCauseHash(string(data))
}

func blockedCauseHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func blockedCauseResumable(issue connector.Issue, signals blockedCauseSignals) bool {
	return dependencyAutoUnblockStartedSignal(issue) ||
		(signals.WorkspacePresent && (signals.UnpushedCommits > 0 || signals.WorkspaceFiles > 0))
}

func blockedCauseTargetState(issue connector.Issue, signals blockedCauseSignals, configured string) string {
	if blockedCauseResumable(issue, signals) {
		return autoPromoteReworkState
	}
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured
	}
	return "Todo"
}

func (o *Orchestrator) recoverCauseBlockedIssue(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	now time.Time,
) bool {
	if normalizeState(issue.State) != normalizeState(blockedStatusState) {
		return false
	}
	dependencyCfg := normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock)
	if reason := o.blockedCauseHoldReason(issue, state, nil, dependencyCfg); reason != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", reason, nil, "")
		return false
	}
	if o.currentBlockedOperatorStop(ctx, state, issue) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "operator_stop", nil, "")
		return false
	}
	withDependencies := o.issueWithDependencyRefs(issue)
	withDependencies, workpadRefs := o.issueWithCurrentWorkpadDependencyRefs(ctx, withDependencies)
	blockers := o.resolveDependencyBlockers(ctx, withDependencies)
	workpadBlockers := dependencyBlockersMatchingRefs(blockers, workpadRefs)
	if reason := o.blockedCauseHoldReason(issue, state, workpadBlockers, dependencyCfg); reason != "" {
		o.recordBlockedRecoveryDecision(
			ctx,
			state,
			withDependencies,
			"hold",
			reason,
			nil,
			"",
			dependencyBlockersNotReady(workpadBlockers, dependencyCfg, o.cfg.TerminalStates)...,
		)
		return false
	}
	if len(withDependencies.BlockedBy) > 0 {
		o.recordBlockedRecoveryDecision(ctx, state, withDependencies, "defer", "dependency_recovery", nil, "")
		return false
	}
	if park, ok := o.latestReworkBreakerPark(ctx, issue); ok {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "rework_breaker_recovery", nil, park.Signature)
		return false
	}
	park, ok := o.currentBlockedRecoveryPark(ctx, state, issue)
	if !ok {
		recoveryCfg := normalizeBlockedRecoveryConfig(o.cfg.BlockedRecovery)
		reasonCode, reasonFound := o.latestWorkflowLaneReason(ctx, issue, issue.State)
		if recoveryCfg.Enabled && reasonFound && blockedRecoveryReasonAllowed(recoveryCfg, reasonCode) {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "pr_maintenance_recovery", nil, "")
		} else {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "no_recovery_predicate", nil, "")
		}
		return false
	}
	if handled, transitioned := o.reconcileBlockedReadyPullRequest(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if park.Owner == blockedRecoveryOwnerHuman || park.Owner == blockedRecoveryOwnerOperator {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "human_recovery", &park, park.CauseFingerprint)
		return false
	}
	if park.Predicate == blockedRecoveryPredicateManaged {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "managed_recovery", &park, park.CauseFingerprint)
		return false
	}
	signals := o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
	currentFingerprint := blockedCauseFingerprint(signals)
	if park.Predicate == blockedRecoveryPredicateFingerprintChange && currentFingerprint == park.CauseFingerprint {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "cause_unchanged", &park, currentFingerprint)
		return false
	}
	if park.Predicate != blockedRecoveryPredicateFingerprintChange &&
		park.Predicate != blockedRecoveryPredicateOncePerFingerprint {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "no_recovery_predicate", &park, currentFingerprint)
		return false
	}
	signature := blockedCauseRecoverySignature(park.Cause, currentFingerprint)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionCauseBlockedRecovery, signature); consumed {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "fingerprint_already_consumed", &park, currentFingerprint)
		return false
	}
	targetState := blockedCauseTargetState(issue, signals, park.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, targetState, now, "cause_blocked_recovery", metadata); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "transition_failed", &park, currentFingerprint)
		return false
	}
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, blockedCauseRecoveryComment(issue, park, targetState, currentFingerprint)); err != nil && o.logger != nil {
			o.logger.Warn("blocked recovery comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	o.logBlockedRecoveryDecision(issue, "transition", "recovery_predicate_satisfied", &park, currentFingerprint)
	return true
}

func (o *Orchestrator) reconcileBlockedReadyPullRequest(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	if !blockedReadyPullRequestRecoverableCause(park) || !implementProgressLinkedPullRequest(issue) {
		return false, false
	}
	signals := o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
	if reason := o.blockedReadyPullRequestDeferredReason(ctx, state, issue, signals, now); reason != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", reason, &park, blockedCauseFingerprint(signals))
		return true, false
	}
	signature := blockedReadyPullRequestSignature(issue, park)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionBlockedReadyPRReconciliation, signature); consumed {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "ready_pr_reconciliation_already_consumed", &park, signature)
		return true, false
	}
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionBlockedReadyPRReconciliation, signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(
		ctx,
		state,
		issue.ID,
		issue,
		autoPromoteMergingState,
		now,
		workflowActionBlockedReadyPRReconciliation,
		metadata,
	); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "ready_pr_reconciliation_transition_failed", &park, signature)
		if o.logger != nil {
			o.logger.Warn("blocked ready pull request reconciliation failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return true, false
	}
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issue.ID, blockedReadyPullRequestComment(issue, park)); err != nil && o.logger != nil {
			o.logger.Warn("blocked ready pull request reconciliation comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
	promoted := promotedIssue(issue, autoPromoteMergingState, now)
	o.recordMergeQueueEntered(state, promoted, now, workflowActionBlockedReadyPRReconciliation)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   workflowActionBlockedReadyPRReconciliation,
		Message: "reconciled " + issueLabel(issue) + " from Blocked to Merging with its ready pull request",
	})
	return true, true
}

func blockedReadyPullRequestRecoverableCause(park workflowLaneBlockedRecoveryMetadata) bool {
	cause := strings.TrimSpace(park.Cause)
	owner := strings.TrimSpace(park.Owner)
	if cause == deliverableRecoveryNeedsHumanReason || strings.HasPrefix(cause, deliverableRecoveryNeedsHumanReason+":") {
		return owner != blockedRecoveryOwnerOperator
	}
	if owner != blockedRecoveryOwnerOrchestrator || strings.TrimSpace(park.RunMode) != RunModeImplement {
		return false
	}
	return cause == strandedUnpushedWorkReason || cause == noProgressLimitReason
}

func (o *Orchestrator) blockedReadyPullRequestDeferredReason(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	signals blockedCauseSignals,
	now time.Time,
) string {
	if !o.cfg.MergeFastPathEnabled {
		return "merge_fast_path_disabled"
	}
	if !stateIn(autoPromoteMergingState, o.cfg.ActiveStates) {
		return "merging_lane_inactive"
	}
	pullRequest := issue.PullRequest
	if pullRequest == nil || pullRequestHydrationBlocksProgress(pullRequest) ||
		strings.TrimSpace(pullRequest.HeadSHA) == "" || strings.TrimSpace(pullRequest.BaseSHA) == "" {
		return "pull_request_hydration_unavailable"
	}
	if !signals.WorkspacePresent || strings.TrimSpace(signals.WorkspaceStatus) != "present" || strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
		return "workspace_head_unavailable"
	}
	if signals.WorkspaceFiles > 0 {
		return "workspace_diff_present"
	}
	if strings.TrimSpace(signals.WorkspaceHeadSHA) != strings.TrimSpace(pullRequest.HeadSHA) {
		return "workspace_pull_request_head_mismatch"
	}
	if !mergeWorkerProgrammaticMergeReady(issue) || !reworkBreakerCIGreen(pullRequest) || len(pullRequest.StaleSuccessfulChecks) > 0 {
		return "pull_request_not_merge_ready"
	}
	if !staleMergingIssueReadyForDispatch(issue, o.cfg) {
		return "merge_dispatch_revoked"
	}
	autoPromoteCfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	autoPromoteCfg.Enabled = true
	if !o.reworkBreakerAutoPromoteGateReady(ctx, state, issue, autoPromoteCfg, now) {
		return "pull_request_gate_not_ready"
	}
	return ""
}

func blockedReadyPullRequestSignature(issue connector.Issue, park workflowLaneBlockedRecoveryMetadata) string {
	return fmt.Sprintf(
		"cause=%s;pr=%d;head=%s",
		blockedCauseHash(strings.TrimSpace(park.Cause)),
		pullRequestNumber(issue),
		strings.TrimSpace(issue.PullRequest.HeadSHA),
	)
}

func blockedReadyPullRequestComment(issue connector.Issue, park workflowLaneBlockedRecoveryMetadata) string {
	return fmt.Sprintf(
		"Reconciled this issue from Blocked to Merging after its Detent-owned recovery cause cleared and the linked pull request met the current merge gate.\n\n- cause: %s\n- pull request: %s\n- head_sha: %s",
		strings.TrimSpace(park.Cause),
		strings.TrimSpace(issue.PullRequest.URL),
		strings.TrimSpace(issue.PullRequest.HeadSHA),
	)
}

func (o *Orchestrator) blockedCauseHoldReason(
	issue connector.Issue,
	state *State,
	workpadBlockers []dependencyBlocker,
	dependencyCfg DependencyAutoUnblockConfig,
) string {
	if reason := BlockedRecoveryHumanHoldReason(issue, o.cfg.AutoPromote.OptoutLabel); reason != "" {
		return reason
	}
	if issue.WorkpadSignal != nil {
		if len(workpadBlockers) > 0 && !dependencyBlockersReady(workpadBlockers, dependencyCfg, o.cfg.TerminalStates) {
			return "workpad_blocker"
		}
	}
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blocked.Source == BlockedSourceOperatorStop {
			return "operator_stop"
		}
	}
	return ""
}

func BlockedRecoveryHumanHoldReason(issue connector.Issue, optoutLabel string) string {
	if issue.WorkpadSignal != nil {
		if issue.WorkpadSignal.Invalid != nil {
			return "invalid_workpad_signal"
		}
		if strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" {
			return "human_action"
		}
	}
	configuredOptout := normalizeLabel(optoutLabel)
	for _, label := range issue.Labels {
		normalized := normalizeLabel(label)
		if normalized == "requires-human-review" || configuredOptout != "" && normalized == configuredOptout {
			return "human_action"
		}
	}
	return ""
}

func (o *Orchestrator) currentBlockedOperatorStop(ctx context.Context, state *State, issue connector.Issue) bool {
	issueID := strings.TrimSpace(issue.ID)
	if state != nil && issueID != "" {
		if blocked, ok := state.Blocked[issueID]; ok && blocked.Source == BlockedSourceOperatorStop {
			return true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	return ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) &&
		blockedEntryMatchesCurrent(issue, entry.Event.StartedAt) &&
		strings.EqualFold(strings.TrimSpace(entry.Event.Reason), string(store.WorkAttemptTerminalOperatorStopped))
}

func (o *Orchestrator) currentBlockedRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (workflowLaneBlockedRecoveryMetadata, bool) {
	issueID := strings.TrimSpace(issue.ID)
	if state != nil && issueID != "" {
		if blocked, ok := state.Blocked[issueID]; ok &&
			blocked.Recovery != nil &&
			blockedEntryMatchesCurrent(issue, blocked.BlockedAt) {
			return *blocked.Recovery, true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok ||
		normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) ||
		!blockedEntryMatchesCurrent(issue, entry.Event.StartedAt) ||
		entry.Metadata.BlockedRecovery == nil {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	return *entry.Metadata.BlockedRecovery, true
}

func blockedCauseRecoverySignature(cause string, fingerprint string) string {
	return strings.TrimSpace(cause) + ":" + strings.TrimSpace(fingerprint)
}

func blockedCauseRecoveryComment(
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	targetState string,
	fingerprint string,
) string {
	return fmt.Sprintf(
		"Blocked recovery predicate satisfied. Moved %s from %s to %s.\n\n- cause: %s\n- predicate: %s\n- cause_fingerprint: %s",
		issueLabel(issue),
		strings.TrimSpace(issue.State),
		strings.TrimSpace(targetState),
		strings.TrimSpace(park.Cause),
		strings.TrimSpace(park.Predicate),
		strings.TrimSpace(fingerprint),
	)
}

func (o *Orchestrator) recordBlockedRecoveryDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	action string,
	reason string,
	park *workflowLaneBlockedRecoveryMetadata,
	fingerprint string,
	unresolvedWorkpadBlockers ...dependencyBlocker,
) {
	o.logBlockedRecoveryDecision(issue, action, reason, park, fingerprint, unresolvedWorkpadBlockers...)
	if state == nil || strings.TrimSpace(issue.ID) == "" {
		return
	}
	entry, ok := state.Blocked[issue.ID]
	if !ok {
		return
	}
	entry.Issue = cloneIssue(issue)
	if park == nil {
		if current, found := o.currentBlockedRecoveryPark(ctx, state, issue); found {
			park = &current
		}
	}
	entry.RecoveryAction = strings.TrimSpace(action)
	entry.RecoveryReason = strings.TrimSpace(reason)
	entry.RecoveryRemedy = BlockedRecoveryOperatorRemedy(issue, reason)
	entry.RecoveryReachability = blockedRecoveryReachability(action)
	entry.NeedsHumanAttention = strings.EqualFold(strings.TrimSpace(action), "hold")
	entry.RecoveryRoot = nil
	if park != nil {
		current := *park
		current.IntentResumable = current.intentResumable()
		current.Resumable = false
		current.Reachability = entry.RecoveryReachability
		current.HoldReason = entry.RecoveryReason
		current.OperatorRemedy = entry.RecoveryRemedy
		entry.Recovery = &current
		entry.RecoveryTarget = current.TargetState
		entry.RecoveryIntentResumable = current.IntentResumable
	} else {
		entry.Recovery = nil
		entry.RecoveryIntentResumable = strings.EqualFold(strings.TrimSpace(action), "defer")
	}
	state.Blocked[issue.ID] = entry
}

func blockedRecoveryReachability(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "hold":
		return "held"
	case "defer":
		return "deferred"
	default:
		return ""
	}
}

func BlockedRecoveryOperatorRemedy(issue connector.Issue, reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "invalid_workpad_signal":
		if issue.PullRequest == nil && issue.PRNumber == nil {
			return "Move the issue to Todo or another fresh-work lane; no pull request exists to resume."
		}
		return "Correct the latest Codex Workpad detent-status block, or move the issue to a fresh-work lane."
	case "human_action":
		if issue.WorkpadSignal != nil && strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" {
			return strings.TrimSpace(issue.WorkpadSignal.HumanAction)
		}
		return "Complete the requested human action, then update the Workpad."
	case "workpad_blocker":
		return "Resolve or remove the current Workpad blocker references."
	case "operator_stop":
		return "Review the operator stop and route the issue to its intended lane."
	case "cause_unchanged":
		return "Change the blocking cause, or move the issue to a lane that starts fresh work."
	case "fingerprint_already_consumed":
		return "Change the blocking cause, or move the issue to a lane that starts fresh work."
	case "transition_failed":
		return "Retry the lane transition after restoring tracker write access."
	case "managed_recovery":
		return "Review the configured recovery owner and move the issue manually if that owner cannot recover it."
	case "no_recovery_predicate":
		return "Add a durable recovery predicate or move the issue to a lane that starts fresh work."
	default:
		return "Review the blocked recovery reason and move the issue to an appropriate lane."
	}
}

func (o *Orchestrator) logBlockedRecoveryDecision(
	issue connector.Issue,
	action string,
	reason string,
	park *workflowLaneBlockedRecoveryMetadata,
	fingerprint string,
	unresolvedWorkpadBlockers ...dependencyBlocker,
) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", strings.TrimSpace(issue.Identifier),
		"action", strings.TrimSpace(action),
		"reason", strings.TrimSpace(reason),
	}
	if park != nil {
		attrs = append(attrs,
			"owner", strings.TrimSpace(park.Owner),
			"cause", strings.TrimSpace(park.Cause),
			"predicate", strings.TrimSpace(park.Predicate),
			"parked_fingerprint", strings.TrimSpace(park.CauseFingerprint),
		)
	}
	if strings.TrimSpace(fingerprint) != "" {
		attrs = append(attrs, "cause_fingerprint", strings.TrimSpace(fingerprint))
	}
	if len(unresolvedWorkpadBlockers) > 0 {
		attrs = append(attrs, "unresolved_workpad_blockers", dependencyAutoUnblockBlockerLabels(unresolvedWorkpadBlockers))
	}
	o.logger.Info("blocked recovery decision", attrs...)
}
