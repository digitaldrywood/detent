package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/phasegit"
)

func (l *LocalGit) PushItemPhase(ctx context.Context, info Info, issue Issue, authorize func(context.Context) error) (string, string, string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", "", "", err
	}
	return phasegit.Push(ctx, info.Path, info.PublishBranch(), func(ctx context.Context) error {
		return l.VerifyReviewTree(ctx, info, issue)
	}, authorize, remoteBranchHead, classifyLandingPush)
}

func (l *LocalGit) PreserveItemPhaseHistory(ctx context.Context, info Info, issue Issue, previous tracker.NativePhaseReceipt, authorize func(context.Context) error) error {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	return phasegit.PreserveHistory(ctx, info.Path, issue.Identifier, previous, func(ctx context.Context) error {
		return l.fetchItemPhase(ctx, info, issue, previous)
	}, authorize, func(ctx context.Context, args ...string) (string, error) {
		return l.runCommit(ctx, info.Path, nil, args...)
	}, remoteBranchHead, refuse(LandRefusalHeadMoved, "the item branch advanced beyond its recorded phase"))
}

func (l *LocalGit) ItemPhaseHead(ctx context.Context, info Info, issue Issue) (string, string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", "", err
	}
	return phasegit.Head(ctx, info.Path)
}

func (l *LocalGit) fetchItemPhase(ctx context.Context, info Info, issue Issue, receipt tracker.NativePhaseReceipt) error {
	return phasegit.Fetch(ctx, info.PublishBranch(), receipt, func(ctx context.Context) string {
		return RepositoryURL(ctx, l.sourceRoot)
	}, issue.PullRequestRepository, l.acquireSourceOperation, l.runGit)
}
