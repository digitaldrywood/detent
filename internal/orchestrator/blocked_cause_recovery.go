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
	"github.com/digitaldrywood/detent/internal/forgeavailability"
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
	workflowActionBlockedReadyPRReconciliation     = "blocked_ready_pr_reconciliation"
	blockedReadyPullRequestLookupAttempts          = 3
	blockedReadyPullRequestLookupBackoff           = 250 * time.Millisecond
	blockedReadyPullRequestLookupFoundReason       = "ready_pr_lookup_found"
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
	if err := o.retryMachineFailure(ctx, state, issue, nextAttempt(state.Retry[issue.ID].Attempt), now, issue.BlockerReason, ""); err != nil {
		if o.logger != nil {
			o.logger.Warn("blocked recovery retry failed", "issue_id", issue.ID, "error", err)
		}
		return false
	}
	return true
}

func (o *Orchestrator) reconcileAttemptTriagePark(ctx context.Context, state *State, issue connector.Issue, now time.Time) (bool, bool) {
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok || entry.Event.Reason != attemptAllowanceExhaustedReason ||
		!workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return false, false
	}
	if BlockedRecoveryHumanHoldReason(issue, o.cfg.AutoPromote.OptoutLabel) != "" {
		return true, false
	}
	if stateIn(entry.Event.PreviousPhaseName, normalizedStates([]string{"In Progress", autoPromoteReworkState})) {
		if err := o.updateIssueState(ctx, state, issue, entry.Event.PreviousPhaseName, now, workflowActionRecordedBlockerRecovery); err != nil {
			return true, false
		}
		delete(state.Blocked, issue.ID)
		o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
		return true, true
	}
	if entry.Metadata.PullRequest == nil || strings.TrimSpace(entry.Metadata.PullRequest.HeadSHA) == "" || o.mergeLaneUnavailableReason() != "" {
		return true, false
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return true, false
	}
	live, err := hydrator.HydratePullRequest(ctx, issue)
	if err != nil || live.PullRequest == nil || pullRequestHydrationBlocksProgress(live.PullRequest) {
		return true, false
	}
	live, ok = o.hydrateAutoPromoteReviewThreads(ctx, live)
	if !ok || !mergeWorkerProgrammaticMergeReady(live, o.cfg) || !reworkBreakerCIGreen(live.PullRequest) ||
		len(live.PullRequest.StaleSuccessfulChecks) > 0 ||
		entry.Metadata.PullRequest.Number != int64(pullRequestNumber(live)) ||
		entry.Metadata.PullRequest.Repository != pullRequestRepository(live) {
		return true, false
	}
	if entry.Metadata.PullRequest.HeadSHA != strings.TrimSpace(live.PullRequest.HeadSHA) &&
		!pullRequestHeadAfter(live, workflowLaneTransitionAt(entry.Event)) {
		return true, false
	}
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	if !cfg.Enabled || autoPromoteHumanReviewRequired(live, cfg, cfg.Gate) {
		return true, false
	}
	candidate := cloneIssue(live)
	candidate.State = cfg.ReworkState
	summary := AutoPromoteSummaryFromIssue(candidate)
	summary.SecurityAudit = o.securityAuditEvaluation(ctx, candidate)
	summary.CompletedFinalState = autoPromoteCompletedFinalState(state, live.ID)
	summary.AutomatedReviewWaitExpired = autoPromoteReviewWaitExpired(state, live.ID, cfg, now)
	if !reworkGateWaitAuditReady(cfg.Gate, summary.SecurityAudit) {
		return true, false
	}
	// The recovery predicate owns audit eligibility here: a missing audit can
	// run in Merging. Evaluate the remaining gates without repeating that check;
	// the instance config still requires the exact-head audit before merging.
	cfg.Gate.SecurityAudit.Enabled = false
	candidate, decision := o.hydrateAutoPromoteWorkpadDecision(ctx, candidate, summary, cfg, now)
	var validatorReady bool
	decision, validatorReady = o.applyValidatorStage(ctx, state, candidate, &summary, decision, cfg, now)
	if !validatorReady || decision.Action != AutoPromoteActionPromote {
		return true, false
	}
	signature := blockedReadyPullRequestSignature(live, workflowLaneBlockedRecoveryMetadata{Cause: attemptAllowanceExhaustedReason})
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionBlockedReadyPRReconciliation, signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, live.ID, live, autoPromoteMergingState, now, workflowActionBlockedReadyPRReconciliation, metadata); err != nil {
		return true, false
	}
	delete(state.Blocked, live.ID)
	o.clearAutoPromotedIssueDispatchMemory(state, live.ID)
	o.recordMergeQueueEntered(state, promotedIssue(live, autoPromoteMergingState, now), now, workflowActionBlockedReadyPRReconciliation)
	return true, true
}

func (o *Orchestrator) reconcileLegacyDeliverableCredentialPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	now time.Time,
) (bool, bool) {
	if strings.TrimSpace(park.Cause) != deliverableConfigurationFailureCause {
		return false, false
	}
	if o.workAttempts == nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "cause_unchanged", &park, park.CauseFingerprint)
		return true, false
	}
	legacyAttempt, err := o.workAttempts.WorkAttempt(ctx, park.WorkAttemptID)
	if err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "cause_unchanged", &park, park.CauseFingerprint)
		return true, false
	}
	legacyCredentialFailure := strings.TrimSpace(legacyAttempt.ErrorClass) == deliverableConfigurationFailureCause &&
		forgeavailability.WorkerGitHubCredentialUnavailable(legacyAttempt.ErrorMessage)
	persistedMetadata, persistedCredentialWait := forgeWaitMetadataFromAttempt(legacyAttempt)
	persistedCredentialWait = persistedCredentialWait && persistedMetadata.ErrorClass == forgeavailability.ClassWorkerGitHubCredentialUnavailable
	if !legacyCredentialFailure && !persistedCredentialWait {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "cause_unchanged", &park, park.CauseFingerprint)
		return true, false
	}
	targetState := strings.TrimSpace(park.TargetState)
	if targetState == "" || normalizeState(targetState) == normalizeState(blockedStatusState) {
		targetState = autoPromoteReworkState
	}
	operation := persistedMetadata.Operation
	if operation == "" {
		operation = "create_pull_request"
		if strings.Contains(strings.ToLower(legacyAttempt.ErrorMessage), "git push") {
			operation = "git push"
		}
	}
	detail := strings.TrimSpace(legacyAttempt.ErrorMessage)
	availabilityErr := forgeavailability.NewError(
		forgeavailability.Scope{Host: forgeHostForIssue(issue, o.cfg.ForgeHost), Operation: operation},
		forgeavailability.ClassWorkerGitHubCredentialUnavailable,
		errors.New(detail),
	)
	running := Running{Issue: issue, Attempt: park.AttemptNumber, WorkAttemptID: park.WorkAttemptID}
	var condition ForgeCondition
	var forgeRetry *runpkg.ForgeRetry
	if persistedCredentialWait {
		o.restoreForgeAvailabilityWait(state, issue, legacyAttempt, persistedMetadata, now)
		condition, _ = forgeCondition(state, persistedMetadata.Host)
		forgeRetry = state.Retry[issue.ID].ForgeRetry
	} else {
		condition = o.registerForgeUnavailable(state, availabilityErr, running, now)
		forgeRetry = &runpkg.ForgeRetry{
			Host:      condition.Host,
			Operation: operation,
			Branch:    strings.TrimSpace(issue.BranchName),
		}
	}
	message := strings.TrimSpace(availabilityErr.Error())
	if legacyCredentialFailure && !o.persistLegacyForgeAvailabilityWait(ctx, running, condition, forgeRetry, message, legacyAttempt.WorkerMetadataJSON) {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", forgeavailability.ClassWorkerGitHubCredentialUnavailable, &park, park.CauseFingerprint)
		return true, false
	}
	state.Retry[issue.ID] = Retry{
		Issue:            cloneIssue(issue),
		Attempt:          park.AttemptNumber,
		DueAt:            condition.NextProbeAt,
		Error:            detail,
		ForgeUnavailable: true,
		ForgeHost:        condition.Host,
		ForgeRetry:       forgeRetry,
	}
	signature := blockedCauseRecoverySignature(deliverableConfigurationFailureCause, strings.TrimSpace(park.CauseFingerprint))
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDStrictWithMetadata(ctx, state, issue.ID, issue, targetState, now, "cause_blocked_recovery", metadata); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", "transition_failed", &park, park.CauseFingerprint)
		return true, false
	}
	issue.State = targetState
	retry := state.Retry[issue.ID]
	retry.Issue = cloneIssue(issue)
	state.Retry[issue.ID] = retry
	delete(state.Blocked, issue.ID)
	o.logBlockedRecoveryDecision(issue, "transition", forgeavailability.ClassWorkerGitHubCredentialUnavailable, &park, park.CauseFingerprint)
	return true, true
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
	targetState := blockedCauseTargetState(issue, signals, park.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, targetState, now, "obsolete_artifact_spend_recovery", metadata); err != nil {
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
		if deliverableRecoveryPark(park) {
			if issue.BranchName == "" {
				_, suffix, found := strings.Cut(park.Cause, "pushed branch ")
				if fields := strings.Fields(suffix); found && len(fields) > 0 {
					issue.BranchName = fields[0]
				}
			}
			if issue.BranchName != "" && strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
				lookup, ok := o.connector.(connector.BranchHeadLookup)
				var err error
				if ok {
					signals.WorkspaceHeadSHA, err = lookup.LookupBranchHead(ctx, pullRequestRepository(issue), issue.BranchName)
				}
				if err != nil || strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
					o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", blockedReadyPullRequestLookupUnavailableReason, &park, blockedCauseFingerprint(park.Cause, signals))
					if err != nil && o.logger != nil {
						o.logger.Warn("blocked deliverable branch lookup unavailable", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
					}
					return true, false
				}
			}
		}
		if issue.BranchName == "" || strings.TrimSpace(signals.WorkspaceHeadSHA) == "" {
			return false, false
		}
		lookedUp, outcome, err := o.lookupBlockedReadyPullRequest(ctx, issue, signals)
		switch outcome {
		case blockedReadyPullRequestLookupFoundReason:
			issue = lookedUp
			signals = o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
			o.recordBlockedRecoveryDecision(ctx, state, issue, "evaluate", outcome, &park, blockedCauseFingerprint(park.Cause, signals))
			if issue.PullRequest != nil && normalizePullRequestState(issue.PullRequest.State) == "merged" {
				summary := staleMergedPullRequestSummaryFromIssue(issue)
				decision := staleMergedPullRequestDecision(issue, summary)
				targetState := staleMergedPullRequestTargetState(decision, o.cfg.AutoPromote, o.cfg.TerminalStates)
				if targetState == "" || !o.applyStaleMergedPullRequestDecision(ctx, state, issue, summary, decision, targetState, now) {
					o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", outcome, &park, blockedCauseFingerprint(park.Cause, signals))
					return true, false
				}
				o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
				return true, true
			}
		case blockedReadyPullRequestLookupNoneReason:
			if deliverableRecoveryPark(park) {
				if _, ok := o.connector.(connector.PullRequestDraftCreator); !ok {
					o.recordBlockedRecoveryDecision(ctx, state, issue, "hold", blockedReadyPullRequestLookupUnavailableReason, &park, blockedCauseFingerprint(park.Cause, signals))
					return true, false
				}
				lookup := deliverableRecoveryLookupResult{
					Branch:               issue.BranchName,
					Repository:           pullRequestRepository(issue),
					HeadSHA:              signals.WorkspaceHeadSHA,
					HydrationState:       deliverableRecoveryHydrationState(issue.PullRequest),
					LookupResult:         "no exact-head pull request",
					Attempts:             blockedReadyPullRequestLookupAttempts,
					CommitsAhead:         1,
					RemoteBranchExists:   true,
					DeliveryStateChecked: true,
				}
				lookup = o.createDeliverableRecoveryPullRequest(ctx, Running{Issue: issue}, lookup)
				if infrastructureErr, infrastructureFailure := o.deliverableRecoveryInfrastructureError(Running{Issue: issue}, lookup.CreateError); infrastructureFailure {
					o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", blockedReadyPullRequestLookupUnavailableReason, &park, blockedCauseFingerprint(park.Cause, signals))
					if o.logger != nil {
						o.logger.Warn("blocked deliverable draft creation unavailable", "issue_id", issue.ID, "identifier", issue.Identifier, "error", infrastructureErr)
					}
					return true, false
				}
				if lookup.PullRequest != nil {
					issue = deliverableRecoveryIssue(Running{Issue: issue}, lookup)
				}
				return o.returnBlockedDeliverableRecoveryToRework(ctx, state, issue, park, lookup.LookupResult, now)
			}
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
	if deliverableRecoveryPark(park) && issue.PullRequest != nil && issue.PullRequest.Draft {
		return o.returnBlockedDeliverableRecoveryToRework(ctx, state, issue, park, "existing draft pull request", now)
	}
	if reason := o.blockedReadyPullRequestDeferredReason(ctx, state, issue, signals, now); reason != "" {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", reason, &park, blockedCauseFingerprint(park.Cause, signals))
		return true, false
	}
	signature := blockedReadyPullRequestSignature(issue, park)
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

func deliverableRecoveryPark(park workflowLaneBlockedRecoveryMetadata) bool {
	cause := strings.TrimSpace(park.Cause)
	return cause == deliverableRecoveryNeedsHumanReason || strings.HasPrefix(cause, deliverableRecoveryNeedsHumanReason+":")
}

func (o *Orchestrator) returnBlockedDeliverableRecoveryToRework(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
	detail string,
	now time.Time,
) (bool, bool) {
	target := normalizeAutoPromoteConfig(o.cfg.AutoPromote).ReworkState
	if strings.TrimSpace(target) == "" {
		target = autoPromoteReworkState
	}
	signature := blockedCauseRecoverySignature(park.Cause, strings.Join([]string{strings.TrimSpace(issue.BranchName), strings.TrimSpace(detail)}, ":"))
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, target, now, "cause_blocked_recovery", metadata); err != nil {
		o.recordBlockedRecoveryDecision(ctx, state, issue, "defer", "transition_failed", &park, signature)
		return true, false
	}
	if o.connector != nil {
		comment := "Detent returned this retired deliverable-recovery park to " + target + ".\n\n- branch: `" + strings.TrimSpace(issue.BranchName) + "`\n- recovery: " + strings.TrimSpace(detail)
		if err := o.connector.CreateComment(ctx, issue.ID, comment); err != nil && o.logger != nil {
			o.logger.Warn("deliverable recovery rework comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	delete(state.Blocked, issue.ID)
	o.clearAutoPromotedIssueDispatchMemory(state, issue.ID)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   workflowActionCauseBlockedRecovery,
		Message: "returned " + issueLabel(issue) + " from its retired deliverable-recovery park to " + target,
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
			if !found {
				return issue, blockedReadyPullRequestLookupNoneReason, nil
			}
			if strings.TrimSpace(pullRequest.BranchName) != branch || strings.TrimSpace(pullRequest.HeadSHA) != headSHA {
				return issue, blockedReadyPullRequestLookupUnavailableReason, fmt.Errorf(
					"exact-head pull request lookup returned branch %q head %q",
					strings.TrimSpace(pullRequest.BranchName),
					strings.TrimSpace(pullRequest.HeadSHA),
				)
			}
			state := normalizePullRequestState(pullRequest.State)
			if state != "open" && state != "merged" {
				return issue, blockedReadyPullRequestLookupNoneReason, nil
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
	if len(signals.TrackedPaths) > 0 || len(signals.UntrackedPaths) > 0 {
		return "workspace_changes_present"
	}
	if signals.PullRequestComparisonAvailable {
		if len(signals.CommitsNotInPullRequest) > 0 {
			return "workspace_commits_not_in_pull_request"
		}
	} else if signals.UnpushedCommits > 0 && strings.TrimSpace(signals.WorkspaceHeadSHA) != strings.TrimSpace(pullRequest.HeadSHA) {
		return "workspace_pull_request_head_mismatch"
	}
	if !mergeWorkerProgrammaticMergeReady(issue, o.cfg) || !reworkBreakerCIGreen(pullRequest) || len(pullRequest.StaleSuccessfulChecks) > 0 {
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

func breakerParkCauseFromLaneReason(reason string) (string, bool) {
	reason = strings.TrimSpace(reason)
	if reason == instantFailureCircuitBreakerLaneReason {
		return instantFailureCircuitBreakerCause, true
	}
	return reason, breakerParkCause(reason)
}

func (o *Orchestrator) normalizeCurrentBreakerRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	park workflowLaneBlockedRecoveryMetadata,
) workflowLaneBlockedRecoveryMetadata {
	if !breakerParkCause(park.Cause) ||
		strings.EqualFold(strings.TrimSpace(park.Owner), blockedRecoveryOwnerHuman) ||
		strings.EqualFold(strings.TrimSpace(park.Owner), blockedRecoveryOwnerOperator) {
		return park
	}
	normalized := park
	normalized.Predicate = strings.TrimSpace(normalized.Predicate)
	if normalized.Predicate == "" || normalized.Predicate == blockedRecoveryPredicateFingerprintChange {
		normalized.Predicate = blockedRecoveryPredicateBreakerCooldown
	}
	entry, entryFound := o.latestWorkflowLaneEntry(ctx, issue)
	if entryFound && normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) && workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		previousState := strings.TrimSpace(entry.Event.PreviousPhaseName)
		if previousState != "" && normalizeState(previousState) != normalizeState(blockedStatusState) {
			normalized.TargetState = previousState
		}
	}
	if normalized == park {
		return normalized
	}
	o.persistCurrentBlockedRecoveryPark(ctx, state, issue, entry, entryFound, normalized)
	return normalized
}

func (o *Orchestrator) deriveCurrentBreakerRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
) (workflowLaneBlockedRecoveryMetadata, bool, string, string) {
	entry, ok := o.latestWorkflowLaneEntry(ctx, issue)
	if !ok || normalizeState(entry.Event.PhaseName) != normalizeState(blockedStatusState) || !workflowLaneEntryMatchesCurrent(issue, entry.Event) {
		return workflowLaneBlockedRecoveryMetadata{}, false, "", ""
	}
	laneReason := strings.TrimSpace(entry.Event.Reason)
	cause, schedulerPark := breakerParkCauseFromLaneReason(laneReason)
	if !schedulerPark {
		return workflowLaneBlockedRecoveryMetadata{}, false, "", ""
	}
	previousState := strings.TrimSpace(entry.Event.PreviousPhaseName)
	if previousState == "" || normalizeState(previousState) == normalizeState(blockedStatusState) {
		return workflowLaneBlockedRecoveryMetadata{}, false, laneReason, "previous_lane_unavailable"
	}
	runMode := RunModeImplement
	if normalizeState(previousState) == normalizeState(autoPromoteMergingState) {
		runMode = RunModeMerge
	}
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		runMode,
		cause,
		blockedRecoveryPredicateBreakerCooldown,
		previousState,
		DiffStats{},
	)
	park := *metadata.BlockedRecovery
	park.TargetState = previousState
	o.persistCurrentBlockedRecoveryPark(ctx, state, issue, entry, true, park)
	return park, true, laneReason, ""
}

func (o *Orchestrator) persistCurrentBlockedRecoveryPark(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	entry workflowTimelineMetadataMatch,
	entryFound bool,
	park workflowLaneBlockedRecoveryMetadata,
) {
	if entryFound && entry.Event.ID > 0 {
		if updater, ok := o.workflowMetrics.(WorkflowMetricsMetadataUpdater); ok {
			metadata := entry.Metadata
			metadata.BlockedRecovery = &park
			if err := updater.UpdateWorkflowPhaseEventMetadata(ctx, entry.Event.ID, workflowLaneMetadataJSON(issue, metadata)); err != nil && o.logger != nil {
				o.logger.Warn("scheduler breaker recovery metadata persistence failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
			}
		}
	}
	if state == nil {
		return
	}
	blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]
	if !ok || !blockedEntryMatchesCurrent(issue, blocked.BlockedAt) {
		return
	}
	blocked.Recovery = &park
	blocked.RecoveryTarget = park.TargetState
	state.Blocked[strings.TrimSpace(issue.ID)] = blocked
}

func (o *Orchestrator) logSchedulerBreakerRecoveryDerivationFailure(issue connector.Issue, laneReason string, reason string) {
	if o == nil || o.logger == nil {
		return
	}
	o.logger.Warn(
		"scheduler breaker recovery derivation failed",
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", strings.TrimSpace(issue.Identifier),
		"lane_reason", strings.TrimSpace(laneReason),
		"reason", strings.TrimSpace(reason),
	)
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
	comment := fmt.Sprintf(
		"Blocked recovery predicate satisfied. Moved %s from %s to %s.\n\n- cause: %s\n- predicate: %s\n- cause_fingerprint: %s",
		issueLabel(issue),
		strings.TrimSpace(issue.State),
		strings.TrimSpace(targetState),
		strings.TrimSpace(park.Cause),
		strings.TrimSpace(park.Predicate),
		strings.TrimSpace(fingerprint),
	)
	if staleReworkPark(park) {
		comment += "\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
	}
	return comment
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
	if index >= 0 && !(signal != nil && signal.Invalid == nil && signal.Source == workpad.SourceStructured && signal.Status == workpad.StatusInProgress && issue.Comments[index].AuthorAuthorized) {
		for _, comment := range issue.Comments {
			prior, ok := workpad.SignalFromComment(comment.Body, comment.URL, dependencyIssueRepo(issue.Identifier))
			if ok && prior != nil && prior.Invalid == nil && prior.Source == workpad.SourceStructured && strings.TrimSpace(prior.HumanAction) != "" {
				return strings.TrimSpace(prior.HumanAction)
			}
		}
	}
	return ""
}
