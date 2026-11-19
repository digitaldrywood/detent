package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (l *LocalGit) checkpointCommit(ctx context.Context, path, tree string, parents []string) (string, error) {
	args := []string{"commit-tree", tree}
	seen := make(map[string]bool)
	for _, parent := range parents {
		if !seen[parent] {
			args = append(args, "-p", parent)
			seen[parent] = true
		}
	}
	args = append(args, "-m", "detent: checkpoint")
	output, err := l.runCommit(ctx, path, nil, args...)
	return strings.TrimSpace(output), err
}

func (l *LocalGit) pushReworkCheckpoint(ctx context.Context, info Info, issue Issue, previous CheckpointRef, validate func(context.Context) error) (_ CheckpointRef, _ bool, err error) {
	paused, err := l.verifyReworkBranch(ctx, info, issue)
	if err != nil || !paused {
		return previous, false, errors.Join(ErrCheckpointUnsafe, err)
	}
	scratch, err := changeSourceScratch()
	if err != nil {
		return previous, false, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	t, err := captureCheckpointTransaction(ctx, info.Path, info.Branch, scratch)
	if err != nil {
		return previous, false, fmt.Errorf("capture transaction: %w", err)
	}
	git := checkpointRefGit(info.Path)
	if err := validateCheckpointTransaction(ctx, git, t); err != nil {
		return previous, false, fmt.Errorf("validate transaction: %w", err)
	}
	data, err := json.Marshal(t)
	if err != nil {
		return previous, false, err
	}
	manifest := filepath.Join(scratch, "transaction.json")
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		return previous, false, err
	}
	output, err := git(ctx, "hash-object", "-w", "--", manifest)
	if err != nil {
		return previous, false, err
	}
	digest := strings.TrimSpace(output)
	if previous.Ref == CheckpointRefName(issue) && previous.TreeSHA == digest && previous.HeadSHA == t.Head {
		return previous, false, nil
	}
	index := checkpointRefGit(info.Path, "GIT_INDEX_FILE="+filepath.Join(scratch, "transaction-index"))
	if _, err := index(ctx, "read-tree", "--empty"); err != nil {
		return previous, false, err
	}
	if _, err := index(ctx, "update-index", "--add", "--cacheinfo", "100644,"+digest+",transaction.json"); err != nil {
		return previous, false, err
	}
	var alternatives strings.Builder
	for i, entry := range t.Index {
		fmt.Fprintf(&alternatives, "100644 %s\talternatives/%d\x00", entry.Blob, i)
	}
	if _, err := runGitAtWithInput(ctx, info.Path, []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "transaction-index")}, false, alternatives.String(), "update-index", "-z", "--index-info"); err != nil {
		return previous, false, err
	}
	if _, err := index(ctx, "read-tree", "--prefix=worktree/", t.Worktree); err != nil {
		return previous, false, err
	}
	output, err = index(ctx, "write-tree")
	if err != nil {
		return previous, false, err
	}
	wrapper := strings.TrimSpace(output)
	parents := []string{t.Head, t.Original, t.Onto}
	if previous.CommitSHA != "" {
		parents = append(parents, previous.CommitSHA)
	}
	commit, err := l.checkpointCommit(ctx, info.Path, wrapper, parents)
	if err != nil {
		return previous, false, err
	}
	output, err = git(ctx, "rev-list", "--objects", "--disk-usage", commit, "--not", "--remotes")
	if err != nil {
		return previous, false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil || size > MaxCheckpointRefBytes {
		return previous, false, errors.Join(ErrCheckpointUnsafe, err)
	}
	if !checkpointTransactionMatches(ctx, info.Path, t) {
		return previous, false, fmt.Errorf("%w: rework changed during capture", ErrCheckpointUnsafe)
	}
	if err := validate(ctx); err != nil {
		return previous, false, err
	}
	ref := CheckpointRefName(issue)
	if _, err := git(ctx, "-c", "core.hooksPath="+os.DevNull, "-c", "push.followTags=false", "push", "--no-verify", "--force", "--recurse-submodules=no", defaultGitRemote, commit+":"+ref); err != nil {
		return previous, false, err
	}
	return CheckpointRef{Ref: ref, CommitSHA: commit, HeadSHA: t.Head, BaseSHA: t.Onto, TreeSHA: digest, Transaction: t}, true, nil
}
