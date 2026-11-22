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
	expected := info.PhaseRemoteHead
	if issue.ExpectedPhaseRemoteHead != "" {
		expected = &issue.ExpectedPhaseRemoteHead
	}
	return phasegit.Push(ctx, info.Path, l.sourceRoot, info.PublishBranch(), repository, expected, authorize, func(ctx context.Context) error {
		if err := verifyPublicationRemote(ctx, info.Path, defaultGitRemote, repository); err != nil {
			return phasePublicationDestinationError{err}
		}
		return nil
	}, func() error {
		return refuse(LandRefusalHeadMoved, "the item branch changed during phase publication")
	})
}

func (l *LocalGit) PreserveItemPhaseHistory(ctx context.Context, info Info, issue Issue, previous PhaseSource, authorize func(context.Context) error) (string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
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
	source := itemPhaseCheckoutSource(info, issue.PhaseSource)
	if source.HeadSHA == "" {
		return ErrItemPhaseSource
	}
	if l.autoBranch {
		ref, err := gitexec.ReadPublicationSource(ctx, info.Path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || strings.TrimSpace(ref) != "refs/heads/"+info.Branch {
			return errors.Join(errors.New("checkout is no longer on the assigned item branch"), err)
		}
	}
	return phasegit.VerifyCleanCheckout(ctx, info.Path, l.sourceRoot, source.HeadSHA, source.TreeSHA)
}

func itemPhaseCheckoutSource(info Info, receipt *tracker.NativePhaseReceipt) PhaseSource {
	if info.RecoveredPhase.HeadSHA != "" {
		return info.RecoveredPhase
	}
	if receipt == nil {
		return PhaseSource{}
	}
	return PhaseSource{Branch: receipt.Branch, HeadSHA: receipt.HeadSHA, TreeSHA: receipt.TreeSHA}
}

func (l *LocalGit) recoverItemPhaseBranch(ctx context.Context, info *Info, issue Issue) error {
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
	if !exists && receipt == nil {
		info.PhaseRemoteHead = &head
		return nil
	}
	if !exists || !gitexec.ValidHead(head) || receipt != nil && (receipt.Branch != branch || !gitexec.ValidHead(receipt.HeadSHA) || !gitexec.ValidHead(receipt.TreeSHA)) {
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
	var ancestor string
	if receipt != nil {
		ancestor = receipt.HeadSHA
	} else {
		base := issue.BaseRef
		if base == "" {
			base, err = l.newBranchBaseRef(ctx)
			if err != nil {
				return err
			}
		}
		ancestor, err = l.resolveWorktreeStartHead(ctx, base)
		if err != nil {
			return err
		}
	}
	if head != ancestor {
		if err := gitexec.VerifyPublicationAncestry(ctx, l.sourceRoot, ancestor, head); err != nil {
			reason := "published item branch does not descend from its latest phase receipt"
			if receipt == nil {
				reason = "published item branch does not descend from the item base"
			}
			return errors.Join(ErrItemPhaseSource, refuse(LandRefusalHeadMoved, reason), err)
		}
	}
	if receipt != nil {
		tree, err := l.itemPhaseTree(ctx, ancestor)
		if err != nil || tree != receipt.TreeSHA {
			return errors.Join(fmt.Errorf("%w: published item tree differs from its latest phase receipt", ErrItemPhaseSource), err)
		}
	}
	info.PhaseRemoteHead = &head
	if receipt == nil || head != receipt.HeadSHA {
		tree, err := l.itemPhaseTree(ctx, head)
		if err != nil {
			return err
		}
		info.RecoveredPhase = PhaseSource{Branch: branch, HeadSHA: head, TreeSHA: tree}
	}
	return nil
}

func (l *LocalGit) itemPhaseTree(ctx context.Context, head string) (string, error) {
	tree, err := gitexec.ReadPublicationSource(ctx, l.sourceRoot, "-c", "advice.graftFileDeprecated=false", "rev-parse", head+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("read published phase tree: %w", err)
	}
	return strings.TrimSpace(tree), nil
}

func (l *LocalGit) fetchItemPhase(ctx context.Context, info Info, issue Issue, source PhaseSource) error {
	return phasegit.Fetch(ctx, info.PublishBranch(), source, func(ctx context.Context) string {
		return RepositoryURL(ctx, l.sourceRoot)
	}, issue.PullRequestRepository, l.acquireSourceOperation, l.runGit)
}

type phasePublicationDestinationError struct{ error }

func (err phasePublicationDestinationError) Unwrap() error { return err.error }

func (phasePublicationDestinationError) FailureAttribution() tracker.FailureAttribution {
	return tracker.FailureAttribution{Outcome: "instance", Reason: "phase publication destination differs from the configured project repository"}
}
