package workspace

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

const maxLandingConflictBytes = 16 << 20

type LandingResolution struct {
	BaseSHA       string
	HeadSHA       string
	ConflictFiles []string
	Unchanged     bool
	Gate          gate.CommandResult
}

type LandingConflictResolver interface {
	ResolveLandingConflict(context.Context, Info, Issue, LandOptions, func(context.Context, string, []string) error) (LandingResolution, error)
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
	root, err := os.OpenRoot(path)
	if err != nil {
		return result, fmt.Errorf("open resolution root: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	conflictSources := make(map[string]string, len(result.ConflictFiles))
	remainingBytes := maxLandingConflictBytes
	for _, name := range result.ConflictFiles {
		data, regular, readErr := readLandingConflictSource(ctx, root, name, remainingBytes)
		if readErr == nil && regular {
			conflictSources[name] = data
			remainingBytes -= len(data)
		} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return result, readErr
		}
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
		return result, fmt.Errorf("%w: unresolved conflicts", ErrMergeResolutionInvalid)
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
	if ok, err := landingResolutionScope(ctx, path, before, after, result.ConflictFiles); err != nil || !ok {
		return result, errors.Join(err, fmt.Errorf("%w: resolution changed nonconflicting files", ErrMergeResolutionInvalid))
	}

	for name, original := range conflictSources {
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), name, original, parser.PackageClauseOnly|parser.ParseComments)
		if parseErr == nil && ast.IsGenerated(parsed) {
			continue
		}
		data, regular, readErr := readLandingConflictSource(ctx, root, name, maxLandingConflictBytes)
		if errors.Is(readErr, os.ErrNotExist) && !strings.Contains(original, "<<<<<<< ") {
			continue
		}
		if readErr != nil {
			return result, readErr
		}
		if !regular || !landingConflictHunksPreserved(original, data) {
			return result, fmt.Errorf("%w: resolution changed nonconflicting hunks", ErrMergeResolutionInvalid)
		}
	}
	if _, err := l.runCommit(ctx, path, nil, "commit", "-m", opts.Message); err != nil {
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

func readLandingConflictSource(ctx context.Context, root *os.Root, name string, limit int) (content string, regular bool, resultErr error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return "", false, fmt.Errorf("inspect conflict source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, nil
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return "", false, fmt.Errorf("%w: conflict source is not a bounded regular file", ErrMergeResolutionInvalid)
	}
	file, err := root.OpenFile(name, os.O_RDONLY|landingConflictOpenFlags, 0)
	if err != nil {
		return "", false, fmt.Errorf("open conflict source: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return "", false, fmt.Errorf("inspect opened conflict source: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > int64(limit) {
		return "", false, fmt.Errorf("%w: conflict source changed while opening", ErrMergeResolutionInvalid)
	}
	var data strings.Builder
	data.Grow(int(opened.Size()))
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		n, readErr := file.Read(buffer[:min(len(buffer), limit-data.Len()+1)])
		if data.Len()+n > limit {
			return "", false, fmt.Errorf("%w: conflict source exceeds byte limit", ErrMergeResolutionInvalid)
		}
		data.Write(buffer[:n])
		if errors.Is(readErr, io.EOF) {
			return data.String(), true, nil
		}
		if readErr != nil {
			return "", false, fmt.Errorf("read conflict source: %w", readErr)
		}
	}
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

func landingResolutionScope(ctx context.Context, path, before, after string, conflicts []string) (bool, error) {
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
	for name, value := range resolved {
		if original[name] == value {
			delete(original, name)
		}
	}
	for name, metadata := range original {
		fields := strings.Fields(metadata)
		if len(fields) != 3 || fields[2] != "0" {
			return false, nil
		}
		data, err := runGitAt(ctx, path, "cat-file", "blob", fields[1])
		if err != nil {
			return false, err
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), name, data, parser.PackageClauseOnly|parser.ParseComments)
		if parseErr != nil || !ast.IsGenerated(parsed) {
			return false, nil
		}
		delete(resolved, name)
	}
	for name, value := range resolved {
		if value != "" && !strings.Contains(before, "\t"+name+"\x00") {
			return false, nil
		}
	}
	return true, nil
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
		return runGitAt(ctx, path, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--unified=0", strings.TrimSpace(from), to)
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

func landingConflictHunksPreserved(original, resolved string) bool {
	var chunks []string
	var outside strings.Builder
	inside := false
	for _, line := range strings.SplitAfter(original, "\n") {
		if !inside && strings.HasPrefix(line, "<<<<<<< ") {
			chunks = append(chunks, outside.String())
			outside.Reset()
			inside = true
		} else if inside && strings.HasPrefix(line, ">>>>>>> ") {
			inside = false
		} else if !inside {
			outside.WriteString(line)
		}
	}
	if len(chunks) == 0 {
		return true
	}
	chunks = append(chunks, outside.String())
	if !strings.HasPrefix(resolved, chunks[0]) {
		return false
	}
	remaining := strings.TrimPrefix(resolved, chunks[0])
	for _, chunk := range chunks[1 : len(chunks)-1] {
		index := strings.Index(remaining, chunk)
		if index < 0 {
			return false
		}
		remaining = remaining[index+len(chunk):]
	}
	return strings.HasSuffix(remaining, chunks[len(chunks)-1])
}
