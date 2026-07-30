package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
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
)

type blockedCauseSignals struct {
	ConfigFingerprint    string            `json:"config_fingerprint,omitempty"`
	ToolingFingerprint   string            `json:"tooling_fingerprint,omitempty"`
	BaseFingerprint      string            `json:"base_fingerprint,omitempty"`
	WorkspaceFingerprint string            `json:"workspace_fingerprint,omitempty"`
	WorkspaceStatus      string            `json:"workspace_status,omitempty"`
	WorkspacePresent     bool              `json:"workspace_present,omitempty"`
	WorkspaceFiles       int               `json:"workspace_files,omitempty"`
	UnpushedCommits      int               `json:"unpushed_commits,omitempty"`
	Health               string            `json:"health,omitempty"`
	Description          string            `json:"description,omitempty"`
	ModelOverride        string            `json:"model_override,omitempty"`
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
			Resumable:        blockedCauseResumable(issue, signals),
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
		WorkspaceFingerprint: strings.TrimSpace(snapshot.WorkspaceFingerprint),
		WorkspaceStatus:      strings.TrimSpace(snapshot.WorkspaceStatus),
		WorkspacePresent:     snapshot.WorkspacePresent,
		WorkspaceFiles:       snapshot.WorkspaceFiles,
		UnpushedCommits:      snapshot.UnpushedCommits,
		Health:               strings.TrimSpace(snapshot.Health),
		Description:          strings.TrimSpace(issue.Description),
		ModelOverride:        strings.TrimSpace(issue.ModelOverride),
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
	if reason := blockedCauseHoldReason(issue, state); reason != "" {
		o.logBlockedRecoveryDecision(issue, "hold", reason, nil, "")
		return false
	}
	if entry, ok := o.latestWorkflowLaneEntry(ctx, issue); ok &&
		normalizeState(entry.Event.PhaseName) == normalizeState(blockedStatusState) &&
		blockedEntryMatchesCurrent(issue, entry.Event.StartedAt) &&
		strings.EqualFold(strings.TrimSpace(entry.Event.Reason), string(store.WorkAttemptTerminalOperatorStopped)) {
		o.logBlockedRecoveryDecision(issue, "hold", "operator_stop", nil, "")
		return false
	}
	withDependencies := o.issueWithDependencyRefs(issue)
	if len(withDependencies.BlockedBy) > 0 {
		o.logBlockedRecoveryDecision(issue, "defer", "dependency_recovery", nil, "")
		return false
	}
	if park, ok := o.latestReworkBreakerPark(ctx, issue); ok {
		o.logBlockedRecoveryDecision(issue, "defer", "rework_breaker_recovery", nil, park.Signature)
		return false
	}
	park, ok := o.currentBlockedRecoveryPark(ctx, state, issue)
	if !ok {
		recoveryCfg := normalizeBlockedRecoveryConfig(o.cfg.BlockedRecovery)
		reasonCode, reasonFound := o.latestWorkflowLaneReason(ctx, issue, issue.State)
		if recoveryCfg.Enabled && reasonFound && blockedRecoveryReasonAllowed(recoveryCfg, reasonCode) {
			o.logBlockedRecoveryDecision(issue, "defer", "pr_maintenance_recovery", nil, "")
		} else {
			o.logBlockedRecoveryDecision(issue, "hold", "no_recovery_predicate", nil, "")
		}
		return false
	}
	if park.Predicate == blockedRecoveryPredicateManaged {
		o.logBlockedRecoveryDecision(issue, "hold", "managed_recovery", &park, park.CauseFingerprint)
		return false
	}
	signals := o.blockedCauseSignals(ctx, issue, park.RunMode, park.TargetState, DiffStats{})
	currentFingerprint := blockedCauseFingerprint(signals)
	if park.Predicate == blockedRecoveryPredicateFingerprintChange && currentFingerprint == park.CauseFingerprint {
		o.logBlockedRecoveryDecision(issue, "hold", "cause_unchanged", &park, currentFingerprint)
		return false
	}
	if park.Predicate != blockedRecoveryPredicateFingerprintChange &&
		park.Predicate != blockedRecoveryPredicateOncePerFingerprint {
		o.logBlockedRecoveryDecision(issue, "hold", "no_recovery_predicate", &park, currentFingerprint)
		return false
	}
	signature := blockedCauseRecoverySignature(park.Cause, currentFingerprint)
	if _, consumed := o.workflowTimelineActionSignature(ctx, issue, workflowActionCauseBlockedRecovery, signature); consumed {
		o.logBlockedRecoveryDecision(issue, "hold", "fingerprint_already_consumed", &park, currentFingerprint)
		return false
	}
	targetState := blockedCauseTargetState(issue, signals, park.TargetState)
	metadata := workflowLaneMetadataWithActionSignature(workflowLaneMetadata{}, workflowActionCauseBlockedRecovery, signature)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, targetState, now, "cause_blocked_recovery", metadata); err != nil {
		o.logBlockedRecoveryDecision(issue, "hold", "transition_failed", &park, currentFingerprint)
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

func blockedCauseHoldReason(issue connector.Issue, state *State) string {
	if issue.WorkpadSignal != nil {
		if issue.WorkpadSignal.Invalid != nil {
			return "invalid_workpad_signal"
		}
		if strings.TrimSpace(issue.WorkpadSignal.HumanAction) != "" {
			return "human_action"
		}
		if len(issue.WorkpadSignal.Blockers) > 0 {
			return "workpad_blocker"
		}
	}
	for _, label := range issue.Labels {
		if normalizeLabel(label) == "requires-human-review" {
			return "human_action"
		}
	}
	if state != nil {
		if blocked, ok := state.Blocked[strings.TrimSpace(issue.ID)]; ok && blocked.Source == BlockedSourceOperatorStop {
			return "operator_stop"
		}
	}
	return ""
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

func (o *Orchestrator) logBlockedRecoveryDecision(
	issue connector.Issue,
	action string,
	reason string,
	park *workflowLaneBlockedRecoveryMetadata,
	fingerprint string,
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
	o.logger.Info("blocked recovery decision", attrs...)
}
