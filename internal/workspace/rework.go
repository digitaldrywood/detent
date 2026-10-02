package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (l *LocalGit) PrepareRework(ctx context.Context, info Info, issue Issue, opts MergePrepareOptions) (MergePrepareResult, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	diff, err := l.DiffStat(ctx, info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return MergePrepareResult{}, err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	if paused {
		return reworkRebaseResult(ctx, info.Path, nil)
	}
	metadata, err := inspectGitMetadata(ctx, info.Path)
	if err != nil {
		return MergePrepareResult{}, err
	}
	for _, prefix := range []string{"", "logs"} {
		parent := filepath.Dir(filepath.Join(metadata.commonDir, prefix, filepath.FromSlash(metadata.headRef)))
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return MergePrepareResult{}, err
		}
	}
	if !diff.IsEmpty() {
		return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	target := strings.TrimSpace(opts.TargetBranch)
	if target == "" {
		target, err = remoteDefaultBranch(ctx, info.Path, remote)
		if err != nil {
			return MergePrepareResult{}, err
		}
	}
	ref := "refs/remotes/" + remote + "/" + target
	if _, err := runGitAt(ctx, info.Path, "fetch", remote, "+refs/heads/"+target+":"+ref); err != nil {
		return MergePrepareResult{}, err
	}
	if _, err := runGitAt(ctx, info.Path, "rebase", "--no-gpg-sign", "--no-update-refs", "--no-autostash", "--no-rebase-merges", ref); err != nil {
		return reworkRebaseResult(ctx, info.Path, err)
	}
	return reworkRebaseResult(ctx, info.Path, nil)
}

func (l *LocalGit) FinalizeRework(ctx context.Context, info Info, issue Issue) error {
	prepared, err := l.PrepareRework(ctx, info, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef})
	if err != nil {
		return err
	}
	if prepared.Status == MergePrepareStatusDirty {
		return fmt.Errorf("%w: rework changes must be committed before finalization", ErrMergeResolutionInvalid)
	}
	info, err = l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return err
	}
	conflicts, err := reworkConflictPaths(ctx, info.Path)
	if err != nil {
		return err
	}
	if len(conflicts) != 0 {
		return fmt.Errorf("%w: unresolved source conflicts: %s", ErrMergeResolutionInvalid, strings.Join(conflicts, ", "))
	}
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		path, err := gitPathFor(ctx, info.Path, kind+"/gpg_sign_opt")
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := runGitAtWithEnv(ctx, info.Path, []string{"GIT_EDITOR=true"}, "-c", "commit.gpgsign=false", "rebase", "--continue"); err != nil {
		result, inspectErr := reworkRebaseResult(ctx, info.Path, err)
		if inspectErr != nil {
			return inspectErr
		}
		return fmt.Errorf("%w: %s", ErrMergeResolutionInvalid, result.Message)
	}
	paused, err = l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return err
	}
	if paused {
		return fmt.Errorf("%w: rebase is still paused", ErrMergeResolutionInvalid)
	}
	return nil
}

func (l *LocalGit) verifyReworkBranch(ctx context.Context, info Info, issue Issue) (bool, error) {
	owned, err := l.infoForIssue(issue)
	if err != nil {
		return false, err
	}
	if info.Path != owned.Path || info.Branch == "" || info.Branch != owned.Branch {
		return false, fmt.Errorf("%w: rework is not on the assigned workspace branch", ErrMergeResolutionInvalid)
	}
	if err := l.validateCreatedWorktree(ctx, info.Path); err != nil {
		return false, err
	}
	paused, err := rebaseInProgress(ctx, info.Path)
	if err != nil {
		return false, err
	}
	if !paused {
		branch, err := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(branch) != "refs/heads/"+info.Branch {
			return false, fmt.Errorf("%w: rework branch changed", ErrMergeResolutionInvalid)
		}
		return false, nil
	}
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		dir, err := gitPathFor(ctx, info.Path, kind)
		if err != nil {
			return false, err
		}
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		name, err := os.ReadFile(filepath.Join(dir, "head-name"))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(name)) != "refs/heads/"+info.Branch {
			return false, fmt.Errorf("%w: paused rebase belongs to another branch", ErrMergeResolutionInvalid)
		}
		original, err := os.ReadFile(filepath.Join(dir, "orig-head"))
		if err != nil {
			return false, err
		}
		branchHead, err := runGitAt(ctx, info.Path, "rev-parse", "refs/heads/"+info.Branch)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(branchHead) != strings.TrimSpace(string(original)) {
			return false, fmt.Errorf("%w: assigned branch moved during rebase", ErrMergeResolutionInvalid)
		}
		updates, err := os.ReadFile(filepath.Join(dir, "update-refs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(updates)) != "" {
			return false, fmt.Errorf("%w: paused rebase updates additional refs", ErrMergeResolutionInvalid)
		}
		if kind == "rebase-merge" {
			todo, err := os.ReadFile(filepath.Join(dir, "git-rebase-todo"))
			if err != nil {
				return false, err
			}
			for _, line := range strings.Split(string(todo), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if !strings.HasPrefix(line, "pick ") && !strings.HasPrefix(line, "p ") {
					return false, fmt.Errorf("%w: paused rebase contains operations outside source replay", ErrMergeResolutionInvalid)
				}
			}
		}
	}
	return true, nil
}

func reworkConflictPaths(ctx context.Context, path string) ([]string, error) {
	output, err := runGitAt(ctx, path, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(output, "\x00"), "\x00"), nil
}

func reworkRebaseResult(ctx context.Context, path string, rebaseErr error) (MergePrepareResult, error) {
	conflicts, err := reworkConflictPaths(ctx, path)
	if err != nil {
		return MergePrepareResult{}, errors.Join(rebaseErr, err)
	}
	if len(conflicts) != 0 {
		return MergePrepareResult{Status: MergePrepareStatusConflict, ConflictPaths: conflicts, Message: "Resolve and stage the paused rebase source conflicts: " + strings.Join(conflicts, ", ")}, nil
	}
	if rebaseErr != nil {
		return MergePrepareResult{}, rebaseErr
	}
	head, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return MergePrepareResult{}, err
	}
	return MergePrepareResult{Status: MergePrepareStatusClean, HeadSHA: strings.TrimSpace(head)}, nil
}
