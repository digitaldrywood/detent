package runner

import (
	"context"
	"errors"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/runner/recoveryfingerprint"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) BlockedRecoverySnapshot(ctx context.Context, req RunRequest) BlockedRecoverySnapshot {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	snapshot := BlockedRecoverySnapshot{
		ConfigFingerprint: blockedRecoveryHashYAML(workflow.Config),
		Health:            "ready",
	}
	role := runtime.effectiveRunRole(runRole(req.Mode, req.Issue))
	_, _, backendConfig, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), role)
	if err != nil {
		snapshot.Health = "route_unavailable:" + recoveryfingerprint.Hash(err.Error())
	} else {
		snapshot.ToolingFingerprint, err = recoveryfingerprint.Tool(backendConfig.Command)
		if err != nil {
			snapshot.Health = "tool_unavailable:" + recoveryfingerprint.Hash(err.Error())
		}
	}
	reader, ok := r.workspaceSnapshot().(workspace.IssueRecoveryStateProvider)
	if !ok {
		snapshot.WorkspaceStatus = "unavailable"
		return snapshot
	}
	issue, known := req.workspaceTarget(ctx, r.projectID)
	if !known {
		snapshot.WorkspaceStatus = "unavailable"
		return snapshot
	}
	recovery, err := reader.IssueRecoveryState(ctx, issue)
	if errors.Is(err, workspace.ErrMissingWorkspace) {
		snapshot.WorkspaceStatus = "missing"
		return snapshot
	}
	if err != nil {
		snapshot.WorkspaceStatus = "error:" + recoveryfingerprint.Hash(err.Error())
		return snapshot
	}
	snapshot.WorkspacePresent = true
	snapshot.BaseFingerprint = strings.TrimSpace(recovery.BaseFingerprint)
	snapshot.HeadSHA = strings.TrimSpace(recovery.HeadSHA)
	snapshot.WorkspaceFiles = recovery.DiffStat.Files
	snapshot.UnpushedCommits = recovery.UnpushedCommits
	snapshot.UnpushedCommitRefs = append([]string(nil), recovery.UnpushedCommitRefs...)
	snapshot.TrackedPaths = append([]string(nil), recovery.TrackedPaths...)
	snapshot.UntrackedPaths = append([]string(nil), recovery.UntrackedPaths...)
	snapshot.CommitsNotInPullRequest = append([]string(nil), recovery.CommitsNotInPullRequest...)
	snapshot.PullRequestComparisonAvailable = recovery.PullRequestComparisonAvailable
	snapshot.WorkspaceFingerprint = strings.TrimSpace(recovery.WorkspaceFingerprint)
	if snapshot.WorkspaceFingerprint == "" {
		snapshot.WorkspaceFingerprint = strings.TrimSpace(recovery.DiffStat.Fingerprint)
	}
	snapshot.WorkspaceStatus = "present"
	return snapshot
}

func blockedRecoveryHashYAML(value any) string {
	data, err := yaml.Marshal(value)
	if err != nil {
		return "marshal_error:" + recoveryfingerprint.Hash(err.Error())
	}
	return recoveryfingerprint.Hash(string(data))
}
