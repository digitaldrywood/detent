package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	spendProgressMetadataKey        = "spend_since_progress_breaker"
	spendProgressReason             = "spend_since_progress_circuit_breaker"
	spendProgressHistorySize        = 200
	spendProgressCaseNoPR           = "spend_without_pr_evidence"
	spendProgressCaseStatic         = "spend_with_static_pr_evidence"
	spendProgressCaseNoArtifact     = "spend_without_artifact_evidence"
	spendProgressCaseStaticArtifact = "spend_with_static_artifact_evidence"
)

type spendProgressDecision struct {
	Enabled              bool
	TokenEnabled         bool
	USDEnabled           bool
	AcceptedStateChange  bool
	AcceptedReason       string
	Since                time.Time
	Spend                store.IssueSpendSince
	ConfiguredTokenLimit int64
	TokenLimit           int64
	ConfiguredLimitUSD   float64
	LimitUSD             float64
	BillingMode          string
	Effort               string
	DeliverableKind      string
	EvidenceChecked      []string
	PRFingerprint        *spendProgressPRFingerprint
	ArtifactFingerprint  *spendProgressArtifactFingerprint
	Case                 string
	BlockedBy            string
	Block                bool
	Warning              string
}

type spendProgressRecord struct {
	AcceptedStateChange  bool                              `json:"accepted_state_change,omitempty"`
	AcceptedReason       string                            `json:"accepted_reason,omitempty"`
	Since                string                            `json:"since,omitempty"`
	TotalTokens          int64                             `json:"total_tokens,omitempty"`
	SpendUSD             float64                           `json:"spend_usd,omitempty"`
	Sessions             int64                             `json:"sessions,omitempty"`
	FirstSessionAt       string                            `json:"first_session_at,omitempty"`
	LastSessionAt        string                            `json:"last_session_at,omitempty"`
	ConfiguredTokenLimit int64                             `json:"configured_token_limit,omitempty"`
	TokenLimit           int64                             `json:"token_limit,omitempty"`
	ConfiguredLimitUSD   float64                           `json:"configured_limit_usd,omitempty"`
	LimitUSD             float64                           `json:"limit_usd,omitempty"`
	BillingMode          string                            `json:"billing_mode,omitempty"`
	Effort               string                            `json:"effort,omitempty"`
	DeliverableKind      string                            `json:"deliverable_kind,omitempty"`
	EvidenceChecked      []string                          `json:"evidence_checked,omitempty"`
	PRFingerprint        *spendProgressPRFingerprint       `json:"pr_fingerprint,omitempty"`
	ArtifactFingerprint  *spendProgressArtifactFingerprint `json:"artifact_fingerprint,omitempty"`
	Case                 string                            `json:"case,omitempty"`
	BlockedBy            string                            `json:"blocked_by,omitempty"`
	BlockReason          string                            `json:"block_reason,omitempty"`
	Warning              string                            `json:"warning,omitempty"`
}

type spendProgressPRFingerprint struct {
	Number         int64  `json:"number,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	CIStatus       string `json:"ci_status,omitempty"`
}

type spendProgressArtifactFingerprint struct {
	ReceiptHash            string `json:"receipt_hash,omitempty"`
	StatusField            string `json:"status_field,omitempty"`
	Status                 string `json:"status,omitempty"`
	DeliverableFingerprint string `json:"deliverable_fingerprint,omitempty"`
	OutputFiles            int    `json:"output_files,omitempty"`
	OutputFingerprint      string `json:"output_fingerprint,omitempty"`
}

func (o *Orchestrator) evaluateSpendProgress(
	ctx context.Context,
	running Running,
	completedAt time.Time,
	accepted bool,
	acceptedReason string,
) spendProgressDecision {
	decision := spendProgressDecision{AcceptedStateChange: accepted, AcceptedReason: strings.TrimSpace(acceptedReason)}
	if o == nil {
		return decision
	}
	decision.BillingMode = workflowconfig.BillingModeMetered
	if o.cfg.subscriptionBilling() {
		decision.BillingMode = workflowconfig.BillingModeSubscription
	}
	decision.TokenEnabled = o.cfg.NoProgressTokenLimit > 0
	decision.USDEnabled = !o.cfg.subscriptionBilling() && o.cfg.NoProgressSpendLimitUSD > 0
	decision.Enabled = decision.TokenEnabled || decision.USDEnabled
	if !decision.Enabled {
		return decision
	}
	decision.ConfiguredTokenLimit = o.cfg.NoProgressTokenLimit
	decision.TokenLimit = decision.ConfiguredTokenLimit
	decision.ConfiguredLimitUSD = o.cfg.NoProgressSpendLimitUSD
	decision.Effort = spendProgressEffort(running)
	decision.LimitUSD = workflowconfig.EffectiveNoProgressSpendLimitUSD(decision.ConfiguredLimitUSD, decision.Effort)
	decision.DeliverableKind = o.spendProgressDeliverableKind(running)
	if decision.DeliverableKind == workflowconfig.DeliverableArtifact {
		decision.ArtifactFingerprint, decision.EvidenceChecked = o.spendProgressArtifactFingerprint(running)
	} else {
		decision.PRFingerprint = spendProgressPRFingerprintFromIssue(running.Issue)
		decision.EvidenceChecked = []string{"issue PR linkage", "hydrated PR metadata", "tracker closing references including Fixes #N"}
	}
	if accepted {
		decision.Since = completedAt
		return decision
	}
	if o.progressSpend == nil {
		decision.Warning = "progress usage store unavailable"
		o.warnSpendProgress(running.Issue, decision.Warning, nil)
		return decision
	}

	attempts, err := o.recentSpendProgressAttempts(ctx, running.Issue)
	if err != nil {
		decision.Warning = err.Error()
		o.warnSpendProgress(running.Issue, "work attempt history lookup failed", err)
		return decision
	}
	if decision.DeliverableKind == workflowconfig.DeliverableArtifact {
		previous, found := latestSpendProgressArtifactFingerprint(attempts)
		reason := spendProgressArtifactAdvance(previous, decision.ArtifactFingerprint)
		if !found {
			reason = spendProgressInitialArtifactReason(decision.ArtifactFingerprint)
		}
		if reason != "" {
			decision.AcceptedStateChange = true
			decision.AcceptedReason = reason
			decision.Since = completedAt
			return decision
		}
	} else {
		if previous, ok := latestSpendProgressPRFingerprint(attempts); ok {
			if reason := spendProgressPRAdvance(previous, decision.PRFingerprint); reason != "" {
				decision.AcceptedStateChange = true
				decision.AcceptedReason = reason
				decision.Since = completedAt
				return decision
			}
		} else if decision.PRFingerprint != nil {
			decision.AcceptedStateChange = true
			decision.AcceptedReason = "pull_request_created"
			decision.Since = completedAt
			return decision
		}
	}
	decision.Since = spendProgressBaseline(running.Issue, attempts)
	creditedAt, err := o.spendProgressCredit(ctx, running.Issue)
	if err != nil {
		decision.Warning = err.Error()
		o.warnSpendProgress(running.Issue, "progress credit lookup failed", err)
		return decision
	}
	if creditedAt.After(decision.Since) {
		decision.Since = creditedAt
	}
	if decision.Since.IsZero() {
		decision.Since = time.Unix(0, 0).UTC()
	}
	spend, err := o.progressSpend.IssueSpendSince(ctx, store.IssueSpendSinceQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(running.Issue.ID),
		Identifier: strings.TrimSpace(running.Issue.Identifier),
		Since:      decision.Since,
	})
	if err != nil {
		decision.Warning = err.Error()
		o.warnSpendProgress(running.Issue, "usage lookup failed", err)
		return decision
	}
	decision.Spend = spend
	if decision.DeliverableKind == workflowconfig.DeliverableArtifact {
		decision.Case = spendProgressCaseNoArtifact
		if spendProgressArtifactFingerprintPresent(decision.ArtifactFingerprint) {
			decision.Case = spendProgressCaseStaticArtifact
		}
	} else {
		decision.Case = spendProgressCaseNoPR
		if decision.PRFingerprint != nil {
			decision.Case = spendProgressCaseStatic
		}
	}
	if spendProgressTokenLimitReached(spend.TotalTokens, decision.TokenLimit) {
		decision.BlockedBy = "tokens"
		decision.Block = true
	} else if decision.USDEnabled && spend.CostUSD > decision.LimitUSD {
		decision.BlockedBy = "usd"
		decision.Block = true
	}
	return decision
}

func (o *Orchestrator) spendProgressCredit(ctx context.Context, issue connector.Issue) (time.Time, error) {
	credits, ok := o.progressSpend.(store.ProgressCreditStore)
	if !ok {
		return time.Time{}, nil
	}
	credit, err := credits.IssueProgressCredit(ctx, store.IssueIdentity{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
	})
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading operator progress credit: %w", err)
	}
	return credit.CreditedAt.UTC(), nil
}

func (o *Orchestrator) spendProgressEnabled() bool {
	return o != nil && (o.cfg.NoProgressTokenLimit > 0 || (!o.cfg.subscriptionBilling() && o.cfg.NoProgressSpendLimitUSD > 0))
}

func spendProgressTokenLimitReached(totalTokens int64, tokenLimit int64) bool {
	return tokenLimit > 0 && totalTokens >= tokenLimit
}

func spendProgressEffort(running Running) string {
	return strings.ToLower(strings.TrimSpace(running.RuntimeIdentity.ReasoningEffort.Value))
}

func (o *Orchestrator) spendProgressDeliverableKind(running Running) string {
	if o != nil {
		switch strings.ToLower(strings.TrimSpace(o.cfg.DeliverableKind)) {
		case workflowconfig.DeliverableArtifact:
			return workflowconfig.DeliverableArtifact
		case workflowconfig.DeliverablePullRequest:
			return workflowconfig.DeliverablePullRequest
		}
	}
	return spendProgressRunningDeliverableKind(running)
}

func spendProgressRunningDeliverableKind(running Running) string {
	values := []string{running.DeliverableKind}
	if running.Issue.Deliverable != nil {
		values = append(values, running.Issue.Deliverable.Kind)
	}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case workflowconfig.DeliverableArtifact, "artifacts", "file", "files":
			return workflowconfig.DeliverableArtifact
		case workflowconfig.DeliverablePullRequest:
			return workflowconfig.DeliverablePullRequest
		}
	}
	return workflowconfig.DeliverablePullRequest
}

func (o *Orchestrator) spendProgressArtifactFingerprint(running Running) (*spendProgressArtifactFingerprint, []string) {
	statusField := gate.Effective(o.cfg.AutoPromote.Gate).Artifact.StatusField
	status, _ := artifactStatusFieldFromIssue(running.Issue, statusField)
	receiptHash := ""
	if signal, ok := autoPromoteIssueWorkpadSignal(running.Issue); ok && signal != nil && signal.Invalid == nil &&
		signal.Source == workpad.SourceStructured && strings.TrimSpace(signal.Status) == workpad.StatusComplete {
		receiptHash = artifactCompletionReceiptHash(running.Issue.Comments)
	}
	fingerprint := &spendProgressArtifactFingerprint{
		ReceiptHash:       receiptHash,
		StatusField:       strings.TrimSpace(statusField),
		Status:            strings.TrimSpace(status),
		OutputFiles:       running.ArtifactEvidence.CurrentFiles,
		OutputFingerprint: strings.TrimSpace(running.ArtifactEvidence.CurrentFingerprint),
	}
	checked := make([]string, 0, 4)
	if running.DispatchProgress.WorkpadRead || running.Issue.Comments != nil {
		checked = append(checked, "completion receipt")
	}
	if fingerprint.StatusField != "" {
		checked = append(checked, "artifact status field "+fingerprint.StatusField)
	}
	if running.Issue.Deliverable != nil {
		encoded, err := json.Marshal(running.Issue.Deliverable)
		if err == nil {
			fingerprint.DeliverableFingerprint = workpad.ContentHash(string(encoded))
		}
		checked = append(checked, "work item deliverable metadata")
	}
	if running.ArtifactEvidence.Available {
		checked = append(checked, "files under the configured artifact output root")
	}
	return fingerprint, checked
}

func latestSpendProgressArtifactFingerprint(attempts []store.WorkAttempt) (*spendProgressArtifactFingerprint, bool) {
	for _, attempt := range attempts {
		record, ok := spendProgressRecordFromAttempt(attempt)
		if !ok || record.ArtifactFingerprint == nil {
			continue
		}
		fingerprint := *record.ArtifactFingerprint
		return &fingerprint, true
	}
	return nil, false
}

func spendProgressArtifactAdvance(previous, current *spendProgressArtifactFingerprint) string {
	if previous == nil || current == nil {
		return ""
	}
	if current.ReceiptHash != "" && current.ReceiptHash != previous.ReceiptHash {
		return "artifact_receipt_changed"
	}
	if current.Status != "" && !strings.EqualFold(strings.TrimSpace(current.Status), strings.TrimSpace(previous.Status)) {
		return "artifact_status_changed"
	}
	if current.DeliverableFingerprint != "" && current.DeliverableFingerprint != previous.DeliverableFingerprint {
		return "artifact_deliverable_changed"
	}
	if current.OutputFiles > 0 && current.OutputFingerprint != "" && current.OutputFingerprint != previous.OutputFingerprint {
		return "artifact_output_changed"
	}
	return ""
}

func spendProgressInitialArtifactReason(current *spendProgressArtifactFingerprint) string {
	if current == nil {
		return ""
	}
	switch {
	case current.ReceiptHash != "":
		return "artifact_receipt_changed"
	case current.Status != "":
		return "artifact_status_changed"
	case current.DeliverableFingerprint != "":
		return "artifact_deliverable_changed"
	case current.OutputFiles > 0 && current.OutputFingerprint != "":
		return "artifact_output_changed"
	default:
		return ""
	}
}

func spendProgressArtifactFingerprintPresent(fingerprint *spendProgressArtifactFingerprint) bool {
	return fingerprint != nil && (fingerprint.ReceiptHash != "" || fingerprint.Status != "" ||
		fingerprint.DeliverableFingerprint != "" || (fingerprint.OutputFiles > 0 && fingerprint.OutputFingerprint != ""))
}

func spendProgressPRFingerprintFromIssue(issue connector.Issue) *spendProgressPRFingerprint {
	number := workAttemptPRNumber(issue)
	if number == nil || *number <= 0 {
		return nil
	}
	fingerprint := &spendProgressPRFingerprint{Number: *number}
	if issue.PullRequest == nil {
		return fingerprint
	}
	fingerprint.HeadSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	fingerprint.MergeableState = strings.ToLower(strings.TrimSpace(issue.PullRequest.MergeableState))
	fingerprint.CIStatus = strings.ToLower(strings.TrimSpace(issue.PullRequest.CIStatus))
	return fingerprint
}

func latestSpendProgressPRFingerprint(attempts []store.WorkAttempt) (*spendProgressPRFingerprint, bool) {
	for _, attempt := range attempts {
		record, ok := spendProgressRecordFromAttempt(attempt)
		if !ok || record.PRFingerprint == nil || record.PRFingerprint.Number <= 0 {
			continue
		}
		fingerprint := *record.PRFingerprint
		return &fingerprint, true
	}
	return nil, false
}

func spendProgressPRAdvance(previous *spendProgressPRFingerprint, current *spendProgressPRFingerprint) string {
	if previous == nil || current == nil {
		return ""
	}
	if previous.Number != current.Number {
		return "pull_request_created"
	}
	if previous.HeadSHA != "" && current.HeadSHA != "" && previous.HeadSHA != current.HeadSHA {
		return "pull_request_head_changed"
	}
	if previous.MergeableState == "dirty" && current.MergeableState == "clean" {
		return "pull_request_mergeable"
	}
	if spendProgressCIFailing(previous.CIStatus) && spendProgressCIPassing(current.CIStatus) {
		return "pull_request_ci_passing"
	}
	return ""
}

func spendProgressCIFailing(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "fail", "failed", "failure", "failing":
		return true
	default:
		return false
	}
}

func spendProgressCIPassing(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pass", "passed", "passing", "success", "successful":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) recentSpendProgressAttempts(ctx context.Context, issue connector.Issue) ([]store.WorkAttempt, error) {
	if o == nil || o.workAttempts == nil {
		return nil, nil
	}
	return o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		Limit:      spendProgressHistorySize,
	})
}

func (o *Orchestrator) refreshSpendProgressIssue(ctx context.Context, issue connector.Issue) (connector.Issue, string) {
	if o.spendProgressDeliverableKind(Running{Issue: issue}) == workflowconfig.DeliverableArtifact {
		refreshed, current := o.refreshImplementCompletionIssue(ctx, issue)
		if !current {
			return refreshed, "artifact progress evidence refresh unavailable"
		}
		return refreshed, ""
	}
	refreshed := cloneIssue(issue)
	if !implementProgressLinkedPullRequest(refreshed) {
		if o == nil || o.connector == nil || strings.TrimSpace(refreshed.ID) == "" {
			return refreshed, "pull request evidence refresh unavailable"
		}
		refresher, ok := o.connector.(connector.PullRequestReferenceRefresher)
		if ok {
			linked, err := refresher.RefreshPullRequestReference(ctx, refreshed)
			if err != nil {
				return refreshed, "pull request reference refresh failed: " + err.Error()
			}
			refreshed = mergeIssueTrackerFields(refreshed, linked)
			if strings.TrimSpace(linked.PRRepository) != "" {
				refreshed.PRRepository = strings.TrimSpace(linked.PRRepository)
			}
		} else {
			issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{refreshed.ID})
			if err != nil {
				return refreshed, "pull request evidence refresh failed: " + err.Error()
			}
			for _, candidate := range issues {
				if strings.TrimSpace(candidate.ID) == strings.TrimSpace(refreshed.ID) {
					refreshed = mergeIssueTrackerFields(refreshed, candidate)
					if strings.TrimSpace(candidate.PRRepository) != "" {
						refreshed.PRRepository = strings.TrimSpace(candidate.PRRepository)
					}
					break
				}
			}
		}
	}
	if !implementProgressLinkedPullRequest(refreshed) {
		return refreshed, ""
	}
	hydrator, ok := o.connector.(connector.PullRequestHydrator)
	if !ok {
		return refreshed, "pull request hydrator unavailable"
	}
	hydrated, err := hydrator.HydratePullRequest(ctx, refreshed)
	if err != nil {
		return refreshed, "pull request hydration failed: " + err.Error()
	}
	if reason := implementProgressHydrationUnavailableReason(hydrated.PullRequest); reason != "" {
		return hydrated, reason
	}
	return hydrated, ""
}

func spendProgressBaseline(issue connector.Issue, attempts []store.WorkAttempt) time.Time {
	baseline := time.Time{}
	if issue.CreatedAt != nil {
		baseline = issue.CreatedAt.UTC()
	}
	for _, attempt := range attempts {
		if !spendProgressAttemptAccepted(attempt) || !attempt.CompletedAt.After(baseline) {
			continue
		}
		baseline = attempt.CompletedAt.UTC()
	}
	return baseline
}

func spendProgressAttemptAccepted(attempt store.WorkAttempt) bool {
	if record, ok := spendProgressRecordFromAttempt(attempt); ok && record.AcceptedStateChange {
		return true
	}
	record, ok := implementProgressRecordFromAttempt(attempt)
	if !ok {
		return false
	}
	switch strings.TrimSpace(record.Reason) {
	case "pull_request_created_or_updated", "signature_changed":
		return true
	case implementOperationalCompletion:
		return strings.TrimSpace(record.CompletionKind) == workpad.CompletionOperational
	default:
		return false
	}
}

func spendProgressRecordFromAttempt(attempt store.WorkAttempt) (spendProgressRecord, bool) {
	var root struct {
		SpendProgress spendProgressRecord `json:"spend_since_progress_breaker"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil {
		return spendProgressRecord{}, false
	}
	record := root.SpendProgress
	if !record.AcceptedStateChange && record.TokenLimit <= 0 && record.LimitUSD <= 0 && strings.TrimSpace(record.BlockReason) == "" {
		return spendProgressRecord{}, false
	}
	return record, true
}

func spendProgressMetadata(decision spendProgressDecision) map[string]any {
	if !decision.Enabled {
		return nil
	}
	record := spendProgressRecord{
		AcceptedStateChange:  decision.AcceptedStateChange,
		AcceptedReason:       decision.AcceptedReason,
		TotalTokens:          decision.Spend.TotalTokens,
		SpendUSD:             decision.Spend.CostUSD,
		Sessions:             decision.Spend.Sessions,
		ConfiguredTokenLimit: decision.ConfiguredTokenLimit,
		TokenLimit:           decision.TokenLimit,
		ConfiguredLimitUSD:   decision.ConfiguredLimitUSD,
		LimitUSD:             decision.LimitUSD,
		BillingMode:          decision.BillingMode,
		Effort:               decision.Effort,
		DeliverableKind:      decision.DeliverableKind,
		EvidenceChecked:      slices.Clone(decision.EvidenceChecked),
		PRFingerprint:        decision.PRFingerprint,
		ArtifactFingerprint:  decision.ArtifactFingerprint,
		Case:                 decision.Case,
		BlockedBy:            decision.BlockedBy,
		Warning:              strings.TrimSpace(decision.Warning),
	}
	if !decision.Since.IsZero() {
		record.Since = decision.Since.UTC().Format(time.RFC3339Nano)
	}
	if !decision.Spend.FirstSessionAt.IsZero() {
		record.FirstSessionAt = decision.Spend.FirstSessionAt.UTC().Format(time.RFC3339Nano)
	}
	if !decision.Spend.LastSessionAt.IsZero() {
		record.LastSessionAt = decision.Spend.LastSessionAt.UTC().Format(time.RFC3339Nano)
	}
	if decision.Block {
		record.BlockReason = spendProgressReason
	}
	return map[string]any{spendProgressMetadataKey: record}
}

func mergeWorkAttemptMetadata(groups ...map[string]any) map[string]any {
	var merged map[string]any
	for _, group := range groups {
		for key, value := range group {
			if merged == nil {
				merged = map[string]any{}
			}
			merged[key] = value
		}
	}
	return merged
}

func implementAcceptedStateChange(running Running, decision implementCompletionProgressDecision) (bool, string) {
	if spendProgressRunningDeliverableKind(running) == workflowconfig.DeliverableArtifact {
		statusField := strings.TrimSpace(running.ArtifactStatusField)
		if statusField == "" {
			statusField = gate.DefaultArtifactStatusField
		}
		currentStatus, _ := artifactStatusFieldFromIssue(decision.Issue, statusField)
		if currentStatus != "" &&
			!strings.EqualFold(strings.TrimSpace(running.DispatchArtifactStatus), strings.TrimSpace(currentStatus)) {
			return true, "artifact_status_changed"
		}
		if running.ArtifactEvidence.Available &&
			running.ArtifactEvidence.CurrentFiles > 0 &&
			strings.TrimSpace(running.ArtifactEvidence.InitialFingerprint) != "" &&
			strings.TrimSpace(running.ArtifactEvidence.CurrentFingerprint) != "" &&
			running.ArtifactEvidence.InitialFingerprint != running.ArtifactEvidence.CurrentFingerprint {
			return true, "artifact_output_changed"
		}
		for _, progressKind := range decision.ProgressKinds {
			switch progressKind {
			case "artifact_receipt", "audit_artifact":
				return true, "artifact_receipt_changed"
			case "workspace_diff":
				return true, "artifact_workspace_changed"
			}
		}
	}
	switch strings.TrimSpace(decision.Reason) {
	case "pull_request_created_or_updated", "signature_changed", implementMergedCompletionReason:
		return true, decision.Reason
	case implementOperationalCompletion:
		if strings.TrimSpace(decision.CompletionKind) == workpad.CompletionOperational {
			return true, decision.Reason
		}
		return false, ""
	default:
		return false, ""
	}
}

func (o *Orchestrator) blockSpendProgress(
	ctx context.Context,
	state *State,
	running Running,
	decision spendProgressDecision,
	blockedAt time.Time,
) bool {
	issue := cloneIssue(running.Issue)
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return false
	}
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		RunModeImplement,
		spendProgressReason,
		blockedRecoveryPredicateFingerprintChange,
		autoPromoteReworkState,
		DiffStats{},
	)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issueID, issue, blockedStatusState, blockedAt, spendProgressReason, metadata, laneMutationRevokeWorker); err != nil {
		if o.logger != nil {
			o.logger.Warn("spend progress circuit breaker state transition failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
		}
		return false
	}
	issue.State = blockedStatusState
	stageUpdatedAt := blockedAt.UTC()
	issue.StageUpdatedAt = &stageUpdatedAt
	if o.connector != nil {
		if err := o.connector.CreateComment(ctx, issueID, spendProgressComment(issue, decision)); err != nil && o.logger != nil {
			o.logger.Warn("spend progress circuit breaker comment failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("spend progress circuit breaker claim release failed", "issue_id", issueID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	if state.PriorAttempts == nil {
		state.PriorAttempts = map[string]runpkg.PriorAttempt{}
	}
	state.PriorAttempts[issueID] = spendProgressRetryHandoff(decision)
	if completed, ok := state.Completed[issueID]; ok {
		completed.Issue = issue
		state.Completed[issueID] = completed
	}
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:          issue,
		Reason:         spendProgressReason,
		RecoveryReason: spendProgressRecoveryReason(decision),
		RecoveryTarget: "Rework",
		BlockedAt:      blockedAt,
		Source:         BlockedSourceProjectStatus,
		Recovery:       metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      blockedAt,
		Event:   "spend_since_progress_circuit_breaker_tripped",
		Message: fmt.Sprintf("parked %s after %s: %s", issueLabel(issue), spendProgressUsageSummary(decision), spendProgressCaseSummary(decision.Case)),
	})
	if o.logger != nil {
		telemetry.LogLifecycleMessage(o.logger, slog.LevelError, telemetry.LifecycleSafetyControl, "spend_since_progress_circuit_breaker_tripped", "no-progress circuit breaker tripped", o.runningLifecycleCorrelation(issue, running),
			"blocked_by", decision.BlockedBy,
			"total_tokens", decision.Spend.TotalTokens,
			"token_limit", decision.TokenLimit,
			"spend_usd", decision.Spend.CostUSD,
			"limit_usd", decision.LimitUSD,
			"billing_mode", decision.BillingMode,
			"sessions", decision.Spend.Sessions,
			"since", decision.Since,
			"case", decision.Case,
			"deliverable_kind", decision.DeliverableKind,
			"evidence_checked", decision.EvidenceChecked,
			"effort", decision.Effort,
		)
	}
	return true
}

func spendProgressComment(issue connector.Issue, decision spendProgressDecision) string {
	var b strings.Builder
	b.WriteString("Routed this issue to Blocked because ")
	b.WriteString(spendProgressCaseSummary(decision.Case))
	b.WriteString(".")
	b.WriteString("\n\n- reason: ")
	b.WriteString(spendProgressReason)
	b.WriteString("\n- case: ")
	b.WriteString(decision.Case)
	b.WriteString("\n- issue: ")
	b.WriteString(issueLabel(issue))
	if decision.DeliverableKind == workflowconfig.DeliverableArtifact {
		b.WriteString("\n- artifact_evidence_checked: ")
		b.WriteString(strings.Join(decision.EvidenceChecked, ", "))
	} else {
		b.WriteString("\n- pr_evidence_checked: issue PR linkage, hydrated PR metadata, and tracker closing references including Fixes #N")
	}
	b.WriteString("\n- billing_mode: ")
	b.WriteString(decision.BillingMode)
	b.WriteString("\n- blocked_by: ")
	b.WriteString(decision.BlockedBy)
	b.WriteString("\n- tokens_since_last_accepted_progress: ")
	b.WriteString(strconv.FormatInt(decision.Spend.TotalTokens, 10))
	if decision.TokenLimit > 0 {
		b.WriteString("\n- no_progress_token_limit: ")
		b.WriteString(strconv.FormatInt(decision.TokenLimit, 10))
	}
	if decision.ConfiguredLimitUSD > 0 {
		b.WriteString("\n- notional_spend_since_last_accepted_progress: ")
		b.WriteString(budget.FormatUSD(decision.Spend.CostUSD))
		b.WriteString("\n- usd_breaker: ")
		if decision.USDEnabled {
			b.WriteString("active")
		} else {
			b.WriteString("inert")
		}
		b.WriteString("\n- no_progress_spend_limit_usd: ")
		b.WriteString(budget.FormatUSD(decision.LimitUSD))
	}
	if decision.USDEnabled && decision.Effort != "" {
		b.WriteString("\n- effective_effort: ")
		b.WriteString(decision.Effort)
		b.WriteString("\n- configured_base_limit_usd: ")
		b.WriteString(budget.FormatUSD(decision.ConfiguredLimitUSD))
	}
	b.WriteString(fmt.Sprintf("\n- sessions: %d", decision.Spend.Sessions))
	if decision.PRFingerprint != nil {
		b.WriteString("\n- pr_number: ")
		b.WriteString(strconv.FormatInt(decision.PRFingerprint.Number, 10))
		if decision.PRFingerprint.HeadSHA != "" {
			b.WriteString("\n- pr_head_sha: ")
			b.WriteString(decision.PRFingerprint.HeadSHA)
		}
		if decision.PRFingerprint.MergeableState != "" {
			b.WriteString("\n- pr_mergeable_state: ")
			b.WriteString(decision.PRFingerprint.MergeableState)
		}
		if decision.PRFingerprint.CIStatus != "" {
			b.WriteString("\n- pr_ci_status: ")
			b.WriteString(decision.PRFingerprint.CIStatus)
		}
	}
	if decision.ArtifactFingerprint != nil {
		if decision.ArtifactFingerprint.StatusField != "" {
			b.WriteString("\n- artifact_status_field: ")
			b.WriteString(decision.ArtifactFingerprint.StatusField)
			b.WriteString("\n- artifact_status: ")
			b.WriteString(decision.ArtifactFingerprint.Status)
		}
		if decision.ArtifactFingerprint.ReceiptHash != "" {
			b.WriteString("\n- artifact_receipt_hash: ")
			b.WriteString(decision.ArtifactFingerprint.ReceiptHash)
		}
		if decision.ArtifactFingerprint.OutputFingerprint != "" {
			b.WriteString("\n- artifact_output_files: ")
			b.WriteString(strconv.Itoa(decision.ArtifactFingerprint.OutputFiles))
			b.WriteString("\n- artifact_output_fingerprint: ")
			b.WriteString(decision.ArtifactFingerprint.OutputFingerprint)
		}
	}
	if !decision.Since.IsZero() {
		b.WriteString("\n- last_accepted_progress_at: ")
		b.WriteString(decision.Since.UTC().Format(time.RFC3339))
	}
	if !decision.Spend.FirstSessionAt.IsZero() && !decision.Spend.LastSessionAt.IsZero() {
		b.WriteString("\n- observed_session_span: ")
		b.WriteString(decision.Spend.LastSessionAt.Sub(decision.Spend.FirstSessionAt).Round(time.Second).String())
	}
	switch decision.Case {
	case spendProgressCaseStatic:
		b.WriteString("\n\nThe linked PR fingerprint stayed static during this spend window. Check merge-train capacity, Merging serialization, and dispatch priority before narrowing the task; this pattern can indicate throughput starvation rather than missing implementation work.")
	case spendProgressCaseStaticArtifact:
		b.WriteString("\n\nThe contracted artifact evidence stayed static during this spend window. Verify that the next attempt changes its completion receipt, artifact status, deliverable metadata, or configured output files before retrying.")
	case spendProgressCaseNoArtifact:
		b.WriteString("\n\nNo contracted artifact evidence was recorded during this spend window. Verify that the workflow can write a completion receipt, artifact status, deliverable metadata, or configured output file before retrying.")
	default:
		b.WriteString("\n\nShrink the task before retrying: split or narrow the scope so the next session can produce a concrete accepted change or linked PR evidence.")
	}
	b.WriteString("\n\nOn the next dispatch, the agent's first tool action must update the Workpad to explain which accepted progress signal was missing and what is concretely different before any other tool use.")
	return b.String()
}

func spendProgressCaseSummary(progressCase string) string {
	switch progressCase {
	case spendProgressCaseStatic:
		return "resource consumption continued while a linked PR existed but could not merge"
	case spendProgressCaseStaticArtifact:
		return "resource consumption continued while artifact evidence stayed static"
	case spendProgressCaseNoArtifact:
		return "resource consumption continued without any artifact evidence"
	default:
		return "resource consumption continued without any PR evidence"
	}
}

func spendProgressBlockMessage(decision spendProgressDecision) string {
	if decision.BlockedBy == "tokens" {
		return fmt.Sprintf(
			"consumed %d tokens since the last accepted state change; configured limit %d",
			decision.Spend.TotalTokens,
			decision.TokenLimit,
		)
	}
	return fmt.Sprintf(
		"computed %s notional USD since the last accepted state change; configured notional limit %s",
		budget.FormatUSD(decision.Spend.CostUSD),
		budget.FormatUSD(decision.LimitUSD),
	)
}

func spendProgressUsageSummary(decision spendProgressDecision) string {
	if decision.BlockedBy == "tokens" {
		return fmt.Sprintf("%d tokens", decision.Spend.TotalTokens)
	}
	return budget.FormatUSD(decision.Spend.CostUSD) + " notional USD"
}

func spendProgressRecoveryReason(decision spendProgressDecision) string {
	switch decision.Case {
	case spendProgressCaseStatic:
		return "inspect merge-train capacity, Merging serialization, and dispatch priority before moving the issue back to Rework"
	case spendProgressCaseStaticArtifact:
		return "identify why the completion receipt, artifact status, deliverable metadata, and output files stayed static before moving the item back to Rework"
	case spendProgressCaseNoArtifact:
		return "restore a writable completion receipt, artifact status, deliverable metadata, or output path before moving the item back to Todo or Rework"
	default:
		return "narrow or split the task, then identify why no linked PR evidence was produced before moving the issue back to Todo or Rework"
	}
}

func spendProgressRetryHandoff(decision spendProgressDecision) runpkg.PriorAttempt {
	missingSignal := "lane transition, pull request creation, or a recognized PR fingerprint advancement"
	switch decision.Case {
	case spendProgressCaseStatic:
		missingSignal = "new PR head commit, dirty-to-clean mergeability, failing-to-passing CI, or merge-train capacity that lets the linked PR advance"
	case spendProgressCaseStaticArtifact, spendProgressCaseNoArtifact:
		missingSignal = "changed completion receipt, artifact status, deliverable metadata, or configured output files"
	}
	prior := runpkg.PriorAttempt{
		Source:               spendProgressReason,
		Reason:               spendProgressCaseSummary(decision.Case),
		ExplainBeforeRetry:   true,
		MissingSignal:        missingSignal,
		ObservedTokens:       decision.Spend.TotalTokens,
		NoProgressTokenLimit: decision.TokenLimit,
	}
	if decision.BillingMode == workflowconfig.BillingModeMetered {
		prior.ObservedSpendUSD = decision.Spend.CostUSD
		prior.NoProgressSpendLimitUSD = decision.LimitUSD
	}
	return prior
}

func (o *Orchestrator) spendProgressPriorAttempt(ctx context.Context, issue connector.Issue) (runpkg.PriorAttempt, bool) {
	if o == nil || o.workAttempts == nil ||
		(o.cfg.NoProgressTokenLimit <= 0 && (o.cfg.subscriptionBilling() || o.cfg.NoProgressSpendLimitUSD <= 0)) {
		return runpkg.PriorAttempt{}, false
	}
	record, ok := o.latestSpendProgressRecord(ctx, issue)
	if !ok || strings.TrimSpace(record.BlockReason) != spendProgressReason {
		return runpkg.PriorAttempt{}, false
	}
	return spendProgressRetryHandoff(spendProgressDecision{
		Spend:                store.IssueSpendSince{CostUSD: record.SpendUSD, TotalTokens: record.TotalTokens, Sessions: record.Sessions},
		ConfiguredTokenLimit: record.ConfiguredTokenLimit,
		TokenLimit:           record.TokenLimit,
		ConfiguredLimitUSD:   record.ConfiguredLimitUSD,
		LimitUSD:             record.LimitUSD,
		BillingMode:          record.BillingMode,
		Effort:               record.Effort,
		DeliverableKind:      record.DeliverableKind,
		EvidenceChecked:      slices.Clone(record.EvidenceChecked),
		PRFingerprint:        record.PRFingerprint,
		ArtifactFingerprint:  record.ArtifactFingerprint,
		Case:                 record.Case,
		BlockedBy:            record.BlockedBy,
	}), true
}

func (o *Orchestrator) latestSpendProgressRecord(ctx context.Context, issue connector.Issue) (spendProgressRecord, bool) {
	if o == nil || o.workAttempts == nil {
		return spendProgressRecord{}, false
	}
	attempts, err := o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		Limit:      1,
	})
	if err != nil || len(attempts) == 0 {
		return spendProgressRecord{}, false
	}
	record, ok := spendProgressRecordFromAttempt(attempts[0])
	return record, ok
}

func (o *Orchestrator) warnSpendProgress(issue connector.Issue, message string, err error) {
	if o == nil || o.logger == nil {
		return
	}
	attrs := []any{"issue_id", issue.ID, "identifier", issue.Identifier, "reason", strings.TrimSpace(message)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	o.logger.Warn("progress usage breaker failed open", attrs...)
}

func priorAttemptPresent(prior runpkg.PriorAttempt) bool {
	return strings.TrimSpace(prior.Source) != "" || strings.TrimSpace(prior.Reason) != "" || prior.ExplainBeforeRetry || prior.Validator.Submitted
}
