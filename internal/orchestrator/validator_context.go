package orchestrator

import (
	"context"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (o *Orchestrator) refreshValidatorContext(ctx context.Context, issue connector.Issue) (connector.Issue, bool) {
	if o.connector == nil {
		return issue, true
	}
	if reader, ok := o.connector.(connector.ValidationIssueReader); ok {
		refreshed, err := reader.FetchValidationIssue(ctx, issue)
		if err != nil || refreshed.ID != issue.ID {
			if o.logger != nil {
				o.logger.Warn("refresh validator task failed", "issue_id", issue.ID, "error", err)
			}
			return issue, false
		}
		return refreshed, true
	}
	// Native/local trackers already expose authoritative reads by identity.
	// Preserve hydrated forge provenance while replacing task inputs.
	issues, err := o.connector.FetchIssueStatesByIDs(ctx, []string{issue.ID})
	if err != nil {
		return issue, false
	}
	for _, current := range issues {
		if current.ID != issue.ID {
			continue
		}
		issue.Title, issue.Description = current.Title, current.Description
		issue.Comments = current.Comments
		issue.WorkpadSignal = current.WorkpadSignal
		if reader, ok := o.connector.(connector.IssueCommentReader); ok {
			comments, err := reader.FetchIssueComments(ctx, issue)
			if err != nil {
				return issue, false
			}
			issue.Comments = comments
		}
		return issue, true
	}
	return issue, false
}
