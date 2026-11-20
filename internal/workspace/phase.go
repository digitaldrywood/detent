package workspace

import (
	"context"
	"strings"

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
	}, authorize, func(ctx context.Context) error {
		repository := l.phaseRepository
		if expected := strings.TrimSpace(issue.PullRequestRepository); expected != "" {
			repository = expected
		}
		if err := verifyPublicationRemote(ctx, info.Path, defaultGitRemote, repository); err != nil {
			return phasePublicationDestinationError{err}
		}
		return nil
	}, remoteBranchHead, classifyLandingPush)
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

type phasePublicationDestinationError struct{ error }

func (err phasePublicationDestinationError) Unwrap() error { return err.error }

func (phasePublicationDestinationError) FailureAttribution() tracker.FailureAttribution {
	return tracker.FailureAttribution{Outcome: "instance", Reason: "phase publication destination differs from the configured project repository"}
}
