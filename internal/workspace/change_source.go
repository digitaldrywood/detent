package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeSource struct {
	Version       tracker.ChangeVersion `json:"version"`
	Bundle        []byte                `json:"bundle,omitempty"`
	PreviousHeads []string              `json:"previous_heads,omitempty"`
	Transferred   bool                  `json:"transferred,omitempty"`
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
		for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if !checkpointPathAllowed(path) || strings.HasPrefix(strings.ToLower(path), ".detent/") {
			return capture, fmt.Errorf("%w: excluded source path %q", ErrCheckpointUnsafe, path)
		}
	}
	if err := checkpointHistory(ctx, git, base, head, paths); err != nil {
		return capture, err
	}
	objects, err := changeSourceObjects(ctx, directory, base, head)
	if err != nil {
		return capture, err
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return capture, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	prefix := filepath.Join(scratch, "source")
	packHash, err := runGitAtWithInput(ctx, directory, nil, false, objects,
		"pack-objects", "--delta-base-offset", "--max-pack-size=0", prefix)
	if err != nil {
		return capture, err
	}
	packPath := prefix + "-" + strings.TrimSpace(packHash) + ".pack"
	header := fmt.Sprintf("# v2 git bundle\n-%s base\n%s HEAD\n\n", base, head)
	info, err := os.Stat(packPath)
	if err != nil {
		return capture, err
	}
	if info.Size()+int64(len(header)) > tracker.MaxChangeSourceBytes {
		return capture, errors.New("change source bundle exceeds the native publication limit")
	}
	pack, err := os.ReadFile(packPath)
	if err != nil {
		return capture, err
	}
	capture.Bundle = append([]byte(header), pack...)
	path := filepath.Join(scratch, "source.bundle")
	if err := os.WriteFile(path, capture.Bundle, 0o600); err != nil {
		return capture, err
	}
	patch, truncated, err := gitDiffArgsWithinLimit(ctx, directory, nil, tracker.MaxChangeDiffBytes,
		"diff", "--no-ext-diff", "--no-textconv", "--find-renames", base, head, "--")
	if err != nil {
		return capture, err
	}
	if !truncated {
		capture.Patch = patch
	}
	digest, err := changeSourceDiff(ctx, directory, base, head)
	if err != nil {
		return capture, err
	}
	capture.Source = tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(capture.Bundle), DiffSHA256: digest, Bytes: int64(len(capture.Bundle))}
	if err := verifyChangeBundle(ctx, directory, path); err != nil {
		return capture, err
	}
	return capture, capture.Source.Validate(base, head, capture.Bundle)
}

func changeSourceObjects(ctx context.Context, directory, base, head string) (string, error) {
	objects, err := runGitAt(ctx, directory, "rev-list", "--objects", "--no-object-names", head, "^"+base)
	if err != nil {
		return "", err
	}
	types, err := runGitAtWithInput(ctx, directory, nil, false, objects, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return "", err
	}
	var retained strings.Builder
	retained.WriteString(objects)
	git := func(ctx context.Context, args ...string) (string, error) { return runGitAt(ctx, directory, args...) }
	for line := range strings.SplitSeq(strings.TrimSpace(types), "\n") {
		tree, kind, ok := strings.Cut(line, " ")
		if !ok || kind != "tree" {
			continue
		}
		attributes, err := git(ctx, "ls-tree", "--format=%(objectname)", tree, "--", ".gitattributes")
		if err != nil {
			return "", err
		}
		if attributes == "" {
			continue
		}
		if err := checkpointBlob(ctx, git, tree+":", ".gitattributes"); err != nil {
			return "", err
		}
		retained.WriteString(attributes)
	}
	return retained.String(), nil
}

func verifyChangeBundle(ctx context.Context, directory, path string) error {
	_, err := runGitAt(ctx, directory, "bundle", "verify", path)
	return err
}

func VerifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validLandingHead(source.BaseSHA) || !validLandingHead(source.HeadSHA) {
		return errors.New("change source verification requires immutable base and head")
	}
	if err := source.Validate(source.BaseSHA, source.HeadSHA, bundle); err != nil {
		return err
	}
	_, pack, _ := bytes.Cut(bundle, []byte("\n\n"))
	if err := changeSourcePackBudget(ctx, pack); err != nil {
		return err
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	git := func(input []byte, args ...string) error {
		command := exec.CommandContext(ctx, "git")
		command.Args = append([]string{"git"}, args...)
		command.Dir = scratch
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_NO_REPLACE_OBJECTS=1")
		command.Stdin = bytes.NewReader(input)
		command.WaitDelay = workspaceCommandWaitDelay
		procgroup.Configure(ctx, command)
		return command.Run()
	}
	if err := git(nil, "init", "--bare", "--object-format=sha1", "--template="); err != nil {
		return err
	}
	if err := git(pack, "-c", "pack.threads=1", "-c", "core.deltaBaseCacheLimit=16m", "index-pack", "--stdin", "--fsck-objects"); err != nil {
		return errors.Join(ctx.Err(), errors.New("source bundle pack is invalid; recreate it with git bundle create <path> HEAD ^<base>"))
	}
	if err := git(nil, "cat-file", "-e", source.HeadSHA+"^{commit}"); err != nil {
		return errors.Join(ctx.Err(), errors.New("source bundle pack lacks the reviewed commit; recreate it with git bundle create <path> HEAD ^<base>"))
	}
	return nil
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
	if err := verifyChangeBundle(ctx, l.sourceRoot, path); err != nil {
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

func (l *LocalGit) prepareChangeSourceWorktree(ctx context.Context, info Info, issue Issue) (bool, error) {
	if issue.Source == nil || issue.Landing != nil {
		return false, nil
	}
	l.createMu.Lock()
	defer l.createMu.Unlock()
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err == nil && paused && issue.Source.Transferred {
		return false, refuse(LandRefusalHeadMoved, "the transferred version has unresolved paused-rebase custody; preserve and reconcile the assigned worktree through source recovery before retrying transfer")
	}
	if err != nil || paused {
		return false, err
	}
	state, err := l.RecoveryState(ctx, info, issue)
	if err != nil {
		return false, err
	}
	if issue.Source.Transferred && issue.Source.Version.Source == nil {
		return false, refuse(LandRefusalMissingHead, "the transferred version has no retained bundle; republish the exact head with source capture before retrying transfer")
	}
	transferred := issue.Source.Transferred
	if !transferred {
		if state.HeadSHA == issue.Source.Version.HeadSHA || len(state.TrackedPaths) != 0 || len(state.UntrackedPaths) != 0 {
			return false, nil
		}
		previous := issue.Source.Version.Source != nil && slices.Contains(issue.Source.PreviousHeads, state.HeadSHA)
		if state.UnpushedCommits > 0 && !previous {
			return false, nil
		}
		if state.HeadSHA != issue.Source.Version.BaseSHA && !previous && !l.headIntegrated(ctx, info.Path, issue, state.HeadSHA) {
			return false, refuse(LandRefusalHeadMoved, fmt.Sprintf("the existing clean worktree head %s differs from the recorded source base and head of version %s; preserve its current head and reconcile the Change before recovery", state.HeadSHA, issue.Source.Version.ID))
		}
	}
	if transferred || state.HeadSHA != issue.Source.Version.BaseSHA {
		if _, err := l.quarantineWorktree(ctx, info.Path); err != nil {
			if transferred {
				return false, refuse(LandRefusalHeadMoved, fmt.Sprintf("transferred version %s has unresolved local source custody; reconcile the assigned worktree through source recovery before retrying transfer: %v", issue.Source.Version.ID, err))
			}
			return false, err
		}
		if _, err := l.runGit(ctx, "branch", "-f", info.Branch, issue.Source.Version.HeadSHA); err != nil {
			return false, err
		}
		created, err := l.ensureWorktree(ctx, info.Path, info.Branch, issue.Source.Version.HeadSHA)
		if err != nil {
			return false, l.preserveFailedWorkspace(ctx, info.Path, err)
		}
		if err := l.validateCreatedWorktree(ctx, info.Path); err != nil {
			return false, l.preserveFailedWorkspace(ctx, info.Path, err)
		}
		return created, nil
	}
	_, err = runGitAt(ctx, info.Path, "-c", "core.hooksPath="+os.DevNull, "reset", "--hard", issue.Source.Version.HeadSHA)
	return false, err
}

func (l *LocalGit) headIntegrated(ctx context.Context, path string, issue Issue, head string) bool {
	base, err := l.localProgressBase(ctx, path, issue)
	if err != nil || !validLandingHead(base) {
		return false
	}
	_, err = runGitAt(ctx, path, "merge-base", "--is-ancestor", head, base)
	return err == nil
}
