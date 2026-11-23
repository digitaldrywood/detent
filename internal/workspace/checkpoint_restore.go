package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
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

func (l *LocalGit) fetchCheckpointRef(ctx context.Context, ref *CheckpointRef) error {
	if !validLandingHead(ref.CommitSHA) || !validLandingHead(ref.TreeSHA) || !strings.HasPrefix(ref.Ref, CheckpointRefPrefix) {
		return errors.New("checkpoint reference is incomplete")
	}
	if _, err := l.runGit(ctx, "cat-file", "-e", ref.CommitSHA+"^{commit}"); err != nil {
		if err := gitexec.FetchCheckpoint(ctx, l.sourceRoot, defaultGitRemote, ref.CommitSHA, ref.Ref); err != nil {
			return err
		}
	}
	kind, err := l.runGit(ctx, "cat-file", "-t", ref.TreeSHA)
	if err != nil {
		return err
	}
	if strings.TrimSpace(kind) == "blob" {
		transaction, err := checkpoint.ReadTransaction(ctx, checkpointRefGit(l.sourceRoot), ref)
		if err != nil {
			return err
		}
		ref.Transaction = transaction
		ref.HeadSHA, ref.BaseSHA = transaction.Head, transaction.Onto
		return nil
	}
	tree, err := l.runGit(ctx, "rev-parse", ref.CommitSHA+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != ref.TreeSHA {
		return errors.Join(errors.New("checkpoint commit differs from its recorded tree"), err)
	}
	parent, err := l.runGit(ctx, "rev-parse", ref.CommitSHA+"^1")
	if err != nil {
		return fmt.Errorf("read checkpoint head: %w", err)
	}
	ref.HeadSHA = strings.TrimSpace(parent)
	return nil
}
