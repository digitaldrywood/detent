package runner

import (
	"context"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) BlockedRecoveryWorkspace(ctx context.Context, req RunRequest) (present, changed bool) {
	reader, ok := r.workspaceSnapshot().(workspace.IssueRecoveryStateProvider)
	if !ok {
		return false, false
	}
	issue, known := req.workspaceTarget(ctx, r.projectID)
	if !known {
		return false, false
	}
	recovery, err := reader.IssueRecoveryState(ctx, issue)
	if err != nil {
		return false, false
	}
	return true, recovery.DiffStat.Files > 0 || recovery.UnpushedCommits > 0
}

func (r *Runner) BlockedRecoveryConfigFingerprint() string {
	workflow, _, _, _ := r.runtimeSnapshot()
	data, err := yaml.Marshal(workflow.Config)
	if err != nil {
		return "marshal_error:" + policy.Digest([]byte(err.Error()))
	}
	return policy.Digest(data)
}
