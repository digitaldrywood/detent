package gitdiff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

const recoveryEvidenceLimit = 20

var (
	diffStatFilesPattern   = regexp.MustCompile(`(\d+)\s+files?\s+changed`)
	diffStatAddedPattern   = regexp.MustCompile(`(\d+)\s+insertions?\(\+\)`)
	diffStatRemovedPattern = regexp.MustCompile(`(\d+)\s+deletions?\(-\)`)
)

var ErrMissingWorkspace = errors.New("workspace missing")

type DiffStat struct {
	Files          int       `json:"files"`
	Added          int       `json:"added"`
	Removed        int       `json:"removed"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
	TreeSHA        string    `json:"tree_sha,omitempty"`
	HeadSHA        string    `json:"head_sha,omitempty"`
	HeadObservedAt time.Time `json:"head_observed_at,omitzero"`
}

func (s DiffStat) IsEmpty() bool {
	return s.Files == 0 && s.Added == 0 && s.Removed == 0 && s.Fingerprint == ""
}

type Diff struct {
	Stat      DiffStat
	Patch     string
	Truncated bool
}

func GitDiffStat(ctx context.Context, workspacePath string) (DiffStat, error) {
	return Stat(ctx, workspacePath, false)
}

func Stat(ctx context.Context, workspacePath string, includeIndex bool) (DiffStat, error) {
	observation, err := Observe(ctx, workspacePath, includeIndex, nil)
	return observation.Stat, err
}

type Observation struct {
	Stat           DiffStat
	TrackedPaths   []string
	UntrackedPaths []string
}

type gitDiffInspection func(context.Context, string, string, string, func(context.Context) error) error

func Observe(ctx context.Context, workspacePath string, includeIndex bool, inspect gitDiffInspection) (Observation, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return Observation{}, errors.New("workspace path is required")
	}
	if _, err := os.Stat(workspacePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Observation{}, fmt.Errorf("%w: %s: %w", ErrMissingWorkspace, workspacePath, err)
		}
		return Observation{}, fmt.Errorf("stat workspace path: %w", err)
	}

	return gitDiffStatOutput(ctx, workspacePath, includeIndex, inspect)
}

func GitDiff(ctx context.Context, workspacePath string, maxBytes int) (Diff, error) {
	return GitDiffFrom(ctx, workspacePath, "", maxBytes)
}

func GitDiffFrom(ctx context.Context, workspacePath string, baseRef string, maxBytes int) (Diff, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return Diff{}, errors.New("workspace path is required")
	}
	if maxBytes < 0 {
		return Diff{}, errors.New("max bytes must be greater than or equal to 0")
	}
	if _, err := os.Stat(workspacePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Diff{}, fmt.Errorf("%w: %s: %w", ErrMissingWorkspace, workspacePath, err)
		}
		return Diff{}, fmt.Errorf("stat workspace path: %w", err)
	}

	indexPath, err := IndexPath(ctx, workspacePath)
	if err != nil {
		return Diff{}, err
	}
	tempIndex, cleanup, err := copyGitIndex(indexPath)
	if err != nil {
		return Diff{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	if _, err := gitexec.WithEnv(ctx, workspacePath, env, "add", "--intent-to-add", "--", "."); err != nil {
		return Diff{}, fmt.Errorf("git add intent to add: %w", err)
	}
	diffBase := gitDiffBase(ctx, workspacePath, baseRef)
	statOutput, err := gitexec.WithEnv(ctx, workspacePath, env, DiagnosticArgs("diff", "--stat", diffBase)...)
	if err != nil {
		return Diff{}, fmt.Errorf("git diff stat: %w", err)
	}
	stat, err := ParseDiffStat(statOutput)
	if err != nil {
		return Diff{}, err
	}
	if maxBytes == 0 {
		return Diff{Stat: stat, Truncated: !stat.IsEmpty()}, nil
	}

	patch, truncated, err := gitDiffOutputWithinLimit(ctx, workspacePath, env, diffBase, maxBytes)
	if err != nil {
		return Diff{}, err
	}
	if truncated {
		patch = ""
	}
	return Diff{Stat: stat, Patch: patch, Truncated: truncated}, nil
}

func gitDiffStatOutput(ctx context.Context, workspacePath string, includeIndex bool, inspect gitDiffInspection) (Observation, error) {
	index, err := Index(ctx, workspacePath, true)
	if err != nil {
		return Observation{}, err
	}
	tempIndex, cleanup, err := copyGitIndex(index.Path)
	if err != nil {
		return Observation{}, err
	}
	defer cleanup()

	if inspect == nil {
		return gitDiffWithIndex(ctx, workspacePath, tempIndex, index, includeIndex)
	}
	var observation Observation
	err = inspect(ctx, workspacePath, tempIndex, index.HeadSHA, func(inspectionCtx context.Context) error {
		var err error
		observation, err = gitDiffWithIndex(inspectionCtx, workspacePath, tempIndex, index, false)
		return err
	})
	return observation, err
}

func gitDiffWithIndex(ctx context.Context, workspacePath, tempIndex string, index IndexObservation, includeIndex bool) (Observation, error) {
	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	addPaths := []string{"."}
	addEnv := env
	var observation Observation
	var trackedPaths []string
	if includeIndex {
		output, err := gitexec.WithEnv(ctx, workspacePath, env, DiagnosticArgs("ls-files", "--others", "--exclude-standard", "-z")...)
		if err != nil {
			return Observation{}, fmt.Errorf("git list untracked workspace changes: %w", err)
		}
		observation.UntrackedPaths = boundedNULFields(output, recoveryEvidenceLimit, false)
		addPaths = strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
		if output == "" {
			addPaths = nil
		}
		addEnv = append(slices.Clone(env), "GIT_LITERAL_PATHSPECS=1")
		trackedPaths, err = gitDiffPaths(ctx, workspacePath, env, []string{"--cached", index.HeadSHA}, nil)
		if err != nil {
			return Observation{}, err
		}
		observation.TrackedPaths = trackedPaths[:min(len(trackedPaths), recoveryEvidenceLimit)]
	}
	if len(addPaths) != 0 {
		args := append([]string{"add", "--intent-to-add", "--"}, addPaths...)
		if _, err := gitexec.WithEnv(ctx, workspacePath, addEnv, args...); err != nil {
			return Observation{}, fmt.Errorf("git add intent to add: %w", err)
		}
	}
	var stat DiffStat
	var err error
	if includeIndex {
		staged, err := gitDiffStatWithEnv(ctx, workspacePath, env, "--cached", index.HeadSHA)
		if err != nil {
			return Observation{}, err
		}
		working, err := gitDiffStatWithEnv(ctx, workspacePath, env)
		if err != nil {
			return Observation{}, err
		}
		paths := slices.Clone(trackedPaths)
		for _, path := range addPaths {
			paths = append(paths, strings.TrimSuffix(path, "/"))
		}
		slices.Sort(paths)
		paths = slices.Compact(paths)
		stat = DiffStat{Files: len(paths), Added: staged.Added + working.Added, Removed: staged.Removed + working.Removed}
		if !stat.IsEmpty() {
			stat.Fingerprint = RecoveryFingerprint("index", staged.Fingerprint, "worktree", working.Fingerprint)
		}
	} else {
		stat, err = gitDiffStatWithEnv(ctx, workspacePath, env, index.HeadSHA)
		if err != nil {
			return Observation{}, err
		}
	}
	stat.TreeSHA = index.TreeSHA
	stat.HeadSHA = index.HeadSHA
	stat.HeadObservedAt = index.HeadObservedAt
	observation.Stat = stat
	return observation, nil
}

func UnpushedCommits(ctx context.Context, workspacePath string, base string, excludedRemoteRefs, extraPublished []string, verifiedRemote bool) (int, []string, error) {
	remoteRefs, err := gitexec.Run(ctx, workspacePath, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return 0, nil, fmt.Errorf("git list remote refs: %w", err)
	}
	hasRemoteRefs := strings.TrimSpace(remoteRefs) != ""
	if !hasRemoteRefs && !verifiedRemote {
		return 0, nil, nil
	}
	revArgs := []string{"HEAD", "--not"}
	if !hasRemoteRefs {
		revArgs = append(revArgs, base)
	} else if len(excludedRemoteRefs) > 0 {
		originHeadTarget := ""
		if target, symErr := gitexec.Run(ctx, workspacePath, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); symErr == nil {
			originHeadTarget = strings.TrimSpace(target)
		}
		excludedHead := false
		for _, excludedRemoteRef := range excludedRemoteRefs {
			if excludedRemoteRef == "" {
				continue
			}
			revArgs = append(revArgs, "--exclude="+excludedRemoteRef)
			if excludedRemoteRef == "refs/remotes/origin/HEAD" && originHeadTarget != "" {
				revArgs = append(revArgs, "--exclude="+originHeadTarget)
				excludedHead = true
			} else if originHeadTarget != "" && originHeadTarget == excludedRemoteRef && !excludedHead {
				revArgs = append(revArgs, "--exclude=refs/remotes/origin/HEAD")
				excludedHead = true
			}
		}
		revArgs = append(revArgs, "--glob=refs/remotes/", base)
	} else {
		revArgs = append(revArgs, "--remotes", base)
	}
	revArgs = append(revArgs, extraPublished...)
	output, err := gitexec.Run(ctx, workspacePath, append([]string{"rev-list", "--count"}, revArgs...)...)
	if err != nil {
		return 0, nil, fmt.Errorf("git count unpushed commits: %w", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, nil, fmt.Errorf("parse unpushed commit count: %w", err)
	}
	if count == 0 {
		return 0, nil, nil
	}
	refs, err := gitCommitEvidence(ctx, workspacePath, revArgs...)
	if err != nil {
		return 0, nil, fmt.Errorf("git list unpushed commits: %w", err)
	}
	return count, refs, nil
}

func gitDiffPaths(ctx context.Context, workspacePath string, env []string, comparisons ...[]string) ([]string, error) {
	var paths []string
	for _, comparison := range comparisons {
		args := append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "-z"}, comparison...)
		output, err := gitexec.WithEnv(ctx, workspacePath, env, DiagnosticArgs(args...)...)
		if err != nil {
			return nil, err
		}
		for path := range strings.SplitSeq(output, "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func CommitsNotInPullRequest(
	ctx context.Context,
	workspacePath string,
	head string,
	pullRequestHead string,
	base string,
) ([]string, bool, error) {
	head = strings.TrimSpace(head)
	pullRequestHead = strings.TrimSpace(pullRequestHead)
	if pullRequestHead == "" {
		return nil, false, nil
	}
	if head == pullRequestHead {
		return nil, true, nil
	}
	if _, err := gitexec.Run(ctx, workspacePath, "cat-file", "-e", pullRequestHead+"^{commit}"); err != nil {
		return nil, false, nil
	}
	refs, err := gitCommitEvidence(ctx, workspacePath, head, "--not", pullRequestHead, base)
	if err != nil {
		return nil, false, fmt.Errorf("git compare workspace commits to pull request: %w", err)
	}
	return refs, true, nil
}

func gitCommitEvidence(ctx context.Context, workspacePath string, revisions ...string) ([]string, error) {
	args := []string{"log", "--format=%H %s%x00", "-n", strconv.Itoa(recoveryEvidenceLimit)}
	args = append(args, revisions...)
	output, err := gitexec.Run(ctx, workspacePath, args...)
	if err != nil {
		return nil, err
	}
	return boundedNULFields(output, recoveryEvidenceLimit, true), nil
}

func boundedNULFields(output string, limit int, trimSpace bool) []string {
	if limit <= 0 {
		return nil
	}
	fields := make([]string, 0, min(strings.Count(output, "\x00"), limit))
	for field := range strings.SplitSeq(output, "\x00") {
		if trimSpace {
			field = strings.TrimSpace(field)
		}
		if field == "" {
			continue
		}
		fields = append(fields, field)
		if len(fields) == limit {
			break
		}
	}
	return fields
}

func RecoveryBaseFingerprint(ctx context.Context, workspacePath string, baseRef string) string {
	if baseRef = strings.TrimSpace(baseRef); baseRef != "" {
		return baseRef
	}
	output, err := gitexec.Run(ctx, workspacePath, "rev-parse", "--verify", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func RecoveryFingerprint(parts ...string) string {
	framed := make([]string, 0, len(parts)*2)
	for _, part := range parts {
		if part == "" {
			continue
		}
		framed = append(framed, strconv.Itoa(len(part))+":", part)
	}
	if len(framed) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(framed, "")))
	return hex.EncodeToString(sum[:])
}

func gitDiffStatWithEnv(ctx context.Context, workspacePath string, env []string, comparison ...string) (DiffStat, error) {
	args := append([]string{"diff", "--stat"}, comparison...)
	output, err := gitexec.WithEnv(ctx, workspacePath, env, DiagnosticArgs(args...)...)
	if err != nil {
		return DiffStat{}, fmt.Errorf("git diff stat: %w", err)
	}
	stat, err := ParseDiffStat(output)
	if err != nil || stat.IsEmpty() {
		return stat, err
	}
	fingerprint, err := gitDiffFingerprint(ctx, workspacePath, env, comparison...)
	if err != nil {
		return DiffStat{}, err
	}
	stat.Fingerprint = fingerprint
	return stat, nil
}

func gitDiffFingerprint(ctx context.Context, workspacePath string, env []string, comparison ...string) (string, error) {
	args := append([]string{"-C", workspacePath, "diff", "--no-ext-diff", "--binary", "--full-index"}, comparison...)
	args = DiagnosticArgs(args...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = append([]string{"git"}, args...)
	cmd.WaitDelay = gitexec.WaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	hash := sha256.New()
	cmd.Stdout = hash
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := commandshell.RunCommand(ctx, cmd); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", &gitexec.CommandError{
			Command:  "git",
			Args:     args,
			ExitCode: exitCode,
			Output:   stderr.String(),
			Err:      err,
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func gitDiffBase(ctx context.Context, workspacePath string, baseRef string) string {
	return gitDiffBaseAt(ctx, workspacePath, baseRef, "HEAD")
}

func gitDiffBaseAt(ctx context.Context, workspacePath, baseRef, head string) string {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return head
	}
	output, err := gitexec.Run(ctx, workspacePath, "merge-base", baseRef, head)
	if err != nil {
		return baseRef
	}
	mergeBase := strings.TrimSpace(output)
	if mergeBase == "" {
		return baseRef
	}
	return mergeBase
}

func gitDiffOutputWithinLimit(ctx context.Context, workspacePath string, env []string, diffBase string, maxBytes int) (string, bool, error) {
	return gitexec.WithinLimit(ctx, workspacePath, env, maxBytes, DiagnosticArgs("diff", diffBase)...)
}

// DiagnosticArgs applies runtime exclusions to one command without changing
// repository ignore rules. Top-level pathspecs also exclude tracked artifacts.
func DiagnosticArgs(args ...string) []string {
	args = append(args, "--", ".")
	for _, exclude := range gitexec.HandoffDiffExcludes {
		args = append(args, ":(top,exclude)"+exclude)
	}
	return args
}

type IndexObservation struct {
	Path           string
	HeadSHA        string
	TreeSHA        string
	HeadObservedAt time.Time
}

func IndexPath(ctx context.Context, workspacePath string) (string, error) {
	index, err := Index(ctx, workspacePath, false)
	return index.Path, err
}

func Index(ctx context.Context, workspacePath string, observeHead bool) (IndexObservation, error) {
	path, err := filepath.Abs(workspacePath)
	if err != nil {
		return IndexObservation{}, fmt.Errorf("resolve workspace path: %w", err)
	}
	args := []string{"rev-parse", "--git-path", "index"}
	if observeHead {
		args = append(args, "HEAD")
	}
	output, err := gitexec.WithEnv(ctx, workspacePath, []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(path)}, args...)
	if err != nil {
		return IndexObservation{}, fmt.Errorf("git index path: %w", err)
	}
	index := IndexObservation{}
	indexPath := strings.TrimSuffix(output, "\n")
	if observeHead {
		separator := strings.LastIndexByte(indexPath, '\n')
		if separator < 0 || separator == len(indexPath)-1 {
			return IndexObservation{}, errors.New("git index head is empty")
		}
		index.HeadSHA = indexPath[separator+1:]
		index.HeadObservedAt = time.Now()
		indexPath = indexPath[:separator]
		tree, err := gitexec.WithEnv(ctx, workspacePath, []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(path)}, "rev-parse", index.HeadSHA+"^{tree}")
		if err != nil {
			return IndexObservation{}, fmt.Errorf("git index tree: %w", err)
		}
		index.TreeSHA = strings.TrimSpace(tree)
		if index.TreeSHA == "" {
			return IndexObservation{}, errors.New("git index tree is empty")
		}
	}
	if indexPath == "" {
		return IndexObservation{}, errors.New("git index path is empty")
	}
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(workspacePath, indexPath)
	}
	index.Path = filepath.Clean(indexPath)
	return index, nil
}

func copyGitIndex(indexPath string) (string, func(), error) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return "", nil, fmt.Errorf("read git index: %w", err)
	}
	file, err := os.CreateTemp("", "detent-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary git index: %w", err)
	}
	tempIndex := file.Name()
	cleanup := func() {
		if err := os.Remove(tempIndex); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	if _, err := file.Write(data); err != nil {
		closeErr := file.Close()
		cleanup()
		if closeErr != nil {
			return "", nil, errors.Join(
				fmt.Errorf("write temporary git index: %w", err),
				fmt.Errorf("close temporary git index: %w", closeErr),
			)
		}
		return "", nil, fmt.Errorf("write temporary git index: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temporary git index: %w", err)
	}
	return tempIndex, cleanup, nil
}

func ParseDiffStat(output string) (DiffStat, error) {
	summary := diffStatSummaryLine(output)
	if summary == "" {
		return DiffStat{}, nil
	}
	if !diffStatFilesPattern.MatchString(summary) {
		return DiffStat{}, fmt.Errorf("parse git diff stat: missing file count in %q", summary)
	}

	return DiffStat{
		Files:   parseDiffStatInt(diffStatFilesPattern, summary),
		Added:   parseDiffStatInt(diffStatAddedPattern, summary),
		Removed: parseDiffStatInt(diffStatRemovedPattern, summary),
	}, nil
}

func diffStatSummaryLine(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func parseDiffStatInt(pattern *regexp.Regexp, input string) int {
	matches := pattern.FindStringSubmatch(input)
	if len(matches) < 2 {
		return 0
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return value
}
