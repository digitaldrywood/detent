package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/instancelock"
)

type LandingBarrierWorkspace interface {
	AcquireLandingBarrierRunner(context.Context) (func() error, error)
	LandingRepository(context.Context) string
	LandingBarrierHead(context.Context, string) (string, error)
	RunLandingBarrier(context.Context, string, string, string) (gate.CommandResult, error)
}

func (l *LocalGit) AcquireLandingBarrierRunner(ctx context.Context) (func() error, error) {
	common, err := gitCommonDir(ctx, l.sourceRoot)
	if err != nil {
		return nil, err
	}
	lock, err := instancelock.Acquire(filepath.Join(common, "detent-landing-barrier.lock"))
	if err != nil {
		return nil, err
	}
	return lock.Close, nil
}

func (l *LocalGit) LandingRepository(ctx context.Context) string {
	return RepositoryURL(ctx, l.sourceRoot)
}

func (l *LocalGit) LandingBarrierHead(ctx context.Context, base string) (string, error) {
	if base == "" {
		_, head, err := remoteDefaultBranchHead(ctx, l.sourceRoot, defaultGitRemote)
		return head, err
	}
	head, exists, err := remoteBranchHead(ctx, l.sourceRoot, defaultGitRemote, base)
	if err == nil && !exists {
		err = fmt.Errorf("remote %s branch %s is missing", defaultGitRemote, base)
	}
	return head, err
}

func (l *LocalGit) RunLandingBarrier(ctx context.Context, id, base, command string) (result gate.CommandResult, resultErr error) {
	digest := sha256.Sum256([]byte(id))
	key := "barrier-" + hex.EncodeToString(digest[:16])
	path := filepath.Join(l.root, key)
	releaseUse, err := l.uses.use(ctx, path)
	if err != nil {
		return result, err
	}
	defer releaseUse()
	info := Info{Path: path, Key: key}
	issue := Issue{ID: key, Identifier: key}
	head, err := l.prepareBarrierWorktree(ctx, path, base)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		release, err := l.acquireSourceOperation(cleanupCtx)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		defer release()
		resultErr = errors.Join(resultErr, l.removeLandingWorktree(cleanupCtx, l.sourceRoot, path))
	}()
	issue.PullRequestHeadSHA = head
	result, resultErr = l.RunReviewCommand(ctx, info, issue, command)
	if !result.StartedAt.IsZero() {
		result.TimingStage = "barrier"
	}
	return result, resultErr
}

type LandingBarrierRepairWorkspace interface {
	PrepareLandingBarrierRepair(ctx context.Context, id, base string) (path, head string, release func() error, err error)
	VerifyLandingBarrierRepair(ctx context.Context, path, head, command string, failed []string) (result gate.CommandResult, changed bool, err error)
	PublishLandingBarrierRepair(ctx context.Context, path, base, head string) (string, error)
	LandingBarrierCommits(ctx context.Context, path, from, to string) ([]string, error)
	CheckoutLandingBarrierCommit(ctx context.Context, path, commit string) error
	RevertLandingBarrierCommit(ctx context.Context, path, head, commit string) error
}

func (l *LocalGit) LandingBarrierCommits(ctx context.Context, path, from, to string) ([]string, error) {
	out, err := runGitAt(ctx, path, "rev-list", "--first-parent", "--reverse", from+".."+to)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (l *LocalGit) CheckoutLandingBarrierCommit(ctx context.Context, path, commit string) error {
	_, err := runGitAt(ctx, path, "checkout", "--detach", "--force", commit)
	return err
}

func (l *LocalGit) RevertLandingBarrierCommit(ctx context.Context, path, head, commit string) error {
	if err := l.CheckoutLandingBarrierCommit(ctx, path, head); err != nil {
		return err
	}
	if _, err := runGitAt(ctx, path, "revert", "--no-edit", commit); err != nil {
		_, abortErr := runGitAt(ctx, path, "revert", "--abort")
		return errors.Join(fmt.Errorf("revert %s: %w", commit, err), abortErr)
	}
	return nil
}

const LandingBarrierFailedEnv = "DETENT_BARRIER_FAILED"

func (l *LocalGit) VerifyLandingBarrierRepair(ctx context.Context, path, head, command string, failed []string) (gate.CommandResult, bool, error) {
	repaired, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return gate.CommandResult{}, false, err
	}
	repaired = strings.TrimSpace(repaired)
	if repaired == head {
		return gate.CommandResult{}, false, nil
	}
	if len(failed) > 0 {
		command = "export " + LandingBarrierFailedEnv + "=" + shellQuoteArg(strings.Join(failed, "\n")) + "; " + command
	}
	key := filepath.Base(path)
	result, err := l.RunReviewCommand(ctx, Info{Path: path, Key: key}, Issue{ID: key, Identifier: key, PullRequestHeadSHA: repaired}, command)
	return result, true, err
}

func shellQuoteArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (l *LocalGit) PrepareLandingBarrierRepair(ctx context.Context, id, base string) (string, string, func() error, error) {
	digest := sha256.Sum256([]byte("repair:" + id))
	path := filepath.Join(l.root, "barrier-repair-"+hex.EncodeToString(digest[:16]))
	releaseUse, err := l.uses.use(ctx, path)
	if err != nil {
		return "", "", nil, err
	}
	head, err := l.prepareBarrierWorktree(ctx, path, base)
	if err != nil {
		releaseUse()
		return "", "", nil, err
	}
	release := func() error {
		defer releaseUse()
		cleanupCtx := context.WithoutCancel(ctx)
		unlock, err := l.acquireSourceOperation(cleanupCtx)
		if err != nil {
			return err
		}
		defer unlock()
		return l.removeLandingWorktree(cleanupCtx, l.sourceRoot, path)
	}
	return path, head, release, nil
}

func (l *LocalGit) prepareBarrierWorktree(ctx context.Context, path, base string) (string, error) {
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if base == "" {
		base, err = remoteDefaultBranch(ctx, l.sourceRoot, defaultGitRemote)
		if err != nil {
			return "", err
		}
	}
	if _, err := l.runGit(ctx, "check-ref-format", "refs/heads/"+base); err != nil {
		return "", err
	}
	baseRef := "refs/remotes/" + defaultGitRemote + "/" + base
	if _, err := l.runGit(ctx, "fetch", defaultGitRemote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return "", err
	}
	head, err := l.runGit(ctx, "rev-parse", baseRef)
	if err != nil {
		return "", err
	}
	if err := l.removeLandingWorktree(ctx, l.sourceRoot, path); err != nil {
		return "", err
	}
	if _, err := l.runGit(ctx, "worktree", "add", "--detach", path, strings.TrimSpace(head)); err != nil {
		return "", fmt.Errorf("create barrier worktree: %w", err)
	}
	return strings.TrimSpace(head), nil
}

func (l *LocalGit) PublishLandingBarrierRepair(ctx context.Context, path, base, head string) (string, error) {
	status, err := runGitAt(ctx, path, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", errors.New("barrier repair left uncommitted changes")
	}
	repaired, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	repaired = strings.TrimSpace(repaired)
	if repaired == head {
		return "", nil
	}
	if _, err := runGitAt(ctx, path, "merge-base", "--is-ancestor", head, repaired); err != nil {
		return "", fmt.Errorf("barrier repair does not descend from %s: %w", head, err)
	}
	if base == "" {
		base, err = remoteDefaultBranch(ctx, l.sourceRoot, defaultGitRemote)
		if err != nil {
			return "", err
		}
	}
	if _, err := runGitAt(ctx, path, "push", "--force-with-lease=refs/heads/"+base+":"+head, defaultGitRemote, repaired+":refs/heads/"+base); err != nil {
		return "", fmt.Errorf("publish barrier repair: %w", err)
	}
	return repaired, nil
}
