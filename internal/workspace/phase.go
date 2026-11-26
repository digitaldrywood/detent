package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/phasegit"
	"github.com/digitaldrywood/detent/internal/workspace/rebase"
)

var ErrItemPhaseSource = errors.New("item phase source requires recovery")

func (l *LocalGit) PushItemPhase(ctx context.Context, info Info, issue Issue, authorize func(context.Context) error) (string, string, string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", "", "", err
	}
	releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
	if err != nil {
		return "", "", "", err
	}
	defer releaseWorktree()
	ctx = withHeldWorktreeOperation(ctx, info.Path)
	paused, pausedErr := rebase.InProgress(ctx, info.Path)
	if pausedErr != nil {
		return "", "", "", pausedErr
	}
	if paused {
		return "", "", "", refuse(LandRefusalHeadMoved, "cannot publish an item phase while a rebase is in progress")
	}
	if !issue.NoChangeNeeded && issue.Source != nil {
		headNow, headErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
		if headErr != nil {
			return "", "", "", headErr
		}
		headNow = strings.TrimSpace(headNow)
		if headNow != issue.Source.Version.HeadSHA {
			for _, recordedRef := range []string{worktreeReworkBranchRef(issue.ProgressBaseRef), worktreeReworkPendingRef} {
				if rawBase, recErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "--quiet", recordedRef+"^{commit}"); recErr == nil && strings.TrimSpace(rawBase) == headNow {
					return "", "", "", refuse(LandRefusalHeadMoved, "reconciled item branch requires a reconciliation commit before publication")
				}
			}
		}
	}
	repository := gitexec.PublicationRepository(ctx, "", issue.PublicationRepository)
	expected := info.PhaseRemoteHead
	if issue.ExpectedPhaseRemoteHead != "" {
		expected = &issue.ExpectedPhaseRemoteHead
	}
	return phasegit.Push(ctx, info.Path, l.sourceRoot, info.PublishBranch(), repository, expected, authorize, func(ctx context.Context) error {
		if err := verifyPublicationRemote(ctx, info.Path, defaultGitRemote, repository); err != nil {
			return phasePublicationDestinationError{err}
		}
		return nil
	}, func(read func(...string) (string, error), head string) error {
		base, err := l.localProgressBase(ctx, info.Path, issue)
		if err != nil {
			return fmt.Errorf("resolve phase source base: %w", err)
		}
		ancestor, err := read("merge-base", "--", base, head)
		if err != nil {
			return fmt.Errorf("resolve phase source merge base: %w", err)
		}
		return checkpoint.PhaseHistory(ctx, func(_ context.Context, args ...string) (string, error) {
			return read(args...)
		}, strings.TrimSpace(ancestor), head)
	}, func() error {
		return refuse(LandRefusalHeadMoved, "the item branch changed during phase publication")
	})
}

func (l *LocalGit) PreserveItemPhaseHistory(ctx context.Context, info Info, issue Issue, previous PhaseSource, authorize func(context.Context) error) (string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
	if err != nil {
		return "", err
	}
	defer releaseWorktree()
	ctx = withHeldWorktreeOperation(ctx, info.Path)
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
	if !worktreeOperationHeld(ctx, info.Path) {
		releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
		if err != nil {
			return "", "", err
		}
		defer releaseWorktree()
	}
	return phasegit.Head(ctx, info.Path)
}

func (l *LocalGit) VerifyItemPhaseCheckout(ctx context.Context, info Info, issue Issue) error {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
	if err != nil {
		return err
	}
	defer releaseWorktree()
	if issue.RetainPhaseCheckout && !info.Created {
		return l.verifyRetainedItemPhaseCheckout(ctx, info, issue)
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

func (l *LocalGit) verifyRetainedItemPhaseCheckout(ctx context.Context, info Info, issue Issue) error {
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return err
	}
	if paused {
		if _, err := l.verifyPausedRebaseState(ctx, info.Path, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef}); err != nil {
			return err
		}
	}
	if issue.PublishedChangeHeadSHA == "" {
		return nil
	}
	head, err := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "refs/heads/"+info.Branch+"^{commit}")
	if err != nil {
		return err
	}
	return verifyPublishedChangeAncestry(ctx, info.Path, issue.PublishedChangeHeadSHA, strings.TrimSpace(head))
}

func validItemPhaseReceipt(info Info, receipt *tracker.NativePhaseReceipt) bool {
	return receipt.Branch == info.PublishBranch() && gitexec.ValidHead(receipt.HeadSHA) && gitexec.ValidHead(receipt.TreeSHA)
}

func verifyPublishedChangeAncestry(ctx context.Context, path, published, head string) error {
	if published == "" || published == head {
		return nil
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, published, head); err != nil {
		return errors.Join(ErrItemPhaseSource, refuse(LandRefusalHeadMoved, "selected item history does not contain the published Change"), err)
	}
	return nil
}

func (l *LocalGit) recoverItemPhaseBranch(ctx context.Context, info *Info, issue Issue) error {
	branch := info.PublishBranch()
	repository := gitexec.PublicationRepository(ctx, "", issue.PublicationRepository)
	if err := verifyPublicationRemote(ctx, l.sourceRoot, defaultGitRemote, repository); err != nil {
		return phasePublicationDestinationError{err}
	}
	head, exists, err := gitexec.PublicationBranchHead(ctx, repository, branch)
	if err != nil {
		return fmt.Errorf("read published item branch: %w", err)
	}
	receipt := issue.PhaseSource
	if !exists && receipt == nil {
		if issue.PublishedChangeHeadSHA != "" {
			return errors.Join(ErrItemPhaseSource, refuse(LandRefusalHeadMoved, "published Change has no item branch"))
		}
		info.PhaseRemoteHead = &head
		return nil
	}
	if !exists || !gitexec.ValidHead(head) || receipt != nil && !validItemPhaseReceipt(*info, receipt) {
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
	fetched, err := phasegit.ResolveFetchedHead(ctx, l.sourceRoot)
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
	if err := verifyPublishedChangeAncestry(ctx, l.sourceRoot, issue.PublishedChangeHeadSHA, head); err != nil {
		return err
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
	return phasegit.Fetch(ctx, l.sourceRoot, info.PublishBranch(), source, func(ctx context.Context) string {
		return RepositoryURL(ctx, l.sourceRoot)
	}, issue.PullRequestRepository, l.acquireSourceOperation, l.runGit)
}

type phasePublicationDestinationError struct{ error }

func (err phasePublicationDestinationError) Unwrap() error { return err.error }

func (phasePublicationDestinationError) FailureAttribution() tracker.FailureAttribution {
	return tracker.FailureAttribution{Outcome: "instance", Reason: "phase publication destination differs from the configured project repository"}
}
