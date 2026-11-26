package workspace

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitdiff"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/rebase"
	"github.com/digitaldrywood/detent/internal/workspace/validationcmd"
)

type DiffStat = gitdiff.DiffStat
type Diff = gitdiff.Diff
type FileDiff = gitdiff.FileDiff
type FileDiffs = gitdiff.FileDiffs

const (
	FileDiffAdded    = gitdiff.FileDiffAdded
	FileDiffModified = gitdiff.FileDiffModified
	FileDiffDeleted  = gitdiff.FileDiffDeleted
	FileDiffRenamed  = gitdiff.FileDiffRenamed
)

func GitDiffStat(ctx context.Context, path string) (DiffStat, error) {
	return gitdiff.GitDiffStat(ctx, path)
}
func GitDiff(ctx context.Context, path string, maxBytes int) (Diff, error) {
	return gitdiff.GitDiff(ctx, path, maxBytes)
}
func GitDiffFrom(ctx context.Context, path, base string, maxBytes int) (Diff, error) {
	return gitdiff.GitDiffFrom(ctx, path, base, maxBytes)
}
func GitFileDiffs(ctx context.Context, path, base string, maxBytes int) (FileDiffs, error) {
	return gitdiff.GitFileDiffs(ctx, path, base, maxBytes)
}
func GitCommitFileDiffs(ctx context.Context, path, base, head string, maxBytes int) (FileDiffs, error) {
	return gitdiff.GitCommitFileDiffs(ctx, path, base, head, maxBytes)
}
func ParseDiffStat(output string) (DiffStat, error)       { return gitdiff.ParseDiffStat(output) }
func AttemptBase(ctx context.Context, path string) string { return gitdiff.AttemptBase(ctx, path) }

type DiffProvider interface {
	Diff(context.Context, Info, Issue, int) (Diff, error)
}

// HeadProvider reports the commit actually checked out for a review.
type HeadProvider interface {
	Head(context.Context, Info, Issue) (string, error)
}

// ReviewTreeVerifier checks that a review worktree contains only committed content.
type ReviewTreeVerifier interface {
	VerifyReviewTree(context.Context, Info, Issue) error
}

// ReviewHeadSeeder aligns an existing review worktree with the requested PR commit.
type ReviewHeadSeeder interface {
	SeedReviewHead(context.Context, Info, Issue) error
}

func (l *LocalGit) SeedReviewHead(ctx context.Context, info Info, issue Issue) error {
	if strings.TrimSpace(issue.PullRequestHeadSHA) == "" {
		return nil
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	remoteHead, err := fetchReviewHead(ctx, normalized.Path, normalized.Branch, issue)
	if err != nil {
		return err
	}
	localHead, err := l.Head(ctx, normalized, issue)
	if err != nil {
		return err
	}
	if err := l.VerifyReviewTree(ctx, normalized, issue); err != nil {
		return err
	}
	if strings.TrimSpace(localHead) == remoteHead {
		return nil
	}
	_, err = runGitAt(ctx, normalized.Path, "reset", "--hard", remoteHead)
	return err
}

func fetchReviewHead(ctx context.Context, path, fallbackBranch string, issue Issue) (string, error) {
	if issue.Source != nil {
		if issue.Source.Version.HeadSHA != strings.TrimSpace(issue.PullRequestHeadSHA) {
			return "", refuse(LandRefusalHeadMoved, "retained review source differs from the requested head")
		}
		head, err := runGitAt(ctx, path, "rev-parse", "--verify", issue.Source.Version.HeadSHA+"^{commit}")
		return strings.TrimSpace(head), err
	}
	branch := strings.TrimSpace(issue.PullRequestBranch)
	if branch == "" {
		branch = strings.TrimSpace(fallbackBranch)
	}
	if branch == "" && issue.PullRequestNumber == 0 {
		return "", errors.New("review branch is empty")
	}
	remoteRef := "refs/remotes/origin/" + branch
	sourceRefs := []string{"refs/heads/" + branch}
	if issue.PullRequestNumber > 0 {
		remoteRef = fmt.Sprintf("refs/remotes/origin/pull/%d/head", issue.PullRequestNumber)
		sourceRefs = []string{fmt.Sprintf("refs/pull/%d/head", issue.PullRequestNumber)}
		if branch != "" {
			sourceRefs = append(sourceRefs, "refs/heads/"+branch)
		}
	}
	remote := "origin"
	if issue.PullRequestNumber > 0 && strings.TrimSpace(issue.PullRequestRepository) != "" {
		repository := strings.TrimSpace(issue.PullRequestRepository)
		if name, _, ok := githubLandingRepository(repository); ok {
			repository = strings.TrimSuffix(name, ".git")
		}
		parts := strings.Split(repository, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repository, " \\:@") {
			return "", fmt.Errorf("invalid pull request repository %q", repository)
		}
		remote = "https://github.com/" + repository + ".git"
	}
	// GitHub's synthetic pull ref can lag its published branch. Both sources
	// belong to the authorized repository and must match the exact reviewed SHA.
	var sourceErr error
	for _, sourceRef := range sourceRefs {
		if err := ctx.Err(); err != nil {
			return "", errors.Join(sourceErr, err)
		}
		if _, err := runGitAt(ctx, path, "fetch", "--no-write-fetch-head", remote, "+"+sourceRef+":"+remoteRef); err != nil {
			sourceErr = errors.Join(sourceErr, fmt.Errorf("fetch review head: %w", err))
			continue
		}
		remoteHead, err := runGitAt(ctx, path, "rev-parse", "--verify", remoteRef)
		if err != nil {
			return "", err
		}
		remoteHead = strings.TrimSpace(remoteHead)
		if remoteHead == strings.TrimSpace(issue.PullRequestHeadSHA) {
			return remoteHead, nil
		}
		sourceErr = errors.Join(sourceErr, refuse(LandRefusalHeadMoved, fmt.Sprintf("review branch head mismatch: fetched %s, expected %s", remoteHead, strings.TrimSpace(issue.PullRequestHeadSHA))))
	}
	return "", sourceErr
}

func (l *LocalGit) VerifyReviewTree(ctx context.Context, info Info, issue Issue) error {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	return validationcmd.VerifyCleanReview(ctx, normalized.Path)
}

func (l *LocalGit) Head(ctx context.Context, info Info, issue Issue) (string, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	head, err := runGitAt(ctx, normalized.Path, "rev-parse", "--verify", "HEAD")
	return strings.TrimSpace(head), err
}

func (l *LocalGit) DiffStat(ctx context.Context, info Info, issue Issue) (DiffStat, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DiffStat{}, err
	}
	return GitDiffStat(ctx, normalized.Path)
}

type LiveDiffStatProvider interface {
	LiveDiffStat(context.Context, Info, Issue) (DiffStat, error)
}

func (l *LocalGit) LiveDiffStat(ctx context.Context, info Info, issue Issue) (DiffStat, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DiffStat{}, err
	}
	observation, err := gitdiff.Observe(ctx, normalized.Path, false, gitexec.WithDiffInspection)
	return observation.Stat, err
}

func (l *LocalGit) RecoveryState(ctx context.Context, info Info, issue Issue) (RecoveryState, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return RecoveryState{}, err
	}
	base, excludedRemoteRefs, extraPublished, verifiedRemote, err := l.localProgressBaseWithSource(ctx, normalized.Path, issue)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("resolve recovery base: %w", err)
	}
	return l.recoveryStateWithBase(ctx, normalized, issue, base, excludedRemoteRefs, extraPublished, verifiedRemote)
}

func (l *LocalGit) recoveryStateWithBase(ctx context.Context, normalized Info, issue Issue, base string, excludedRemoteRefs, extraPublished []string, verifiedRemote bool) (RecoveryState, error) {
	observation, err := gitdiff.Observe(ctx, normalized.Path, true, nil)
	if err != nil {
		return RecoveryState{}, err
	}
	unpushedCommits, unpushedCommitRefs, err := gitdiff.UnpushedCommits(ctx, normalized.Path, base, excludedRemoteRefs, extraPublished, verifiedRemote)
	if err != nil {
		return RecoveryState{}, err
	}
	stat := observation.Stat
	head := stat.HeadSHA
	commitsNotInPullRequest, comparisonAvailable, err := gitdiff.CommitsNotInPullRequest(
		ctx,
		normalized.Path,
		head,
		issue.PullRequestHeadSHA,
		base,
	)
	if err != nil {
		return RecoveryState{}, err
	}
	rebasePaused, err := rebase.InProgress(ctx, normalized.Path)
	if err != nil {
		return RecoveryState{}, err
	}
	return RecoveryState{
		DiffStat:                       stat,
		BaseFingerprint:                gitdiff.RecoveryBaseFingerprint(ctx, normalized.Path, issue.BaseRef),
		HeadSHA:                        head,
		WorkspaceFingerprint:           gitdiff.RecoveryFingerprint(head, stat.Fingerprint),
		UnpushedCommits:                unpushedCommits,
		UnpushedCommitRefs:             unpushedCommitRefs,
		TrackedPaths:                   observation.TrackedPaths,
		UntrackedPaths:                 observation.UntrackedPaths,
		CommitsNotInPullRequest:        commitsNotInPullRequest,
		PullRequestComparisonAvailable: comparisonAvailable,
		RebaseInProgress:               rebasePaused,
	}, nil
}

func (l *LocalGit) DeliverableState(ctx context.Context, info Info, issue Issue) (DeliverableState, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DeliverableState{}, err
	}
	state, err := gitDeliveryState(ctx, normalized.Path, normalized.PublishBranch(), issue.BaseRef)
	if err != nil {
		return DeliverableState{}, err
	}
	return state, nil
}

func (l *LocalGit) Diff(ctx context.Context, info Info, issue Issue, maxBytes int) (Diff, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return Diff{}, err
	}
	return GitDiffFrom(ctx, normalized.Path, issue.BaseRef, maxBytes)
}

func gitDeliveryState(ctx context.Context, workspacePath string, branch string, baseRef string) (DeliverableState, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return DeliverableState{}, errors.New("inspect delivery state: workspace branch is required")
	}
	base := strings.TrimSpace(baseRef)
	if base == "" {
		branch, head, err := remoteDefaultBranchHead(ctx, workspacePath, defaultGitRemote)
		if err != nil {
			return DeliverableState{}, fmt.Errorf("inspect delivery base: %w", err)
		}
		if _, err := runGitAt(ctx, workspacePath, "fetch", "--no-tags", defaultGitRemote, "refs/heads/"+branch); err != nil {
			return DeliverableState{}, fmt.Errorf("fetch delivery base %s/%s: %w", defaultGitRemote, branch, err)
		}
		base = head
	}
	output, err := runGitAt(ctx, workspacePath, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return DeliverableState{}, fmt.Errorf("count local commits ahead: %w", err)
	}
	commitsAhead, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return DeliverableState{}, fmt.Errorf("parse local commits ahead: %w", err)
	}
	localHead, err := runGitAt(ctx, workspacePath, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return DeliverableState{}, fmt.Errorf("inspect local branch %s: %w", branch, err)
	}
	remoteHead, remoteBranchExists, err := remoteBranchHead(ctx, workspacePath, defaultGitRemote, branch)
	if err != nil {
		return DeliverableState{}, fmt.Errorf("inspect remote branch %s/%s: %w", defaultGitRemote, branch, err)
	}
	return DeliverableState{
		CommitsAhead:       commitsAhead,
		Remote:             defaultGitRemote,
		RemoteRef:          "refs/heads/" + branch,
		LocalHeadSHA:       strings.TrimSpace(localHead),
		RemoteHeadSHA:      strings.TrimSpace(remoteHead),
		RemoteBranchExists: remoteBranchExists,
	}, nil
}
