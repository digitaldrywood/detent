package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *runnerEventSession) ProjectPolicyRevision(ctx context.Context, project tracker.ProjectID, policyID string) (policystore.Revision, error) {
	if project == f.command.Project && f.policy.err == nil && f.policy.value.PolicyID == policyID {
		return projectPolicyCurrentRecord(f.policy.value), nil
	}
	return f.Writer.ProjectPolicyRevision(ctx, project, policyID)
}
