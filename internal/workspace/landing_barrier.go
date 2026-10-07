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
	prepare := func() (string, error) {
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
	head, err := prepare()
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
	return l.RunReviewCommand(ctx, info, issue, command)
}
