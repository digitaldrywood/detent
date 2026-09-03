package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
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
	blockedCauseFingerprintVersion                 = 3
	workflowActionCauseBlockedRecovery             = "cause_blocked_recovery"
	workflowActionBlockedReadyPRReconciliation     = "blocked_ready_pr_reconciliation"
	blockedReadyPullRequestLookupAttempts          = 3
	blockedReadyPullRequestLookupBackoff           = 250 * time.Millisecond
	blockedReadyPullRequestLookupFoundReason       = "ready_pr_lookup_found"
	blockedReadyPullRequestLookupNoneReason        = "ready_pr_lookup_none"
	blockedReadyPullRequestLookupUnavailableReason = "ready_pr_lookup_unavailable"
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
	signals := o.blockedCauseSignals(ctx, issue, runMode, targetState, fallback)
	targetState = blockedCauseTargetState(issue, signals, targetState)
	return workflowLaneMetadata{
		BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{
			Owner:                   blockedRecoveryOwnerOrchestrator,
			Cause:                   cause,
			Predicate:               strings.TrimSpace(predicate),
			CauseFingerprint:        blockedCauseFingerprint(cause, signals),
			CauseFingerprintVersion: blockedCauseFingerprintVersion,
			TargetState:             targetState,
			RunMode:                 strings.TrimSpace(runMode),
			IntentResumable:         blockedCauseResumable(issue, signals),
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
) bool {
	if normalizeState(issue.State) != normalizeState(blockedStatusState) {
		return false
	}
	dependencyCfg := normalizeDependencyAutoUnblockConfig(o.cfg.DependencyAutoUnblock)
	if o.currentBlockedOperatorStop(ctx, state, issue) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "operator_stop", nil, "")
		return false
	}
	_, currentParkFound := o.currentBlockedRecoveryPark(ctx, state, issue)
	withDependencies := o.issueWithDependencyRefs(issue)
	withDependencies, workpadRefs, workpadCurrent := o.issueWithCurrentWorkpadDependencyRefs(ctx, withDependencies)
	blockers := o.resolveDependencyBlockers(ctx, withDependencies)
	withDependencies.BlockedBy = dependencyResolvedBlockerRefs(blockers)
	references := make(map[string]connector.Issue, len(blockers))
	for _, blocker := range blockers {
		if blocker.Resolved {
			references[normalizedIssueIdentifier(blocker.Issue.Identifier)] = blocker.Issue
		}
	}
	recorded := recordedBlockerEvaluation{}
	if issue.WorkpadSignal == nil || len(issue.WorkpadSignal.Blockers) == 0 || workpadCurrent {
		recorded = o.evaluateRecordedBlockers(ctx, state, issue, references, now)
	}
	if recorded.Found {
		if recorded.Unverifiable {
			reason := "unverifiable_blocker"
			if issue.WorkpadSignal != nil {
				switch {
				case issue.WorkpadSignal.Invalid != nil:
					reason = BlockedRecoveryHumanHoldReason(issue, o.cfg.AutoPromote.OptoutLabel)
					if reason == "invalid_workpad_signal" && len(dependencyBlockersNotReady(blockers, dependencyCfg, o.cfg.TerminalStates)) == 0 {
						if handled, transitioned := o.reconcileInvalidWorkpadPark(ctx, state, issue, now); handled {
							return transitioned
						}
					}
				case strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "":
					reason = "human_action"
				}
			}
			o.recordBlockedRecoveryDecision(ctx, state, withDependencies, "hold", reason, nil, "")
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			return false
		}
		if recorded.Holds {
			action := "defer"
			if recorded.HumanOwned {
				action = "hold"
			}
			o.recordBlockedRecoveryDecision(ctx, state, withDependencies, action, "recorded_blocker_holds", nil, "")
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			return false
		}
		if len(blockers) > 0 && !dependencyBlockersReady(blockers, dependencyCfg, o.cfg.TerminalStates) {
			o.recordBlockedRecoveryDecision(
				ctx,
				state,
				withDependencies,
				"defer",
				"dependency_recovery",
				nil,
				"",
				dependencyBlockersNotReady(blockers, dependencyCfg, o.cfg.TerminalStates)...,
			)
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			return false
		}
		if !currentParkFound {
			if o.applyRecordedBlockerRecovery(ctx, state, withDependencies, blockers, recorded.Evidence, now) {
				return true
			}
			o.recordBlockedRecoveryDecision(ctx, state, withDependencies, "hold", "transition_failed", nil, "")
			setBlockedEvidence(state, issue.ID, recorded.Evidence)
			return false
		}
		setBlockedEvidence(state, issue.ID, recorded.Evidence)
	}
	workpadBlockers := dependencyBlockersMatchingRefs(blockers, workpadRefs)
	holdReason := o.blockedCauseHoldReason(issue, state, workpadBlockers, dependencyCfg, workpadCurrent)
	if holdReason != "" && holdReason != "invalid_workpad_signal" {
		o.recordBlockedRecoveryDecision(
			ctx,
			state,
			withDependencies,
			"hold",
			holdReason,
			nil,
			"",
			dependencyBlockersNotReady(workpadBlockers, dependencyCfg, o.cfg.TerminalStates)...,
		)
		return false
	}
	unresolvedBlockers := dependencyBlockersNotReady(blockers, dependencyCfg, o.cfg.TerminalStates)
	if len(unresolvedBlockers) > 0 {
		o.recordBlockedRecoveryDecision(ctx, state, withDependencies, "defer", "dependency_recovery", nil, "", unresolvedBlockers...)
		return false
	}
	if holdReason == "invalid_workpad_signal" {
		if handled, transitioned := o.reconcileInvalidWorkpadPark(ctx, state, issue, now); handled {
			return transitioned
		}
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", holdReason, nil, "")
		return false
	}
	if park, ok := o.latestReworkBreakerPark(ctx, issue); ok {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "rework_breaker_recovery", nil, park.Signature)
		return false
	}
	park, ok := o.currentBlockedRecoveryParkWithLegacyRESTBudget(ctx, state, issue)
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
	if handled, transitioned := o.reconcileObsoleteArtifactSpendProgressPark(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if handled, transitioned := o.reconcilePersistentlyMissingRequiredCheckPark(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if handled, transitioned := o.reconcileBlockedReadyPullRequest(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if park.Owner == blockedRecoveryOwnerHuman || park.Owner == blockedRecoveryOwnerOperator {
		reason := strings.TrimSpace(park.HoldReason)
		if reason == "" {
			reason = "human_recovery"
		}
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", reason, &park, park.CauseFingerprint)
		return false
	}
	if handled, transitioned := o.reconcileRepeatedFailureGitHubRESTBudgetPark(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if handled, transitioned := o.reconcileLifetimeLimitPark(ctx, state, issue, park, now); handled {
		return transitioned
	}
	if park.Predicate == blockedRecoveryPredicateManaged {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "managed_recovery", &park, park.CauseFingerprint)
		return false
	}
	if park.Predicate != blockedRecoveryPredicateFingerprintChange &&
		park.Predicate != blockedRecoveryPredicateOncePerFingerprint {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "no_recovery_predicate", &park, park.CauseFingerprint)
		return false
	}
	breakerPark := breakerParkCause(park.Cause)
	signals := blockedCauseSignals{}
	currentFingerprint := ""
	if park.CauseFingerprintVersion != blockedCauseFingerprintVersion {
		signals = o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
		currentFingerprint = blockedCauseFingerprint(park.Cause, signals)
		rebased, ok := o.rebaselineLegacyBlockedRecoveryPark(ctx, state, issue, park, signals, currentFingerprint)
		if !ok {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "legacy_cause_fingerprint", &park, park.CauseFingerprint)
			return false
		}
		park = rebased
	}
	if breakerPark {
		parkedAt, found := o.currentBlockedRecoveryParkedAt(ctx, state, issue)
		if !found {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "breaker_cooldown_unavailable", &park, park.CauseFingerprint)
			return false
		}
		cooldown := normalizeBlockedRecoveryConfig(o.cfg.BlockedRecovery).BreakerCooldown
		if now.Before(parkedAt.Add(cooldown)) {
			o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "breaker_cooldown_active", &park, park.CauseFingerprint)
			return false
		}
	}
	if currentFingerprint == "" {
		signals = o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
		currentFingerprint = blockedCauseFingerprint(park.Cause, signals)
	}
	if park.Predicate == blockedRecoveryPredicateFingerprintChange && currentFingerprint == park.CauseFingerprint {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "cause_unchanged", &park, currentFingerprint)
		return false
	}
	signature := blockedCauseRecoverySignature(park.Cause, currentFingerprint)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionCauseBlockedRecovery, signature); consumed {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "fingerprint_already_consumed", &park, currentFingerprint)
		return false
	}
	targetState := blockedCauseTargetState(issue, signals, park.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, targetState, now, "cause_blocked_recovery", metadata, laneMutationPreserveOwnership); err != nil {
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

func (o *Orchestrator) reconcileObsoleteArtifactSpendProgressPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	if strings.TrimSpace(park.Cause) != spendProgressReason ||
		o.spendProgressDeliverableKind(Running{Issue: issue}) != workflowconfig.DeliverableArtifact {
		return false, false
	}
	record, ok := o.latestSpendProgressRecord(ctx, issue)
	if !ok || strings.TrimSpace(record.BlockReason) != spendProgressReason || strings.TrimSpace(record.Case) != spendProgressCaseNoPR {
		return false, false
	}
	signals := o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
	fingerprint := blockedCauseFingerprint(park.Cause, signals)
	signature := blockedCauseRecoverySignature("obsolete_artifact_pr_evidence", fingerprint)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionCauseBlockedRecovery, signature); consumed {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "obsolete_artifact_spend_recovery_already_consumed", &park, fingerprint)
		return true, false
	}
	targetState := blockedCauseTargetState(issue, signals, park.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, targetState, now, "obsolete_artifact_spend_recovery", metadata, laneMutationPreserveOwnership); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "obsolete_artifact_spend_recovery_transition_failed", &park, fingerprint)
		return true, false
	}
	if o.connector != nil {
		comment := "Recovered this artifact-deliverable item from Blocked because its persisted spend breaker checked PR evidence that the project cannot produce. Future spend-progress checks use artifact receipts, status, deliverable metadata, and output evidence."
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("obsolete artifact spend recovery comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	o.logBlockedRecoveryDecision(issue, "transition", "obsolete_artifact_spend_recovery", &park, fingerprint)
	return true, true
}

func (o *Orchestrator) rebaselineLegacyBlockedRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	signals blockedCauseSignals,
	fingerprint string,
) (workflowLaneBlockedRecoveryMetadata, bool) {
	if o == nil || o.workflowMetrics == nil || strings.TrimSpace(park.Cause) == "" || strings.TrimSpace(fingerprint) == "" {
		return park, false
	}
	updater, ok := o.workflowMetrics.(WorkflowMetricsMetadataUpdater)
	if !ok {
		return park, false
	}
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok ||
		entry.Event.ID <= 0 ||
		normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) ||
		!blockedEntryMatchesCurrent(issue, entry.Event.StartedAt) ||
		entry.Metadata.BlockedRecovery == nil ||
		!sameBlockedRecoveryPark(*entry.Metadata.BlockedRecovery, park) {
		return park, false
	}

	rebased := park
	rebased.Cause = strings.TrimSpace(rebased.Cause)
	rebased.CauseFingerprint = strings.TrimSpace(fingerprint)
	rebased.CauseFingerprintVersion = blockedCauseFingerprintVersion
	rebased.TargetState = blockedCauseTargetState(issue, signals, rebased.TargetState)
	rebased.RunMode = strings.TrimSpace(rebased.RunMode)
	rebased.IntentResumable = blockedCauseResumable(issue, signals)
	rebased.Resumable = false
	rebased.Reachability = ""
	rebased.HoldReason = ""
	rebased.OperatorRemedy = ""

	metadata := entry.Metadata
	metadata.BlockedRecovery = &rebased
	if err := updater.UpdateWorkflowPhaseEventMetadata(ctx, entry.Event.ID, workflowLaneMetadataJSON(issue, metadata)); err != nil {
		if o.logger != nil {
			o.logger.Warn("legacy blocked recovery fingerprint rebaseline failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
		return park, false
	}
	if state != nil {
		if blocked, found := state.Blocked[strings.TrimSpace(issue.ID)]; found {
			blocked.Recovery = &rebased
			state.Blocked[strings.TrimSpace(issue.ID)] = blocked
		}
	}
	if o.logger != nil {
		o.logger.Info("legacy blocked recovery fingerprint rebaselined", "issue_id", issue.ID, "identifier", issue.Identifier, "cause", rebased.Cause, "fingerprint_version", blockedCauseFingerprintVersion)
	}
	return rebased, true
}

func sameBlockedRecoveryPark(left workflowLaneBlockedRecoveryMetadata, right workflowLaneBlockedRecoveryMetadata) bool {
	return strings.EqualFold(strings.TrimSpace(left.Owner), strings.TrimSpace(right.Owner)) &&
		strings.TrimSpace(left.Cause) == strings.TrimSpace(right.Cause) &&
		strings.TrimSpace(left.Predicate) == strings.TrimSpace(right.Predicate) &&
		strings.TrimSpace(left.CauseFingerprint) == strings.TrimSpace(right.CauseFingerprint) &&
		left.CauseFingerprintVersion == right.CauseFingerprintVersion
}

func (o *Orchestrator) reconcileInvalidWorkpadPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	now time.Time,
) (bool, bool) {
	park, ok := o.currentBlockedRecoveryParkWithLegacyRESTBudget(ctx, state, issue)
	if !ok {
		return false, false
	}
	readyPullRequestIssue := cloneIssue(issue)
	readyPullRequestIssue.WorkpadSignal = nil
	readyPullRequestIssue.Comments = nil
	if handled, transitioned := o.reconcileBlockedReadyPullRequest(ctx, state, readyPullRequestIssue, park, now); handled {
		return true, transitioned
	}
	if !strings.EqualFold(strings.TrimSpace(park.Owner), blockedRecoveryOwnerOrchestrator) {
		return false, false
	}
	return o.reconcileRepeatedFailureGitHubRESTBudgetPark(ctx, state, issue, park, now)
}

func (o *Orchestrator) reconcileBlockedReadyPullRequest(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	if !blockedReadyPullRequestRecoverableCause(park) {
		return false, false
	}
	signals := o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
	if !implementProgressLinkedPullRequest(issue) {
		issue.BranchName = blockedReadyPullRequestBranch(state, issue)
		if issue.BranchName == "" || strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
			return false, false
		}
		lookedUp, outcome, err := o.lookupBlockedReadyPullRequest(ctx, issue, signals)
		switch outcome {
		case blockedReadyPullRequestLookupFoundReason:
			issue = lookedUp
			signals = o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
			o.recordBlockedRecoveryDecision(ctx, state, issue, "evaluate", outcome, &park, blockedCauseFingerprint(park.Cause, signals))
		case blockedReadyPullRequestLookupNoneReason:
			o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", outcome, &park, blockedCauseFingerprint(park.Cause, signals))
			return true, false
		default:
			o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", blockedReadyPullRequestLookupUnavailableReason, &park, blockedCauseFingerprint(park.Cause, signals))
			if err != nil && o.logger != nil {
				o.logger.Warn("blocked ready pull request lookup unavailable", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
			}
			return true, false
		}
	}
	if reason := o.blockedReadyPullRequestDeferredReason(ctx, state, issue, signals, now); reason != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", reason, &park, blockedCauseFingerprint(park.Cause, signals))
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
		laneMutationPreserveOwnership,
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

func blockedReadyPullRequestBranch(state *State, issue connector.Issue) string {
	if branch := strings.TrimSpace(issue.BranchName); branch != "" {
		return branch
	}
	if state == nil {
		return ""
	}
	blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(blocked.Issue.BranchName)
}

func (o *Orchestrator) lookupBlockedReadyPullRequest(
	ctx context.Context,
	issue connector.Issue,
	signals blockedCauseSignals,
) (connector.Issue, string, error) {
	repository := pullRequestRepository(issue)
	branch := strings.TrimSpace(issue.BranchName)
	headSHA := strings.TrimSpace(signals.WorkspaceHeadSHA)
	if repository == "" || branch == "" || headSHA == "" {
		return issue, blockedReadyPullRequestLookupUnavailableReason, errors.New(
			"exact-head pull request lookup requires repository, branch, and workspace head",
		)
	}
	lookup, ok := o.connector.(connector.PullRequestHeadLookup)
	if !ok {
		return issue, blockedReadyPullRequestLookupUnavailableReason, errors.New("connector does not support exact-head pull request lookup")
	}

	var lookupErr error
	for attempt := 1; attempt <= blockedReadyPullRequestLookupAttempts; attempt++ {
		pullRequest, found, err := lookup.LookupPullRequestByHead(ctx, repository, branch, headSHA)
		if err == nil {
			if !found || normalizePullRequestState(pullRequest.State) != "open" {
				return issue, blockedReadyPullRequestLookupNoneReason, nil
			}
			if strings.TrimSpace(pullRequest.BranchName) != branch || strings.TrimSpace(pullRequest.HeadSHA) != headSHA {
				return issue, blockedReadyPullRequestLookupUnavailableReason, fmt.Errorf(
					"exact-head pull request lookup returned branch %q head %q",
					strings.TrimSpace(pullRequest.BranchName),
					strings.TrimSpace(pullRequest.HeadSHA),
				)
			}
			candidate := cloneIssue(issue)
			candidate.PRRepository = repository
			candidate.PullRequest = &pullRequest
			candidate.PRNumber = &pullRequest.Number
			hydrator, ok := o.connector.(connector.PullRequestHydrator)
			if !ok {
				return issue, blockedReadyPullRequestLookupUnavailableReason, errors.New("connector does not support pull request hydration")
			}
			hydrated, err := hydrator.HydratePullRequest(ctx, candidate)
			if err != nil {
				return issue, blockedReadyPullRequestLookupUnavailableReason, fmt.Errorf("hydrate exact-head pull request: %w", err)
			}
			if hydrated.PullRequest == nil || hydrated.PullRequest.Number != pullRequest.Number ||
				strings.TrimSpace(hydrated.PullRequest.BranchName) != branch || strings.TrimSpace(hydrated.PullRequest.HeadSHA) != headSHA {
				return issue, blockedReadyPullRequestLookupUnavailableReason, errors.New("exact-head pull request hydration returned inconsistent pull request data")
			}
			return hydrated, blockedReadyPullRequestLookupFoundReason, nil
		}
		lookupErr = err
		if attempt == blockedReadyPullRequestLookupAttempts {
			break
		}
		wait := o.deliverableRecoveryWait
		if wait == nil {
			wait = waitForDispatchBackoff
		}
		if !wait(ctx, blockedReadyPullRequestLookupBackoff*time.Duration(1<<(attempt-1))) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				lookupErr = ctxErr
			} else {
				lookupErr = errors.New("exact-head pull request lookup retry interrupted")
			}
			break
		}
	}
	return issue, blockedReadyPullRequestLookupUnavailableReason, lookupErr
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
	return cause == strandedUnpushedWorkReason ||
		cause == noProgressLimitReason ||
		cause == dispatchLoopDetectedReason ||
		cause == repeatedFailureCircuitBreakerCause ||
		cause == terminalAttemptRetryLimitCause
}

func (o *Orchestrator) blockedReadyPullRequestDeferredReason(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	signals blockedCauseSignals,
	now time.Time,
) string {
	if reason := o.mergeLaneUnavailableReason(); reason != "" {
		return reason
	}
	autoPromoteCfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	if !autoPromoteCfg.Enabled {
		return "auto_promote_disabled"
	}
	pullRequest := issue.PullRequest
	if pullRequest == nil || pullRequestHydrationBlocksProgress(pullRequest) ||
		strings.TrimSpace(pullRequest.HeadSHA) == "" || strings.TrimSpace(pullRequest.BaseSHA) == "" {
		return "pull_request_hydration_unavailable"
	}
	if !signals.WorkspacePresent || strings.TrimSpace(signals.WorkspaceStatus) != "present" || strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
		return "workspace_head_unavailable"
	}
	if len(signals.TrackedPaths) > 0 {
		return "workspace_tracked_changes_present"
	}
	if signals.PullRequestComparisonAvailable {
		if len(signals.CommitsNotInPullRequest) > 0 {
			return "workspace_commits_not_in_pull_request"
		}
	} else if signals.UnpushedCommits > 0 && strings.TrimSpace(signals.WorkspaceHeadSHA) != strings.TrimSpace(pullRequest.HeadSHA) {
		return "workspace_pull_request_head_mismatch"
	}
	if !mergeWorkerProgrammaticMergeReady(issue) || !reworkBreakerCIGreen(pullRequest) || len(pullRequest.StaleSuccessfulChecks) > 0 {
		return "pull_request_not_merge_ready"
	}
	if !staleMergingIssueReadyForDispatch(issue, o.cfg) {
		return "merge_dispatch_revoked"
	}
	if !o.reworkBreakerAutoPromoteGateReady(ctx, state, issue, autoPromoteCfg, now) {
		return "pull_request_gate_not_ready"
	}
	return ""
}

func (o *Orchestrator) mergeLaneUnavailableReason() string {
	if !o.cfg.MergeFastPathEnabled {
		return "merge_fast_path_disabled"
	}
	if !stateIn(autoPromoteMergingState, o.cfg.ActiveStates) {
		return "merging_lane_inactive"
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
	if issue.WorkpadSignal != nil && issue.WorkpadSignal.Invalid != nil {
		return "invalid_workpad_signal"
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
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
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
		!workflowLaneEntryMatchesCurrent(issue, entry.Event) ||
		entry.Metadata.BlockedRecovery == nil {
		return workflowLaneBlockedRecoveryMetadata{}, false
	}
	return *entry.Metadata.BlockedRecovery, true
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
	if ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) &&
		workflowLaneEntryMatchesCurrent(issue, entry.Event) &&
		entry.Metadata.BlockedRecovery != nil &&
		!parkedAt.IsZero() {
		return parkedAt, true
	}
	if issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() {
		return issue.StageUpdatedAt.UTC(), true
	}
	return time.Time{}, false
}

func (o *Orchestrator) currentBlockedRecoveryParkWithLegacyRESTBudget(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (workflowLaneBlockedRecoveryMetadata, bool) {
	park, ok := o.currentBlockedRecoveryPark(ctx, state, issue)
	if ok {
		return park, true
	}
	return o.currentLegacyRepeatedFailureGitHubRESTBudgetPark(ctx, issue)
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
		current.OperatorRemedy = entry.RecoveryRemedy
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
	if strings.Contains(reason, "already_consumed") {
		return false
	}
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
	case "fingerprint_already_consumed":
		return "Change the blocking cause, or move the issue to a lane that starts fresh work."
	case "legacy_cause_fingerprint":
		return "Park predates the current recovery schema; move the issue to Todo or Rework to re-evaluate it."
	case "breaker_cooldown_active":
		return "Detent will re-evaluate this circuit-breaker park after its cooldown expires."
	case "breaker_cooldown_unavailable":
		return "Restore the recorded Blocked transition time or move the issue to a lane that starts fresh work."
	case "transition_failed":
		return "Retry the lane transition after restoring tracker write access."
	case githubRESTBudgetWaitingReason, githubRESTBudgetObservationPendingReason:
		return "Detent will return the issue to its prior lane after the matching GitHub REST budget rises above the worker reserve."
	case githubRESTBudgetRearmConsumedReason:
		return "Wait for the next GitHub REST reset window, or move the issue manually after confirming capacity is stable."
	case "managed_recovery":
		return "Review the configured recovery owner and move the issue manually if that owner cannot recover it."
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
