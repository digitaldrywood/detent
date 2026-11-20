package runner

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
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
	issue := runworkspace.Issue(project, req.Issue)
	var owner runworkspace.AttemptOwner
	if execution, ok := req.Execution.(runworkspace.AttemptOwner); ok {
		owner = execution
	}
	attempt, known := runworkspace.ResolveAttempt(ctx, req.AttemptWorkspace, owner, req.Issue)
	issue.AttemptWorkspace = attempt
	return issue, known
}
