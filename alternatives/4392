package dispatchplan

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func PullRequestHydrationBlocksDispatch(issue connector.Issue, hydrationBlocksProgress bool) bool {
	pullRequest := issue.PullRequest
	if !hydrationBlocksProgress {
		return false
	}
	if normalizeState(issue.State) != "todo" {
		return true
	}
	return pullRequest.Number > 0 ||
		strings.TrimSpace(pullRequest.URL) != "" ||
		strings.TrimSpace(pullRequest.BranchName) != "" ||
		normalizeState(pullRequest.State) != ""
}

func ArtifactGateWaitStatusBlocksDispatch(issue connector.Issue, cfg gate.Config, status string) bool {
	cfg = gate.Effective(cfg)
	if cfg.Kind != gate.KindArtifact {
		return false
	}
	status = NormalizeArtifactGateStatus(status)
	if status == "" {
		return false
	}
	for _, waitStatus := range cfg.Artifact.WaitStatuses {
		if status == NormalizeArtifactGateStatus(waitStatus) {
			return ArtifactGateWaitStatusIsCurrent(issue, cfg.Artifact.StatusField)
		}
	}
	return false
}

func ArtifactGateWaitStatusIsCurrent(issue connector.Issue, statusField string) bool {
	for field, updatedAt := range issue.FieldUpdatedAt {
		if strings.EqualFold(strings.TrimSpace(field), strings.TrimSpace(statusField)) &&
			issue.StageUpdatedAt != nil && !issue.StageUpdatedAt.IsZero() && !updatedAt.IsZero() {
			return updatedAt.After(*issue.StageUpdatedAt)
		}
	}
	if issue.StageUpdatedAt == nil || issue.StageUpdatedAt.IsZero() || issue.UpdatedAt == nil || issue.UpdatedAt.IsZero() {
		return true
	}
	return !issue.StageUpdatedAt.Equal(*issue.UpdatedAt)
}

func NormalizeArtifactGateStatus(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
