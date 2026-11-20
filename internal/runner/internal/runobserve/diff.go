package runobserve

import (
	"context"
	"log/slog"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func SampleDiff(ctx context.Context, backend workspace.Backend, info workspace.Info, issue workspace.Issue, logger *slog.Logger) (workspace.DiffStat, error) {
	stat, err := backend.DiffStat(ctx, info, issue)
	if err != nil {
		if workspace.IsMissingWorkspaceError(err) {
			logger.Info(
				"workspace live diff stat skipped",
				slog.String("issue_id", issue.ID),
				slog.String("issue_identifier", issue.Identifier),
				slog.String("workspace_path", info.Path),
				slog.String("phase", "live"),
				slog.String("error", err.Error()),
			)
			return workspace.DiffStat{}, err
		}
		logger.Warn(
			"workspace live diff stat failed",
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("workspace_path", info.Path),
			slog.String("phase", "live"),
			slog.String("error", err.Error()),
		)
		return workspace.DiffStat{}, err
	}

	return stat, nil
}

type DiffStats struct {
	FilesChanged                   int
	AddedLines                     int
	RemovedLines                   int
	UnpushedCommits                int
	UnpushedCommitRefs             []string
	TrackedPaths                   []string
	UntrackedPaths                 []string
	CommitsNotInPullRequest        []string
	PullRequestComparisonAvailable bool
	RecoveryStateExpected          bool
	RecoveryStateAvailable         bool
	CommitsAhead                   int
	RemoteBranchExists             bool
	DeliveryStateChecked           bool
	TreeSHA                        string
	HeadSHA                        string
	Fingerprint                    string
	Status                         string
}

func DiffStatsFromWorkspace(stat workspace.DiffStat) DiffStats {
	status := "clean"
	if stat.Files != 0 || stat.Added != 0 || stat.Removed != 0 {
		status = "changed"
	}
	return DiffStats{
		FilesChanged: stat.Files,
		AddedLines:   stat.Added,
		RemovedLines: stat.Removed,
		Fingerprint:  strings.TrimSpace(stat.Fingerprint),
		HeadSHA:      stat.HeadSHA,
		TreeSHA:      stat.TreeSHA,
		Status:       status,
	}
}

func (diffStats *DiffStats) ApplyRecoveryState(state *workspace.RecoveryState) {
	if diffStats == nil || state == nil {
		return
	}
	diffStats.UnpushedCommits = state.UnpushedCommits
	diffStats.UnpushedCommitRefs = append([]string(nil), state.UnpushedCommitRefs...)
	diffStats.TrackedPaths = append([]string(nil), state.TrackedPaths...)
	diffStats.UntrackedPaths = append([]string(nil), state.UntrackedPaths...)
	diffStats.CommitsNotInPullRequest = append([]string(nil), state.CommitsNotInPullRequest...)
	diffStats.PullRequestComparisonAvailable = state.PullRequestComparisonAvailable
	diffStats.RecoveryStateAvailable = true
	diffStats.HeadSHA = strings.TrimSpace(state.HeadSHA)
}

func (diffStats DiffStats) Empty() bool {
	return diffStats.FilesChanged == 0 &&
		diffStats.AddedLines == 0 &&
		diffStats.RemovedLines == 0 &&
		diffStats.UnpushedCommits == 0 &&
		len(diffStats.UnpushedCommitRefs) == 0 &&
		len(diffStats.TrackedPaths) == 0 &&
		len(diffStats.UntrackedPaths) == 0 &&
		len(diffStats.CommitsNotInPullRequest) == 0 &&
		!diffStats.PullRequestComparisonAvailable &&
		!diffStats.RecoveryStateExpected &&
		!diffStats.RecoveryStateAvailable &&
		diffStats.CommitsAhead == 0 &&
		!diffStats.RemoteBranchExists &&
		!diffStats.DeliveryStateChecked &&
		strings.TrimSpace(diffStats.HeadSHA) == "" &&
		strings.TrimSpace(diffStats.Fingerprint) == "" &&
		strings.TrimSpace(diffStats.Status) == ""
}
