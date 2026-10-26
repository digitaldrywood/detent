package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	CheckpointRefPrefix   = tracker.CheckpointRefPrefix
	MaxCheckpointRefBytes = 20 << 20
)

type CheckpointRef struct {
	Ref         string
	CommitSHA   string
	HeadSHA     string
	BaseSHA     string
	TreeSHA     string
	transaction *checkpointTransaction
}

type CheckpointRefPublisher interface {
	PushCheckpointRef(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error) (CheckpointRef, bool, error)
}

type CheckpointWorktree interface {
	RestoreCheckpointForFinalization(context.Context, Info, Issue, CheckpointRef) (Info, Issue, error)
}

func (l *LocalGit) RestoreCheckpointForFinalization(ctx context.Context, original Info, issue Issue, checkpoint CheckpointRef) (Info, Issue, error) {
	issue.Identifier += "-finalization-" + checkpoint.CommitSHA
	issue.BranchName = ""
	issue.Source = nil
	issue.Checkpoint = &checkpoint
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
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = issue.Identifier
	}
	return CheckpointRefPrefix + SafeKey(id)
}

func checkpointRefGit(path string, env ...string) checkpointGitRunner {
	env = append([]string{
		"GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0",
	}, env...)
	return func(ctx context.Context, args ...string) (string, error) {
		return runGitAtWithEnv(ctx, path, env, args...)
	}
}

func (l *LocalGit) PushCheckpointRef(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error) (_ CheckpointRef, _ bool, err error) {
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
	if previous.CommitSHA != "" && previous.HeadSHA == head && previous.TreeSHA == tree {
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

// checkpointSnapshot writes HEAD plus every uncommitted, non-ignored change
// into the temporary index, leaving excluded paths at their HEAD version.
func checkpointSnapshot(ctx context.Context, git, index checkpointGitRunner) (head, tree string, included []string, err error) {
	output, err := git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", nil, fmt.Errorf("read checkpoint head: %w", err)
	}
	head = strings.TrimSpace(output)
	if _, err := index(ctx, "read-tree", "HEAD"); err != nil {
		return "", "", nil, fmt.Errorf("seed checkpoint index: %w", err)
	}
	if _, err := index(ctx, "add", "-A", "--", "."); err != nil {
		return "", "", nil, fmt.Errorf("stage checkpoint content: %w", err)
	}
	changed, err := index(ctx, "diff", "--cached", "--name-only", "--no-renames", "-z", "HEAD", "--")
	if err != nil {
		return "", "", nil, err
	}
	var excluded []string
	for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
		switch {
		case path == "":
		case !checkpointPathAllowed(path) || slices.ContainsFunc(detentHandoffDiffExcludes, func(exclude string) bool { return strings.HasPrefix(path, exclude) }):
			excluded = append(excluded, path)
		default:
			included = append(included, path)
		}
	}
	if len(excluded) != 0 {
		if _, err := index(ctx, append([]string{"reset", "-q", "HEAD", "--"}, excluded...)...); err != nil {
			return "", "", nil, fmt.Errorf("exclude checkpoint paths: %w", err)
		}
	}
	output, err = index(ctx, "write-tree")
	if err != nil {
		return "", "", nil, fmt.Errorf("write checkpoint tree: %w", err)
	}
	return head, strings.TrimSpace(output), included, nil
}

// checkpointMatches reports whether an existing worktree already holds the
// checkpoint, so restoring it would only quarantine an identical copy.
func checkpointMatches(ctx context.Context, path string, checkpoint *CheckpointRef) (matched bool) {
	if checkpoint.transaction != nil {
		return checkpointTransactionMatches(ctx, path, checkpoint.transaction)
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return false
	}
	defer func() { matched = os.RemoveAll(scratch) == nil && matched }()
	head, tree, _, err := checkpointSnapshot(ctx, checkpointRefGit(path), checkpointRefGit(path, "GIT_INDEX_FILE="+filepath.Join(scratch, "index")))
	return err == nil && head == checkpoint.HeadSHA && tree == checkpoint.TreeSHA
}

func checkpointUnpublishedHistory(ctx context.Context, git checkpointGitRunner, head string) error {
	commits, err := git(ctx, "rev-list", head, "--not", "--remotes")
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(commits) {
		changed, err := git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return err
		}
		for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
			if path == "" {
				continue
			}
			if !checkpointPathAllowed(path) {
				return fmt.Errorf("%w: unpublished commit contains excluded path %q", ErrCheckpointUnsafe, path)
			}
			if err := checkpointBlob(ctx, git, commit+":", path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *LocalGit) bindCheckpointBranch(issue Issue, info Info, checkpoint *CheckpointRef) error {
	if checkpoint.transaction == nil {
		return nil
	}
	if !issue.NativeRework || checkpoint.Ref != CheckpointRefName(issue) {
		return fmt.Errorf("%w: checkpoint belongs to another rework branch", ErrCheckpointUnsafe)
	}
	if checkpoint.transaction.Branch != info.Branch {
		pattern := `^detent/[a-z0-9_.-]+-` + regexp.QuoteMeta(strings.ToLower(SafeKey(issue.Identifier))) + `-[a-f0-9]{12}$`
		if nativeProjectID(issue) == "" || !l.autoBranch || issue.WorkspaceSession || issue.Landing != nil || strings.TrimSpace(issue.BranchName) != "" || !regexp.MustCompile(pattern).MatchString(checkpoint.transaction.Branch) {
			return fmt.Errorf("%w: checkpoint belongs to another rework branch", ErrCheckpointUnsafe)
		}
	}
	transaction := *checkpoint.transaction
	transaction.Branch = info.Branch
	checkpoint.transaction = &transaction
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
		checkpoint.transaction = transaction
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

func restoreCheckpointTree(ctx context.Context, path string, checkpoint *CheckpointRef) error {
	if checkpoint.transaction != nil {
		return restoreCheckpointTransaction(ctx, path, checkpoint)
	}
	if _, err := runGitAt(ctx, path, "-c", "core.hooksPath="+os.DevNull, "read-tree", "-u", "--reset", checkpoint.TreeSHA); err != nil {
		return fmt.Errorf("restore checkpoint tree: %w", err)
	}
	if _, err := runGitAt(ctx, path, "reset", "-q", "HEAD"); err != nil {
		return fmt.Errorf("unstage restored checkpoint: %w", err)
	}
	return nil
}
