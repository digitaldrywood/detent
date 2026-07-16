package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultMergeRemote = "origin"

func (l *LocalGit) PrepareMerge(
	ctx context.Context,
	info Info,
	issue Issue,
	opts MergePrepareOptions,
) (MergePrepareResult, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultMergeRemote
	}
	targetBranch := strings.TrimSpace(opts.TargetBranch)
	if targetBranch == "" {
		targetBranch, err = remoteDefaultBranch(ctx, normalized.Path, remote)
		if err != nil {
			return MergePrepareResult{}, fmt.Errorf("resolve remote default branch: %w", err)
		}
	}
	targetRef := remote + "/" + targetBranch
	fetchRefspec := "+refs/heads/" + targetBranch + ":refs/remotes/" + targetRef

	if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, fetchRefspec); err != nil {
		return MergePrepareResult{}, errors.Join(
			fmt.Errorf("git fetch %s %s: %w", remote, fetchRefspec, err),
			abortRebaseIfInProgress(ctx, normalized.Path),
		)
	}
	if _, err := runGitAt(ctx, normalized.Path, "rebase", targetRef); err != nil {
		abortErr := abortRebaseIfInProgress(ctx, normalized.Path)
		if abortErr != nil {
			return MergePrepareResult{}, errors.Join(
				fmt.Errorf("git rebase %s: %w", targetRef, err),
				abortErr,
			)
		}
		return MergePrepareResult{
			Status:  MergePrepareStatusConflict,
			Message: commandErrorOutput(err),
		}, nil
	}

	diffStat, err := l.DiffStat(ctx, normalized, issue)
	if err != nil {
		return MergePrepareResult{}, errors.Join(
			fmt.Errorf("workspace diff stat after rebase: %w", err),
			abortRebaseIfInProgress(ctx, normalized.Path),
		)
	}
	if diffStat != (DiffStat{}) {
		return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diffStat}, nil
	}

	branch := strings.TrimSpace(normalized.Branch)
	if branch == "" {
		return MergePrepareResult{}, errors.New("workspace branch is required for merge fast-path push")
	}
	remoteHead, remoteBranchExists, err := remoteBranchHead(ctx, normalized.Path, remote, branch)
	if err != nil {
		return MergePrepareResult{}, errors.Join(
			fmt.Errorf("inspect remote branch %s/%s: %w", remote, branch, err),
			abortRebaseIfInProgress(ctx, normalized.Path),
		)
	}
	localHead, err := runGitAt(ctx, normalized.Path, "rev-parse", "HEAD")
	if err != nil {
		return MergePrepareResult{}, errors.Join(
			fmt.Errorf("inspect local branch head: %w", err),
			abortRebaseIfInProgress(ctx, normalized.Path),
		)
	}
	headChanged := !remoteBranchExists || !strings.EqualFold(strings.TrimSpace(localHead), strings.TrimSpace(remoteHead))
	pushArgs := []string{"push"}
	if remoteBranchExists {
		pushArgs = append(pushArgs, "--force-with-lease=refs/heads/"+branch+":"+remoteHead)
	}
	pushArgs = append(pushArgs, remote, "HEAD:"+branch)
	if _, err := runGitAt(ctx, normalized.Path, pushArgs...); err != nil {
		return MergePrepareResult{}, errors.Join(
			fmt.Errorf("git %s: %w", strings.Join(pushArgs, " "), err),
			abortRebaseIfInProgress(ctx, normalized.Path),
		)
	}
	return MergePrepareResult{Status: MergePrepareStatusClean, DiffStat: diffStat, HeadChanged: headChanged}, nil
}

func remoteDefaultBranch(ctx context.Context, workspacePath string, remote string) (string, error) {
	output, err := runGitAt(ctx, workspacePath, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "ref:" || fields[2] != "HEAD" {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		if branch != fields[1] && strings.TrimSpace(branch) != "" {
			return branch, nil
		}
	}
	return "", fmt.Errorf("remote %s HEAD is not a branch", remote)
}

func remoteBranchHead(ctx context.Context, workspacePath string, remote string, branch string) (string, bool, error) {
	ref := "refs/heads/" + branch
	output, err := runGitAt(ctx, workspacePath, "ls-remote", remote, ref)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != ref {
			continue
		}
		return fields[0], true, nil
	}
	return "", false, nil
}

func abortRebaseIfInProgress(ctx context.Context, workspacePath string) error {
	inProgress, err := rebaseInProgress(ctx, workspacePath)
	if err != nil {
		return err
	}
	if !inProgress {
		return nil
	}
	if _, err := runGitAt(ctx, workspacePath, "rebase", "--abort"); err != nil {
		return fmt.Errorf("git rebase --abort: %w", err)
	}
	return nil
}

func rebaseInProgress(ctx context.Context, workspacePath string) (bool, error) {
	for _, gitPath := range []string{"rebase-merge", "rebase-apply"} {
		path, err := gitPathFor(ctx, workspacePath, gitPath)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func gitPathFor(ctx context.Context, workspacePath string, gitPath string) (string, error) {
	output, err := runGitAt(ctx, workspacePath, "rev-parse", "--git-path", gitPath)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(output)
	if path == "" {
		return "", errors.New("git path is empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspacePath, path)
	}
	return filepath.Clean(path), nil
}

func commandErrorOutput(err error) string {
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return strings.TrimSpace(commandErr.Output)
	}
	return strings.TrimSpace(err.Error())
}
