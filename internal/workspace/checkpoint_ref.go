package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type CheckpointRefPublisher interface {
	PushCheckpointRef(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error) (CheckpointRef, bool, error)
}

type CheckpointWorktree interface {
	RestoreCheckpointForFinalization(context.Context, Info, Issue, CheckpointRef) (Info, Issue, error)
}

func (l *LocalGit) RestoreCheckpointForFinalization(ctx context.Context, original Info, issue Issue, checkpoint CheckpointRef) (Info, Issue, error) {
	issue.Identifier += "-finalization-" + checkpoint.CommitSHA
	issue.BranchName = ""
	issue.Source, issue.PhaseSource = nil, nil
	issue.finalizationBranch = original.PublishBranch()
	issue.Checkpoint, issue.RequireCheckpoint = &checkpoint, true
	info, err := l.Create(ctx, issue)
	if err != nil {
		return Info{}, issue, err
	}
	if info.Path == original.Path || info.Branch == original.Branch {
		return Info{}, issue, errors.New("checkpoint finalization requires a separate workspace and branch")
	}
	if _, err := runGitAt(ctx, info.Path, "read-tree", checkpoint.TreeSHA); err != nil {
		return Info{}, issue, fmt.Errorf("stage captured checkpoint tree: %w", err)
	}
	return info, issue, nil
}

func CheckpointRefName(issue Issue) string {
	if owner := issue.AttemptWorkspace.Key(); owner != "" {
		return CheckpointRefPrefix + owner
	}
	return legacyCheckpointRefName(issue)
}

func legacyCheckpointRefName(issue Issue) string {
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = issue.Identifier
	}
	return CheckpointRefPrefix + SafeKey(id)
}

func ownsCheckpointRef(issue Issue, ref string) bool {
	if ref == CheckpointRefName(issue) {
		return true
	}
	current, previous := issue.AttemptWorkspace, issue.RecoveryAttemptWorkspace
	if current.Key() == "" {
		return false
	}
	if previous != (AttemptWorkspace{}) && (previous.Key() == "" || current.Organization != previous.Organization || current.Project != previous.Project) {
		return false
	}
	return ref == legacyCheckpointRefName(issue) || previous.Key() != "" && ref == CheckpointRefPrefix+previous.Key()
}

func (l *LocalGit) PushCheckpointRef(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error) (_ CheckpointRef, _ bool, err error) {
	if !issue.AttemptWorkspace.permitsCheckout(info.Key) || !issue.AttemptWorkspace.permitsCheckout(filepath.Base(info.Path)) {
		return previous, false, ErrCheckpointUnsafe
	}
	if issue.AttemptWorkspace != (AttemptWorkspace{}) {
		expected, err := l.infoForIssue(issue)
		if err != nil || expected.Key != info.Key || expected.Path != info.Path || previous.Ref != "" && !ownsCheckpointRef(issue, previous.Ref) {
			return previous, false, errors.Join(ErrCheckpointUnsafe, err)
		}
	}
	if validate == nil {
		return previous, false, fmt.Errorf("%w: ownership validation is required", ErrCheckpointUnsafe)
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return previous, false, err
	}
	defer release()
	git := checkpointRefGit(info.Path)
	for _, pending := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if _, err := git(ctx, "rev-parse", "-q", "--verify", pending); err == nil {
			return previous, false, fmt.Errorf("%w: %s operation in progress", ErrCheckpointUnsafe, pending)
		}
	}
	if paused, pauseErr := rebaseInProgress(ctx, info.Path); pauseErr != nil {
		return previous, false, pauseErr
	} else if paused && issue.NativeRework {
		return l.pushReworkCheckpoint(ctx, info, issue, previous, validate)
	}
	if _, err := git(ctx, "symbolic-ref", "-q", "HEAD"); err != nil {
		return previous, false, fmt.Errorf("%w: worktree is not on its branch (rebase or detached HEAD in progress)", ErrCheckpointUnsafe)
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return previous, false, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	index := checkpointRefGit(info.Path, "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	head, tree, included, err := checkpointSnapshot(ctx, git, index)
	if err != nil {
		return previous, false, err
	}
	if previous.Ref == CheckpointRefName(issue) && previous.CommitSHA != "" && previous.HeadSHA == head && previous.TreeSHA == tree {
		return previous, false, nil
	}
	for _, path := range included {
		if err := checkpointBlob(ctx, index, ":", path); err != nil {
			return previous, false, err
		}
	}
	if err := checkpointUnpublishedHistory(ctx, git, head); err != nil {
		return previous, false, err
	}
	parents := []string{head}
	if previous.CommitSHA != "" {
		parents = append(parents, previous.CommitSHA)
	}
	commit, err := l.checkpointCommit(ctx, info.Path, tree, parents)
	if err != nil {
		return previous, false, fmt.Errorf("create checkpoint commit: %w", err)
	}
	output, err := git(ctx, "rev-list", "--objects", "--disk-usage", commit, "--not", "--remotes")
	if err != nil {
		return previous, false, fmt.Errorf("measure checkpoint: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return previous, false, fmt.Errorf("measure checkpoint: %w", err)
	}
	if size > MaxCheckpointRefBytes {
		return previous, false, fmt.Errorf("%w: %d bytes of unpublished objects exceed the %d byte checkpoint limit", ErrCheckpointUnsafe, size, MaxCheckpointRefBytes)
	}
	base := ""
	for _, ref := range []string{strings.TrimSpace(issue.BaseRef), "refs/remotes/" + defaultGitRemote + "/HEAD"} {
		if ref == "" {
			continue
		}
		if output, err := git(ctx, "merge-base", head, ref); err == nil {
			base = strings.TrimSpace(output)
			break
		}
	}
	ref := CheckpointRefName(issue)
	if err := validate(ctx); err != nil {
		return previous, false, err
	}
	if _, err := git(ctx, "-c", "core.hooksPath="+os.DevNull, "-c", "push.followTags=false", "push", "--no-verify", "--force", "--recurse-submodules=no", defaultGitRemote, commit+":"+ref); err != nil {
		return previous, false, fmt.Errorf("push checkpoint ref: %w", err)
	}
	return CheckpointRef{Ref: ref, CommitSHA: commit, HeadSHA: head, BaseSHA: base, TreeSHA: tree}, true, nil
}

// deleteCheckpointRef retires a terminal item's checkpoint ref as part of the
// existing terminal workspace cleanup. A failure leaves only a stale ref.
func (l *LocalGit) deleteCheckpointRef(ctx context.Context, issue Issue) {
	if issue.ReviewVersionID != "" || !issue.Terminal || strings.TrimSpace(issue.ID) == "" && strings.TrimSpace(issue.Identifier) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	git := checkpointRefGit(l.sourceRoot)
	if _, err := git(ctx, "-c", "core.hooksPath="+os.DevNull, "push", "--no-verify", defaultGitRemote, ":"+CheckpointRefName(issue)); err != nil {
		l.logger.Debug("checkpoint ref not deleted", slog.String("ref", CheckpointRefName(issue)), slog.Any("error", err))
	}
}

var ErrLocalCheckpointChanged = errors.New("local_checkpoint_changed")

func (l *LocalGit) verifiedLocalCheckpointHead(ctx context.Context, target Info, issue Issue) (string, error) {
	checkpoint := issue.LocalCheckpoint
	if checkpoint == nil {
		return "", nil
	}
	exists, _, err := pathExists(target.Path)
	if err != nil {
		return "", err
	}
	prior := target
	source := issue
	if !exists {
		source.AttemptWorkspace = issue.RecoveryAttemptWorkspace
		if source.AttemptWorkspace != (AttemptWorkspace{}) && (source.AttemptWorkspace.Organization != issue.AttemptWorkspace.Organization || source.AttemptWorkspace.Project != issue.AttemptWorkspace.Project) {
			return "", ErrLocalCheckpointChanged
		}
		prior, err = l.Existing(source)
		if errors.Is(err, ErrMissingWorkspace) && source.AttemptWorkspace.Key() != "" {
			source.AttemptWorkspace = AttemptWorkspace{}
			prior, err = l.Existing(source)
		}
		if err != nil {
			return "", errors.Join(ErrLocalCheckpointChanged, err)
		}
	}
	if err := l.verifyLocalCheckpoint(ctx, prior, source); err != nil {
		return "", err
	}
	return checkpoint.HeadSHA, nil
}

func (l *LocalGit) verifyLocalCheckpoint(ctx context.Context, info Info, issue Issue) error {
	checkpoint := issue.LocalCheckpoint
	if !validLandingHead(checkpoint.HeadSHA) || checkpoint.WorkspaceDigest == "" || !l.isSourceWorktree(ctx, info.Path) {
		return ErrLocalCheckpointChanged
	}
	matches, _, err := l.workspaceOnExpectedBranch(ctx, info.Path, info.Branch)
	if err != nil || !matches {
		return errors.Join(ErrLocalCheckpointChanged, err)
	}
	observed, err := l.RecoveryState(ctx, info, issue)
	if err != nil || observed.HeadSHA != checkpoint.HeadSHA || observed.WorkspaceFingerprint != checkpoint.WorkspaceDigest ||
		len(observed.TrackedPaths) != 0 || len(observed.UntrackedPaths) != 0 {
		return errors.Join(ErrLocalCheckpointChanged, err)
	}
	return nil
}

func (l *LocalGit) bindCheckpointBranch(issue Issue, info Info, checkpoint *CheckpointRef) error {
	if checkpoint.Transaction == nil {
		return nil
	}
	if !issue.NativeRework || !ownsCheckpointRef(issue, checkpoint.Ref) {
		return fmt.Errorf("%w: checkpoint belongs to another rework branch", ErrCheckpointUnsafe)
	}
	if checkpoint.Transaction.Branch != info.Branch {
		pattern := `^detent/[a-z0-9_.-]+-` + regexp.QuoteMeta(strings.ToLower(SafeKey(issue.Identifier))) + `-[a-f0-9]{12}$`
		source := issue
		source.AttemptWorkspace = issue.RecoveryAttemptWorkspace
		sourceInfo, err := l.infoForIssue(source)
		if err != nil {
			return err
		}
		legacy := nativeProjectID(issue) != "" && regexp.MustCompile(pattern).MatchString(checkpoint.Transaction.Branch)
		if issue.AttemptWorkspace.Key() != "" && info.PublicationBranch != "" {
			legacy = info.PublicationBranch == checkpoint.Transaction.Branch
		}
		canonical := source.AttemptWorkspace.Key() != "" && sourceInfo.Branch == checkpoint.Transaction.Branch
		if !l.autoBranch || issue.WorkspaceSession || issue.Landing != nil ||
			issue.AttemptWorkspace.Key() == "" && strings.TrimSpace(issue.BranchName) != "" || !legacy && !canonical {
			return fmt.Errorf("%w: checkpoint belongs to another rework branch", ErrCheckpointUnsafe)
		}
	}
	transaction := *checkpoint.Transaction
	transaction.Branch = info.Branch
	checkpoint.Transaction = &transaction
	return nil
}

func (l *LocalGit) fetchCheckpointRef(ctx context.Context, checkpoint *CheckpointRef) error {
	if !validLandingHead(checkpoint.CommitSHA) || !validLandingHead(checkpoint.TreeSHA) || !strings.HasPrefix(checkpoint.Ref, CheckpointRefPrefix) {
		return errors.New("checkpoint reference is incomplete")
	}
	if _, err := l.runGit(ctx, "cat-file", "-e", checkpoint.CommitSHA+"^{commit}"); err != nil {
		if _, err := l.runGit(ctx, remoteGitArgs("fetch", "--no-tags", "--no-write-fetch-head", defaultGitRemote, checkpoint.CommitSHA)...); err != nil {
			if _, refErr := l.runGit(ctx, remoteGitArgs("fetch", "--no-tags", "--no-write-fetch-head", defaultGitRemote, "+"+checkpoint.Ref+":"+checkpoint.Ref)...); refErr != nil {
				return fmt.Errorf("fetch checkpoint %s: %w", checkpoint.Ref, errors.Join(err, refErr))
			}
		}
	}
	kind, err := l.runGit(ctx, "cat-file", "-t", checkpoint.TreeSHA)
	if err != nil {
		return err
	}
	if strings.TrimSpace(kind) == "blob" {
		transaction, err := readCheckpointTransaction(ctx, checkpointRefGit(l.sourceRoot), checkpoint)
		if err != nil {
			return err
		}
		checkpoint.Transaction = transaction
		checkpoint.HeadSHA, checkpoint.BaseSHA = transaction.Head, transaction.Onto
		return nil
	}
	tree, err := l.runGit(ctx, "rev-parse", checkpoint.CommitSHA+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != checkpoint.TreeSHA {
		return errors.Join(errors.New("checkpoint commit differs from its recorded tree"), err)
	}
	parent, err := l.runGit(ctx, "rev-parse", checkpoint.CommitSHA+"^1")
	if err != nil {
		return fmt.Errorf("read checkpoint head: %w", err)
	}
	checkpoint.HeadSHA = strings.TrimSpace(parent)
	return nil
}
