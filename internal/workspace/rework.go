package workspace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
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
		raw, readErr := readRegularGitFile(ontoPath)
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
	stat, err := gitDiffStat(ctx, replay, true)
	if err != nil {
		return false, err
	}
	if workspaceRecoveryFingerprint(sourceHead, stat.Fingerprint) != sourceDigest {
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
		actual, err := readRegularGitFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		expected, err := readRegularGitFile(filepath.Join(replayDir, name))
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
	var rebaseSnapshotBefore rebaseSnapshot
	if active, activeErr := rebaseInProgress(ctx, info.Path); activeErr == nil && active {
		snap, snapErr := pausedRebaseSnapshot(ctx, info.Path)
		if snapErr != nil {
			return MergePrepareResult{}, snapErr
		}
		rebaseSnapshotBefore = snap
	}
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil {
		if active, stateErr := rebaseInProgress(ctx, info.Path); stateErr == nil && active {
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
		onto := pausedState.onto
		resolvedTarget := pausedState.resolvedTarget
		activeCommit := pausedState.activeCommit
		canSkipEmptyApply := pausedState.canSkipEmpty
		rescheduledPick := pausedState.rescheduledPick
		pendingCommits := pausedState.pendingCommits
		prepared.BaseSHA = onto
		base = pausedState.targetBase
		targetBranch = resolvedTarget
		if prepared.Status == MergePrepareStatusConflict {
			if err := validate(ctx); err != nil {
				return MergePrepareResult{}, err
			}
			stillPaused, pausedErr := rebaseInProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			snapAfter, snapAfterErr := pausedRebaseSnapshot(ctx, info.Path)
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
			stillPaused, pausedErr := rebaseInProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			snapAfter, snapAfterErr := pausedRebaseSnapshot(ctx, info.Path)
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
				if err := verifyStagedExactReplayTree(ctx, info.Path, strings.TrimSpace(headNow), activeCommit); err != nil {
					return MergePrepareResult{}, err
				}
			} else if err := verifyStagedConflictResolution(ctx, info.Path, strings.TrimSpace(headNow), activeCommit); err != nil {
				return MergePrepareResult{}, err
			}
		}
		rebaseSnapshotNow, snapErr := pausedRebaseSnapshot(ctx, info.Path)
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
		rebaseSnapshotAfterValidate, snapAfterErr := pausedRebaseSnapshot(ctx, info.Path)
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
				if len(deletedPaths) > 0 && hasUntrackedResetObstruction(info.Path, deletedPaths, stagedAddedPaths) {
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
				if hasUntrackedResetObstruction(info.Path, pendingAddedPaths, trackedExisting) {
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
					skipSnapAfter, snapSkipAfterErr := pausedRebaseSnapshot(ctx, info.Path)
					if snapSkipAfterErr != nil {
						return MergePrepareResult{}, snapSkipAfterErr
					}
					if skipSnapAfter.meta != rebaseSnapshotBefore.meta {
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
		diff, err := gitDiffStat(ctx, info.Path, true)
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
		indexBefore, err := gitIndexChecksum(ctx, info.Path)
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
		indexNow, err := gitIndexChecksum(ctx, info.Path)
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
						diff, diffErr := gitDiffStat(ctx, info.Path, true)
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
		var conflictSnapBefore rebaseSnapshot
		if prepared.Status == MergePrepareStatusConflict {
			snap, snapErr := pausedRebaseSnapshot(ctx, info.Path)
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
			stillPaused, pausedErr := rebaseInProgress(ctx, info.Path)
			if pausedErr != nil {
				return MergePrepareResult{}, pausedErr
			}
			if !stillPaused {
				return MergePrepareResult{}, fmt.Errorf("%w: paused rebase ended before base recording", ErrMergeResolutionInvalid)
			}
			conflictSnapAfter, snapAfterErr := pausedRebaseSnapshot(ctx, info.Path)
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
			diff, diffErr := gitDiffStat(ctx, info.Path, true)
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
		repository := gitexec.PublicationRepository("", configured)
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

type pausedRebaseState struct {
	onto            string
	targetBase      string
	resolvedTarget  string
	activeCommit    string
	canSkipEmpty    bool
	rescheduledPick bool
	pendingCommits  []string
}

func (l *LocalGit) verifyPausedRebaseState(ctx context.Context, path string, issue Issue, opts MergePrepareOptions) (pausedRebaseState, error) {
	var onto string
	var rebaseKind string
	for _, kind := range []string{"rebase-merge", "rebase-apply"} {
		ontoPath, err := gitPathFor(ctx, path, kind+"/onto")
		if err != nil {
			return pausedRebaseState{}, err
		}
		raw, err := readRegularGitFile(ontoPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return pausedRebaseState{}, err
		}
		onto = strings.TrimSpace(string(raw))
		rebaseKind = kind
		break
	}
	if onto == "" {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase integration base is unavailable", ErrMergeResolutionInvalid)
	}
	if !gitexec.ValidHead(onto) {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase integration base is invalid", ErrMergeResolutionInvalid)
	}
	base, target, err := l.resolveReworkTargetBase(ctx, path, issue, opts)
	if err != nil {
		return pausedRebaseState{}, err
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, base); err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase target is not on the selected integration branch", ErrMergeResolutionInvalid)
		}
		return pausedRebaseState{}, fmt.Errorf("verify paused rebase integration base: %w", err)
	}
	head, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return pausedRebaseState{}, err
	}
	head = strings.TrimSpace(head)
	if !gitexec.ValidHead(head) {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head is invalid", ErrMergeResolutionInvalid)
	}
	headNamePath, err := gitPathFor(ctx, path, rebaseKind+"/head-name")
	if err != nil {
		return pausedRebaseState{}, err
	}
	rawHeadName, err := readRegularGitFile(headNamePath)
	if err != nil {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase branch metadata is unavailable", ErrMergeResolutionInvalid)
	}
	headName := strings.TrimSpace(string(rawHeadName))
	if !strings.HasPrefix(headName, "refs/heads/") {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase branch metadata is invalid", ErrMergeResolutionInvalid)
	}
	branchTip, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", headName+"^{commit}")
	if err != nil {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase branch tip is unavailable", ErrMergeResolutionInvalid)
	}
	branchTip = strings.TrimSpace(branchTip)
	origHeadPath, err := gitPathFor(ctx, path, rebaseKind+"/orig-head")
	if err != nil {
		return pausedRebaseState{}, err
	}
	rawOrigHead, err := readRegularGitFile(origHeadPath)
	if err != nil {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase original head is unavailable", ErrMergeResolutionInvalid)
	}
	origHead := strings.TrimSpace(string(rawOrigHead))
	if !gitexec.ValidHead(origHead) || origHead != branchTip {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase original head does not match assigned branch", ErrMergeResolutionInvalid)
	}
	expectedHead := onto
	maxReplayed := 0
	var accounted []string
	var stoppedCommit string
	var rescheduledPick bool
	var pendingCommits []string
	if rebaseKind == "rebase-merge" {
		rewrittenPath, err := gitPathFor(ctx, path, "rebase-merge/rewritten-list")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawRewritten, err := readRegularGitFile(rewrittenPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return pausedRebaseState{}, err
		}
		rewritten := make(map[string]string)
		for line := range strings.SplitSeq(string(rawRewritten), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) < 2 || !gitexec.ValidHead(fields[0]) || !gitexec.ValidHead(fields[1]) {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase rewritten commit list is invalid", ErrMergeResolutionInvalid)
			}
			if _, dup := rewritten[fields[0]]; dup {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase duplicates a rewritten commit", ErrMergeResolutionInvalid)
			}
			if err := gitexec.VerifyPublicationAncestry(ctx, path, fields[0], origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return pausedRebaseState{}, fmt.Errorf("%w: paused rebase replayed commit is not on the assigned branch", ErrMergeResolutionInvalid)
				}
				return pausedRebaseState{}, fmt.Errorf("verify paused rebase source commit: %w", err)
			}
			rewritten[fields[0]] = fields[1]
		}
		stoppedPath, err := gitPathFor(ctx, path, "rebase-merge/stopped-sha")
		if err != nil {
			return pausedRebaseState{}, err
		}
		if rawStopped, sErr := readRegularGitFile(stoppedPath); sErr == nil {
			stopped := strings.TrimSpace(string(rawStopped))
			if stopped != "" {
				if !gitexec.ValidHead(stopped) {
					return pausedRebaseState{}, fmt.Errorf("%w: paused rebase stopped commit is invalid", ErrMergeResolutionInvalid)
				}
				if err := gitexec.VerifyPublicationAncestry(ctx, path, stopped, origHead); err != nil {
					var commandErr *gitexec.CommandError
					if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
						return pausedRebaseState{}, fmt.Errorf("%w: paused rebase stopped commit is not on the assigned branch", ErrMergeResolutionInvalid)
					}
					return pausedRebaseState{}, fmt.Errorf("verify paused rebase stopped commit: %w", err)
				}
				stoppedCommit = stopped
			}
		} else if !errors.Is(sErr, os.ErrNotExist) {
			return pausedRebaseState{}, sErr
		}
		donePath, err := gitPathFor(ctx, path, "rebase-merge/done")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawDone, doneErr := readRegularGitFile(donePath)
		if doneErr != nil && !errors.Is(doneErr, os.ErrNotExist) {
			return pausedRebaseState{}, doneErr
		}
		if errors.Is(doneErr, os.ErrNotExist) && len(rewritten) > 0 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase completed step log is unavailable", ErrMergeResolutionInvalid)
		}
		var doneSources []string
		for line := range strings.SplitSeq(string(rawDone), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || (fields[0] != "pick" && fields[0] != "p") {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase completed log contains operations outside source replay", ErrMergeResolutionInvalid)
			}
			pickSHA, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--end-of-options", fields[1]+"^{commit}")
			if err != nil {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase completed commit is invalid", ErrMergeResolutionInvalid)
			}
			pickSHA = strings.TrimSpace(pickSHA)
			if err := gitexec.VerifyPublicationAncestry(ctx, path, pickSHA, origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return pausedRebaseState{}, fmt.Errorf("%w: paused rebase completed commit is not on the assigned branch", ErrMergeResolutionInvalid)
				}
				return pausedRebaseState{}, fmt.Errorf("verify paused rebase completed commit: %w", err)
			}
			if len(doneSources) == 0 || doneSources[len(doneSources)-1] != pickSHA {
				doneSources = append(doneSources, pickSHA)
			}
		}
		todoPath, err := gitPathFor(ctx, path, "rebase-merge/git-rebase-todo")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawTodo, err := readRegularGitFile(todoPath)
		if err != nil {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase todo is unavailable", ErrMergeResolutionInvalid)
		}
		var todoSources []string
		for line := range strings.SplitSeq(string(rawTodo), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 || (fields[0] != "pick" && fields[0] != "p") {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase contains operations outside source replay", ErrMergeResolutionInvalid)
			}
			pickSHA, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--end-of-options", fields[1]+"^{commit}")
			if err != nil {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase pending commit is invalid", ErrMergeResolutionInvalid)
			}
			todoSources = append(todoSources, strings.TrimSpace(pickSHA))
		}
		var rescheduledCommit string
		if stoppedCommit != "" && len(doneSources) > 0 && doneSources[len(doneSources)-1] == stoppedCommit {
			doneSources = doneSources[:len(doneSources)-1]
		} else if stoppedCommit == "" && len(doneSources) > 0 && len(todoSources) > 0 && doneSources[len(doneSources)-1] == todoSources[0] {
			doneSources = doneSources[:len(doneSources)-1]
			rescheduledPick = true
			rescheduledCommit = todoSources[0]
		}
		consumedRewritten := make(map[string]struct{}, len(rewritten))
		for _, src := range doneSources {
			if dst, ok := rewritten[src]; ok {
				consumedRewritten[src] = struct{}{}
				if err := verifyReplayedCommit(ctx, path, expectedHead, src, dst); err != nil {
					return pausedRebaseState{}, err
				}
				expectedHead = dst
				maxReplayed++
				accounted = append(accounted, src)
				continue
			}
			fastForwarded := false
			srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
			if pErr != nil {
				var commandErr *gitexec.CommandError
				if !errors.As(pErr, &commandErr) || commandErr.ExitCode != 1 {
					return pausedRebaseState{}, pErr
				}
			} else if strings.TrimSpace(srcParent) == expectedHead {
				isAnc, ancErr := gitexec.VerifyPublicationAncestryComplete(ctx, path, src, head)
				if ancErr != nil {
					return pausedRebaseState{}, ancErr
				}
				if isAnc {
					expectedHead = src
					maxReplayed++
					fastForwarded = true
				}
			}
			if !fastForwarded {
				if err := verifyCommitRedundant(ctx, path, expectedHead, src); err != nil {
					return pausedRebaseState{}, err
				}
			}
			accounted = append(accounted, src)
		}
		if len(consumedRewritten) != len(rewritten) {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase rewritten commit list does not match completed rebase steps", ErrMergeResolutionInvalid)
		}
		if stoppedCommit != "" {
			accounted = append(accounted, stoppedCommit)
		}
		accounted = append(accounted, todoSources...)
		pendingCommits = todoSources
		if err := verifyAssignedCommitsAccounted(ctx, path, onto, origHead, accounted); err != nil {
			return pausedRebaseState{}, err
		}
		if rescheduledPick && stoppedCommit == "" {
			stoppedCommit = rescheduledCommit
		}
	} else {
		nextPath, err := gitPathFor(ctx, path, "rebase-apply/next")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawNext, err := readRegularGitFile(nextPath)
		if err != nil {
			return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase state is unavailable", ErrMergeResolutionInvalid)
		}
		nextNum, err := strconv.Atoi(strings.TrimSpace(string(rawNext)))
		if err != nil || nextNum < 1 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch index is invalid", ErrMergeResolutionInvalid)
		}
		lastPath, err := gitPathFor(ctx, path, "rebase-apply/last")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawLast, err := readRegularGitFile(lastPath)
		if err != nil {
			return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch count is unavailable", ErrMergeResolutionInvalid)
		}
		lastNum, err := strconv.Atoi(strings.TrimSpace(string(rawLast)))
		if err != nil || lastNum < nextNum || lastNum > 10000 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch count is invalid", ErrMergeResolutionInvalid)
		}
		maxReplayed = nextNum - 1
		var appliedSources []string
		var activeSource string
		for i := 1; i <= lastNum; i++ {
			patchPath, err := gitPathFor(ctx, path, fmt.Sprintf("rebase-apply/%04d", i))
			if err != nil {
				return pausedRebaseState{}, err
			}
			firstLine, err := readRegularGitFirstLine(patchPath)
			if err != nil {
				return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch metadata is unavailable", ErrMergeResolutionInvalid)
			}
			fields := strings.Fields(firstLine)
			if len(fields) < 2 || fields[0] != "From" || !gitexec.ValidHead(fields[1]) {
				return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch source is invalid", ErrMergeResolutionInvalid)
			}
			if err := gitexec.VerifyPublicationAncestry(ctx, path, fields[1], origHead); err != nil {
				var commandErr *gitexec.CommandError
				if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
					return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase patch is not on the assigned branch", ErrMergeResolutionInvalid)
				}
				return pausedRebaseState{}, fmt.Errorf("verify paused apply rebase source commit: %w", err)
			}
			accounted = append(accounted, fields[1])
			if i < nextNum {
				appliedSources = append(appliedSources, fields[1])
			} else {
				if err := verifyApplyPatchMatchesCommit(ctx, path, patchPath, fields[1], true); err != nil {
					return pausedRebaseState{}, err
				}
				if i == nextNum {
					activeSource = fields[1]
				}
			}
		}
		if err := verifyAssignedCommitsAccounted(ctx, path, onto, origHead, accounted); err != nil {
			return pausedRebaseState{}, err
		}
		if activeSource != "" {
			activePatchPath, err := gitPathFor(ctx, path, "rebase-apply/patch")
			if err != nil {
				return pausedRebaseState{}, err
			}
			if err := verifyActiveApplyPatchMatchesCommit(ctx, path, activePatchPath, activeSource); err != nil {
				return pausedRebaseState{}, err
			}
		}
		abortSafetyPath, err := gitPathFor(ctx, path, "rebase-apply/abort-safety")
		if err != nil {
			return pausedRebaseState{}, err
		}
		rawSafety, err := readRegularGitFile(abortSafetyPath)
		if err == nil {
			safety := strings.TrimSpace(string(rawSafety))
			if !gitexec.ValidHead(safety) {
				return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase safety head is invalid", ErrMergeResolutionInvalid)
			}
			expectedHead = safety
		} else if !errors.Is(err, os.ErrNotExist) {
			return pausedRebaseState{}, err
		} else if maxReplayed > 0 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase safety head is unavailable", ErrMergeResolutionInvalid)
		}
		if head != expectedHead {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head does not match active rebase state", ErrMergeResolutionInvalid)
		}
		if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, head); err != nil {
			var commandErr *gitexec.CommandError
			if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
				return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head does not descend from its target", ErrMergeResolutionInvalid)
			}
			return pausedRebaseState{}, fmt.Errorf("verify paused rebase head ancestry: %w", err)
		}
		rawReplayed, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--reverse", onto+".."+head)
		if err != nil {
			return pausedRebaseState{}, err
		}
		replayed := strings.Fields(rawReplayed)
		if len(replayed) > len(appliedSources) {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head contains unexpected commits", ErrMergeResolutionInvalid)
		}
		prev := onto
		srcIdx := 0
		for _, dst := range replayed {
			matched := false
			for srcIdx < len(appliedSources) {
				src := appliedSources[srcIdx]
				srcIdx++
				replayCheckErr := verifyReplayedCommit(ctx, path, prev, src, dst)
				if replayCheckErr == nil {
					matched = true
					break
				}
				if !errors.Is(replayCheckErr, ErrMergeResolutionInvalid) {
					return pausedRebaseState{}, replayCheckErr
				}
				if err := verifyCommitRedundant(ctx, path, prev, src); err != nil {
					return pausedRebaseState{}, err
				}
			}
			if !matched {
				return pausedRebaseState{}, fmt.Errorf("%w: paused apply rebase replayed commit does not match assigned source", ErrMergeResolutionInvalid)
			}
			prev = dst
		}
		for srcIdx < len(appliedSources) {
			if err := verifyCommitRedundant(ctx, path, prev, appliedSources[srcIdx]); err != nil {
				return pausedRebaseState{}, err
			}
			srcIdx++
		}
		var canSkipEmptyApply bool
		if activeSource != "" {
			skipErr := verifyCommitRedundant(ctx, path, head, activeSource)
			if skipErr == nil {
				canSkipEmptyApply = true
			} else if !errors.Is(skipErr, ErrMergeResolutionInvalid) {
				return pausedRebaseState{}, skipErr
			}
		}
		return pausedRebaseState{
			onto:           onto,
			targetBase:     base,
			resolvedTarget: target,
			activeCommit:   activeSource,
			canSkipEmpty:   canSkipEmptyApply,
		}, nil
	}
	if head != expectedHead {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head does not match active rebase state", ErrMergeResolutionInvalid)
	}
	if err := gitexec.VerifyPublicationAncestry(ctx, path, onto, head); err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head does not descend from its target", ErrMergeResolutionInvalid)
		}
		return pausedRebaseState{}, fmt.Errorf("verify paused rebase head ancestry: %w", err)
	}
	rawCount, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--count", onto+".."+head)
	if err != nil {
		return pausedRebaseState{}, err
	}
	replayedCount, err := strconv.Atoi(strings.TrimSpace(rawCount))
	if err != nil || replayedCount > maxReplayed {
		return pausedRebaseState{}, fmt.Errorf("%w: paused rebase head contains unexpected commits", ErrMergeResolutionInvalid)
	}
	var canDropEmptyActive bool
	if stoppedCommit != "" && !rescheduledPick {
		dropErr := verifyCommitRedundant(ctx, path, head, stoppedCommit)
		if dropErr == nil {
			canDropEmptyActive = true
		} else if !errors.Is(dropErr, ErrMergeResolutionInvalid) {
			return pausedRebaseState{}, dropErr
		}
	}
	return pausedRebaseState{
		onto:            onto,
		targetBase:      base,
		resolvedTarget:  target,
		activeCommit:    stoppedCommit,
		canSkipEmpty:    canDropEmptyActive,
		rescheduledPick: rescheduledPick,
		pendingCommits:  pendingCommits,
	}, nil
}

func openRegularGitFile(path string) (*os.File, os.FileInfo, error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	linfo, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !linfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: git metadata file is not regular", ErrMergeResolutionInvalid)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|landingConflictOpenFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(linfo, info) {
		file.Close()
		return nil, nil, fmt.Errorf("%w: git metadata file is not regular", ErrMergeResolutionInvalid)
	}
	return file, info, nil
}

func readRegularGitFile(path string) ([]byte, error) {
	file, _, err := openRegularGitFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func hashRegularGitFile(h io.Writer, path string, buf []byte) error {
	file, _, err := openRegularGitFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.CopyBuffer(h, file, buf)
	return err
}

func readRegularGitFirstLine(path string) (string, error) {
	file, _, err := openRegularGitFile(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	line, err := bufio.NewReader(io.LimitReader(file, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func gitIndexChecksum(ctx context.Context, path string) (string, error) {
	indexPath, err := gitPathFor(ctx, path, "index")
	if err != nil {
		return "", err
	}
	file, _, err := openRegularGitFile(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, file, make([]byte, 32*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type rebaseSnapshot struct {
	meta  string
	index string
}

func pausedRebaseSnapshot(ctx context.Context, path string) (rebaseSnapshot, error) {
	head, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return rebaseSnapshot{}, err
	}
	gitDir, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return rebaseSnapshot{}, err
	}
	gitDir = strings.TrimSpace(gitDir)
	indexSum, err := gitIndexChecksum(ctx, path)
	if err != nil {
		return rebaseSnapshot{}, err
	}
	rawHead, err := readRegularGitFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return rebaseSnapshot{}, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(head)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(rawHead)
	_, _ = h.Write([]byte{0})
	buf := make([]byte, 32*1024)
	for _, rel := range []string{
		"rebase-merge/onto",
		"rebase-merge/head-name",
		"rebase-merge/orig-head",
		"rebase-merge/rewritten-list",
		"rebase-merge/git-rebase-todo",
		"rebase-merge/stopped-sha",
		"rebase-merge/update-refs",
		"rebase-merge/autostash",
		"rebase-merge/strategy",
		"rebase-merge/strategy_opts",
		"rebase-apply/onto",
		"rebase-apply/head-name",
		"rebase-apply/orig-head",
		"rebase-apply/next",
		"rebase-apply/last",
		"rebase-apply/abort-safety",
		"rebase-apply/patch",
		"rebase-apply/update-refs",
		"rebase-apply/autostash",
		"rebase-apply/strategy",
		"rebase-apply/strategy_opts",
	} {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		if err := hashRegularGitFile(h, filepath.Join(gitDir, filepath.FromSlash(rel)), buf); err != nil {
			return rebaseSnapshot{}, err
		}
		_, _ = h.Write([]byte{0})
	}
	if rawLast, err := readRegularGitFile(filepath.Join(gitDir, "rebase-apply", "last")); err == nil {
		if lastNum, convErr := strconv.Atoi(strings.TrimSpace(string(rawLast))); convErr == nil && lastNum >= 1 && lastNum <= 10000 {
			for i := 1; i <= lastNum; i++ {
				rel := fmt.Sprintf("rebase-apply/%04d", i)
				_, _ = h.Write([]byte(rel))
				_, _ = h.Write([]byte{0})
				if rErr := hashRegularGitFile(h, filepath.Join(gitDir, filepath.FromSlash(rel)), buf); rErr != nil {
					return rebaseSnapshot{}, rErr
				}
				_, _ = h.Write([]byte{0})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return rebaseSnapshot{}, err
	}
	for _, headNameRel := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		rawHeadName, rErr := readRegularGitFile(filepath.Join(gitDir, filepath.FromSlash(headNameRel)))
		if rErr != nil {
			if !errors.Is(rErr, os.ErrNotExist) {
				return rebaseSnapshot{}, rErr
			}
			continue
		}
		headName := strings.TrimSpace(string(rawHeadName))
		if headName == "" {
			continue
		}
		tip, tipErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", headName+"^{commit}")
		if tipErr != nil {
			var commandErr *gitexec.CommandError
			if !errors.As(tipErr, &commandErr) || commandErr.ExitCode != 1 {
				return rebaseSnapshot{}, tipErr
			}
			tip = ""
		}
		_, _ = h.Write([]byte("head-tip:" + headName))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strings.TrimSpace(tip)))
		_, _ = h.Write([]byte{0})
	}
	return rebaseSnapshot{
		meta:  hex.EncodeToString(h.Sum(nil)),
		index: indexSum,
	}, nil
}

func verifyActiveApplyPatchMatchesCommit(ctx context.Context, path, patchPath, commitSHA string) error {
	file, _, err := openRegularGitFile(patchPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var prefix [5]byte
	n, rErr := file.Read(prefix[:])
	if rErr != nil && !errors.Is(rErr, io.EOF) {
		return rErr
	}
	if n == 0 {
		return nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return verifyApplyPatchFileMatchesCommit(ctx, path, file, commitSHA, bytes.HasPrefix(prefix[:n], []byte("From ")))
}

func verifyApplyPatchMatchesCommit(ctx context.Context, path, patchPath, commitSHA string, isMail bool) error {
	file, _, err := openRegularGitFile(patchPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return verifyApplyPatchFileMatchesCommit(ctx, path, file, commitSHA, isMail)
}

func verifyApplyPatchFileMatchesCommit(ctx context.Context, path string, patchFile *os.File, commitSHA string, isMail bool) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", commitSHA+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused apply rebase patch parent is unavailable", ErrMergeResolutionInvalid)
	}
	wantTree, tErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", commitSHA+"^{tree}")
	if tErr != nil {
		return tErr
	}
	gotTree, applyErr := gitexec.IsolatedApplyTree(ctx, path, strings.TrimSpace(srcParent), patchFile, isMail)
	if applyErr != nil {
		var commandErr *gitexec.CommandError
		if errors.As(applyErr, &commandErr) {
			return fmt.Errorf("%w: paused apply rebase patch does not apply cleanly to source parent", ErrMergeResolutionInvalid)
		}
		return applyErr
	}
	if gotTree != strings.TrimSpace(wantTree) {
		return fmt.Errorf("%w: paused apply rebase patch body does not match assigned commit", ErrMergeResolutionInvalid)
	}
	return nil
}

func hasUntrackedResetObstruction(root string, deletedPaths, addedPaths []string) bool {
	type trackedEntry struct {
		info       os.FileInfo
		parentInfo os.FileInfo
		base       string
	}
	trackedAdded := make([]trackedEntry, 0, len(addedPaths))
	for _, rel := range addedPaths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		parentInfo, pErr := os.Stat(filepath.Dir(full))
		if pErr != nil {
			continue
		}
		trackedAdded = append(trackedAdded, trackedEntry{
			info:       info,
			parentInfo: parentInfo,
			base:       filepath.Base(full),
		})
	}
	isTrackedAdded := func(full string, fi os.FileInfo) bool {
		parentInfo, err := os.Stat(filepath.Dir(full))
		if err != nil {
			return false
		}
		base := filepath.Base(full)
		for _, tracked := range trackedAdded {
			if !os.SameFile(fi, tracked.info) || !os.SameFile(parentInfo, tracked.parentInfo) {
				continue
			}
			if base == tracked.base {
				return true
			}
			if strings.EqualFold(base, tracked.base) {
				entries, rErr := os.ReadDir(filepath.Dir(full))
				if rErr != nil {
					return false
				}
				matches := 0
				for _, entry := range entries {
					if strings.EqualFold(entry.Name(), base) {
						matches++
					}
				}
				if matches == 1 {
					return true
				}
			}
		}
		return false
	}
	errObstructed := errors.New("obstructed")
	for _, del := range deletedPaths {
		parts := strings.Split(del, "/")
		cur := root
		for i, part := range parts {
			if part == "" {
				continue
			}
			cur = filepath.Join(cur, part)
			fi, err := os.Lstat(cur)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return true
			}
			if !fi.IsDir() {
				if isTrackedAdded(cur, fi) {
					break
				}
				return true
			}
			if i == len(parts)-1 {
				walkErr := filepath.WalkDir(cur, func(p string, d os.DirEntry, wErr error) error {
					if wErr != nil {
						return errObstructed
					}
					if d.IsDir() {
						return nil
					}
					entryInfo, statErr := os.Lstat(p)
					if statErr != nil || !isTrackedAdded(p, entryInfo) {
						return errObstructed
					}
					return nil
				})
				if walkErr != nil {
					return true
				}
			}
		}
	}
	return false
}

func verifyStagedExactReplayTree(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase rescheduled commit parent is unavailable", ErrMergeResolutionInvalid)
	}
	return gitexec.IsolatedMergeTreeIndex(ctx, path, strings.TrimSpace(srcParent), prev, src, func(_ func(...string) (string, error), diffIndex func(tree string) (string, error), merged string, mErr error) error {
		if mErr != nil {
			return fmt.Errorf("%w: paused rebase rescheduled commit has unexpected merge state", ErrMergeResolutionInvalid)
		}
		mergedTree, _, _ := strings.Cut(merged, "\x00")
		diffRaw, dErr := diffIndex(strings.TrimSpace(mergedTree))
		if dErr != nil {
			return dErr
		}
		if strings.TrimSpace(diffRaw) != "" {
			return fmt.Errorf("%w: paused rebase rescheduled pick has extra staged changes", ErrMergeResolutionInvalid)
		}
		return nil
	})
}

func verifyStagedConflictResolution(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase active commit parent is unavailable", ErrMergeResolutionInvalid)
	}
	return gitexec.IsolatedMergeTreeIndex(ctx, path, strings.TrimSpace(srcParent), prev, src, func(read func(...string) (string, error), diffIndex func(tree string) (string, error), merged string, mErr error) error {
		prevTree, tErr := read("rev-parse", "--verify", prev+"^{tree}")
		if tErr != nil {
			return tErr
		}
		var mergedTree string
		conflictedPaths := make(map[string]struct{})
		if mErr == nil {
			t, _, _ := strings.Cut(merged, "\x00")
			mergedTree = strings.TrimSpace(t)
		} else {
			var commandErr *gitexec.CommandError
			if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
				return fmt.Errorf("verify paused rebase staged resolution: %w", mErr)
			}
			header, _, _ := strings.Cut(commandErr.Output, "\x00\x00")
			parts := strings.Split(header, "\x00")
			if len(parts) == 0 || !gitexec.ValidHead(strings.TrimSpace(parts[0])) {
				return fmt.Errorf("%w: paused rebase conflict merge tree is unavailable", ErrMergeResolutionInvalid)
			}
			mergedTree = strings.TrimSpace(parts[0])
			for _, name := range parts[1:] {
				if name != "" {
					conflictedPaths[name] = struct{}{}
				}
			}
		}
		cleanMergedRaw, cErr := read("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", strings.TrimSpace(prevTree), mergedTree)
		if cErr != nil {
			return cErr
		}
		requiredClean := make(map[string]struct{})
		for _, name := range strings.Split(strings.TrimSuffix(cleanMergedRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, conflicted := conflictedPaths[name]; !conflicted {
				requiredClean[name] = struct{}{}
			}
		}
		if len(requiredClean) == 0 {
			return nil
		}
		diffRaw, dErr := diffIndex(mergedTree)
		if dErr != nil {
			return dErr
		}
		for _, name := range strings.Split(strings.TrimSuffix(diffRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, ok := requiredClean[name]; ok {
				return fmt.Errorf("%w: paused rebase staged resolution alters or drops non-conflicted source files", ErrMergeResolutionInvalid)
			}
		}
		return nil
	})
}

func verifyAssignedCommitsAccounted(ctx context.Context, path, onto, origHead string, accounted []string) error {
	rawAllowed, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--reverse", "--topo-order", "--no-merges", onto+".."+origHead)
	if err != nil {
		return err
	}
	allowedOrder := make(map[string]int)
	for idx, sha := range strings.Fields(rawAllowed) {
		allowedOrder[sha] = idx
	}
	seen := make(map[string]struct{}, len(accounted))
	lastIdx := -1
	for _, sha := range accounted {
		idx, ok := allowedOrder[sha]
		if !ok {
			return fmt.Errorf("%w: paused rebase includes a commit outside the assigned branch range", ErrMergeResolutionInvalid)
		}
		if _, dup := seen[sha]; dup {
			return fmt.Errorf("%w: paused rebase duplicates an assigned commit", ErrMergeResolutionInvalid)
		}
		if idx <= lastIdx {
			return fmt.Errorf("%w: paused rebase reorders assigned commits", ErrMergeResolutionInvalid)
		}
		lastIdx = idx
		seen[sha] = struct{}{}
	}
	rawExpected, err := gitexec.ReadPublicationSource(ctx, path, "rev-list", "--cherry-pick", "--right-only", "--no-merges", onto+"..."+origHead)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(rawExpected) {
		if _, ok := seen[sha]; ok {
			continue
		}
		empty, emptyErr := isEmptyCommit(ctx, path, sha)
		if emptyErr != nil {
			return emptyErr
		}
		if empty {
			continue
		}
		return fmt.Errorf("%w: paused rebase is missing an assigned commit", ErrMergeResolutionInvalid)
	}
	return nil
}

func isEmptyCommit(ctx context.Context, path, sha string) (bool, error) {
	parent, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", sha+"^")
	if err != nil {
		var commandErr *gitexec.CommandError
		if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
			return false, nil
		}
		return false, err
	}
	commitTree, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", sha+"^{tree}")
	if err != nil {
		return false, err
	}
	parentTree, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", strings.TrimSpace(parent)+"^{tree}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(commitTree) == strings.TrimSpace(parentTree), nil
}

func verifyCommitRedundant(ctx context.Context, path, prev, src string) error {
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase dropped commit cannot be verified as redundant", ErrMergeResolutionInvalid)
	}
	return gitexec.IsolatedMergeTree(ctx, path, strings.TrimSpace(srcParent), prev, src, func(read func(...string) (string, error), merged string, mErr error) error {
		prevTree, tErr := read("rev-parse", "--verify", prev+"^{tree}")
		if tErr != nil {
			return tErr
		}
		if mErr == nil {
			mergedTree, _, _ := strings.Cut(merged, "\x00")
			if strings.TrimSpace(mergedTree) != strings.TrimSpace(prevTree) {
				return fmt.Errorf("%w: paused rebase dropped commit is not redundant against target", ErrMergeResolutionInvalid)
			}
			return nil
		}
		var commandErr *gitexec.CommandError
		if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
			return fmt.Errorf("verify paused rebase dropped commit: %w", mErr)
		}
		header, _, _ := strings.Cut(commandErr.Output, "\x00\x00")
		parts := strings.Split(header, "\x00")
		if len(parts) == 0 || !gitexec.ValidHead(strings.TrimSpace(parts[0])) {
			return fmt.Errorf("%w: paused rebase dropped conflict merge tree is unavailable", ErrMergeResolutionInvalid)
		}
		mergedTree := strings.TrimSpace(parts[0])
		conflictedPaths := make(map[string]struct{}, len(parts)-1)
		for _, name := range parts[1:] {
			if name != "" {
				conflictedPaths[name] = struct{}{}
			}
		}
		diffPathsRaw, dErr := read("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", strings.TrimSpace(prevTree), mergedTree)
		if dErr != nil {
			return dErr
		}
		for _, name := range strings.Split(strings.TrimSuffix(diffPathsRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, ok := conflictedPaths[name]; !ok {
				return fmt.Errorf("%w: paused rebase dropped commit has non-conflicted changes", ErrMergeResolutionInvalid)
			}
		}
		return nil
	})
}

func verifyReplayedCommit(ctx context.Context, path, prev, src, dst string) error {
	if dst == prev {
		return verifyCommitRedundant(ctx, path, prev, src)
	}
	parent, err := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", dst+"^")
	if err != nil || strings.TrimSpace(parent) != prev {
		return fmt.Errorf("%w: paused rebase replayed commit parent does not match active target", ErrMergeResolutionInvalid)
	}
	srcMeta, err := gitexec.ReadPublicationSource(ctx, path, "show", "-s", "--format=%an%x00%ae%x00%at%x00%s", src)
	if err != nil {
		return err
	}
	dstMeta, err := gitexec.ReadPublicationSource(ctx, path, "show", "-s", "--format=%an%x00%ae%x00%at%x00%s", dst)
	if err != nil {
		return err
	}
	if srcMeta != dstMeta {
		return fmt.Errorf("%w: paused rebase replayed commit metadata does not match assigned source", ErrMergeResolutionInvalid)
	}
	srcParent, pErr := gitexec.ReadPublicationSource(ctx, path, "rev-parse", "--verify", "--quiet", src+"^")
	if pErr != nil {
		return fmt.Errorf("%w: paused rebase source commit parent is unavailable", ErrMergeResolutionInvalid)
	}
	return gitexec.IsolatedMergeTree(ctx, path, strings.TrimSpace(srcParent), prev, src, func(read func(...string) (string, error), merged string, mErr error) error {
		if mErr == nil {
			mergedTree, _, _ := strings.Cut(merged, "\x00")
			dstTree, tErr := read("rev-parse", "--verify", dst+"^{tree}")
			if tErr != nil {
				return tErr
			}
			if strings.TrimSpace(mergedTree) != strings.TrimSpace(dstTree) {
				return fmt.Errorf("%w: paused rebase replayed commit content does not match assigned source", ErrMergeResolutionInvalid)
			}
			return nil
		}
		var commandErr *gitexec.CommandError
		if !errors.As(mErr, &commandErr) || commandErr.ExitCode != 1 {
			return fmt.Errorf("verify paused rebase replayed tree: %w", mErr)
		}
		header, _, _ := strings.Cut(commandErr.Output, "\x00\x00")
		parts := strings.Split(header, "\x00")
		if len(parts) == 0 || !gitexec.ValidHead(strings.TrimSpace(parts[0])) {
			return fmt.Errorf("%w: paused rebase conflict merge tree is unavailable", ErrMergeResolutionInvalid)
		}
		mergedTree := strings.TrimSpace(parts[0])
		conflictedPaths := make(map[string]struct{}, len(parts)-1)
		for _, name := range parts[1:] {
			if name != "" {
				conflictedPaths[name] = struct{}{}
			}
		}
		prevTree, tErr := read("rev-parse", "--verify", prev+"^{tree}")
		if tErr != nil {
			return tErr
		}
		cleanMergedRaw, cErr := read("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", strings.TrimSpace(prevTree), mergedTree)
		if cErr != nil {
			return cErr
		}
		requiredClean := make(map[string]struct{})
		for _, name := range strings.Split(strings.TrimSuffix(cleanMergedRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, conflicted := conflictedPaths[name]; !conflicted {
				requiredClean[name] = struct{}{}
			}
		}
		if len(requiredClean) == 0 {
			return nil
		}
		diffPathsRaw, dErr := read("diff-tree", "--no-commit-id", "--name-only", "-r", "-z", mergedTree, dst+"^{tree}")
		if dErr != nil {
			return dErr
		}
		for _, name := range strings.Split(strings.TrimSuffix(diffPathsRaw, "\x00"), "\x00") {
			if name == "" {
				continue
			}
			if _, ok := requiredClean[name]; ok {
				return fmt.Errorf("%w: paused rebase replayed commit alters or drops non-conflicted source files", ErrMergeResolutionInvalid)
			}
		}
		return nil
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
		indexBefore, err := gitIndexChecksum(ctx, info.Path)
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
			indexNow, indexErr := gitIndexChecksum(ctx, info.Path)
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
		name, err := readRegularGitFile(filepath.Join(dir, "head-name"))
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(string(name)) != "refs/heads/"+info.Branch {
			return false, fmt.Errorf("%w: paused rebase belongs to another branch", ErrMergeResolutionInvalid)
		}
		original, err := readRegularGitFile(filepath.Join(dir, "orig-head"))
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
		updates, err := readRegularGitFile(filepath.Join(dir, "update-refs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(updates)) != "" {
			return false, fmt.Errorf("%w: paused rebase updates additional refs", ErrMergeResolutionInvalid)
		}
		autostash, err := readRegularGitFile(filepath.Join(dir, "autostash"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if strings.TrimSpace(string(autostash)) != "" {
			return false, fmt.Errorf("%w: paused rebase carries autostash changes", ErrMergeResolutionInvalid)
		}
		for _, meta := range []string{"strategy", "strategy_opts"} {
			rawMeta, metaErr := readRegularGitFile(filepath.Join(dir, meta))
			if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
				return false, metaErr
			}
			if strings.TrimSpace(string(rawMeta)) != "" {
				return false, fmt.Errorf("%w: paused rebase uses custom merge strategy", ErrMergeResolutionInvalid)
			}
		}
		if kind == "rebase-merge" {
			todo, err := readRegularGitFile(filepath.Join(dir, "git-rebase-todo"))
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
