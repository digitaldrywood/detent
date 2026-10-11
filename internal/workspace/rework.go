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
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return MergePrepareResult{}, err
	}
	defer release()
	return l.prepareRework(ctx, info, issue, opts)
}

func (l *LocalGit) VerifyReworkRecovery(ctx context.Context, info Info, issue Issue, sourceHead, sourceDigest string, observed RecoveryState) (verified bool, err error) {
	info, err = l.normalizeInfo(info, issue)
	if err != nil {
		return false, err
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return false, err
	}
	if sourceHead == "" || sourceDigest != workspaceRecoveryFingerprint(sourceHead, "") {
		return false, nil
	}
	branchHead, err := runGitAt(ctx, info.Path, "rev-parse", "refs/heads/"+info.Branch)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(branchHead) != sourceHead {
		return false, nil
	}
	dir, err := gitPathFor(ctx, info.Path, "rebase-merge")
	if err != nil {
		return false, err
	}
	ontoBytes, err := os.ReadFile(filepath.Join(dir, "onto"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	onto := strings.TrimSpace(string(ontoBytes))
	base, err := l.localProgressBase(ctx, info.Path, issue)
	if err != nil {
		return false, err
	}
	if _, err := runGitAt(ctx, info.Path, "merge-base", "--is-ancestor", onto, base); err != nil {
		var commandErr *CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return false, nil
		}
		return false, err
	}
	scratch, err := os.MkdirTemp("", "detent-rework-verification-*")
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	replay := filepath.Join(scratch, "source")
	if _, err := runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "clone", "--shared", "--no-checkout", "--", info.Path, replay); err != nil {
		return false, err
	}
	if _, err := runGitAt(ctx, replay, "-c", "core.hooksPath="+os.DevNull, "checkout", "-B", info.Branch, sourceHead); err != nil {
		return false, err
	}
	stat, err := gitDiffStat(ctx, replay, true)
	if err != nil {
		return false, err
	}
	if workspaceRecoveryFingerprint(sourceHead, stat.Fingerprint) != sourceDigest {
		return false, nil
	}
	_, rebaseErr := runGitAt(ctx, replay, "-c", "core.hooksPath="+os.DevNull, "-c", "rerere.enabled=false", "rebase", "--no-gpg-sign", "--no-update-refs", "--no-autostash", "--no-rebase-merges", onto)
	prepared, err := reworkRebaseResult(ctx, replay, rebaseErr)
	if err != nil || prepared.Status != MergePrepareStatusConflict {
		return false, err
	}
	head, err := runGitAt(ctx, replay, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	stat, err = gitDiffStat(ctx, replay, true)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(head) != observed.HeadSHA || workspaceRecoveryFingerprint(strings.TrimSpace(head), stat.Fingerprint) != observed.WorkspaceFingerprint {
		return false, nil
	}
	replayDir, err := gitPathFor(ctx, replay, "rebase-merge")
	if err != nil {
		return false, err
	}
	for _, name := range []string{"head-name", "orig-head", "onto", "stopped-sha", "done", "git-rebase-todo", "autostash", "strategy", "strategy_opts", "rewritten-list", "msgnum", "end", "message", "author-script"} {
		actual, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		expected, err := os.ReadFile(filepath.Join(replayDir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(actual)) != strings.TrimSpace(string(expected)) {
			return false, nil
		}
	}
	actualIndex, err := runGitAt(ctx, info.Path, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return false, err
	}
	expectedIndex, err := runGitAt(ctx, replay, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return false, err
	}
	if actualIndex != expectedIndex {
		return false, nil
	}
	paused, err = l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return false, err
	}
	current, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return false, err
	}
	return current.HeadSHA == observed.HeadSHA && current.WorkspaceFingerprint == observed.WorkspaceFingerprint, nil
}

func (l *LocalGit) prepareRework(ctx context.Context, info Info, issue Issue, opts MergePrepareOptions) (MergePrepareResult, error) {
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	if paused {
		prepared, err := reworkRebaseResult(ctx, info.Path, nil)
		if err != nil {
			return MergePrepareResult{}, err
		}
		for _, kind := range []string{"rebase-merge", "rebase-apply"} {
			path, err := gitPathFor(ctx, info.Path, kind+"/onto")
			if err != nil {
				return MergePrepareResult{}, err
			}
			onto, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return MergePrepareResult{}, err
			}
			prepared.BaseSHA = strings.TrimSpace(string(onto))
			return prepared, nil
		}
		return MergePrepareResult{}, fmt.Errorf("%w: paused rebase integration base is unavailable", ErrMergeResolutionInvalid)
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
	diff, err := l.DiffStat(ctx, info, issue)
	if err != nil {
		return MergePrepareResult{}, err
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
	base, err := runGitAt(ctx, info.Path, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return MergePrepareResult{}, err
	}
	base = strings.TrimSpace(base)
	_, rebaseErr := runGitAt(ctx, info.Path, "rebase", "--no-gpg-sign", "--no-update-refs", "--no-autostash", "--no-rebase-merges", base)
	prepared, err := reworkRebaseResult(ctx, info.Path, rebaseErr)
	prepared.BaseSHA = base
	return prepared, err
}

func (l *LocalGit) FinalizeNativeWork(ctx context.Context, info Info, issue Issue, validate func(context.Context) error) (string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return "", err
	}
	if validate == nil {
		return "", errors.New("native completion authority is unavailable")
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	if !paused {
		staged, err := runGitAt(ctx, info.Path, "diff", "--cached", "--name-only", "-z")
		if err != nil {
			return "", err
		}
		if staged != "" {
			if _, err := runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", "fix: complete "+issue.Identifier); err != nil {
				return "", err
			}
		}
	}
	if !issue.NativeRework {
		if paused {
			return "", fmt.Errorf("%w: native code completion has a paused rebase", ErrMergeResolutionInvalid)
		}
		return "", nil
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	startedPaused := paused
	prepared, err := l.prepareRework(ctx, info, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef})
	if err != nil {
		return "", err
	}
	if prepared.Status == MergePrepareStatusDirty {
		return "", fmt.Errorf("%w: rework changes must be staged before finalization", ErrMergeResolutionInvalid)
	}
	paused, err = l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return prepared.BaseSHA, err
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	conflicts, err := reworkConflictPaths(ctx, info.Path)
	if err != nil {
		return "", err
	}
	if len(conflicts) != 0 {
		if !startedPaused {
			return "", nil
		}
		return "", fmt.Errorf("%w: unresolved source conflicts: %s", ErrMergeResolutionInvalid, strings.Join(conflicts, ", "))
	}
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		path, err := gitPathFor(ctx, info.Path, kind+"/gpg_sign_opt")
		if err != nil {
			return "", err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if _, err := runGitAtWithEnv(ctx, info.Path, []string{"GIT_EDITOR=true"}, "-c", "commit.gpgsign=false", "rebase", "--continue"); err != nil {
		result, inspectErr := reworkRebaseResult(ctx, info.Path, err)
		if inspectErr != nil {
			return "", inspectErr
		}
		if result.Status == MergePrepareStatusConflict {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s", ErrMergeResolutionInvalid, result.Message)
	}
	paused, err = l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		return "", err
	}
	if paused {
		return "", fmt.Errorf("%w: rebase is still paused", ErrMergeResolutionInvalid)
	}
	return prepared.BaseSHA, nil
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
