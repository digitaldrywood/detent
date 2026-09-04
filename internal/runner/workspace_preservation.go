package runner

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var ErrWorkspacePreservationUnavailable = errors.New("workspace backend does not support retention")

type WorkspacePreserver interface {
	PreserveWorkspace(context.Context, connector.Issue) (workspace.Preservation, error)
}

func (r *Runner) PreserveWorkspace(ctx context.Context, issue connector.Issue) (workspace.Preservation, error) {
	preserver, ok := r.workspace.(workspace.IssuePreserver)
	if !ok {
		return workspace.Preservation{}, ErrWorkspacePreservationUnavailable
	}
	return preserver.PreserveIssue(ctx, workspaceIssue(r.projectID, issue))
}
