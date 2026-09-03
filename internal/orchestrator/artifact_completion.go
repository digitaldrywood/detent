package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	artifactGateConvergenceLimit               = 3
	artifactGateConvergenceMetadataKey         = "artifact_gate_convergence"
	artifactGateConvergenceReason              = "artifact_gate_convergence_breaker"
	artifactGateConvergenceBlockedReasonPrefix = "artifact gate convergence breaker: "
)

type artifactGateConvergenceRecord struct {
	StatusField          string `json:"status_field"`
	DispatchStatus       string `json:"dispatch_status"`
	CompletionStatus     string `json:"completion_status"`
	Unchanged            bool   `json:"unchanged"`
	ConsecutiveUnchanged int    `json:"consecutive_unchanged"`
	Limit                int    `json:"limit"`
	Tripped              bool   `json:"tripped,omitempty"`
	Warning              string `json:"warning,omitempty"`
}

func (o *Orchestrator) applyArtifactGateCompletionFields(
	ctx context.Context,
	issue connector.Issue,
	dispatchWorkpadHash string,
	dispatchWorkpadRead bool,
) connector.Issue {
	issue = cloneIssue(issue)
	if o == nil || o.connector == nil {
		return issue
	}
	gateConfig := gate.Effective(o.cfg.AutoPromote.Gate)
	if gateConfig.Kind != gate.KindArtifact {
		return issue
	}

	var workpadCurrent bool
	issue, workpadCurrent = o.refreshImplementCompletionIssue(ctx, issue)
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	if !ok || signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured || len(signal.Fields) == 0 {
		return issue
	}
	completionWorkpadHash := artifactGateWorkpadStatusHash(issue.Comments)
	if !workpadCurrent || !dispatchWorkpadRead || completionWorkpadHash == "" || completionWorkpadHash == dispatchWorkpadHash {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate Workpad field update skipped",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"reason", "Workpad status block content was not updated during the completed attempt",
			)
		}
		return issue
	}
	if strings.TrimSpace(signal.Status) != workpad.StatusComplete {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate Workpad field update rejected",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"error", fmt.Errorf("status %q must be complete when setting artifact gate fields", signal.Status),
			)
		}
		return issue
	}
	fieldName, value, err := artifactGateCompletionFieldUpdate(signal.Fields, gateConfig.Artifact)
	if err != nil {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate Workpad field update rejected",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"error", err,
			)
		}
		return issue
	}
	if err := o.connector.SetField(ctx, issue.ID, fieldName, value); err != nil {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate Workpad field update failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"status_field", fieldName,
				"status", value,
				"error", err,
			)
		}
		return issue
	}
	if issue.Fields == nil {
		issue.Fields = map[string]string{}
	}
	for existing := range issue.Fields {
		if strings.EqualFold(strings.TrimSpace(existing), fieldName) {
			delete(issue.Fields, existing)
		}
	}
	issue.Fields[fieldName] = value
	return issue
}

func artifactGateCompletionFieldUpdate(fields map[string]string, cfg gate.ArtifactConfig) (string, string, error) {
	cfg = gate.Effective(gate.Config{Kind: gate.KindArtifact, Artifact: cfg}).Artifact
	fieldName := strings.TrimSpace(cfg.StatusField)
	if len(fields) != 1 {
		return "", "", fmt.Errorf("fields must contain only the configured artifact status field %q", fieldName)
	}
	for name, rawValue := range fields {
		if !strings.EqualFold(strings.TrimSpace(name), fieldName) {
			return "", "", fmt.Errorf("field %q is not the configured artifact status field %q", name, fieldName)
		}
		value := strings.TrimSpace(rawValue)
		if value == "" {
			return "", "", fmt.Errorf("configured artifact status field %q is blank", fieldName)
		}
		if !artifactGateCompletionStatusAllowed(value, cfg) {
			return "", "", fmt.Errorf("status %q is not configured in pass_statuses, wait_statuses, or rework_statuses", value)
		}
		return fieldName, value, nil
	}
	return "", "", fmt.Errorf("configured artifact status field %q is missing", fieldName)
}

func artifactGateCompletionStatusAllowed(status string, cfg gate.ArtifactConfig) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	for _, values := range [][]string{cfg.PassStatuses, cfg.WaitStatuses, cfg.ReworkStatuses} {
		for _, value := range values {
			if status == strings.ToLower(strings.TrimSpace(value)) {
				return true
			}
		}
	}
	return false
}

func artifactGateWorkpadStatusHash(comments []connector.IssueComment) string {
	for index := len(comments) - 1; index >= 0; index-- {
		comment := comments[index]
		if !autoPromoteIsWorkpadComment(comment.Body) {
			continue
		}
		content, ok := workpad.LastStatusBlock(comment.Body)
		if !ok {
			return ""
		}
		return workpad.ContentHash(content)
	}
	return ""
}

func artifactCompletionReceiptHash(comments []connector.IssueComment) string {
	for index := len(comments) - 1; index >= 0; index-- {
		body := strings.TrimSpace(comments[index].Body)
		if !autoPromoteIsWorkpadComment(body) {
			continue
		}
		return workpad.ContentHash(body)
	}
	return ""
}

func (o *Orchestrator) artifactGateDispatchWorkpadSnapshot(ctx context.Context, issue connector.Issue) (string, []connector.IssueComment, bool) {
	if o == nil || o.connector == nil || gate.Effective(o.cfg.AutoPromote.Gate).Kind != gate.KindArtifact {
		return "", nil, false
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate dispatch Workpad snapshot failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"reason", "issue comment reader unavailable",
			)
		}
		return "", nil, false
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate dispatch Workpad snapshot failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"error", err,
			)
		}
		return "", nil, false
	}
	return artifactGateWorkpadStatusHash(comments), comments, true
}

func (o *Orchestrator) evaluateArtifactGateConvergence(
	ctx context.Context,
	dispatched connector.Issue,
	completed connector.Issue,
	running Running,
) artifactGateConvergenceRecord {
	if o == nil {
		return artifactGateConvergenceRecord{}
	}
	gateConfig := gate.Effective(o.cfg.AutoPromote.Gate)
	if gateConfig.Kind != gate.KindArtifact {
		return artifactGateConvergenceRecord{}
	}
	statusField := strings.TrimSpace(gateConfig.Artifact.StatusField)
	dispatchStatus, _ := artifactStatusFieldFromIssue(dispatched, statusField)
	completionStatus, _ := artifactStatusFieldFromIssue(completed, statusField)
	record := artifactGateConvergenceRecord{
		StatusField:      statusField,
		DispatchStatus:   strings.TrimSpace(dispatchStatus),
		CompletionStatus: strings.TrimSpace(completionStatus),
		Limit:            artifactGateConvergenceLimit,
	}
	record.Unchanged = strings.EqualFold(record.DispatchStatus, record.CompletionStatus)
	if !record.Unchanged {
		return record
	}

	record.ConsecutiveUnchanged = 1
	attempts, err := o.recentArtifactGateConvergenceAttempts(ctx, completed, running)
	if err != nil {
		record.Warning = err.Error()
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate convergence history lookup failed",
				"issue_id", completed.ID,
				"identifier", completed.Identifier,
				"status_field", statusField,
				"error", err,
			)
		}
	} else {
		record.ConsecutiveUnchanged += consecutiveArtifactGateConvergenceAttempts(attempts, record)
	}
	record.Tripped = record.ConsecutiveUnchanged >= record.Limit
	if o.logger != nil {
		o.logger.WarnContext(ctx, "artifact gate completion left status field unchanged",
			"issue_id", completed.ID,
			"identifier", completed.Identifier,
			"status_field", statusField,
			"dispatch_status", dispatchStatus,
			"completion_status", completionStatus,
			"consecutive_unchanged", record.ConsecutiveUnchanged,
			"limit", record.Limit,
			"tripped", record.Tripped,
		)
	}
	return record
}

func (o *Orchestrator) recentArtifactGateConvergenceAttempts(
	ctx context.Context,
	issue connector.Issue,
	running Running,
) ([]store.WorkAttempt, error) {
	if o == nil || o.workAttempts == nil {
		return nil, nil
	}
	return o.workAttempts.ListRecentTerminalWorkAttempts(ctx, store.WorkAttemptHistoryQuery{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		IssueID:    strings.TrimSpace(issue.ID),
		Identifier: strings.TrimSpace(issue.Identifier),
		IssueURL:   strings.TrimSpace(issue.URL),
		WorkerType: workAttemptWorkerType(issue, running.Mode),
		Limit:      artifactGateConvergenceLimit + 1,
	})
}

func consecutiveArtifactGateConvergenceAttempts(attempts []store.WorkAttempt, current artifactGateConvergenceRecord) int {
	count := 0
	for _, attempt := range attempts {
		record, ok := artifactGateConvergenceRecordFromAttempt(attempt)
		if !ok ||
			(attempt.TerminalState != store.WorkAttemptTerminalSuccess && attempt.TerminalState != store.WorkAttemptTerminalNoProgress) ||
			!record.Unchanged ||
			!strings.EqualFold(strings.TrimSpace(record.StatusField), strings.TrimSpace(current.StatusField)) ||
			!strings.EqualFold(strings.TrimSpace(record.DispatchStatus), strings.TrimSpace(current.DispatchStatus)) ||
			!strings.EqualFold(strings.TrimSpace(record.CompletionStatus), strings.TrimSpace(current.CompletionStatus)) {
			return count
		}
		count++
	}
	return count
}

func artifactGateConvergenceRecordFromAttempt(attempt store.WorkAttempt) (artifactGateConvergenceRecord, bool) {
	var root struct {
		Record artifactGateConvergenceRecord `json:"artifact_gate_convergence"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(attempt.WorkerMetadataJSON)), &root); err != nil {
		return artifactGateConvergenceRecord{}, false
	}
	if strings.TrimSpace(root.Record.StatusField) == "" || root.Record.Limit <= 0 {
		return artifactGateConvergenceRecord{}, false
	}
	return root.Record, true
}

func artifactGateConvergenceMetadata(record artifactGateConvergenceRecord) map[string]any {
	if strings.TrimSpace(record.StatusField) == "" {
		return nil
	}
	return map[string]any{artifactGateConvergenceMetadataKey: record}
}

func (o *Orchestrator) parkArtifactGateConvergence(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	attempt int,
	completedAt time.Time,
	record artifactGateConvergenceRecord,
) {
	if state == nil {
		return
	}
	issue = cloneIssue(issue)
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		RunModeImplement,
		artifactGateConvergenceReason,
		blockedRecoveryPredicateFingerprintChange,
		autoPromoteReworkState,
		DiffStats{},
	)
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issue.ID, issue, blockedStatusState, completedAt, artifactGateConvergenceReason, metadata, laneMutationRevokeWorker); err != nil {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate convergence breaker state transition failed",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"target_state", blockedStatusState,
				"error", err,
			)
		}
	} else {
		issue.State = blockedStatusState
	}
	if o.connector != nil {
		body := artifactGateConvergenceComment(issue, attempt, record)
		if err := o.connector.CreateComment(ctx, issue.ID, body); err != nil && o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate convergence breaker comment failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
		}
	}
	if err := o.abandonClaim(ctx, issue.ID); err != nil && o.logger != nil {
		o.logger.WarnContext(ctx, "artifact gate convergence breaker claim release failed", "issue_id", issue.ID, "identifier", issue.Identifier, "error", err)
	}
	delete(state.Claimed, issue.ID)
	delete(state.Completed, issue.ID)
	delete(state.Retry, issue.ID)
	delete(state.BudgetRefusals, issue.ID)
	delete(state.PriorAttempts, issue.ID)
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issue.ID] = Blocked{
		Issue:          issue,
		Reason:         artifactGateConvergenceBlockedReason(record),
		RecoveryReason: "review the artifact and update the configured gate status before moving the item out of Blocked",
		RecoveryTarget: autoPromoteReworkState,
		BlockedAt:      completedAt,
		Source:         BlockedSourceProjectStatus,
		Recovery:       metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      completedAt,
		Event:   artifactGateConvergenceReason,
		Message: "parked " + issueLabel(issue) + " after repeated successful attempts left the artifact gate status unchanged",
	})
	if o.logger != nil {
		o.logger.WarnContext(ctx, "artifact gate convergence breaker tripped",
			"event", artifactGateConvergenceReason,
			"issue_id", issue.ID,
			"identifier", issue.Identifier,
			"attempt", attempt,
			"status_field", record.StatusField,
			"status", record.CompletionStatus,
			"consecutive_unchanged", record.ConsecutiveUnchanged,
			"limit", record.Limit,
			"target_state", blockedStatusState,
		)
	}
}

func artifactGateConvergenceBlockedReason(record artifactGateConvergenceRecord) string {
	return artifactGateConvergenceBlockedReasonPrefix + fmt.Sprintf(
		"%s remained %s after %d consecutive successful attempts",
		strings.TrimSpace(record.StatusField),
		strconv.Quote(strings.TrimSpace(record.CompletionStatus)),
		record.ConsecutiveUnchanged,
	)
}

func artifactGateConvergenceComment(issue connector.Issue, attempt int, record artifactGateConvergenceRecord) string {
	var b strings.Builder
	b.WriteString("Detent stopped redispatching this item after ")
	b.WriteString(strconv.Itoa(record.ConsecutiveUnchanged))
	b.WriteString(" consecutive successful attempts left the configured artifact gate status unchanged.")
	b.WriteString("\n\nIssue parked in `")
	b.WriteString(strings.TrimSpace(issue.State))
	b.WriteString("`.")
	b.WriteString("\n\n- issue: ")
	b.WriteString(issueLabel(issue))
	b.WriteString("\n- latest_attempt: ")
	b.WriteString(strconv.Itoa(attempt))
	b.WriteString("\n- status_field: `")
	b.WriteString(strings.TrimSpace(record.StatusField))
	b.WriteString("`")
	b.WriteString("\n- unchanged_status: `")
	b.WriteString(strings.TrimSpace(record.CompletionStatus))
	b.WriteString("`")
	b.WriteString("\n\nReview the artifact and set the configured field to the appropriate pass or wait status before moving the item out of Blocked. If further rework is required, correct the worker instructions before moving it back to Rework; another unchanged success will remain breaker-protected.")
	return b.String()
}
