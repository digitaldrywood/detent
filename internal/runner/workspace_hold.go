package runner

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) InspectWorkspaceBranchHold(ctx context.Context, issue connector.Issue) (WorkspaceBranchHold, error) {
	provider, ok := r.workspace.(workspace.BranchHoldProvider)
	if !ok {
		return WorkspaceBranchHold{}, nil
	}
	hold, held, err := provider.BranchHold(ctx, workspaceIssue(r.projectID, issue))
	if err != nil {
		return WorkspaceBranchHold{}, err
	}
	return WorkspaceBranchHold{
		Branch:       strings.TrimSpace(hold.Branch),
		WorktreePath: strings.TrimSpace(hold.Path),
		PRNumber:     ciTriggerPullRequestNumber(issue),
		Held:         held,
	}, nil
}

func workspaceBranchHeldError(err error, issue connector.Issue) (*WorkspaceBranchHeldError, bool) {
	var heldErr *workspace.BranchHeldError
	if !errors.As(err, &heldErr) || heldErr == nil {
		return nil, false
	}
	return &WorkspaceBranchHeldError{
		Branch:       strings.TrimSpace(heldErr.Branch),
		WorktreePath: strings.TrimSpace(heldErr.Path),
		PRNumber:     ciTriggerPullRequestNumber(issue),
	}, true
}
