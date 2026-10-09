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
	RunLandingBarrier(context.Context, string, string, string, LandingBarrierCallbacks) (gate.CommandResult, error)
}

type LandingBarrierCallbacks struct {
	Record func(context.Context, gate.CommandResult) error
	Repair func(context.Context, Info, gate.CommandResult) error
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

func (l *LocalGit) RunLandingBarrier(ctx context.Context, id, base, command string, callbacks LandingBarrierCallbacks) (result gate.CommandResult, resultErr error) {
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
	check := func() error {
		current, err := runGitAt(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		issue.PullRequestHeadSHA = strings.TrimSpace(current)
		result, err = l.RunReviewCommand(ctx, info, issue, command)
		if err == nil && callbacks.Record != nil {
			err = callbacks.Record(ctx, result)
		}
		return err
	}
	if err := check(); err != nil || result.ExitCode == 0 || callbacks.Repair == nil {
		return result, err
	}
	for ctx.Err() == nil {
		if result.ExitCode != 0 {
			if err := callbacks.Repair(ctx, info, result); err != nil {
				return result, err
			}
			current, err := runGitAt(ctx, path, "rev-parse", "HEAD")
			if err != nil {
				return result, err
			}
			if strings.TrimSpace(current) != result.HeadSHA {
				return result, errors.New("barrier repair agent changed the host-owned head")
			}
			staged, err := runGitAt(ctx, path, "diff", "--cached", "--name-only")
			if err != nil || strings.TrimSpace(staged) == "" {
				return result, err
			}
			if _, err := runGitAt(ctx, path, "commit", "-m", "fix: repair rolling landing barrier"); err != nil {
				return result, err
			}
			if err := check(); err != nil {
				return result, err
			}
			if result.ExitCode != 0 {
				continue
			}
		}
		release, err := l.acquireSourceOperation(ctx)
		if err != nil {
			return result, err
		}
		baseRef := "refs/remotes/" + defaultGitRemote + "/" + base
		next, pushErr := func() (string, error) {
			defer release()
			if _, err := l.runGit(ctx, "fetch", defaultGitRemote, "+refs/heads/"+base+":"+baseRef); err != nil {
				return "", err
			}
			next, err := l.runGit(ctx, "rev-parse", baseRef)
			if err != nil {
				return "", err
			}
			next = strings.TrimSpace(next)
			if next != head {
				_, err := runGitAt(ctx, path, "rebase", "--onto", next, head)
				return next, err
			}
			if err := l.VerifyReviewTree(ctx, info, issue); err != nil {
				return next, err
			}
			_, err = runGitAt(ctx, path, "push", "--force-with-lease=refs/heads/"+base+":"+head, defaultGitRemote, result.HeadSHA+":refs/heads/"+base)
			return next, err
		}()
		if pushErr != nil {
			return result, pushErr
		}
		if next == head {
			return result, nil
		}
		head = next
		if err := check(); err != nil {
			return result, err
		}
	}
	return result, ctx.Err()
}
