package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

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
