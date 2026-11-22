package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/phasegit"
)

var ErrItemPhaseSource = errors.New("item phase source requires recovery")

func (l *LocalGit) PushItemPhase(ctx context.Context, info Info, issue Issue, authorize func(context.Context) error) (string, string, string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", "", "", err
	}
	repository := gitexec.PublicationRepository("", issue.PublicationRepository)
	return phasegit.Push(ctx, info.Path, l.sourceRoot, info.PublishBranch(), repository, authorize, func(ctx context.Context) error {
		if err := verifyPublicationRemote(ctx, info.Path, defaultGitRemote, repository); err != nil {
			return phasePublicationDestinationError{err}
		}
		return nil
	}, func() error {
		return refuse(LandRefusalHeadMoved, "the item branch changed during phase publication")
	})
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

func (l *LocalGit) VerifyItemPhaseCheckout(ctx context.Context, info Info, issue Issue) error {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	if issue.PhaseSource == nil {
		return ErrItemPhaseSource
	}
	if l.autoBranch {
		ref, err := gitexec.ReadPublicationSource(ctx, info.Path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || strings.TrimSpace(ref) != "refs/heads/"+info.Branch {
			return errors.Join(errors.New("checkout is no longer on the assigned item branch"), err)
		}
	}
	return phasegit.VerifyCleanCheckout(ctx, info.Path, l.sourceRoot, *issue.PhaseSource)
}

func (l *LocalGit) verifyItemPhaseBranch(ctx context.Context, info Info, issue Issue) error {
	branch := info.PublishBranch()
	repository := gitexec.PublicationRepository("", issue.PublicationRepository)
	if err := verifyPublicationRemote(ctx, l.sourceRoot, defaultGitRemote, repository); err != nil {
		return phasePublicationDestinationError{err}
	}
	head, exists, err := gitexec.PublicationBranchHead(ctx, repository, branch)
	if err != nil {
		return fmt.Errorf("read published item branch: %w", err)
	}
	receipt := issue.PhaseSource
	if receipt == nil {
		if !exists || issue.Source != nil && issue.Source.Version.HeadSHA == head {
			return nil
		}
		return fmt.Errorf("%w: published item head has no matching phase receipt or Change source", ErrItemPhaseSource)
	}
	if !exists || receipt.Branch != branch || head != receipt.HeadSHA || !gitexec.ValidHead(head) || !gitexec.ValidHead(receipt.TreeSHA) {
		return fmt.Errorf("%w: published item branch differs from its latest phase receipt", ErrItemPhaseSource)
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	ref := "refs/heads/" + branch
	if _, err := l.runGit(ctx, remoteGitArgs("fetch", "--no-tags", "--no-recurse-submodules", defaultGitRemote, ref)...); err != nil {
		return fmt.Errorf("fetch published item branch: %w", withCommandOutput(err))
	}
	fetched, err := gitexec.ReadPublicationSource(ctx, l.sourceRoot, "-c", "advice.graftFileDeprecated=false", "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(fetched) != head {
		return fmt.Errorf("%w: published item branch changed during fetch", ErrItemPhaseSource)
	}
	tree, err := gitexec.ReadPublicationSource(ctx, l.sourceRoot, "-c", "advice.graftFileDeprecated=false", "rev-parse", head+"^{tree}")
	if err != nil {
		return fmt.Errorf("read published phase tree: %w", err)
	}
	if strings.TrimSpace(tree) != receipt.TreeSHA {
		return fmt.Errorf("%w: published item tree differs from its latest phase receipt", ErrItemPhaseSource)
	}
	if changeHead := issue.PhaseChangeHeadSHA; changeHead != "" {
		if err := gitexec.VerifyPublicationAncestry(ctx, l.sourceRoot, changeHead, head); err != nil {
			return fmt.Errorf("%w: published item phase does not descend from the current Change head: %w", ErrItemPhaseSource, err)
		}
	}
	return nil
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
