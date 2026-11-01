package cli

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func emptyProcessSource(context.Context, string) ([]int, error) {
	return nil, nil
}

func testWorkspaceSupport() workspacerunner.Support {
	return workspacerunner.Support{Terminal: true, Isolation: workspacesession.IsolationUser}
}
