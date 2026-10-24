package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrWorkspacePreserved = errors.New("workspace retained for recovery")

type Preservation struct {
	LocalChangesVerified bool              `json:"local_changes_verified"`
	Delivery             *DeliverableState `json:"delivery,omitempty"`
	DeliveryError        string            `json:"delivery_error,omitempty"`
	Path                 string            `json:"path"`
	Branch               string            `json:"branch,omitempty"`
	Preserved            bool              `json:"preserved"`
	Files                int               `json:"files,omitempty"`
	HeadSHA              string            `json:"head_sha,omitempty"`
	UnpushedCommits      int               `json:"unpushed_commits,omitempty"`
	TrackedPaths         []string          `json:"tracked_paths,omitempty"`
	UntrackedPaths       []string          `json:"untracked_paths,omitempty"`
}

type IssuePreserver interface {
	PreserveIssue(context.Context, Issue) (Preservation, error)
}

func (l *LocalGit) PreserveIssue(ctx context.Context, issue Issue) (Preservation, error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return Preservation{}, err
	}
	result := Preservation{Path: info.Path, Branch: info.Branch}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, ErrMissingWorkspace
	}
	if err := l.recordCleanupOwnership(ctx, info, issue, isDir); err != nil {
		return result, err
	}
	result.Preserved = true
	recovery, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return result, fmt.Errorf("inspect retained workspace: %w", err)
	}
	result.HeadSHA = recovery.HeadSHA
	result.UnpushedCommits, err = retainedGitCommitCount(ctx, info.Path, "HEAD")
	if err != nil {
		return result, err
	}
	result.TrackedPaths = recovery.TrackedPaths
	result.UntrackedPaths = recovery.UntrackedPaths
	result.LocalChangesVerified = true
	delivery, err := gitDeliveryState(ctx, info.Path, info.Branch, issue.BaseRef)
	if err != nil {
		result.DeliveryError = err.Error()
	} else {
		result.Delivery = &delivery
	}
	return result, nil
}

func (l *LocalGit) checkWorkspaceCleanup(ctx context.Context, info Info, issue Issue) error {
	session := sessionOwned(info, issue)
	recordPath := cleanupOwnershipRecordRelativePath(info.Path)
	record, err := l.readOwnershipRecord(recordPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w at %s: read workspace retention: %w", ErrWorkspacePreserved, info.Path, err)
	}
	if err == nil && !l.validOwnershipRecord(ctx, recordPath, record) {
		return fmt.Errorf("%w: invalid workspace retention record: %s", ErrWorkspacePreserved, info.Path)
	}
	if err == nil && record.WorkspaceSession {
		session = true
	}
	if err := l.checkCleanupBranch(ctx, info.Branch, session, issue); err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	if !exists {
		return nil
	}
	if !isDir || !l.isSourceWorktree(ctx, info.Path) {
		if exists && record.CleanupStarted && !l.isGitWorkspace(ctx, info.Path) {
			return nil
		}
		return fmt.Errorf("%w at %s: worktree registration is unavailable or not managed by source", ErrWorkspacePreserved, info.Path)
	}
	changed, err := l.worktreeHasChanges(ctx, info.Path)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	if session {
		// A workspace session checks out a commit somebody else already
		// owns, on a detached HEAD. Measuring its HEAD against the remotes
		// says nothing about work at risk, so what the session could have
		// produced is checked instead: edits on disk, commits on its own
		// branch, and commits past the head_sha it was opened on.
		if changed {
			return fmt.Errorf("%w at %s: uncommitted files remain in the workspace session", ErrWorkspacePreserved, info.Path)
		}
		return l.checkWorkspaceSessionCommits(ctx, info.Path, issue.PullRequestHeadSHA)
	}
	if changed {
		return fmt.Errorf("%w at %s: uncommitted files remain", ErrWorkspacePreserved, info.Path)
	}
	if deliveredCleanupHead(issue) != "" {
		head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("%w at %s: inspect landed workspace head: %w", ErrWorkspacePreserved, info.Path, err)
		}
		if strings.TrimSpace(head) == deliveredCleanupHead(issue) {
			return nil
		}
	}
	unpushed, err := retainedGitCommitCount(ctx, info.Path, "HEAD")
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrWorkspacePreserved, info.Path, err)
	}
	if unpushed > 0 || changed {
		return fmt.Errorf("%w at %s: unpushed commits or uncommitted files remain", ErrWorkspacePreserved, info.Path)
	}
	return nil
}

// checkWorkspaceSessionCommits retains a session worktree that has committed
// past the commit it was opened on, or, when that commit is unknown, one whose
// HEAD holds commits no branch, tag or remote-tracking ref reaches.
func (l *LocalGit) checkWorkspaceSessionCommits(ctx context.Context, path string, headSHA string) error {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		// Without the head the session opened on, HEAD is measured against
		// every branch, tag and remote-tracking ref: a commit made on a
		// detached HEAD in the terminal is reachable from none of them.
		output, err := runGitAt(ctx, path, "rev-list", "--count", "HEAD", "--not", "--branches", "--tags", "--remotes")
		if err != nil {
			return fmt.Errorf("%w at %s: inspect workspace session head: %w", ErrWorkspacePreserved, path, err)
		}
		if strings.TrimSpace(output) != "0" {
			return fmt.Errorf("%w at %s: the workspace session holds commits no ref reaches", ErrWorkspacePreserved, path)
		}
		return nil
	}
	output, err := runGitAt(ctx, path, "rev-list", "--count", "HEAD", "--not", headSHA)
	if err != nil {
		return fmt.Errorf("%w at %s: compare workspace session with %s: %w", ErrWorkspacePreserved, path, headSHA, err)
	}
	if strings.TrimSpace(output) != "0" {
		return fmt.Errorf("%w at %s: the workspace session committed past %s", ErrWorkspacePreserved, path, headSHA)
	}
	return nil
}

// deliveredCleanupHead consolidates recorded native landings and freshly
// verified merged PR delivery. Only the exact delivered head is disposable.
func deliveredCleanupHead(issue Issue) string {
	if issue.Terminal && strings.TrimSpace(issue.CleanupDeliveredHeadSHA) != "" {
		return strings.TrimSpace(issue.CleanupDeliveredHeadSHA)
	}
	if issueLandedAtHead(issue) {
		return strings.TrimSpace(issue.LandedHeadSHA)
	}
	return ""
}

func issueLandedAtHead(issue Issue) bool {
	return issue.Terminal && strings.TrimSpace(issue.LandedHeadSHA) != ""
}

func (l *LocalGit) checkCleanupBranch(ctx context.Context, branch string, session bool, issue Issue) error {
	if !l.autoBranch || !strings.HasPrefix(branch, autoBranchPrefix) {
		return nil
	}
	exists, err := l.branchExists(ctx, branch)
	if err != nil || !exists {
		return err
	}
	if session {
		// A session branch is a parking spot for a read-only checkout, so it
		// is removable while it holds no commit that lives on no other ref.
		// --single-worktree keeps --all from counting the session worktree's
		// own detached HEAD as another ref holding the commit.
		ref := "refs/heads/" + branch
		output, err := l.runGit(ctx, "rev-list", "--count", "--single-worktree", ref, "--not", "--exclude="+ref, "--all")
		if err != nil {
			return fmt.Errorf("inspect workspace session branch: %w", err)
		}
		if strings.TrimSpace(output) != "0" {
			return fmt.Errorf("%w: workspace session branch %s contains commits held by no other ref", ErrWorkspacePreserved, branch)
		}
		return nil
	}
	if deliveredCleanupHead(issue) != "" {
		head, err := l.runGit(ctx, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("inspect landed cleanup branch: %w", err)
		}
		if strings.TrimSpace(head) == deliveredCleanupHead(issue) {
			return nil
		}
	}
	count, err := retainedGitCommitCount(ctx, l.sourceRoot, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("inspect cleanup branch: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("%w: branch %s contains commits absent from remote refs", ErrWorkspacePreserved, branch)
	}
	return nil
}

func (l *LocalGit) beginWorkspaceCleanup(ctx context.Context, info Info, issue Issue, isDir bool) error {
	if err := l.checkWorkspaceCleanup(ctx, info, issue); err != nil {
		return err
	}
	if err := l.recordCleanupOwnership(ctx, info, issue, isDir); err != nil {
		return err
	}
	record, err := l.readOwnershipRecord(cleanupOwnershipRecordRelativePath(info.Path))
	if err != nil {
		return err
	}
	record.CleanupStarted = true
	return l.writeOwnershipRecord(record)
}

func retainedGitCommitCount(ctx context.Context, path, revision string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, failedWorkspacePreservationTimeout)
	defer cancel()
	// A remote-tracking ref can outlive a deleted or force-pushed branch. Only
	// exclude commits whose tracking tip is still advertised by a live remote.
	// Unfetched remote tips provide no local ancestry proof, so retain the work
	// until the ordinary fetch path makes that evidence available.
	output, err := runGitAt(ctx, path, "for-each-ref", "--format=%(objectname)", "refs/remotes/")
	if err != nil {
		return 0, fmt.Errorf("inspect remote tracking refs: %w", err)
	}
	known := make(map[string]bool)
	for _, sha := range strings.Fields(output) {
		known[sha] = true
	}
	output, err = runGitAt(ctx, path, "remote")
	if err != nil {
		return 0, fmt.Errorf("list cleanup remotes: %w", err)
	}
	args := []string{"rev-list", "--count", revision, "--not"}
	for _, remote := range strings.Fields(output) {
		refs, err := runGitAtWithEnv(ctx, path, []string{"GIT_TERMINAL_PROMPT=0"}, "ls-remote", "--heads", "--refs", remote)
		if err != nil {
			return 0, fmt.Errorf("verify cleanup remote %s: %w", remote, err)
		}
		for _, line := range strings.Split(refs, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && known[fields[0]] {
				args = append(args, fields[0])
				delete(known, fields[0])
			}
		}
	}
	output, err = runGitAt(ctx, path, args...)
	if err != nil {
		return 0, fmt.Errorf("inspect retained commits: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, fmt.Errorf("parse retained commit count: %w", err)
	}
	return count, nil
}

func (f *Filesystem) PreserveIssue(ctx context.Context, issue Issue) (Preservation, error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return Preservation{}, err
	}
	result := Preservation{Path: info.Path}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, ErrMissingWorkspace
	}
	if !isDir {
		return result, fmt.Errorf("filesystem workspace is not a directory: %s", info.Path)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return result, err
	}
	defer f.closeRoot("preservation", root)
	if err := root.MkdirAll(filesystemRetentionPath(info), 0o700); err != nil {
		return result, fmt.Errorf("retain filesystem workspace: %w", err)
	}
	result.Preserved = true
	stat, err := f.DiffStat(ctx, info, issue)
	if err != nil {
		return result, fmt.Errorf("inspect retained filesystem workspace: %w", err)
	}
	result.Files = stat.Files
	result.LocalChangesVerified = true
	return result, nil
}

func (f *Filesystem) checkPreservedWorkspace(info Info) error {
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return err
	}
	defer f.closeRoot("preservation", root)
	if _, err := root.Lstat(filesystemRetentionPath(info)); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read filesystem workspace retention: %w", err)
	}
	return fmt.Errorf("%w at %s: filesystem output requires explicit disposition", ErrWorkspacePreserved, info.Path)
}

func filesystemRetentionPath(info Info) string {
	return filepath.Join(info.Key, ".detent", "retained")
}

// sessionRecorded reports session ownership from the caller's issue, the
// cleanup record, or the key-derived session branch.
func (l *LocalGit) sessionRecorded(info Info, issue Issue) bool {
	if sessionOwned(info, issue) {
		return true
	}
	record, err := l.readOwnershipRecord(cleanupOwnershipRecordRelativePath(info.Path))
	return err == nil && record.WorkspaceSession
}
