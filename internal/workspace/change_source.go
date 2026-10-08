package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeSource struct {
	Version tracker.ChangeVersion `json:"version"`
	Bundle  []byte                `json:"bundle,omitempty"`
}

func changeSourceScratch() (string, error) {
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if root := os.Getenv(key); root != "" {
			return os.MkdirTemp(root, "detent-change-source-*")
		}
	}
	return os.MkdirTemp("", "detent-change-source-*")
}

func changeSourceDiff(ctx context.Context, directory, base, head string) (string, error) {
	diff, err := runGitAt(ctx, directory, "diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	return tracker.ChangeSourceDigest([]byte(diff)), err
}

func CaptureChangeSource(ctx context.Context, directory, base, head string) (capture tracker.ChangeSourceCapture, err error) {
	if !validLandingHead(base) || !validLandingHead(head) || base == head {
		return capture, errors.New("change source capture requires immutable base and head")
	}
	observed, err := runGitAt(ctx, directory, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(observed) != head {
		return capture, errors.Join(errors.New("change source capture differs from the finalized head"), err)
	}
	git := func(ctx context.Context, args ...string) (string, error) { return runGitAt(ctx, directory, args...) }
	commits, err := git(ctx, "rev-list", base+".."+head)
	if err != nil {
		return capture, err
	}
	var paths []string
	for _, commit := range strings.Fields(commits) {
		changed, err := git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return capture, err
		}
		paths = append(paths, strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00")...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if tracker.DiffPathDenied(path) || strings.HasPrefix(strings.ToLower(path), ".detent/") {
			return capture, fmt.Errorf("%w: excluded source path %q", ErrCheckpointUnsafe, path)
		}
	}
	if err := checkpointHistory(ctx, git, base, head, paths); err != nil {
		return capture, err
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return capture, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	path := filepath.Join(scratch, "source.bundle")
	if _, err := git(ctx, "bundle", "create", path, "HEAD", "^"+base); err != nil {
		return capture, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return capture, err
	}
	if info.Size() > tracker.MaxChangeSourceBytes {
		return capture, errors.New("change source bundle exceeds the native publication limit")
	}
	capture.Bundle, err = os.ReadFile(path)
	if err != nil {
		return capture, err
	}
	digest, err := changeSourceDiff(ctx, directory, base, head)
	if err != nil {
		return capture, err
	}
	capture.Source = tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(capture.Bundle), DiffSHA256: digest, Bytes: int64(len(capture.Bundle))}
	if err := verifyChangeBundle(ctx, directory, path, head); err != nil {
		return capture, err
	}
	return capture, capture.Source.Validate(base, head, capture.Bundle)
}

func verifyChangeBundle(ctx context.Context, directory, path, head string) error {
	heads, err := runGitAt(ctx, directory, "bundle", "list-heads", path)
	if err != nil || strings.TrimSpace(heads) != head+" HEAD" {
		return errors.Join(errors.New("source bundle does not advertise only the reviewed head"), err)
	}
	_, err = runGitAt(ctx, directory, "bundle", "verify", path)
	return err
}

func (l *LocalGit) importChangeSource(ctx context.Context, source *ChangeSource) (err error) {
	version := source.Version
	if !validLandingHead(version.HeadSHA) || !validLandingHead(version.BaseSHA) || version.Repository == "" || RepositoryURL(ctx, l.sourceRoot) != version.Repository {
		return refuse(LandRefusalMissingHead, "the retained Change source does not match the authorized repository")
	}
	if version.Source == nil {
		if _, err := l.runGit(ctx, "cat-file", "-e", version.HeadSHA+"^{commit}"); err != nil {
			return refuse(LandRefusalMissingHead, "this legacy Change has no retained source; resume on the source-owning runner and republish the exact head with source capture")
		}
		return nil
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, source.Bundle); err != nil {
		return err
	}
	if _, err := l.runGit(ctx, "cat-file", "-e", version.BaseSHA+"^{commit}"); err != nil {
		if _, err := l.runGit(ctx, "fetch", "--no-tags", "--no-write-fetch-head", defaultGitRemote, version.BaseSHA); err != nil {
			return fmt.Errorf("retrieve retained source base: %w", err)
		}
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	path := filepath.Join(scratch, "source.bundle")
	if err := os.WriteFile(path, source.Bundle, 0o600); err != nil {
		return err
	}
	if err := verifyChangeBundle(ctx, l.sourceRoot, path, version.HeadSHA); err != nil {
		return err
	}
	if _, err := l.runGit(ctx, "-c", "fetch.fsckObjects=true", "bundle", "unbundle", path); err != nil {
		return err
	}
	digest, err := changeSourceDiff(ctx, l.sourceRoot, version.BaseSHA, version.HeadSHA)
	if err != nil || digest != version.Source.DiffSHA256 {
		return errors.Join(errors.New("recovered Change source diff differs from the immutable version"), err)
	}
	return nil
}

func (l *LocalGit) prepareChangeSourceWorktree(ctx context.Context, info Info, issue Issue) error {
	if issue.Source == nil || issue.Landing != nil {
		return nil
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || paused {
		return err
	}
	state, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return err
	}
	if state.HeadSHA == issue.Source.Version.HeadSHA || len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 || state.UnpushedCommits > 0 {
		return nil
	}
	if state.HeadSHA != issue.Source.Version.BaseSHA {
		return refuse(LandRefusalHeadMoved, "the existing clean worktree differs from the recorded source base and head; preserve its current head and reconcile the Change before recovery")
	}
	_, err = runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "reset", "--hard", issue.Source.Version.HeadSHA)
	return err
}
