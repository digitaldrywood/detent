package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
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
	index := currentWorkpadCommentIndex(comments)
	if index < 0 {
		return ""
	}
	content, ok := workpad.LastStatusBlock(comments[index].Body)
	if !ok {
		return ""
	}
	return workpad.ContentHash(content)
}

func artifactCompletionReceiptHash(comments []connector.IssueComment) string {
	index := currentWorkpadCommentIndex(comments)
	if index < 0 {
		return ""
	}
	return workpad.ContentHash(strings.TrimSpace(comments[index].Body))
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
	if !record.Unchanged || o.implementCompletionAccepted(running, completed, running.DiffStats) {
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

func (o *Orchestrator) parkArtifactGateConvergence(ctx context.Context, state *State, event runpkg.Completion, running Running) {
	o.retryMachineCompletion(ctx, state, event, running, nextAttempt(running.Attempt), artifactGateConvergenceReason)
}
