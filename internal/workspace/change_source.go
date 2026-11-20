package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/digitaldrywood/detent/internal/workspace/changesource"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (l *LocalGit) CaptureRecoveryChangeSource(ctx context.Context, issue Issue, base, head string) (tracker.ChangeSourceCapture, error) {
	current, previous := issue.AttemptWorkspace, issue.RecoveryAttemptWorkspace
	if current.Key() == "" {
		return tracker.ChangeSourceCapture{}, ErrMissingWorkspace
	}
	if previous != (AttemptWorkspace{}) {
		if previous.Key() == "" || previous.Organization != current.Organization || previous.Project != current.Project {
			return tracker.ChangeSourceCapture{}, ErrMissingWorkspace
		}
		issue.AttemptWorkspace = previous
	}
	info, err := l.Existing(issue)
	if errors.Is(err, ErrMissingWorkspace) && previous != (AttemptWorkspace{}) {
		issue.AttemptWorkspace = AttemptWorkspace{}
		info, err = l.Existing(issue)
	}
	if err != nil {
		return tracker.ChangeSourceCapture{}, err
	}
	if !l.isSourceWorktree(ctx, info.Path) {
		return tracker.ChangeSourceCapture{}, ErrLocalCheckpointChanged
	}
	return CaptureChangeSource(ctx, info.Path, base, head)
}

func (l *LocalGit) importWorkspaceChangeSource(ctx context.Context, issue Issue) error {
	source := issue.Source
	if source.PushedPhase == nil {
		release, err := l.acquireSourceOperation(ctx)
		if err != nil {
			return err
		}
		defer release()
		return l.importChangeSource(ctx, source)
	}
	version := source.Version
	if !validLandingHead(version.BaseSHA) || version.HeadSHA != source.PushedPhase.HeadSHA || version.PolicyID != source.PushedPhase.PolicyID || version.Repository != RepositoryURL(ctx, l.sourceRoot) {
		return refuse(LandRefusalMissingHead, "the pushed phase does not match the selected Change source")
	}
	itemIssue := issue
	itemIssue.Landing = nil
	itemIssue.ReviewVersionID = ""
	itemInfo, err := l.infoForIssue(itemIssue)
	if err != nil {
		return err
	}
	if err := l.fetchItemPhase(ctx, itemInfo, itemIssue, *source.PushedPhase); err != nil {
		return err
	}
	if version.Source != nil {
		digest, err := changeSourceDiff(ctx, l.sourceRoot, version.BaseSHA, version.HeadSHA)
		if err != nil || digest != version.Source.DiffSHA256 {
			return errors.Join(errors.New("pushed Change source diff differs from the immutable version"), err)
		}
	}
	return nil
}

func (l *LocalGit) importChangeSource(ctx context.Context, source *ChangeSource) (err error) {
	version := source.Version
	if !validLandingHead(version.HeadSHA) || !validLandingHead(version.BaseSHA) || version.Repository == "" || RepositoryURL(ctx, l.sourceRoot) != version.Repository {
		return refuse(LandRefusalMissingHead, "the retained Change source does not match the authorized repository")
	}
	if version.Source == nil {
		if _, err := l.runGit(ctx, "cat-file", "-e", version.HeadSHA+"^{commit}"); err != nil {
			return refuse(LandRefusalMissingHead, "this legacy Change has no retained source; resume on the source-owning runner and republish the exact head with source capture")
		}
		return nil
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, source.Bundle); err != nil {
		return err
	}
	if _, err := l.runGit(ctx, "cat-file", "-e", version.BaseSHA+"^{commit}"); err != nil {
		if _, err := l.runGit(ctx, "fetch", "--no-tags", "--no-write-fetch-head", defaultGitRemote, version.BaseSHA); err != nil {
			return fmt.Errorf("retrieve retained source base: %w", err)
		}
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	path := filepath.Join(scratch, "source.bundle")
	if err := os.WriteFile(path, source.Bundle, 0o600); err != nil {
		return err
	}
	if err := verifyChangeBundle(ctx, l.sourceRoot, path); err != nil {
		return err
	}
	if _, err := l.runGit(ctx, "-c", "fetch.fsckObjects=true", "bundle", "unbundle", path); err != nil {
		return err
	}
	digest, err := changeSourceDiff(ctx, l.sourceRoot, version.BaseSHA, version.HeadSHA)
	if err != nil || digest != version.Source.DiffSHA256 {
		return errors.Join(errors.New("recovered Change source diff differs from the immutable version"), err)
	}
	return nil
}

func (l *LocalGit) prepareChangeSourceWorktree(ctx context.Context, info Info, issue Issue) (bool, error) {
	if issue.Source == nil || issue.Landing != nil {
		return false, nil
	}
	l.createMu.Lock()
	defer l.createMu.Unlock()
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err == nil && paused && issue.Source.Transferred {
		return false, refuse(LandRefusalHeadMoved, "the transferred version has unresolved paused-rebase custody; preserve and reconcile the assigned worktree through source recovery before retrying transfer")
	}
	if err != nil || paused {
		return false, err
	}
	state, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return false, err
	}
	if issue.Source.Transferred && issue.Source.Version.Source == nil {
		return false, refuse(LandRefusalMissingHead, "the transferred version has no retained bundle; republish the exact head with source capture before retrying transfer")
	}
	transferred := issue.Source.Transferred
	if transferred && state.HeadSHA == issue.Source.Version.HeadSHA && len(state.TrackedPaths) == 0 && len(state.UntrackedPaths) == 0 {
		return false, nil
	}
	if !transferred {
		if state.HeadSHA == issue.Source.Version.HeadSHA || len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 {
			return false, nil
		}
		previous := issue.Source.Version.Source != nil && slices.Contains(issue.Source.PreviousHeads, state.HeadSHA)
		if state.UnpushedCommits > 0 && !previous {
			return false, nil
		}
		if state.HeadSHA != issue.Source.Version.BaseSHA && !previous && !l.headIntegrated(ctx, info.Path, issue, state.HeadSHA) {
			return false, refuse(LandRefusalHeadMoved, fmt.Sprintf("the existing clean worktree head %s differs from the recorded source base and head of version %s; preserve its current head and reconcile the Change before recovery", state.HeadSHA, issue.Source.Version.ID))
		}
	}
	if transferred || state.HeadSHA != issue.Source.Version.BaseSHA {
		if _, err := l.quarantineWorktree(ctx, info.Path); err != nil {
			if transferred {
				return false, refuse(LandRefusalHeadMoved, fmt.Sprintf("transferred version %s has unresolved local source custody; reconcile the assigned worktree through source recovery before retrying transfer: %v", issue.Source.Version.ID, err))
			}
			return false, err
		}
		if _, err := l.runGit(ctx, "branch", "-f", info.Branch, issue.Source.Version.HeadSHA); err != nil {
			return false, err
		}
		created, err := l.ensureWorktree(ctx, info.Path, info.Branch, issue.Source.Version.HeadSHA)
		if err != nil {
			return false, l.preserveFailedWorkspace(ctx, info.Path, err)
		}
		if err := l.validateCreatedWorktree(ctx, info.Path); err != nil {
			return false, l.preserveFailedWorkspace(ctx, info.Path, err)
		}
		return created, nil
	}
	_, err = runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "reset", "--hard", issue.Source.Version.HeadSHA)
	return false, err
}

func (l *LocalGit) headIntegrated(ctx context.Context, path string, issue Issue, head string) bool {
	base, err := l.localProgressBase(ctx, path, issue)
	if err != nil || !validLandingHead(base) {
		return false
	}
	_, err = runGitAt(ctx, path, "merge-base", "--is-ancestor", head, base)
	return err == nil
}

type ChangeSource = changesource.Source

func changeSourceDiff(ctx context.Context, dir, base, head string) (string, error) {
	return changesource.Diff(ctx, dir, base, head)
}
func CaptureChangeSource(ctx context.Context, dir, base, head string) (tracker.ChangeSourceCapture, error) {
	return changesource.Capture(ctx, dir, base, head)
}
func VerifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
	return changesource.Verify(ctx, source, bundle)
}
func changeSourcePackBudget(ctx context.Context, pack []byte) error {
	return changesource.PackBudget(ctx, pack)
}

func verifyChangeBundle(ctx context.Context, dir, path string) error {
	return changesource.VerifyBundle(ctx, dir, path)
}
