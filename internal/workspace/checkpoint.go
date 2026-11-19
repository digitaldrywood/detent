package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type CheckpointBackend interface {
	PrepareCheckpoint(context.Context, Info, Issue) (CheckpointPlan, error)
	Checkpoint(context.Context, CheckpointPlan, CheckpointSelection, func(context.Context) error, procgroup.Environment) (string, error)
}

func (l *LocalGit) PrepareCheckpoint(ctx context.Context, info Info, issue Issue) (CheckpointPlan, error) {
	expected, err := l.infoForIssue(issue)
	if err != nil {
		return CheckpointPlan{}, err
	}
	automatic := issue
	automatic.BranchName = ""
	if !l.autoBranch || expected.Branch == "" || expected.PublicationBranch != l.publicationBranch(automatic, expected.Key) || info.PublicationBranch != "" && info.PublicationBranch != expected.PublicationBranch || info.Path != expected.Path || info.Branch != l.branchName(automatic, expected.Key) {
		return CheckpointPlan{}, fmt.Errorf("%w: workspace is not an automatically owned issue branch", ErrCheckpointUnsafe)
	}
	info.PublicationBranch = expected.PublicationBranch
	if !l.isSourceWorktree(ctx, info.Path) {
		return CheckpointPlan{}, fmt.Errorf("%w: workspace ownership is unavailable", ErrCheckpointUnsafe)
	}
	if err := l.recordCleanupOwnership(ctx, info, issue, true); err != nil {
		return CheckpointPlan{}, err
	}
	gitDir, err := runGitAt(ctx, info.Path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return CheckpointPlan{}, err
	}
	plan := CheckpointPlan{Info: info, Journal: filepath.Join(strings.TrimSpace(gitDir), "detent-checkpoint.json")}
	if err := WriteCheckpointRecord(plan, CheckpointRecord{Reason: "session_started", Status: "active", Detail: "Worker may have local work. No checkpoint handshake or publication has been verified; inspect the retained workspace after an interrupted session."}); err != nil {
		return plan, err
	}
	remote, err := runGitAt(ctx, info.Path, "remote", "get-url", "--push", defaultGitRemote)
	if err != nil {
		return plan, err
	}
	plan.RemoteURL = strings.TrimSpace(remote)
	base := strings.TrimSpace(issue.BaseRef)
	if base == "" {
		_, base, err = remoteDefaultBranchHead(ctx, info.Path, defaultGitRemote)
		if err != nil {
			return plan, err
		}
	}
	baseSHA, err := runGitAt(ctx, info.Path, "merge-base", "HEAD", base)
	if err != nil {
		return plan, err
	}
	plan.BaseSHA = strings.TrimSpace(baseSHA)
	return plan, nil
}

func (l *LocalGit) Checkpoint(ctx context.Context, plan CheckpointPlan, selection CheckpointSelection, validate func(context.Context) error, environment procgroup.Environment) (string, error) {
	if validate == nil || !selection.Reviewed || selection.HeadSHA == "" || plan.RemoteURL == "" || plan.BaseSHA == "" {
		return "", fmt.Errorf("%w: explicit reviewed selection and ownership validation are required", ErrCheckpointUnsafe)
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	git := checkpointGit(plan.Info.Path, environment)
	if err := checkpointGuard(ctx, git, plan, selection.HeadSHA, validate); err != nil {
		return "", err
	}
	paths := slices.Clone(selection.Paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if !checkpointPathAllowed(path) {
			return "", fmt.Errorf("%w: excluded path %q", ErrCheckpointUnsafe, path)
		}
		stat, err := os.Lstat(filepath.Join(plan.Info.Path, filepath.FromSlash(path)))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err == nil && !stat.Mode().IsRegular() {
			return "", fmt.Errorf("%w: selection must name regular files", ErrCheckpointUnsafe)
		}
	}
	if err := checkpointHistory(ctx, git, plan.BaseSHA, selection.HeadSHA, paths); err != nil {
		return "", err
	}
	if len(paths) > 0 {
		changed, err := git(ctx, append([]string{"diff", "--name-only", "--no-renames", "-z", "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", err
		}
		untracked, err := git(ctx, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, paths...)...)
		if err != nil {
			return "", err
		}
		dirty := strings.Split(strings.TrimSuffix(changed+untracked, "\x00"), "\x00")
		if changed+untracked != "" {
			if _, err := git(ctx, append([]string{"add", "--"}, dirty...)...); err != nil {
				return "", err
			}
		}
		diff, err := git(ctx, append([]string{"diff", "--cached", "--name-only", "-z", "HEAD", "--"}, paths...)...)
		if err != nil {
			return "", err
		}
		if diff != "" {
			for _, path := range strings.Split(strings.TrimSuffix(diff, "\x00"), "\x00") {
				if err := checkpointBlob(ctx, git, ":", path); err != nil {
					return "", err
				}
			}
			if err := checkpointGuard(ctx, git, plan, selection.HeadSHA, validate); err != nil {
				return "", err
			}
			if _, err := l.runCommit(ctx, plan.Info.Path, checkpointEnvironment(environment), append([]string{"commit", "--only", "-m", "chore(recovery): checkpoint intended issue work", "--"}, dirty...)...); err != nil {
				return "", err
			}
		}
	}
	head, err := git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	if err := checkpointHistory(ctx, git, plan.BaseSHA, head, paths); err != nil {
		return head, err
	}
	if head == plan.BaseSHA {
		return head, nil
	}
	remoteHead, err := checkpointRemoteHead(ctx, git, plan)
	if err != nil {
		return head, err
	}
	if remoteHead != "" && remoteHead != head {
		if _, err := git(ctx, "merge-base", "--is-ancestor", remoteHead, head); err != nil {
			return head, fmt.Errorf("%w: remote branch advanced; retain local work for reconciliation", ErrCheckpointUnsafe)
		}
	}
	if err := checkpointGuard(ctx, git, plan, head, validate); err != nil {
		return head, err
	}
	if remoteHead != head {
		if _, err := git(ctx, "-c", "push.followTags=false", "push", "--recurse-submodules=no", "--force-with-lease=refs/heads/"+plan.Info.PublishBranch()+":"+remoteHead, plan.RemoteURL, head+":refs/heads/"+plan.Info.PublishBranch()); err != nil {
			return head, err
		}
	}
	verified, err := checkpointRemoteHead(ctx, git, plan)
	if err != nil || verified != head {
		return head, errors.Join(fmt.Errorf("%w: remote checkpoint could not be verified", ErrCheckpointUnsafe), err)
	}
	return head, nil
}
