package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func (o *Orchestrator) applyArtifactGateCompletionFields(ctx context.Context, issue connector.Issue, startedAt time.Time) connector.Issue {
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
	if !workpadCurrent || !artifactGateCompletionWorkpadUpdatedSince(issue.Comments, startedAt) {
		if o.logger != nil {
			o.logger.WarnContext(ctx, "artifact gate Workpad field update skipped",
				"issue_id", issue.ID,
				"identifier", issue.Identifier,
				"reason", "Workpad status block was not updated during the completed attempt",
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

func artifactGateCompletionWorkpadUpdatedSince(comments []connector.IssueComment, startedAt time.Time) bool {
	if startedAt.IsZero() {
		return false
	}
	for index := len(comments) - 1; index >= 0; index-- {
		comment := comments[index]
		if !autoPromoteIsWorkpadComment(comment.Body) {
			continue
		}
		updatedAt := comment.UpdatedAt
		if updatedAt == nil {
			updatedAt = comment.CreatedAt
		}
		return updatedAt != nil && !updatedAt.Before(startedAt)
	}
	return false
}

func (o *Orchestrator) warnUnchangedArtifactGateCompletion(ctx context.Context, dispatched connector.Issue, completed connector.Issue) {
	if o == nil || o.logger == nil {
		return
	}
	gateConfig := gate.Effective(o.cfg.AutoPromote.Gate)
	if gateConfig.Kind != gate.KindArtifact {
		return
	}
	statusField := gateConfig.Artifact.StatusField
	dispatchStatus, _ := artifactStatusFieldFromIssue(dispatched, statusField)
	completionStatus, _ := artifactStatusFieldFromIssue(completed, statusField)
	if !strings.EqualFold(strings.TrimSpace(dispatchStatus), strings.TrimSpace(completionStatus)) {
		return
	}
	o.logger.WarnContext(ctx, "artifact gate completion left status field unchanged",
		"issue_id", completed.ID,
		"identifier", completed.Identifier,
		"status_field", statusField,
		"dispatch_status", dispatchStatus,
		"completion_status", completionStatus,
	)
}
