package runner

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var ErrWorkspacePreservationUnavailable = errors.New("workspace backend does not support retention")

type WorkspacePreserver interface {
	PreserveWorkspace(context.Context, connector.Issue) (workspace.Preservation, error)
}

func (r *Runner) PreserveWorkspace(ctx context.Context, issue connector.Issue) (workspace.Preservation, error) {
	target, known := (RunRequest{Issue: issue}).workspaceTarget(ctx, r.projectID)
	if !known {
		return workspace.Preservation{}, ErrWorkspacePreservationUnavailable
	}
	return preserveWorkspace(ctx, r.workspaceSnapshot(), target)
}

func preserveWorkspace(ctx context.Context, backend workspace.Backend, issue workspace.Issue) (workspace.Preservation, error) {
	preserver, ok := backend.(workspace.IssuePreserver)
	if !ok {
		return workspace.Preservation{}, ErrWorkspacePreservationUnavailable
	}
	return preserver.PreserveIssue(ctx, issue)
}

func (req RunRequest) workspaceTarget(ctx context.Context, project string) (workspace.Issue, bool) {
	issue := workspaceIssue(project, req.Issue)
	attempt := req.AttemptWorkspace
	if bound := workspace.AttemptWorkspaceFromContext(ctx); bound != (workspace.AttemptWorkspace{}) {
		if attempt != (workspace.AttemptWorkspace{}) && attempt != bound {
			return issue, false
		}
		attempt = bound
	}
	if execution, ok := req.Execution.(interface {
		AttemptWorkspace() workspace.AttemptWorkspace
	}); ok {
		bound := execution.AttemptWorkspace()
		if attempt != (workspace.AttemptWorkspace{}) && attempt != bound {
			return issue, false
		}
		attempt = bound
	}
	issue.AttemptWorkspace = attempt
	org, projectID := req.Issue.Metadata["hub_organization_id"], req.Issue.Metadata["hub_project_id"]
	if attempt == (workspace.AttemptWorkspace{}) {
		return issue, org == "" && projectID == "" && !strings.HasPrefix(req.Issue.ID, "wi_")
	}
	return issue, attempt.Key() != "" && (org == "" || org == attempt.Organization) && (projectID == "" || projectID == attempt.Project)
}
