package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gopkg.in/yaml.v3"

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
		snapshot.Health = "route_unavailable:" + blockedRecoveryHash(err.Error())
	} else {
		snapshot.ToolingFingerprint, err = blockedRecoveryToolFingerprint(backendConfig.Command)
		if err != nil {
			snapshot.Health = "tool_unavailable:" + blockedRecoveryHash(err.Error())
		}
	}
	reader, ok := r.workspace.(workspace.IssueRecoveryStateProvider)
	if !ok {
		snapshot.WorkspaceStatus = "unavailable"
		return snapshot
	}
	recovery, err := reader.IssueRecoveryState(ctx, workspaceIssue(r.projectID, req.Issue))
	if errors.Is(err, workspace.ErrMissingWorkspace) {
		snapshot.WorkspaceStatus = "missing"
		return snapshot
	}
	if err != nil {
		snapshot.WorkspaceStatus = "error:" + blockedRecoveryHash(err.Error())
		return snapshot
	}
	snapshot.WorkspacePresent = true
	snapshot.BaseFingerprint = strings.TrimSpace(recovery.BaseFingerprint)
	snapshot.HeadSHA = strings.TrimSpace(recovery.HeadSHA)
	snapshot.WorkspaceFiles = recovery.DiffStat.Files
	snapshot.UnpushedCommits = recovery.UnpushedCommits
	snapshot.UnpushedCommitRefs = append([]string(nil), recovery.UnpushedCommitRefs...)
	snapshot.TrackedPaths = append([]string(nil), recovery.TrackedPaths...)
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
		return "marshal_error:" + blockedRecoveryHash(err.Error())
	}
	return blockedRecoveryHash(string(data))
}

func blockedRecoveryHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func blockedRecoveryToolFingerprint(command string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return "", errors.New("agent backend command is empty")
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return blockedRecoveryHash(command + ":" + err.Error()), err
	}
	info, err := os.Stat(path)
	if err != nil {
		return blockedRecoveryHash(path + ":" + err.Error()), err
	}
	return blockedRecoveryHash(fmt.Sprintf(
		"command=%s;path=%s;size=%d;mode=%s;modified=%d",
		command,
		path,
		info.Size(),
		info.Mode(),
		info.ModTime().UnixNano(),
	)), nil
}
