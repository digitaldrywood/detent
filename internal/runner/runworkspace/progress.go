package runworkspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var ErrWorkerProcessReap = errors.New("worker process reap failed")

func WithHooks(ctx context.Context) (context.Context, func(*error)) {
	ctx, stop := workspace.WithHookProcesses(ctx)
	return ctx, func(primary *error) {
		if err := stop(); err != nil {
			if *primary != nil {
				*primary = errors.Join(*primary, err)
				return
			}
			*primary = fmt.Errorf("%w: %w", ErrWorkerProcessReap, err)
		}
	}
}

type AttemptOwner interface {
	AttemptWorkspace() workspace.AttemptWorkspace
}

func ResolveAttempt(ctx context.Context, attempt workspace.AttemptWorkspace, execution AttemptOwner, issue connector.Issue) (workspace.AttemptWorkspace, bool) {
	if bound := workspace.AttemptWorkspaceFromContext(ctx); bound != (workspace.AttemptWorkspace{}) {
		if attempt != (workspace.AttemptWorkspace{}) && attempt != bound {
			return workspace.AttemptWorkspace{}, false
		}
		attempt = bound
	}
	if execution != nil {
		bound := execution.AttemptWorkspace()
		if attempt != (workspace.AttemptWorkspace{}) && attempt != bound {
			return workspace.AttemptWorkspace{}, false
		}
		attempt = bound
	}
	org, projectID := issue.Metadata["hub_organization_id"], issue.Metadata["hub_project_id"]
	if attempt == (workspace.AttemptWorkspace{}) {
		return attempt, org == "" && projectID == "" && !strings.HasPrefix(issue.ID, "wi_")
	}
	return attempt, attempt.Key() != "" && (org == "" || org == attempt.Organization) && (projectID == "" || projectID == attempt.Project)
}
