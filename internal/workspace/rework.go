package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitdiff"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/rebase"
)

type ReworkPreparationReason string

const (
	ReworkPreparationFailed           ReworkPreparationReason = "preparation_failed"
	ReworkPreparationDirtyWorktree    ReworkPreparationReason = "dirty_worktree"
	ReworkPreparationRebaseInProgress ReworkPreparationReason = "rebase_in_progress"
	ReworkPreparationSigningFailed    ReworkPreparationReason = "signing_failed"
	ReworkPreparationFetchFailed      ReworkPreparationReason = "fetch_failed"
	ReworkPreparationGitFailed        ReworkPreparationReason = "git_failed"
)

type ReworkPreparationError struct {
	Reason ReworkPreparationReason
	Err    error
}

func (e *ReworkPreparationError) Error() string {
	return "rework preparation failed: " + string(e.Reason)
}

func (e *ReworkPreparationError) Unwrap() error {
	return e.Err
}

func reworkPreparationError(err error) error {
	if err == nil {
		return nil
	}
	var preparationErr *ReworkPreparationError
	if errors.As(err, &preparationErr) {
		return err
	}
	reason := ReworkPreparationFailed
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		reason = ReworkPreparationGitFailed
		output := commandErr.Output
		switch {
		case strings.Contains(output, "cannot rebase: You have unstaged changes"), strings.Contains(output, "Your index contains uncommitted changes"), strings.Contains(output, "you have staged changes in your working tree"):
			reason = ReworkPreparationDirtyWorktree
		case strings.Contains(output, commitSigningFailureMarker), strings.Contains(output, "gpg failed to sign the data"), strings.Contains(output, "signing failed:"):
			reason = ReworkPreparationSigningFailed
		case strings.Contains(output, "already a rebase-merge directory"), strings.Contains(output, "already a rebase-apply directory"), strings.Contains(output, "It looks like 'git am' is in progress"):
			reason = ReworkPreparationRebaseInProgress
		}
	}
	return &ReworkPreparationError{Reason: reason, Err: err}
}

func reworkWorktreeDirty(ctx context.Context, path string) (bool, error) {
	status, err := runGitAtWithEnv(ctx, path, []string{"GIT_OPTIONAL_LOCKS=0"}, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignore-submodules=none")
	return status != "", err
}

func reworkHasUnstagedChanges(ctx context.Context, path string) (bool, error) {
	unstaged, err := runGitAt(ctx, path, "diff", "--ignore-submodules=none", "--name-only", "-z")
	if err != nil || unstaged != "" {
		return unstaged != "", err
	}
	untracked, err := runGitAt(ctx, path, "ls-files", "--others", "--exclude-standard", "-z")
	return untracked != "", err
}

func (l *LocalGit) PrepareRework(ctx context.Context, info Info, issue Issue, opts MergePrepareOptions, validate func(context.Context) error) (result MergePrepareResult, err error) {
	defer func() { err = reworkPreparationError(err) }()
	info, err = l.normalizeInfo(info, issue)
	if err != nil {
		return MergePrepareResult{}, err
	}
	releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
	if err != nil {
		return MergePrepareResult{}, err
	}
	defer releaseWorktree()
	return l.prepareRework(ctx, info, issue, opts, validate)
}

func (l *LocalGit) VerifyReworkRecovery(ctx context.Context, info Info, issue Issue, sourceHead, sourceDigest string, observed RecoveryState) (verified bool, err error) {
	info, err = l.normalizeInfo(info, issue)
	if err != nil {
		return false, err
	}
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return false, err
	}
	var onto string
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		ontoPath, pathErr := gitPathFor(ctx, info.Path, kind+"/onto")
		if pathErr != nil {
			return false, pathErr
		}
		raw, readErr := rebase.ReadRegularGitFile(ontoPath)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		onto = strings.TrimSpace(string(raw))
		break
	}
	if onto == "" || !gitexec.ValidHead(onto) {
		return false, nil
	}
	remoteBase, _, fetchErr := l.resolveReworkTargetBase(ctx, info.Path, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef})
	if fetchErr != nil {
		return false, fetchErr
	}
	if ancErr := gitexec.VerifyPublicationAncestry(ctx, info.Path, onto, remoteBase); ancErr != nil {
		var ancCmdErr *gitexec.CommandError
		if errors.As(ancErr, &ancCmdErr) && ancCmdErr.ExitCode == 1 {
			return false, nil
		}
		return false, ancErr
	}

	if issue.Checkpoint != nil {
		checkpoint := *issue.Checkpoint
		if err := l.fetchCheckpointRef(ctx, &checkpoint); err != nil {
			return false, err
		}
		if err := l.bindCheckpointBranch(issue, info, &checkpoint); err != nil {
			return false, err
		}
		if checkpoint.Transaction != nil {
			current, err := l.RecoveryState(ctx, info, issue)
			if err != nil {
				return false, err
			}
			return current.HeadSHA == observed.HeadSHA && current.WorkspaceFingerprint == observed.WorkspaceFingerprint && checkpointMatches(ctx, info.Path, &checkpoint), nil
		}
	}
	if sourceHead == "" || sourceDigest != gitdiff.RecoveryFingerprint(sourceHead, "") {
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
	if _, statErr := os.Stat(filepath.Join(dir, "onto")); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	} else if statErr != nil {
		return false, statErr
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
	stat, err := gitdiff.Stat(ctx, replay, true)
	if err != nil {
		return false, err
	}
	if gitdiff.RecoveryFingerprint(sourceHead, stat.Fingerprint) != sourceDigest {
		return false, nil
	}
	_, rebaseErr := l.runCommit(ctx, replay, nil, "-c", "core.hooksPath="+os.DevNull, "-c", "rerere.enabled=false", "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", onto)
	prepared, err := reworkRebaseResult(ctx, replay, rebaseErr)
	if err != nil || prepared.Status != MergePrepareStatusConflict {
		return false, err
	}
	head, err := runGitAt(ctx, replay, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	stat, err = gitdiff.Stat(ctx, replay, true)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(head) != observed.HeadSHA || gitdiff.RecoveryFingerprint(strings.TrimSpace(head), stat.Fingerprint) != observed.WorkspaceFingerprint {
		return false, nil
	}
	replayDir, err := gitPathFor(ctx, replay, "rebase-merge")
	if err != nil {
		return false, err
	}
	for _, name := range []string{"head-name", "orig-head", "onto", "stopped-sha", "done", "git-rebase-todo", "autostash", "strategy", "strategy_opts", "rewritten-list", "msgnum", "end", "message", "author-script"} {
		actual, err := rebase.ReadRegularGitFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		expected, err := rebase.ReadRegularGitFile(filepath.Join(replayDir, name))
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

func (l *LocalGit) prepareRework(ctx context.Context, info Info, issue Issue, opts MergePrepareOptions, validate func(context.Context) error) (MergePrepareResult, error) {
	var rebaseSnapshotBefore rebase.MetadataSnapshot
	if active, activeErr := rebase.InProgress(ctx, info.Path); activeErr == nil && active {
		snap, snapErr := rebase.Snapshot(ctx, info.Path)
		if snapErr != nil {
			return MergePrepareResult{}, snapErr
		}
		rebaseSnapshotBefore = snap
	}
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		if active, stateErr := rebase.InProgress(ctx, info.Path); stateErr == nil && active {
			return MergePrepareResult{}, &ReworkPreparationError{Reason: ReworkPreparationRebaseInProgress, Err: err}
		}
		return MergePrepareResult{}, err
	}
	if validate == nil {
		return MergePrepareResult{}, errors.New("native completion authority is unavailable")
	}
	var expectedHead string
	if !paused {
		headInit, headInitErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
		if headInitErr != nil {
			return MergePrepareResult{}, headInitErr
		}
		expectedHead = strings.TrimSpace(headInit)
	}
	if err := validate(ctx); err != nil {
		return MergePrepareResult{}, err
	}
	var base string
	var targetBranch string
	if paused {
		prepared, err := reworkRebaseResult(ctx, info.Path, nil)
		if err != nil {
			return MergePrepareResult{}, err
		}
		pausedState, err := l.verifyPausedRebaseState(ctx, info.Path, issue, opts)
		if err != nil {
			return MergePrepareResult{}, err
		}
		onto := pausedState.Onto
		resolvedTarget := pausedState.ResolvedTarget
		activeCommit := pausedState.ActiveCommit
		canSkipEmptyApply := pausedState.CanSkipEmpty
		rescheduledPick := pausedState.RescheduledPick
		pendingCommits := pausedState.PendingCommits
		prepared.BaseSHA = onto
		base = pausedState.TargetBase
		targetBranch = resolvedTarget
		if prepared.Status == MergePrepareStatusConflict {
			if err := validate(ctx); err != nil {
				return MergePrepareResult{}, err
			}
			stillPaused, pausedErr := rebase.InProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			snapAfter, snapAfterErr := rebase.Snapshot(ctx, info.Path)
			if snapAfterErr != nil {
				return MergePrepareResult{}, snapAfterErr
			}
			if snapAfter != rebaseSnapshotBefore {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed before base recording", ErrMergeResolutionInvalid)
			}
			conflictsAfter, cAfterErr := reworkConflictPaths(ctx, info.Path)
			if cAfterErr != nil {
				return MergePrepareResult{}, cAfterErr
			}
			if !slices.Equal(conflictsAfter, prepared.ConflictPaths) {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase conflict paths changed before base recording", ErrMergeResolutionInvalid)
			}
			if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, resolvedTarget, onto); refErr != nil {
				return MergePrepareResult{}, refErr
			}
			return prepared, nil
		}
		unstaged, err := reworkHasUnstagedChanges(ctx, info.Path)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if unstaged {
			if err := validate(ctx); err != nil {
				return MergePrepareResult{}, err
			}
			stillPaused, pausedErr := rebase.InProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			snapAfter, snapAfterErr := rebase.Snapshot(ctx, info.Path)
			if snapAfterErr != nil {
				return MergePrepareResult{}, snapAfterErr
			}
			if snapAfter != rebaseSnapshotBefore {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed before base recording", ErrMergeResolutionInvalid)
			}
			unstagedAfter, uAfterErr := reworkHasUnstagedChanges(ctx, info.Path)
			if uAfterErr != nil {
				return MergePrepareResult{}, uAfterErr
			}
			if !unstagedAfter {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase dirty state changed before base recording", ErrMergeResolutionInvalid)
			}
			if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, resolvedTarget, onto); refErr != nil {
				return MergePrepareResult{}, refErr
			}
			prepared.Status = MergePrepareStatusDirty
			return prepared, nil
		}
		stagedBeforeCheck, sBeforeErr := runGitAt(ctx, info.Path, "diff", "--cached", "--ignore-submodules=none", "--name-only", "-z")
		if sBeforeErr != nil {
			return MergePrepareResult{}, sBeforeErr
		}
		if activeCommit != "" && (!rescheduledPick || stagedBeforeCheck != "") {
			headNow, headErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
			if headErr != nil {
				return MergePrepareResult{}, headErr
			}
			if rescheduledPick {
				if err := rebase.VerifyStagedExactReplayTree(ctx, info.Path, strings.TrimSpace(headNow), activeCommit); err != nil {
					return MergePrepareResult{}, err
				}
			} else if err := rebase.VerifyStagedConflictResolution(ctx, info.Path, strings.TrimSpace(headNow), activeCommit); err != nil {
				return MergePrepareResult{}, err
			}
		}
		rebaseSnapshotNow, snapErr := rebase.Snapshot(ctx, info.Path)
		if snapErr != nil {
			return MergePrepareResult{}, snapErr
		}
		if rebaseSnapshotNow != rebaseSnapshotBefore {
			return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed before continuation", ErrMergeResolutionInvalid)
		}
		stagedNow, sErr := runGitAt(ctx, info.Path, "diff", "--cached", "--ignore-submodules=none", "--name-only", "-z")
		if sErr != nil {
			return MergePrepareResult{}, sErr
		}
		if stagedNow == "" && !canSkipEmptyApply && !rescheduledPick {
			return MergePrepareResult{}, fmt.Errorf("%w: paused rebase resolution drops non-conflicted changes", ErrMergeResolutionInvalid)
		}
		if err := validate(ctx); err != nil {
			return MergePrepareResult{}, err
		}
		rebaseSnapshotAfterValidate, snapAfterErr := rebase.Snapshot(ctx, info.Path)
		if snapAfterErr != nil {
			return MergePrepareResult{}, snapAfterErr
		}
		if rebaseSnapshotAfterValidate != rebaseSnapshotBefore {
			return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed before continuation", ErrMergeResolutionInvalid)
		}
		unstagedBeforeContinue, uBeforeErr := reworkHasUnstagedChanges(ctx, info.Path)
		if uBeforeErr != nil {
			return MergePrepareResult{}, uBeforeErr
		}
		if unstagedBeforeContinue {
			return MergePrepareResult{}, fmt.Errorf("%w: paused rebase working tree changed before continuation", ErrMergeResolutionInvalid)
		}
		if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, resolvedTarget, onto); refErr != nil {
			return MergePrepareResult{}, refErr
		}
		if rescheduledPick {
			var stagedAddedPaths []string
			if stagedNow != "" {
				deletedRaw, delErr := runGitAt(ctx, info.Path, "diff", "--cached", "--no-renames", "--diff-filter=D", "--ignore-submodules=none", "--name-only", "-z")
				if delErr != nil {
					return MergePrepareResult{}, delErr
				}
				var deletedPaths []string
				for _, name := range strings.Split(strings.TrimSuffix(deletedRaw, "\x00"), "\x00") {
					if name != "" {
						deletedPaths = append(deletedPaths, name)
					}
				}
				addedRaw, addErr := runGitAt(ctx, info.Path, "diff", "--cached", "--no-renames", "--diff-filter=A", "--ignore-submodules=none", "--name-only", "-z")
				if addErr != nil {
					return MergePrepareResult{}, addErr
				}
				for _, name := range strings.Split(strings.TrimSuffix(addedRaw, "\x00"), "\x00") {
					if name != "" {
						stagedAddedPaths = append(stagedAddedPaths, name)
					}
				}
				if len(deletedPaths) > 0 && rebase.HasUntrackedResetObstruction(info.Path, deletedPaths, stagedAddedPaths) {
					return MergePrepareResult{}, fmt.Errorf("%w: paused rebase rescheduled pick reset would overwrite an untracked file", ErrMergeResolutionInvalid)
				}
			}
			var pendingAddedPaths []string
			if len(pendingCommits) > 0 {
				headForReplay, hReplayErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
				if hReplayErr != nil {
					return MergePrepareResult{}, hReplayErr
				}
				var replayErr error
				pendingAddedPaths, replayErr = gitexec.IsolatedReplayWritePaths(ctx, info.Path, strings.TrimSpace(headForReplay), pendingCommits)
				if replayErr != nil {
					return MergePrepareResult{}, replayErr
				}
			}
			if len(pendingAddedPaths) > 0 {
				seenLookup := make(map[string]struct{}, len(pendingAddedPaths))
				lookupPaths := make([]string, 0, len(pendingAddedPaths))
				for _, name := range pendingAddedPaths {
					for idx := strings.IndexByte(name, '/'); idx >= 0; {
						prefix := name[:idx]
						if _, seen := seenLookup[prefix]; !seen {
							seenLookup[prefix] = struct{}{}
							lookupPaths = append(lookupPaths, prefix)
						}
						next := strings.IndexByte(name[idx+1:], '/')
						if next < 0 {
							break
						}
						idx += 1 + next
					}
					if _, seen := seenLookup[name]; !seen {
						seenLookup[name] = struct{}{}
						lookupPaths = append(lookupPaths, name)
					}
				}
				lsArgs := append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--name-only", "HEAD", "--"}, lookupPaths...)
				headTrackedRaw, hErr := gitexec.ReadPublicationSource(ctx, info.Path, lsArgs...)
				if hErr != nil {
					return MergePrepareResult{}, hErr
				}
				trackedExisting := append([]string(nil), stagedAddedPaths...)
				for _, name := range strings.Split(strings.TrimSuffix(headTrackedRaw, "\x00"), "\x00") {
					if name != "" {
						trackedExisting = append(trackedExisting, name)
					}
				}
				if rebase.HasUntrackedResetObstruction(info.Path, pendingAddedPaths, trackedExisting) {
					return MergePrepareResult{}, fmt.Errorf("%w: paused rebase rescheduled pick would overwrite an untracked file", ErrMergeResolutionInvalid)
				}
			}
			if stagedNow != "" {
				if _, resetErr := runGitAt(ctx, info.Path, "reset", "--hard", "HEAD"); resetErr != nil {
					return MergePrepareResult{}, resetErr
				}
			}
		}
		continueArgs := []string{"-c", "rerere.enabled=false"}
		if rescheduledPick {
			continueArgs = append(continueArgs,
				"-c", "merge.directoryRenames=conflict",
				"-c", "merge.renames=true",
				"-c", "merge.renameLimit=0",
				"-c", "diff.renames=true",
				"-c", "diff.renameLimit=0",
			)
		}
		continueArgs = append(continueArgs, "rebase", "--continue")
		replayOut, replayErr := l.runCommit(ctx, info.Path, []string{"LC_ALL=C"}, continueArgs...)
		var commandErr *CommandError
		if errors.As(replayErr, &commandErr) && (strings.Contains(replayOut, "No changes - did you forget to use 'git add'?") || strings.Contains(commandErr.Output, "No changes - did you forget to use 'git add'?") || strings.Contains(commandErr.Output, `run "git rebase --skip"`)) {
			if conflicts, cErr := reworkConflictPaths(ctx, info.Path); cErr == nil && len(conflicts) == 0 {
				if staged, sErr := runGitAt(ctx, info.Path, "diff", "--cached", "--ignore-submodules=none", "--name-only", "-z"); sErr == nil && staged == "" {
					if !canSkipEmptyApply {
						return MergePrepareResult{}, fmt.Errorf("%w: paused apply rebase active commit has non-conflicted changes", ErrMergeResolutionInvalid)
					}
					if err := validate(ctx); err != nil {
						return MergePrepareResult{}, err
					}
					skipSnapAfter, snapSkipAfterErr := rebase.Snapshot(ctx, info.Path)
					if snapSkipAfterErr != nil {
						return MergePrepareResult{}, snapSkipAfterErr
					}
					if skipSnapAfter.Metadata != rebaseSnapshotBefore.Metadata {
						return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed before skip", ErrMergeResolutionInvalid)
					}
					conflictsAfter, cAfterErr := reworkConflictPaths(ctx, info.Path)
					if cAfterErr != nil {
						return MergePrepareResult{}, cAfterErr
					}
					if len(conflictsAfter) > 0 {
						return MergePrepareResult{}, fmt.Errorf("%w: paused rebase conflicts changed before skip", ErrMergeResolutionInvalid)
					}
					stagedAfter, sAfterErr := runGitAt(ctx, info.Path, "diff", "--cached", "--ignore-submodules=none", "--name-only", "-z")
					if sAfterErr != nil {
						return MergePrepareResult{}, sAfterErr
					}
					if stagedAfter != "" {
						return MergePrepareResult{}, fmt.Errorf("%w: paused rebase staged changes changed before skip", ErrMergeResolutionInvalid)
					}
					unstagedAfter, uAfterErr := reworkHasUnstagedChanges(ctx, info.Path)
					if uAfterErr != nil {
						return MergePrepareResult{}, uAfterErr
					}
					if unstagedAfter {
						return MergePrepareResult{}, fmt.Errorf("%w: paused rebase working tree changed before skip", ErrMergeResolutionInvalid)
					}
					_, replayErr = l.runCommit(ctx, info.Path, []string{"LC_ALL=C"}, "-c", "rerere.enabled=false", "rebase", "--skip")
				}
			}
		}
		replayed, err := reworkRebaseResult(ctx, info.Path, replayErr)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if replayed.Status == MergePrepareStatusDirty && errors.As(replayErr, &commandErr) && strings.Contains(commandErr.Output, "you have staged changes in your working tree") {
			if err := validate(ctx); err != nil {
				return MergePrepareResult{}, err
			}
			_, commitErr := l.runCommit(ctx, info.Path, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", "fix: complete "+issue.Identifier)
			if commitErr != nil {
				return MergePrepareResult{}, commitErr
			}
			if err := validate(ctx); err != nil {
				return MergePrepareResult{}, err
			}
			_, replayErr = l.runCommit(ctx, info.Path, []string{"LC_ALL=C"}, "-c", "rerere.enabled=false", "rebase", "--continue")
			replayed, err = reworkRebaseResult(ctx, info.Path, replayErr)
			if err != nil {
				return MergePrepareResult{}, err
			}
		}
		if replayed.Status != MergePrepareStatusClean {
			replayed.BaseSHA = prepared.BaseSHA
			return replayed, nil
		}
		paused, err = l.verifyReworkBranch(ctx, info, issue)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if paused {
			return MergePrepareResult{}, fmt.Errorf("%w: rebase is still paused", ErrMergeResolutionInvalid)
		}
		expectedHead = replayed.HeadSHA
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
	dirty, err := reworkWorktreeDirty(ctx, info.Path)
	if err != nil {
		return MergePrepareResult{}, err
	}
	if dirty {
		diff, err := gitdiff.Stat(ctx, info.Path, true)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if issue.Checkpoint == nil {
			return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
		}
		checkpoint := *issue.Checkpoint
		if err := l.fetchCheckpointRef(ctx, &checkpoint); err != nil {
			return MergePrepareResult{}, err
		}
		headBefore, err := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return MergePrepareResult{}, err
		}
		headBefore = strings.TrimSpace(headBefore)
		indexBefore, err := rebase.IndexChecksum(ctx, info.Path)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if checkpoint.Transaction != nil || !checkpointMatches(ctx, info.Path, &checkpoint) {
			return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
		}
		staged, err := runGitAt(ctx, info.Path, "diff", "--cached", "--raw", "HEAD")
		if err != nil {
			return MergePrepareResult{}, err
		}
		if staged != "" {
			staged, err = runGitAt(ctx, info.Path, "diff", "--cached", "--raw", checkpoint.TreeSHA)
			if err != nil {
				return MergePrepareResult{}, err
			}
			if staged != "" {
				return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
			}
		}
		if err := validate(ctx); err != nil {
			return MergePrepareResult{}, err
		}
		branchNow, branchErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
		if branchErr != nil || strings.TrimSpace(branchNow) != "refs/heads/"+info.Branch {
			return MergePrepareResult{}, fmt.Errorf("%w: rework branch changed", ErrMergeResolutionInvalid)
		}
		headNow, err := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return MergePrepareResult{}, err
		}
		indexNow, err := rebase.IndexChecksum(ctx, info.Path)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if strings.TrimSpace(headNow) != headBefore || indexNow != indexBefore {
			return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
		}
		if _, err := runGitAt(ctx, info.Path, "read-tree", checkpoint.TreeSHA); err != nil {
			return MergePrepareResult{}, fmt.Errorf("stage restored rework checkpoint: %w", err)
		}
		if _, err := l.runCommit(ctx, info.Path, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", "fix: restore "+issue.Identifier+" checkpoint"); err != nil {
			return MergePrepareResult{}, fmt.Errorf("commit restored rework checkpoint: %w", err)
		}
		restoredHead, restoredHeadErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
		if restoredHeadErr != nil {
			return MergePrepareResult{}, restoredHeadErr
		}
		expectedHead = strings.TrimSpace(restoredHead)
		dirty, err := reworkWorktreeDirty(ctx, info.Path)
		if err != nil {
			return MergePrepareResult{}, err
		}
		if dirty {
			return MergePrepareResult{Status: MergePrepareStatusDirty, DiffStat: diff}, nil
		}
	}
	if base == "" {
		base, targetBranch, err = l.resolveReworkTargetBase(ctx, info.Path, issue, opts)
		if err != nil {
			return MergePrepareResult{}, err
		}
	}
	if err := validate(ctx); err != nil {
		return MergePrepareResult{}, err
	}
	if issue.Source != nil {
		version := issue.Source.Version
		head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
		if err != nil {
			return MergePrepareResult{}, err
		}
		head = strings.TrimSpace(head)
		if expectedHead != "" && head != expectedHead {
			return MergePrepareResult{}, fmt.Errorf("%w: rework head changed during verification", ErrMergeResolutionInvalid)
		}
		_, sourceErr := runGitAt(ctx, info.Path, "merge-base", "--is-ancestor", version.HeadSHA, head)
		if sourceErr != nil {
			var commandErr *CommandError
			if !errors.As(sourceErr, &commandErr) || commandErr.ExitCode != 1 {
				return MergePrepareResult{}, fmt.Errorf("verify retained rework ancestry: %w", sourceErr)
			}
		}
		alreadyRebasedToBase := false
		if head != version.HeadSHA {
			for _, ref := range []string{worktreeReworkBranchRef(opts.TargetBranch), worktreeReworkBranchRef(targetBranch), worktreeReworkPendingRef} {
				if rawBase, recErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); recErr == nil && strings.TrimSpace(rawBase) == head {
					alreadyRebasedToBase = true
					break
				}
			}
		}
		if sourceErr == nil && !alreadyRebasedToBase {
			merged, err := runGitAt(ctx, info.Path, "merge-tree", "--write-tree", "--merge-base="+version.BaseSHA, base, head)
			if err != nil {
				var commandErr *CommandError
				if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
					return MergePrepareResult{}, fmt.Errorf("compare retained rework source: %w", err)
				}
			} else {
				tree, err := runGitAt(ctx, info.Path, "rev-parse", base+"^{tree}")
				if err != nil {
					return MergePrepareResult{}, err
				}
				if strings.TrimSpace(merged) == strings.TrimSpace(tree) {
					if err := validate(ctx); err != nil {
						return MergePrepareResult{}, err
					}
					branchNow, branchErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
					if branchErr != nil || strings.TrimSpace(branchNow) != "refs/heads/"+info.Branch {
						return MergePrepareResult{}, fmt.Errorf("%w: rework branch changed", ErrMergeResolutionInvalid)
					}
					headNow, headErr := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
					if headErr != nil {
						return MergePrepareResult{}, headErr
					}
					if strings.TrimSpace(headNow) != head {
						return MergePrepareResult{}, fmt.Errorf("%w: rework head changed during verification", ErrMergeResolutionInvalid)
					}
					if base != head {
						if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, targetBranch, base); refErr != nil {
							return MergePrepareResult{}, refErr
						}
					}
					dirtyNow, dirtyErr := reworkWorktreeDirty(ctx, info.Path)
					if dirtyErr != nil {
						return MergePrepareResult{}, dirtyErr
					}
					if dirtyNow {
						diff, diffErr := gitdiff.Stat(ctx, info.Path, true)
						if diffErr != nil {
							return MergePrepareResult{}, diffErr
						}
						return MergePrepareResult{Status: MergePrepareStatusDirty, BaseSHA: version.BaseSHA, HeadSHA: head, DiffStat: diff}, nil
					}
					return MergePrepareResult{Status: MergePrepareStatusClean, BaseSHA: version.BaseSHA, HeadSHA: head}, nil
				}
			}
		}
	}
	if err := validate(ctx); err != nil {
		return MergePrepareResult{}, err
	}
	branchNow, branchErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
	if branchErr != nil || strings.TrimSpace(branchNow) != "refs/heads/"+info.Branch {
		return MergePrepareResult{}, fmt.Errorf("%w: rework branch changed", ErrMergeResolutionInvalid)
	}
	headBeforeStart, headBeforeStartErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
	if headBeforeStartErr != nil {
		return MergePrepareResult{}, headBeforeStartErr
	}
	if expectedHead != "" && strings.TrimSpace(headBeforeStart) != expectedHead {
		return MergePrepareResult{}, fmt.Errorf("%w: rework head changed before rebase", ErrMergeResolutionInvalid)
	}
	if gitexec.ValidHead(base) {
		if prevPending, pErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "--quiet", worktreeReworkPendingRef+"^{commit}"); pErr == nil {
			if prev := strings.TrimSpace(prevPending); prev != "" && prev == strings.TrimSpace(headBeforeStart) {
				if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, targetBranch, prev); refErr != nil {
					return MergePrepareResult{}, refErr
				}
			}
		}
		if _, refErr := runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "update-ref", "--no-deref", worktreeReworkPendingRef, base); refErr != nil {
			return MergePrepareResult{}, fmt.Errorf("record pending rework base: %w", refErr)
		}
	}
	_, rebaseErr := l.runCommit(ctx, info.Path, []string{"LC_ALL=C"}, "-c", "rerere.enabled=false", "rebase", "--no-update-refs", "--no-autostash", "--no-rebase-merges", base)
	prepared, err := reworkRebaseResult(ctx, info.Path, rebaseErr)
	if err == nil && (prepared.Status == MergePrepareStatusClean || prepared.Status == MergePrepareStatusConflict) {
		var conflictSnapBefore rebase.MetadataSnapshot
		if prepared.Status == MergePrepareStatusConflict {
			snap, snapErr := rebase.Snapshot(ctx, info.Path)
			if snapErr != nil {
				return MergePrepareResult{}, snapErr
			}
			conflictSnapBefore = snap
		}
		if valErr := validate(ctx); valErr != nil {
			return MergePrepareResult{}, valErr
		}
		if prepared.Status == MergePrepareStatusClean {
			branchAfter, branchAfterErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
			if branchAfterErr != nil || strings.TrimSpace(branchAfter) != "refs/heads/"+info.Branch {
				return MergePrepareResult{}, fmt.Errorf("%w: rework branch changed after rebase", ErrMergeResolutionInvalid)
			}
			headAfter, headAfterErr := gitexec.ReadPublicationSource(ctx, info.Path, "rev-parse", "--verify", "HEAD^{commit}")
			if headAfterErr != nil {
				return MergePrepareResult{}, headAfterErr
			}
			if strings.TrimSpace(headAfter) != prepared.HeadSHA {
				return MergePrepareResult{}, fmt.Errorf("%w: rework head changed after rebase", ErrMergeResolutionInvalid)
			}
		}
		if prepared.Status == MergePrepareStatusConflict {
			stillPaused, pausedErr := rebase.InProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			conflictSnapAfter, snapAfterErr := rebase.Snapshot(ctx, info.Path)
			if snapAfterErr != nil {
				return MergePrepareResult{}, snapAfterErr
			}
			if conflictSnapAfter != conflictSnapBefore {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase state changed after rebase", ErrMergeResolutionInvalid)
			}
			conflictsAfter, cAfterErr := reworkConflictPaths(ctx, info.Path)
			if cAfterErr != nil {
				return MergePrepareResult{}, cAfterErr
			}
			if !slices.Equal(conflictsAfter, prepared.ConflictPaths) {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase conflict paths changed after rebase", ErrMergeResolutionInvalid)
			}
		}
		if refErr := recordWorktreeReworkBase(ctx, info.Path, opts.TargetBranch, targetBranch, base); refErr != nil {
			err = refErr
		}
	}
	if err == nil && prepared.Status == MergePrepareStatusClean {
		dirtyNow, dirtyErr := reworkWorktreeDirty(ctx, info.Path)
		if dirtyErr != nil {
			return MergePrepareResult{}, dirtyErr
		}
		if dirtyNow {
			diff, diffErr := gitdiff.Stat(ctx, info.Path, true)
			if diffErr != nil {
				return MergePrepareResult{}, diffErr
			}
			prepared.Status = MergePrepareStatusDirty
			prepared.DiffStat = diff
		}
	}
	prepared.BaseSHA = base
	return prepared, err
}

func recordWorktreeReworkBase(ctx context.Context, path, requestedTarget, resolvedTarget, base string) error {
	if !gitexec.ValidHead(base) {
		return nil
	}
	recordRefs := []string{worktreeReworkBranchRef(resolvedTarget)}
	if strings.TrimSpace(requestedTarget) == "" && strings.TrimSpace(resolvedTarget) != "" {
		recordRefs = append(recordRefs, worktreeReworkDefaultRef)
	}
	for _, targetRef := range recordRefs {
		if _, err := runGitAt(ctx, path, "-c", "core.hooksPath="+os.DevNull, "update-ref", "--no-deref", targetRef, base); err != nil {
			return fmt.Errorf("record verified rework base: %w", err)
		}
	}
	return nil
}

func (l *LocalGit) resolveReworkFetchRepository(ctx context.Context, path, remote string, issue Issue) (string, error) {
	if configured := strings.TrimSpace(issue.PublicationRepository); configured != "" {
		repository := gitexec.PublicationRepository(ctx, "", configured)
		if repository == "" {
			return "", refuse(LandRefusalProtected, "the configured project publication repository cannot be resolved")
		}
		verifiedURL, err := gitexec.VerifyPublicationRemoteFetchURL(ctx, path, remote, repository)
		if err != nil {
			return "", refuse(LandRefusalProtected, err.Error())
		}
		return verifiedURL, nil
	}
	rawURL, err := gitexec.RunRemoteFetch(ctx, path, "remote", "get-url", remote)
	if err != nil {
		return "", &ReworkPreparationError{Reason: ReworkPreparationFetchFailed, Err: err}
	}
	fetchRepo := strings.TrimSuffix(rawURL, "\n")
	if err := gitexec.VerifyRemoteFetchDestination(ctx, path, fetchRepo); err != nil {
		return "", refuse(LandRefusalProtected, err.Error())
	}
	return fetchRepo, nil
}

func (l *LocalGit) resolveRemoteHeadTarget(ctx context.Context, path, remote string, issue Issue) (string, string, error) {
	if remote == "" {
		remote = defaultGitRemote
	}
	fetchRepo, err := l.resolveReworkFetchRepository(ctx, path, remote, issue)
	if err != nil {
		return "", "", err
	}
	output, err := gitexec.ReadRemoteRefs(ctx, path, fetchRepo, true, "HEAD")
	if err != nil {
		return "", "", err
	}
	var headSHA string
	var branch string
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" && strings.HasPrefix(fields[1], "refs/heads/") {
			branch = strings.TrimPrefix(fields[1], "refs/heads/")
			continue
		}
		if len(fields) >= 2 && fields[1] == "HEAD" && gitexec.ValidHead(fields[0]) {
			headSHA = fields[0]
		}
	}
	return headSHA, branch, nil
}

func (l *LocalGit) resolveReworkTargetBase(ctx context.Context, path string, issue Issue, opts MergePrepareOptions) (string, string, error) {
	if issue.ReleaseDelivery != nil {
		return issue.ReleaseDelivery.BaseSHA, strings.TrimSpace(opts.TargetBranch), nil
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	fetchRepo, err := l.resolveReworkFetchRepository(ctx, path, remote, issue)
	if err != nil {
		return "", "", err
	}
	target := strings.TrimSpace(opts.TargetBranch)
	if target == "" {
		target, err = gitexec.IsolatedRemoteDefaultBranch(ctx, path, fetchRepo)
		if err != nil {
			return "", "", err
		}
	}
	base, err := gitexec.FetchRemoteBranch(ctx, path, remote, fetchRepo, target)
	if err != nil {
		return "", "", &ReworkPreparationError{Reason: ReworkPreparationFetchFailed, Err: err}
	}
	return base, target, nil
}

func (l *LocalGit) verifyPausedRebaseState(ctx context.Context, path string, issue Issue, opts MergePrepareOptions) (rebase.State, error) {
	return rebase.VerifyPaused(ctx, path, func(ctx context.Context) (string, string, error) {
		return l.resolveReworkTargetBase(ctx, path, issue, opts)
	})
}

type SourceFinalizationIncomplete struct {
	Status MergePrepareStatus
}

func (e *SourceFinalizationIncomplete) Error() string {
	if e.Status == MergePrepareStatusConflict {
		return "source finalization has unresolved rebase conflicts"
	}
	return "source finalization has uncommitted changes"
}

func (*SourceFinalizationIncomplete) Unwrap() error { return ErrMergeResolutionInvalid }

func verifyFinalizedWorktree(ctx context.Context, path string) error {
	dirty, err := reworkWorktreeDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return &SourceFinalizationIncomplete{Status: MergePrepareStatusDirty}
	}
	return nil
}

func (l *LocalGit) FinalizeNativeWork(ctx context.Context, info Info, issue Issue, validate func(context.Context) error) (string, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	releaseWorktree, err := acquireWorktreeOperation(ctx, info.Path)
	if err != nil {
		return "", err
	}
	defer releaseWorktree()
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
		headBefore, err := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return "", err
		}
		headBefore = strings.TrimSpace(headBefore)
		indexBefore, err := rebase.IndexChecksum(ctx, info.Path)
		if err != nil {
			return "", err
		}
		unstaged, err := reworkHasUnstagedChanges(ctx, info.Path)
		if err != nil {
			return "", err
		}
		if unstaged {
			return "", &SourceFinalizationIncomplete{Status: MergePrepareStatusDirty}
		}
		staged, err := runGitAt(ctx, info.Path, "diff", "--cached", "--ignore-submodules=none", "--name-only", "-z")
		if err != nil {
			return "", err
		}
		if staged != "" && issue.NoChangeNeeded {
			return "", errors.New("no-change completion requires no staged source changes")
		}
		if staged != "" {
			if err := validate(ctx); err != nil {
				return "", err
			}
			branchNow, branchErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
			if branchErr != nil || strings.TrimSpace(branchNow) != "refs/heads/"+info.Branch {
				return "", fmt.Errorf("%w: finalized branch changed before commit", ErrMergeResolutionInvalid)
			}
			headNow, headErr := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
			if headErr != nil {
				return "", headErr
			}
			indexNow, indexErr := rebase.IndexChecksum(ctx, info.Path)
			if indexErr != nil {
				return "", indexErr
			}
			if strings.TrimSpace(headNow) != headBefore || indexNow != indexBefore {
				return "", fmt.Errorf("%w: finalized checkout changed before commit", ErrMergeResolutionInvalid)
			}
			unstagedNow, unstagedErr := reworkHasUnstagedChanges(ctx, info.Path)
			if unstagedErr != nil {
				return "", unstagedErr
			}
			if unstagedNow {
				return "", &SourceFinalizationIncomplete{Status: MergePrepareStatusDirty}
			}
			_, commitErr := l.runCommit(ctx, info.Path, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", "fix: complete "+issue.Identifier)
			if commitErr != nil {
				return "", commitErr
			}
		}
	}
	if !issue.NativeRework && !issue.NoChangeNeeded && !paused {
		return "", verifyFinalizedWorktree(ctx, info.Path)
	}
	if err := validate(ctx); err != nil {
		return "", err
	}
	prepared, err := l.prepareRework(ctx, info, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef}, validate)
	if err != nil {
		return "", err
	}
	if prepared.Status == MergePrepareStatusDirty || prepared.Status == MergePrepareStatusConflict {
		return "", &SourceFinalizationIncomplete{Status: prepared.Status}
	}
	if !issue.NoChangeNeeded && issue.Source != nil && prepared.HeadSHA == prepared.BaseSHA && prepared.HeadSHA != issue.Source.Version.HeadSHA {
		if err := validate(ctx); err != nil {
			return "", err
		}
		branchNow, branchErr := runGitAt(ctx, info.Path, "symbolic-ref", "HEAD")
		if branchErr != nil || strings.TrimSpace(branchNow) != "refs/heads/"+info.Branch {
			return "", fmt.Errorf("%w: finalized branch changed before reconciliation commit", ErrMergeResolutionInvalid)
		}
		headNow, headErr := runGitAt(ctx, info.Path, "rev-parse", "--verify", "HEAD")
		if headErr != nil {
			return "", headErr
		}
		if strings.TrimSpace(headNow) != prepared.HeadSHA {
			return "", fmt.Errorf("%w: finalized head changed before reconciliation commit", ErrMergeResolutionInvalid)
		}
		if err := verifyFinalizedWorktree(ctx, info.Path); err != nil {
			return "", err
		}
		_, commitErr := l.runCommit(ctx, info.Path, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "--allow-empty", "-m", "fix: reconcile "+issue.Identifier)
		if commitErr != nil {
			return "", commitErr
		}
	}
	return prepared.BaseSHA, verifyFinalizedWorktree(ctx, info.Path)
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
	paused, err := rebase.InProgress(ctx, info.Path)
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
	if ref, err := gitexec.ReadPublicationSource(ctx, info.Path, "symbolic-ref", "--quiet", "HEAD"); err == nil && strings.TrimSpace(ref) != "" {
		return false, fmt.Errorf("%w: paused rebase HEAD must be detached", ErrMergeResolutionInvalid)
	} else if err != nil {
		var commandErr *gitexec.CommandError
		if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
			return false, err
		}
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
		name, err := rebase.ReadRegularGitFile(filepath.Join(dir, "head-name"))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(name)) != "refs/heads/"+info.Branch {
			return false, fmt.Errorf("%w: paused rebase belongs to another branch", ErrMergeResolutionInvalid)
		}
		original, err := rebase.ReadRegularGitFile(filepath.Join(dir, "orig-head"))
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
		updates, err := rebase.ReadRegularGitFile(filepath.Join(dir, "update-refs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(updates)) != "" {
			return false, fmt.Errorf("%w: paused rebase updates additional refs", ErrMergeResolutionInvalid)
		}
		autostash, err := rebase.ReadRegularGitFile(filepath.Join(dir, "autostash"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(autostash)) != "" {
			return false, fmt.Errorf("%w: paused rebase carries autostash changes", ErrMergeResolutionInvalid)
		}
		for _, meta := range []string{"strategy", "strategy_opts"} {
			rawMeta, metaErr := rebase.ReadRegularGitFile(filepath.Join(dir, meta))
			if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
				return false, metaErr
			}
			if strings.TrimSpace(string(rawMeta)) != "" {
				return false, fmt.Errorf("%w: paused rebase uses custom merge strategy", ErrMergeResolutionInvalid)
			}
		}
		if kind == "rebase-merge" {
			todo, err := rebase.ReadRegularGitFile(filepath.Join(dir, "git-rebase-todo"))
			if err != nil {
				return false, err
			}
			for _, line := range strings.Split(string(todo), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) < 2 || (fields[0] != "pick" && fields[0] != "p") {
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
		classified := reworkPreparationError(rebaseErr)
		var preparationErr *ReworkPreparationError
		if errors.As(classified, &preparationErr) && preparationErr.Reason == ReworkPreparationDirtyWorktree {
			return MergePrepareResult{Status: MergePrepareStatusDirty, Message: "Preserve the retained source changes and finish the rebase."}, nil
		}
		return MergePrepareResult{}, classified
	}
	head, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return MergePrepareResult{}, err
	}
	return MergePrepareResult{Status: MergePrepareStatusClean, HeadSHA: strings.TrimSpace(head)}, nil
}
