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
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	blockedRecoveryOwnerOrchestrator               = "orchestrator"
	blockedRecoveryOwnerHuman                      = "human"
	blockedRecoveryOwnerOperator                   = "operator"
	blockedRecoveryPredicateFingerprintChange      = "fingerprint_changed"
	blockedRecoveryPredicateOncePerFingerprint     = "once_per_fingerprint"
	blockedRecoveryPredicateManaged                = "managed"
	blockedRecoveryPredicateBreakerCooldown        = "breaker_cooldown_elapsed"
	blockedRecoveryReasonBreakerCooldownActive     = "breaker_cooldown_active"
	blockedCauseFingerprintVersion                 = 3
	workflowActionCauseBlockedRecovery             = "cause_blocked_recovery"
	blockedReadyPullRequestLookupNoneReason        = "ready_pr_lookup_none"
	blockedReadyPullRequestLookupUnavailableReason = "ready_pr_lookup_unavailable"
	instantFailureCircuitBreakerCause              = "instant_failure_circuit_breaker"
	instantFailureCircuitBreakerLaneReason         = "instant_fail_circuit_breaker"
	schedulerParkRecoveryUnavailableReason         = "scheduler_park_recovery_unavailable"
)

type blockedCauseSignals struct {
	ConfigFingerprint              string            `json:"config_fingerprint,omitempty"`
	ToolingFingerprint             string            `json:"tooling_fingerprint,omitempty"`
	BaseFingerprint                string            `json:"base_fingerprint,omitempty"`
	WorkspaceHeadSHA               string            `json:"workspace_head_sha,omitempty"`
	WorkspaceFingerprint           string            `json:"workspace_fingerprint,omitempty"`
	WorkspaceStatus                string            `json:"workspace_status,omitempty"`
	WorkspacePresent               bool              `json:"workspace_present,omitempty"`
	WorkspaceFiles                 int               `json:"workspace_files,omitempty"`
	UnpushedCommits                int               `json:"unpushed_commits,omitempty"`
	UnpushedCommitRefs             []string          `json:"unpushed_commit_refs,omitempty"`
	TrackedPaths                   []string          `json:"tracked_paths,omitempty"`
	UntrackedPaths                 []string          `json:"untracked_paths,omitempty"`
	CommitsNotInPullRequest        []string          `json:"commits_not_in_pull_request,omitempty"`
	PullRequestComparisonAvailable bool              `json:"pull_request_comparison_available,omitempty"`
	Health                         string            `json:"health,omitempty"`
	Description                    string            `json:"description,omitempty"`
	ModelOverride                  string            `json:"model_override,omitempty"`
	Labels                         []string          `json:"labels,omitempty"`
	Workpad                        *workpad.Signal   `json:"workpad,omitempty"`
	Fields                         map[string]string `json:"fields,omitempty"`
	PRNumber                       int               `json:"pr_number,omitempty"`
	PRHeadSHA                      string            `json:"pr_head_sha,omitempty"`
	PRBaseSHA                      string            `json:"pr_base_sha,omitempty"`
	PRDiffFingerprint              string            `json:"pr_diff_fingerprint,omitempty"`
}

type blockedCauseIdentity struct {
	Cause              string            `json:"cause,omitempty"`
	ConfigFingerprint  string            `json:"config_fingerprint,omitempty"`
	ToolingFingerprint string            `json:"tooling_fingerprint,omitempty"`
	BaseFingerprint    string            `json:"base_fingerprint,omitempty"`
	Health             string            `json:"health,omitempty"`
	Description        string            `json:"description,omitempty"`
	ModelOverride      string            `json:"model_override,omitempty"`
	Labels             []string          `json:"labels,omitempty"`
	Fields             map[string]string `json:"fields,omitempty"`
	PRNumber           int               `json:"pr_number,omitempty"`
	PRHeadSHA          string            `json:"pr_head_sha,omitempty"`
	PRBaseSHA          string            `json:"pr_base_sha,omitempty"`
	PRDiffFingerprint  string            `json:"pr_diff_fingerprint,omitempty"`
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
	cause = strings.TrimSpace(cause)
	predicate = strings.TrimSpace(predicate)
	if breakerParkCause(cause) && predicate == blockedRecoveryPredicateFingerprintChange {
		predicate = blockedRecoveryPredicateBreakerCooldown
		if previousState := strings.TrimSpace(issue.State); previousState != "" && normalizeState(previousState) != normalizeState(blockedStatusState) {
			targetState = previousState
		}
	}
	signals := o.blockedCauseSignals(ctx, issue, runMode, targetState, fallback)
	if predicate != blockedRecoveryPredicateBreakerCooldown || strings.TrimSpace(targetState) == "" {
		targetState = blockedCauseTargetState(issue, signals, targetState)
	}
	return workflowLaneMetadata{
		BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
			Owner:                   blockedRecoveryOwnerOrchestrator,
			SourceRevision:          holdSourceRevision(issue),
			ExpiresAt:               time.Now().UTC().Add(o.cfg.MaxRetryBackoff),
			Cause:                   cause,
			Predicate:               predicate,
			CauseFingerprint:        blockedCauseFingerprint(cause, signals),
			CauseFingerprintVersion: blockedCauseFingerprintVersion,
			TargetState:             targetState,
			RunMode:                 strings.TrimSpace(runMode),
			IntentResumable:         predicate == blockedRecoveryPredicateBreakerCooldown || blockedCauseResumable(issue, signals),
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
		snapshot.UnpushedCommitRefs = append([]string(nil), fallback.UnpushedCommitRefs...)
		snapshot.TrackedPaths = append([]string(nil), fallback.TrackedPaths...)
		snapshot.UntrackedPaths = append([]string(nil), fallback.UntrackedPaths...)
		snapshot.CommitsNotInPullRequest = append([]string(nil), fallback.CommitsNotInPullRequest...)
		snapshot.PullRequestComparisonAvailable = fallback.PullRequestComparisonAvailable
		snapshot.WorkspaceStatus = "present"
	}
	signals := blockedCauseSignals{
		ConfigFingerprint:              strings.TrimSpace(snapshot.ConfigFingerprint),
		ToolingFingerprint:             strings.TrimSpace(snapshot.ToolingFingerprint),
		BaseFingerprint:                strings.TrimSpace(snapshot.BaseFingerprint),
		WorkspaceHeadSHA:               strings.TrimSpace(snapshot.HeadSHA),
		WorkspaceFingerprint:           strings.TrimSpace(snapshot.WorkspaceFingerprint),
		WorkspaceStatus:                strings.TrimSpace(snapshot.WorkspaceStatus),
		WorkspacePresent:               snapshot.WorkspacePresent,
		WorkspaceFiles:                 snapshot.WorkspaceFiles,
		UnpushedCommits:                snapshot.UnpushedCommits,
		UnpushedCommitRefs:             append([]string(nil), snapshot.UnpushedCommitRefs...),
		TrackedPaths:                   append([]string(nil), snapshot.TrackedPaths...),
		UntrackedPaths:                 append([]string(nil), snapshot.UntrackedPaths...),
		CommitsNotInPullRequest:        append([]string(nil), snapshot.CommitsNotInPullRequest...),
		PullRequestComparisonAvailable: snapshot.PullRequestComparisonAvailable,
		Health:                         strings.TrimSpace(snapshot.Health),
		Description:                    strings.TrimSpace(issue.Description),
		ModelOverride:                  strings.TrimSpace(issue.ModelOverride),
		Labels:                         blockedCauseLabels(issue.Labels, o.cfg.StatusLabelPrefix),
		Workpad:                        blockedCauseWorkpadSignal(issue.WorkpadSignal),
		Fields:                         blockedCauseFields(issue.Fields),
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
	cloned := workpad.CloneSignal(signal)
	if cloned == nil {
		return nil
	}
	cloned.CommentURL = ""
	cloned.Fields = blockedCauseFields(cloned.Fields)
	return cloned
}

func blockedCauseLabels(labels []string, statusLabelPrefix string) []string {
	normalized := normalizeLabels(labels)
	statusLabelPrefix = strings.ToLower(strings.TrimSpace(statusLabelPrefix))
	if statusLabelPrefix != "" {
		normalized = slices.DeleteFunc(normalized, func(label string) bool {
			return strings.HasPrefix(label, statusLabelPrefix)
		})
	}
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

func blockedCauseFingerprint(cause string, signals blockedCauseSignals) string {
	identity := blockedCauseIdentity{
		Cause:              strings.TrimSpace(cause),
		ConfigFingerprint:  signals.ConfigFingerprint,
		ToolingFingerprint: signals.ToolingFingerprint,
		Health:             signals.Health,
		Description:        signals.Description,
		ModelOverride:      signals.ModelOverride,
		Labels:             signals.Labels,
		Fields:             signals.Fields,
		PRNumber:           signals.PRNumber,
		PRHeadSHA:          signals.PRHeadSHA,
		PRBaseSHA:          signals.PRBaseSHA,
		PRDiffFingerprint:  signals.PRDiffFingerprint,
	}
	if !breakerParkCause(cause) {
		identity.BaseFingerprint = signals.BaseFingerprint
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return blockedCauseHash(err.Error())
	}
	return blockedCauseHash(string(data))
}

func breakerParkCause(cause string) bool {
	cause = strings.ToLower(strings.TrimSpace(cause))
	if strings.HasPrefix(cause, tokenCeilingBlockedReasonPrefix) ||
		strings.HasPrefix(cause, repeatedFailureBlockedReasonPrefix) {
		return true
	}
	switch cause {
	case dispatchLoopDetectedReason,
		noProgressLimitReason,
		spendProgressReason,
		instantFailureCircuitBreakerCause,
		repeatedFailureCircuitBreakerCause,
		"token_ceiling_circuit_breaker",
		terminalAttemptRetryLimitCause,
		workspacePreparationRetryLimitCause:
		return true
	default:
		return false
	}
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
	cohort ...*blockedRecoveryDependencyEvidence,
) bool {
	if normalizeState(issue.State) != normalizeState(blockedStatusState) && (issue.IssueContract == nil || issue.IssueContract.ReturnState == "" || normalizeState(issue.State) != normalizeState(o.cfg.StopRunTargetState)) {
		return false
	}
	issue = issue.WithNativeWorkpadAuthority()
	if o.currentBlockedOperatorStop(ctx, state, issue) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "operator_stop", nil, "")
		return false
	}
	if explicitHumanAction(issue) != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "human_action", nil, "")
		return false
	}
	if o.currentBlockedHumanIntent(ctx, state, issue) {
		parkedAt, found := o.currentBlockedRecoveryParkedAt(ctx, state, issue)
		if !found {
			return false
		}
		evidence := clearedHumanActionEvidence(issue, parkedAt, now)
		if evidence == nil {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "human_action", nil, "")
			return false
		}
		return o.applyRecordedBlockerRecovery(ctx, state, issue, nil, []telemetry.BlockerEvidence{*evidence}, now)
	}
	signal, _ := rawIssueWorkpadSignal(issue)
	if signal != nil && signal.Invalid == nil {
		dependencies := workpad.CloneSignal(signal)
		dependencies.Blockers = nil
		for _, blocker := range signal.Blockers {
			if blocker.Owner == workpad.BlockerOwnerOrchestrator && blocker.Predicate != nil && blocker.Predicate.Type == workpad.PredicateIssueState {
				dependencies.Blockers = append(dependencies.Blockers, blocker)
			}
		}
		recorded := o.evaluateRecordedBlockerSignal(ctx, state, issue, dependencies, nil, now)
		if recorded.Found {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", string(BlockedRecoveryReasonDependencyBlocker), nil, "")
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			if recorded.Holds {
				return false
			}
			if !recorded.Unverifiable {
				withDependencies, _, _ := o.issueWithCurrentWorkpadDependencyRefs(ctx, o.issueWithDependencyRefs(issue))
				blockers := o.resolveDependencyBlockers(ctx, withDependencies)
				return o.applyRecordedBlockerRecovery(ctx, state, withDependencies, blockers, recorded.Evidence, now)
			}
			if o.logger != nil {
				o.logger.Info("retry unverifiable dependency evaluation", "issue_id", issue.ID, "blocker_evidence", recorded.Evidence)
			}
		}
	}
	if err := o.retryMachineFailure(ctx, state, issue, nextAttempt(state.Retry[issue.ID].Attempt), now, issue.BlockerReason, ""); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery retry failed", "issue_id", issue.ID, "error", err)
		}
		return false
	}
	return true
}

func (o *Orchestrator) blockedCauseHoldReason(
	issue connector.Issue,
	state *State,
	workpadBlockers []dependencyBlocker,
	dependencyCfg DependencyAutoUnblockConfig,
	workpadCurrent bool,
) string {
	if reason := BlockedRecoveryHumanHoldReason(issue, o.cfg.AutoPromote.OptoutLabel); reason != "" {
		return reason
	}
	if workpadCurrent && workpadHasRecordedNonDependencyBlocker(issue.WorkpadSignal) {
		return "recorded_blocker"
	}
	if len(workpadBlockers) > 0 && !dependencyBlockersReady(workpadBlockers, dependencyCfg, o.cfg.TerminalStates) {
		return "workpad_blocker"
	}
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blocked.Source == BlockedSourceOperatorStop {
			return "operator_stop"
		}
	}
	return ""
}

func workpadHasRecordedNonDependencyBlocker(signal *workpad.Signal) bool {
	if signal == nil || signal.Invalid != nil || strings.TrimSpace(signal.Status) != workpad.StatusBlocked {
		return false
	}
	for _, blocker := range signal.Blockers {
		if blocker.Unverifiable || blocker.Predicate != nil && blocker.Predicate.Type != workpad.PredicateIssueState {
			return true
		}
	}
	return false
}

func BlockedRecoveryHumanHoldReason(issue connector.Issue, optoutLabel string) string {
	if issue.WorkpadSignal != nil {
		if strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" || issue.WorkpadSignal.ReasonCode == "permission_wait" {
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
	if issue.WorkpadSignal != nil && issue.WorkpadSignal.Invalid != nil {
		return "invalid_workpad_signal"
	}
	return ""
}

func (o *Orchestrator) currentBlockedOperatorStop(ctx context.Context, state *State, issue connector.Issue) bool {
	if state != nil {
		if intent, ok := state.Blocked[issue.ID]; ok && intent.Source == BlockedSourceOperatorStop {
			return true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	return ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) &&
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
		(strings.EqualFold(strings.TrimSpace(entry.Event.Reason), string(store.WorkAttemptTerminalOperatorStopped)) || strings.TrimSpace(entry.Event.Reason) == "operator_move" && entry.Metadata.Provenance.Basis == provenance.BasisAuthenticatedHuman)
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
			blockedEntryMatchesCurrent(issue, blocked.BlockedAt) && blocked.Recovery.validate(issue, time.Now()) == nil {
			return *blocked.Recovery, true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok ||
		normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) ||
		!workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	if entry.Metadata.BlockedRecovery != nil && entry.Metadata.BlockedRecovery.validate(issue, time.Now()) == nil {
		return *entry.Metadata.BlockedRecovery, true
	}
	return workflowLaneBlockedRecoveryMetadata{}, false
}

func (o *Orchestrator) currentBlockedHumanIntent(ctx context.Context, state *State, issue connector.Issue) bool {
	if explicitHumanAction(issue) != "" {
		return true
	}
	if state != nil {
		if blocked, ok := state.Blocked[issue.ID]; ok && blockedEntryMatchesCurrent(issue, blocked.BlockedAt) && explicitHumanAction(blocked.Issue) != "" {
			return true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok || normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) || !workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return false
	}
	if strings.TrimSpace(entry.Metadata.HumanAction) != "" {
		return true
	}
	signal, parsed := workpad.SignalFromComment(entry.Metadata.ReasonDetail, "", "")
	if parsed && signal != nil && signal.Invalid == nil && strings.TrimSpace(signal.HumanAction) != "" {
		return true
	}
	return false
}

func (o *Orchestrator) currentBlockedRecoveryParkedAt(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (time.Time, bool) {
	issueID := strings.TrimSpace(issue.ID)
	if state != nil && issueID != "" {
		if blocked, ok := state.Blocked[issueID]; ok &&
			blocked.Recovery != nil &&
			blockedEntryMatchesCurrent(issue, blocked.BlockedAt) &&
			!blocked.BlockedAt.IsZero() {
			return blocked.BlockedAt.UTC(), true
		}
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	parkedAt := workflowLaneTransitionAt(entry.Event)
	target := blockedStatusState
	if issue.IssueContract != nil && issue.IssueContract.ReturnState != "" {
		target = firstNonBlank(o.cfg.StopRunTargetState, blockedStatusState)
	}
	if ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(target) &&
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
		!parkedAt.IsZero() {
		return parkedAt, true
	}
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return issue.StageUpdatedAt.UTC(), true
	}
	return time.Time{}, false
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
	providedPark := park != nil
	if park == nil {
		if current, found := o.currentBlockedRecoveryPark(ctx, state, issue); found {
			park = &current
		}
	}
	entry, ok := state.Blocked[issue.ID]
	if !ok {
		if !blockedRecoveryDecisionShouldMaterialize(reason) {
			return
		}
		blockedAt := time.Time{}
		if issue.StageUpdatedAt != nil {
			blockedAt = issue.StageUpdatedAt.UTC()
		}
		o.setBlockedStatusIssue(ctx, state, issue, blockedAt)
		entry, ok = state.Blocked[issue.ID]
		if !ok {
			return
		}
	}
	previousRecoveryReason := strings.TrimSpace(entry.RecoveryReason)
	entry.Issue = cloneIssue(issue)
	if trackerReason := strings.TrimSpace(issue.BlockerReason); trackerReason != "" {
		entry.Reason = trackerReason
	} else if strings.EqualFold(previousRecoveryReason, string(BlockedRecoveryReasonDependencyBlocker)) ||
		strings.EqualFold(previousRecoveryReason, "dependency_recovery") ||
		strings.EqualFold(strings.TrimSpace(entry.Reason), "blocked by project status") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry.Reason)), "waiting on ") {
		entry.Reason = ""
	}
	entry.RecoveryAction = strings.TrimSpace(action)
	entry.RecoveryReason = strings.TrimSpace(reason)
	entry.RecoveryRemedy = BlockedRecoveryOperatorRemedy(issue, reason)
	entry.RecoveryReachability = blockedRecoveryReachability(action)
	entry.NeedsHumanAttention = strings.EqualFold(strings.TrimSpace(action), "hold")
	entry.BlockerEvidence = nil
	entry.RecoveryRoot = nil
	if park != nil {
		current := *park
		if providedPark {
			if cause := strings.TrimSpace(current.Cause); cause != "" {
				entry.Reason = cause
			}
			if remedy := strings.TrimSpace(current.OperatorRemedy); remedy != "" {
				entry.RecoveryRemedy = remedy
			}
		}
		current.IntentResumable = current.intentResumable()
		current.Resumable = false
		current.Reachability = entry.RecoveryReachability
		current.HoldReason = entry.RecoveryReason
		if !staleReworkPark(current) {
			current.OperatorRemedy = entry.RecoveryRemedy
		}
		entry.Recovery = &current
		entry.RecoveryTarget = current.TargetState
		entry.RecoveryIntentResumable = current.IntentResumable
	} else {
		entry.Recovery = nil
		entry.RecoveryIntentResumable = strings.EqualFold(strings.TrimSpace(action), "defer")
	}
	if strings.EqualFold(strings.TrimSpace(reason), "dependency_recovery") && len(unresolvedWorkpadBlockers) > 0 {
		entry.Reason = blockedDependencyWaitingReason(unresolvedWorkpadBlockers)
	}
	state.Blocked[issue.ID] = entry
}

func blockedRecoveryDecisionShouldMaterialize(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case "no_recovery_predicate", "pr_maintenance_recovery", "rework_breaker_recovery":
		return false
	default:
		return true
	}
}

func blockedDependencyWaitingReason(blockers []dependencyBlocker) string {
	parts := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		part := dependencyBlockerLabel(blocker)
		if state := dependencyBlockerState(blocker); state != "" {
			part += " (" + state + ")"
		}
		parts = append(parts, part)
	}
	return "waiting on " + strings.Join(parts, ", ")
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
	case "unverifiable_blocker":
		return "Replace the free-text blocker with a typed predicate, or resolve it and update the Workpad."
	case "recorded_blocker_holds":
		return "Detent will re-check the recorded blocker predicate on the next tick."
	case "recorded_blocker":
		return "Detent is retaining the issue while its recorded blocker predicate is active."
	case "workpad_blocker":
		return "Resolve or remove the current Workpad blocker references."
	case "operator_stop":
		return "Review the operator stop and route the issue to its intended lane."
	case "cause_unchanged":
		return "Change the blocking cause, or move the issue to a lane that starts fresh work."
	case blockedRecoveryReasonBreakerCooldownActive:
		return "Detent will return the issue to its prior lane after the breaker cooldown ends."
	case "breaker_cooldown_unavailable":
		return "Restore the recorded Blocked transition time or move the issue to a lane that starts fresh work."
	case schedulerParkRecoveryUnavailableReason:
		return "Restore the scheduler-recorded previous lane or move the issue to the intended recovery lane."
	case "transition_failed":
		return "Retry the lane transition after restoring tracker write access."
	case githubRESTBudgetWaitingReason, githubRESTBudgetObservationPendingReason:
		return "Detent will return the issue to its prior lane after the matching GitHub REST budget rises above the worker reserve."
	case blockedReadyPullRequestLookupNoneReason:
		return "No PR found for the current workspace branch and head."
	case blockedReadyPullRequestLookupUnavailableReason:
		return "Retry exact-head pull request lookup after connector access recovers."
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

func explicitHumanAction(issue connector.Issue) string {
	if issue.IssueContract != nil && strings.TrimSpace(issue.IssueContract.HumanAction) != "" {
		return issue.IssueContract.HumanAction
	}
	signal, _ := rawIssueWorkpadSignal(issue)
	if current := issue.WorkpadSignal; current != nil && current.RecordedAt != nil && (signal == nil || signal.RecordedAt == nil || signal.RecordedAt.Before(*current.RecordedAt)) {
		signal = current
	}
	if signal != nil && signal.Invalid == nil && signal.Source == workpad.SourceStructured && strings.TrimSpace(signal.HumanAction) != "" {
		return strings.TrimSpace(signal.HumanAction)
	}
	index := currentWorkpadCommentIndex(issue.Comments)
	if index >= 0 && (signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured || signal.Status != workpad.StatusInProgress || !issue.Comments[index].AuthorAuthorized) {
		for _, comment := range issue.Comments {
			prior, ok := workpad.SignalFromComment(comment.Body, comment.URL, dependencyIssueRepo(issue.Identifier))
			if ok && prior != nil && prior.Invalid == nil && prior.Source == workpad.SourceStructured && strings.TrimSpace(prior.HumanAction) != "" {
				return strings.TrimSpace(prior.HumanAction)
			}
		}
	}
	return ""
}
