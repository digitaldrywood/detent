package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerPreservesWorkspaceThroughBackend(t *testing.T) {
	t.Parallel()

	for _, supported := range []bool{true, false} {
		name := "unsupported"
		if supported {
			name = "supported"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			probe := &workspacePreservationProbe{}
			runner := &Runner{projectID: "detent", workspace: probe}
			if !supported {
				runner.workspace = &fakeWorkspaceBackend{}
			}
			issue := connector.Issue{ID: "2138", Identifier: "digitaldrywood/detent#2138"}
			result, err := runner.PreserveWorkspace(t.Context(), issue)
			if !supported {
				if !errors.Is(err, ErrWorkspacePreservationUnavailable) || result.Preserved {
					t.Fatalf("unsupported preservation = %#v, error = %v", result, err)
				}
				return
			}
			if err != nil || !result.Preserved || result.Path != "/retained/2138" || probe.issue.ID != issue.ID || probe.issue.ProjectID != "detent" {
				t.Fatalf("preservation = %#v, issue = %#v, error = %v", result, probe.issue, err)
			}
		})
	}
}

type workspacePreservationProbe struct {
	fakeWorkspaceBackend
	issue workspace.Issue
}

func (p *workspacePreservationProbe) PreserveIssue(_ context.Context, issue workspace.Issue) (workspace.Preservation, error) {
	p.issue = issue
	return workspace.Preservation{Path: "/retained/2138", Preserved: true}, nil
}
