package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

var ErrLandingConflictUnresolved = errors.New("unresolved conflicts")

type LandingResolution struct {
	BaseSHA       string
	HeadSHA       string
	ConflictFiles []string
	Unchanged     bool
	Gate          gate.CommandResult
}

type LandingConflictResolver interface {
	InspectLandingConflict(context.Context, Info, Issue, LandOptions) (string, bool, error)
	ResolveLandingConflict(context.Context, Info, Issue, LandOptions, func(context.Context, string, []string) error) (LandingResolution, error)
}

func (l *LocalGit) InspectLandingConflict(ctx context.Context, info Info, issue Issue, opts LandOptions) (string, bool, error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", false, err
	}
	if opts.Authorize != nil {
		if err := opts.Authorize(ctx); err != nil {
			return "", false, err
		}
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return "", false, err
	}
	defer release()
	if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
		return "", false, err
	}
	head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(head) != opts.HeadSHA {
		return "", false, refuse(LandRefusalHeadMoved, "landing inspection requires the reviewed head")
	}
	remote := opts.Remote
	if remote == "" {
		remote = defaultGitRemote
	}
	target := opts.TargetBranch
	if target == "" {
		target, err = remoteDefaultBranch(ctx, info.Path, remote)
		if err != nil {
			return "", false, err
		}
	}
	base, exists, err := remoteBranchHead(ctx, info.Path, remote, target)
	if err != nil {
		return "", false, err
	}
	if !exists {
		return "", false, refuse(LandRefusalMissingHead, "the integration branch is unavailable")
	}
	if _, err := runGitAt(ctx, info.Path, "fetch", "--no-tags", "--no-write-fetch-head", remote, base); err != nil {
		return "", false, err
	}
	method := opts.Method
	if method == "" {
		method = "squash"
	}
	if method != "squash" && method != "merge" && method != "rebase" {
		return base, false, fmt.Errorf("unsupported merge method %q", method)
	}
	delivered, err := deliveredLanding(ctx, info.Path, opts, base, target, method)
	if err != nil || delivered.MergeSHA != "" {
		return base, false, err
	}
	_, err = runGitAt(ctx, info.Path, "merge-tree", "--write-tree", base, opts.HeadSHA)
	if err == nil {
		return base, false, nil
	}
	if ctx.Err() != nil {
		return base, false, ctx.Err()
	}
	var command *CommandError
	if errors.As(err, &command) && command.ExitCode == 1 {
		tree, _, _ := strings.Cut(command.Output, "\n")
		if validLandingHead(strings.TrimSpace(tree)) {
			return base, true, nil
		}
	}
	return base, false, err
}

func (l *LocalGit) ResolveLandingConflict(ctx context.Context, info Info, issue Issue, opts LandOptions, resolve func(context.Context, string, []string) error) (result LandingResolution, resultErr error) {
	info, err := l.normalizeInfo(info, issue)
	if err != nil {
		return result, err
	}
	if opts.Authorize != nil {
		if err := opts.Authorize(ctx); err != nil {
			return result, err
		}
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
		return result, err
	}
	head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(head) != opts.HeadSHA {
		return result, refuse(LandRefusalHeadMoved, "merge resolution requires the reviewed head")
	}
	remote := opts.Remote
	if remote == "" {
		remote = defaultGitRemote
	}
	target := opts.TargetBranch
	if target == "" {
		target, err = remoteDefaultBranch(ctx, info.Path, remote)
		if err != nil {
			return result, err
		}
	}
	ref := "refs/remotes/" + remote + "/" + target
	if _, err := runGitAt(ctx, info.Path, "fetch", remote, "+refs/heads/"+target+":"+ref); err != nil {
		return result, err
	}
	base, err := runGitAt(ctx, info.Path, "rev-parse", ref)
	if err != nil {
		return result, err
	}
	result.BaseSHA = strings.TrimSpace(base)
	if opts.ExpectedBaseSHA != "" && result.BaseSHA != opts.ExpectedBaseSHA {
		return result, &LandRefusal{Kind: LandRefusalBaseMoved, BaseSHA: result.BaseSHA, TargetBranch: target, Reason: "the integration base changed after conflict inspection"}
	}
	path := filepath.Join(l.root, "resolution-"+info.Key)
	if _, err := runGitAt(ctx, info.Path, "worktree", "add", "--detach", path, result.BaseSHA); err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if release == nil {
			var lockErr error
			release, lockErr = l.acquireSourceOperation(cleanupCtx)
			if lockErr != nil {
				resultErr = errors.Join(resultErr, lockErr)
				return
			}
		}
		resultErr = errors.Join(resultErr, l.removeLandingWorktree(cleanupCtx, l.sourceRoot, path))
	}()
	_, mergeErr := runGitAt(ctx, path, "-c", "rerere.enabled=false", "merge", "--squash", "--no-commit", opts.HeadSHA)
	conflicts, err := runGitAt(ctx, path, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return result, err
	}
	result.ConflictFiles = nonemptyLandingPaths(conflicts)
	if mergeErr != nil && len(result.ConflictFiles) == 0 {
		return result, mergeErr
	}
	before, err := runGitAt(ctx, path, "ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	release()
	release = nil
	if len(result.ConflictFiles) > 0 {
		if err := resolve(ctx, path, result.ConflictFiles); err != nil {
			return result, fmt.Errorf("resolve landing conflicts: %w", err)
		}
	}
	currentHead, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(currentHead) != result.BaseSHA {
		return result, fmt.Errorf("%w: resolution changed the integration base", ErrMergeResolutionInvalid)
	}
	remaining, err := runGitAt(ctx, path, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return result, err
	}
	if remaining != "" {
		return result, fmt.Errorf("%w: %w", ErrMergeResolutionInvalid, ErrLandingConflictUnresolved)
	}
	if _, err := runGitAt(ctx, path, "diff", "--quiet"); err != nil {
		return result, fmt.Errorf("%w: resolution must be staged", ErrMergeResolutionInvalid)
	}
	untracked, err := runGitAt(ctx, path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return result, err
	}
	if untracked != "" {
		return result, fmt.Errorf("%w: resolution left untracked source", ErrMergeResolutionInvalid)
	}
	after, err := runGitAt(ctx, path, "ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	if !landingResolutionScope(before, after, result.ConflictFiles) {
		return result, fmt.Errorf("%w: resolution changed nonconflicting files", ErrMergeResolutionInvalid)
	}
	if opts.PreserveSourceHistory {
		tree, err := runGitAt(ctx, path, "write-tree")
		if err != nil {
			return result, err
		}
		resolved, err := l.runCommit(ctx, path, nil, "commit-tree", strings.TrimSpace(tree), "-p", result.BaseSHA, "-p", opts.HeadSHA, "-m", opts.Message)
		if err != nil {
			return result, err
		}
		if _, err := runGitAt(ctx, path, "update-ref", "HEAD", strings.TrimSpace(resolved), result.BaseSHA); err != nil {
			return result, err
		}
	} else if _, err := l.runCommit(ctx, path, nil, "-c", "core.hooksPath="+os.DevNull, "commit", "-m", opts.Message); err != nil {
		return result, err
	}
	resolved, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.HeadSHA = strings.TrimSpace(resolved)
	result.Unchanged, err = sameLandingDiff(ctx, path, opts.HeadSHA, result.HeadSHA, result.BaseSHA)
	if err != nil {
		return result, err
	}
	validationInfo := info
	validationInfo.Path = path
	result.Gate, err = l.validateLanding(ctx, validationInfo, issue, opts.ValidationCommand, result.HeadSHA, result.BaseSHA)
	if err != nil {
		return result, err
	}
	if _, err := runGitAt(ctx, path, "diff", "--quiet", "HEAD"); err != nil {
		return result, fmt.Errorf("%w: gate changed the resolution", ErrMergeResolutionInvalid)
	}
	currentHead, err = runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(currentHead) != result.HeadSHA {
		return result, fmt.Errorf("%w: gate changed the resolution head", ErrMergeResolutionInvalid)
	}
	if opts.Authorize != nil {
		if err := opts.Authorize(ctx); err != nil {
			return result, err
		}
	}
	release, err = l.acquireSourceOperation(ctx)
	if err != nil {
		return result, err
	}

	if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
		return result, err
	}
	currentHead, err = runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(currentHead) != opts.HeadSHA {
		return result, refuse(LandRefusalHeadMoved, "reviewed source moved during merge resolution")
	}
	if !result.Unchanged {
		if _, err := runGitAt(ctx, info.Path, "reset", "--hard", result.HeadSHA); err != nil {
			return result, err
		}
	}
	return result, nil
}

func nonemptyLandingPaths(value string) []string {
	paths := []string{}
	for _, path := range strings.Split(value, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func landingResolutionScope(before, after string, conflicts []string) bool {
	allowed := make(map[string]bool, len(conflicts))
	for _, name := range conflicts {
		allowed[name] = true
	}
	entries := func(value string) map[string]string {
		out := make(map[string]string)
		for _, entry := range strings.Split(value, "\x00") {
			metadata, name, ok := strings.Cut(entry, "\t")
			if ok && !allowed[name] {
				out[name] += metadata
			}
		}
		return out
	}
	original, resolved := entries(before), entries(after)
	if len(original) != len(resolved) {
		return false
	}
	for name, value := range resolved {
		if orig, existed := original[name]; !existed || orig != value {
			return false
		}
	}
	return true
}

type landingDiffHunk struct{ oldStart, oldLines, newStart, newLines int }

func parseLandingHunk(line string) (landingDiffHunk, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return landingDiffHunk{}, errors.New("invalid landing diff hunk")
	}
	span := func(value string) (int, int, error) {
		parts := strings.SplitN(value[1:], ",", 2)
		start, err := strconv.Atoi(parts[0])
		count := 1
		if err == nil && len(parts) == 2 {
			count, err = strconv.Atoi(parts[1])
		}
		return start, count, err
	}
	oldStart, oldLines, err := span(fields[1])
	if err != nil {
		return landingDiffHunk{}, err
	}
	newStart, newLines, err := span(fields[2])
	if err != nil {
		return landingDiffHunk{}, err
	}
	return landingDiffHunk{oldStart, oldLines, newStart, newLines}, nil
}

func sameLandingDiff(ctx context.Context, path, reviewed, resolved, base string) (bool, error) {
	fork, err := runGitAt(ctx, path, "merge-base", reviewed, base)
	if err != nil {
		return false, err
	}
	diff := func(from, to string) (string, error) {
		return runGitAt(ctx, path, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none", "--binary", "--unified=0", strings.TrimSpace(from), to)
	}
	baseDiff, err := diff(fork, base)
	if err != nil {
		return false, err
	}
	baseHunks := make(map[string][]landingDiffHunk)
	file := ""
	for _, line := range strings.Split(baseDiff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			file = line
		}
		if strings.HasPrefix(line, "@@ ") {
			hunk, err := parseLandingHunk(line)
			if err != nil {
				return false, err
			}
			baseHunks[file] = append(baseHunks[file], hunk)
		}
	}
	normalize := func(value string, translate bool) (string, bool, error) {
		var out strings.Builder
		binary := strings.Contains(value, "\nGIT binary patch\n")
		for _, line := range strings.Split(value, "\n") {
			if strings.HasPrefix(line, "diff --git ") {
				file = line
			}
			if strings.HasPrefix(line, "index ") && !binary {
				continue
			}
			if strings.HasPrefix(line, "@@ ") {
				hunk, err := parseLandingHunk(line)
				if err != nil {
					return "", false, err
				}
				offset := 0
				if translate {
					for _, change := range baseHunks[file] {
						if change.oldLines == 0 {
							if change.oldStart < hunk.oldStart {
								offset += change.newLines
							} else if change.oldStart < hunk.oldStart+hunk.oldLines-1 || hunk.oldLines == 0 && change.oldStart == hunk.oldStart {
								return "", false, nil
							}
						} else if change.oldStart+change.oldLines <= hunk.oldStart {
							offset += change.newLines - change.oldLines
						} else if change.oldStart < hunk.oldStart+max(1, hunk.oldLines) {
							return "", false, nil
						}
					}
				}
				line = fmt.Sprintf("@@ -%d,%d +%d,%d @@", hunk.oldStart+offset, hunk.oldLines, hunk.newStart+offset, hunk.newLines)
			}
			out.WriteString(line)
			out.WriteByte('\n')
		}
		return out.String(), true, nil
	}
	originalDiff, err := diff(fork, reviewed)
	if err != nil {
		return false, err
	}
	integratedDiff, err := diff(base, resolved)
	if err != nil {
		return false, err
	}
	original, valid, err := normalize(originalDiff, true)
	if err != nil || !valid {
		return false, err
	}
	integrated, _, err := normalize(integratedDiff, false)
	return original == integrated, err
}
