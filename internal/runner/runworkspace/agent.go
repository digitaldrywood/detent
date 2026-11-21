package runworkspace

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const (
	RunModeImplement = "implement"
	RunModePlan      = "plan"
	RunModeMerge     = "merge"
	RunModeRoutine   = "routine"
	RunModeTriage    = "triage"

	RoleCode    = "code"
	RolePlan    = "plan"
	RoleRework  = "rework"
	RoleMerge   = "merge"
	RoleRoutine = "routine"
)

func SelectorContext(ctx selector.Context, workflow config.Workflow) selector.Context {
	if strings.TrimSpace(ctx.Persona) == "" {
		ctx.Persona = workflow.Config.Tracker.Assignee
	}
	return ctx
}

func NormalizeRunMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", RunModeImplement:
		return RunModeImplement
	case RunModePlan:
		return RunModePlan
	case RunModeMerge:
		return RunModeMerge
	case RunModeRoutine:
		return RunModeRoutine
	case RunModeTriage:
		return RunModeTriage
	default:
		return RunModeImplement
	}
}

func AgentTurnDeliverable(cfg config.Config, issue connector.Issue, mode string) (string, string) {
	if cfg.Tracker.Kind == config.TrackerHubNative {
		return "", ""
	}
	switch NormalizeRunMode(mode) {
	case RunModeImplement, RunModeMerge:
	default:
		return "", ""
	}
	repository := strings.TrimSpace(issue.PRRepository)
	if repository == "" {
		repository = strings.TrimSpace(cfg.Tracker.Repository)
	}
	return strings.TrimSpace(cfg.Deliverable.Kind), repository
}

func RunRole(mode string, issue connector.Issue) string {
	switch NormalizeRunMode(mode) {
	case RunModePlan:
		return RolePlan
	case RunModeMerge:
		if issue.NativeLandingHeadSHA != "" {
			return RoleMerge
		}
	case RunModeRoutine:
		return RoleRoutine
	}
	switch strings.ToLower(strings.TrimSpace(issue.State)) {
	case RoleRework:
		return RoleRework
	case RoleMerge, "merging":
		return RoleMerge
	default:
		return RoleCode
	}
}

func ExtraWritableRootsForWorkspace(ctx context.Context, workspaceKind string, workspacePath string, hostCommits bool, logger *slog.Logger) []string {
	if workspaceKind == config.WorkspaceFilesystem {
		return nil
	}
	roots, err := workspace.GitMetadataWritableRoots(ctx, workspacePath)
	if err != nil {
		if logger != nil {
			logger.Warn("workspace git metadata writable roots unavailable", slog.String("workspace_path", workspacePath), slog.Any("error", err))
		}
		return nil
	}
	if !hostCommits {
		return roots
	}
	directories := roots[:0]
	for _, root := range roots {
		info, err := os.Stat(root)
		if err == nil && info.IsDir() {
			directories = append(directories, root)
		}
	}
	return directories
}

func Issue(projectID string, issue connector.Issue) workspace.Issue {
	landedHeadSHA := strings.TrimSpace(issue.Metadata["hub_landed_head_sha"])
	if issue.PullRequest != nil {
		switch strings.ToUpper(strings.TrimSpace(issue.PullRequest.State)) {
		case "MERGED":
			issue.PullRequest = nil
		case "CLOSED":
			issue.PullRequest = nil
		}
	}
	baseRef := ""
	progressBaseRef := ""
	if issue.PullRequest != nil {
		baseRef = strings.TrimSpace(issue.PullRequest.BaseSHA)
		progressBaseRef = strings.TrimSpace(issue.PullRequest.BaseRef)
	}
	return workspace.Issue{
		CleanupInUse:            issue.Metadata["workspace_cleanup_running"] == "true",
		ProjectID:               projectID,
		ID:                      issue.ID,
		Identifier:              issue.Identifier,
		Terminal:                issue.Closed,
		LandedHeadSHA:           landedHeadSHA,
		CleanupDeliveredHeadSHA: issue.CleanupDeliveredHeadSHA,
		BranchName:              issue.BranchName,
		BaseRef:                 baseRef,
		ProgressBaseRef:         progressBaseRef,
		PullRequestHeadSHA:      pullRequestHeadSHA(issue.PullRequest),
		PullRequestRepository:   strings.TrimSpace(issue.PRRepository),
		PullRequestNumber:       workspacePullRequestNumber(issue.PullRequest),
		PullRequestBranch:       pullRequestBranch(issue.PullRequest),
	}
}

func pullRequestBranch(pr *connector.PullRequest) string {
	if pr == nil {
		return ""
	}
	return strings.TrimSpace(pr.BranchName)
}

func workspacePullRequestNumber(pr *connector.PullRequest) int {
	if pr == nil {
		return 0
	}
	return pr.Number
}

func pullRequestHeadSHA(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	return strings.TrimSpace(pullRequest.HeadSHA)
}
