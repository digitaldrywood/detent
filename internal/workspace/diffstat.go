package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const recoveryEvidenceLimit = 20

var (
	diffStatFilesPattern   = regexp.MustCompile(`(\d+)\s+files?\s+changed`)
	diffStatAddedPattern   = regexp.MustCompile(`(\d+)\s+insertions?\(\+\)`)
	diffStatRemovedPattern = regexp.MustCompile(`(\d+)\s+deletions?\(-\)`)
)

var detentHandoffDiffExcludes = []string{
	".detent/tmp/",
	".detent/worker-tmp/",
}

type DiffStat struct {
	Files          int       `json:"files"`
	Added          int       `json:"added"`
	Removed        int       `json:"removed"`
	Fingerprint    string    `json:"fingerprint,omitempty"`
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

type DiffProvider interface {
	Diff(context.Context, Info, Issue, int) (Diff, error)
}

// HeadProvider reports the commit actually checked out for a review.
type HeadProvider interface {
	Head(context.Context, Info, Issue) (string, error)
}

// ReviewTreeVerifier checks that a review worktree contains only committed content.
type ReviewTreeVerifier interface {
	VerifyReviewTree(context.Context, Info, Issue) error
}

// ReviewHeadSeeder aligns an existing review worktree with the requested PR commit.
type ReviewHeadSeeder interface {
	SeedReviewHead(context.Context, Info, Issue) error
}

func (l *LocalGit) SeedReviewHead(ctx context.Context, info Info, issue Issue) error {
	if strings.TrimSpace(issue.PullRequestHeadSHA) == "" {
		return nil
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	remoteHead, err := fetchReviewHead(ctx, normalized.Path, normalized.Branch, issue)
	if err != nil {
		return err
	}
	localHead, err := l.Head(ctx, normalized, issue)
	if err != nil {
		return err
	}
	if err := l.VerifyReviewTree(ctx, normalized, issue); err != nil {
		return err
	}
	if strings.TrimSpace(localHead) == remoteHead {
		return nil
	}
	_, err = runGitAt(ctx, normalized.Path, "reset", "--hard", remoteHead)
	return err
}

func fetchReviewHead(ctx context.Context, path, fallbackBranch string, issue Issue) (string, error) {
	branch := strings.TrimSpace(issue.PullRequestBranch)
	if branch == "" {
		branch = strings.TrimSpace(fallbackBranch)
	}
	if branch == "" && issue.PullRequestNumber == 0 {
		return "", errors.New("review branch is empty")
	}
	remoteRef := "refs/remotes/origin/" + branch
	sourceRef := "refs/heads/" + branch
	if issue.PullRequestNumber > 0 {
		remoteRef = fmt.Sprintf("refs/remotes/origin/pull/%d/head", issue.PullRequestNumber)
		sourceRef = fmt.Sprintf("refs/pull/%d/head", issue.PullRequestNumber)
	}
	remote := "origin"
	if issue.PullRequestNumber > 0 && strings.TrimSpace(issue.PullRequestRepository) != "" {
		repository := strings.TrimSpace(issue.PullRequestRepository)
		parts := strings.Split(repository, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repository, " \\:@") {
			return "", fmt.Errorf("invalid pull request repository %q", repository)
		}
		remote = "https://github.com/" + repository + ".git"
	}
	if _, err := runGitAt(ctx, path, "fetch", "--no-write-fetch-head", remote, "+"+sourceRef+":"+remoteRef); err != nil {
		return "", fmt.Errorf("fetch review head: %w", err)
	}
	remoteHead, err := runGitAt(ctx, path, "rev-parse", "--verify", remoteRef)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(remoteHead) != strings.TrimSpace(issue.PullRequestHeadSHA) {
		return "", refuse(LandRefusalHeadMoved, fmt.Sprintf("review branch head mismatch: fetched %s, expected %s", strings.TrimSpace(remoteHead), strings.TrimSpace(issue.PullRequestHeadSHA)))
	}
	return strings.TrimSpace(remoteHead), nil
}

func (l *LocalGit) VerifyReviewTree(ctx context.Context, info Info, issue Issue) error {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	changes, err := runGitAt(ctx, normalized.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(changes) != "" {
		return errors.New("review workspace has local changes")
	}
	return nil
}

func (l *LocalGit) Head(ctx context.Context, info Info, issue Issue) (string, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return "", err
	}
	return runGitAt(ctx, normalized.Path, "rev-parse", "--verify", "HEAD")
}

func (l *LocalGit) DiffStat(ctx context.Context, info Info, issue Issue) (DiffStat, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DiffStat{}, err
	}
	return GitDiffStat(ctx, normalized.Path)
}

func (l *LocalGit) RecoveryState(ctx context.Context, info Info, issue Issue) (RecoveryState, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return RecoveryState{}, err
	}
	stat, err := gitDiffStat(ctx, normalized.Path, true)
	if err != nil {
		return RecoveryState{}, err
	}
	base, err := l.localProgressBase(ctx, normalized.Path, issue)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("resolve recovery base: %w", err)
	}
	unpushedCommits, unpushedCommitRefs, err := gitUnpushedCommitEvidence(ctx, normalized.Path, base)
	if err != nil {
		return RecoveryState{}, err
	}
	trackedPaths, err := gitTrackedPaths(ctx, normalized.Path)
	if err != nil {
		return RecoveryState{}, err
	}
	untrackedPaths, err := gitUntrackedPaths(ctx, normalized.Path)
	if err != nil {
		return RecoveryState{}, err
	}
	head, err := runGitAt(ctx, normalized.Path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return RecoveryState{}, fmt.Errorf("git resolve recovery head: %w", err)
	}
	head = strings.TrimSpace(head)
	commitsNotInPullRequest, comparisonAvailable, err := gitCommitsNotInPullRequest(
		ctx,
		normalized.Path,
		head,
		issue.PullRequestHeadSHA,
		base,
	)
	if err != nil {
		return RecoveryState{}, err
	}
	return RecoveryState{
		DiffStat:                       stat,
		BaseFingerprint:                gitRecoveryBaseFingerprint(ctx, normalized.Path, issue.BaseRef),
		HeadSHA:                        head,
		WorkspaceFingerprint:           workspaceRecoveryFingerprint(head, stat.Fingerprint),
		UnpushedCommits:                unpushedCommits,
		UnpushedCommitRefs:             unpushedCommitRefs,
		TrackedPaths:                   trackedPaths,
		UntrackedPaths:                 untrackedPaths,
		CommitsNotInPullRequest:        commitsNotInPullRequest,
		PullRequestComparisonAvailable: comparisonAvailable,
	}, nil
}

func (l *LocalGit) DeliverableState(ctx context.Context, info Info, issue Issue) (DeliverableState, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return DeliverableState{}, err
	}
	state, err := gitDeliveryState(ctx, normalized.Path, normalized.Branch, issue.BaseRef)
	if err != nil {
		return DeliverableState{}, err
	}
	return state, nil
}

func (l *LocalGit) Diff(ctx context.Context, info Info, issue Issue, maxBytes int) (Diff, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return Diff{}, err
	}
	return GitDiffFrom(ctx, normalized.Path, issue.BaseRef, maxBytes)
}

func GitDiffStat(ctx context.Context, workspacePath string) (DiffStat, error) {
	return gitDiffStat(ctx, workspacePath, false)
}

func gitDiffStat(ctx context.Context, workspacePath string, includeIndex bool) (DiffStat, error) {
	if strings.TrimSpace(workspacePath) == "" {
		return DiffStat{}, errors.New("workspace path is required")
	}
	if _, err := os.Stat(workspacePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DiffStat{}, fmt.Errorf("%w: %s: %w", ErrMissingWorkspace, workspacePath, err)
		}
		return DiffStat{}, fmt.Errorf("stat workspace path: %w", err)
	}

	stat, err := gitDiffStatOutput(ctx, workspacePath, includeIndex)
	if err != nil {
		return DiffStat{}, err
	}
	return stat, nil
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

	indexPath, err := gitIndexPath(ctx, workspacePath)
	if err != nil {
		return Diff{}, err
	}
	tempIndex, cleanup, err := copyGitIndex(indexPath)
	if err != nil {
		return Diff{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	if _, err := runGitAtWithEnv(ctx, workspacePath, env, "add", "--intent-to-add", "--", "."); err != nil {
		return Diff{}, fmt.Errorf("git add intent to add: %w", err)
	}
	diffBase := gitDiffBase(ctx, workspacePath, baseRef)
	statOutput, err := runGitAtWithEnv(ctx, workspacePath, env, gitDiagnosticArgs("diff", "--stat", diffBase)...)
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

func gitDiffStatOutput(ctx context.Context, workspacePath string, includeIndex bool) (DiffStat, error) {
	index, err := gitIndexLookup(ctx, workspacePath, true)
	if err != nil {
		return DiffStat{}, err
	}
	tempIndex, cleanup, err := copyGitIndex(index.Path)
	if err != nil {
		return DiffStat{}, err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tempIndex}
	addPaths := []string{"."}
	addEnv := env
	if includeIndex {
		output, err := runGitAtWithEnv(ctx, workspacePath, env, gitDiagnosticArgs("ls-files", "--others", "--exclude-standard", "-z")...)
		if err != nil {
			return DiffStat{}, fmt.Errorf("git list untracked workspace changes: %w", err)
		}
		addPaths = strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
		if output == "" {
			addPaths = nil
		}
		addEnv = append(slices.Clone(env), "GIT_LITERAL_PATHSPECS=1")
	}
	if len(addPaths) != 0 {
		args := append([]string{"add", "--intent-to-add", "--"}, addPaths...)
		if _, err := runGitAtWithEnv(ctx, workspacePath, addEnv, args...); err != nil {
			return DiffStat{}, fmt.Errorf("git add intent to add: %w", err)
		}
	}
	var stat DiffStat
	if includeIndex {
		staged, err := gitDiffStatWithEnv(ctx, workspacePath, env, "--cached", index.HeadSHA)
		if err != nil {
			return DiffStat{}, err
		}
		working, err := gitDiffStatWithEnv(ctx, workspacePath, env)
		if err != nil {
			return DiffStat{}, err
		}
		paths, err := gitDiffPaths(ctx, workspacePath, env, []string{"--cached", index.HeadSHA}, nil)
		if err != nil {
			return DiffStat{}, err
		}
		stat = DiffStat{Files: len(paths), Added: staged.Added + working.Added, Removed: staged.Removed + working.Removed}
		if !stat.IsEmpty() {
			stat.Fingerprint = workspaceRecoveryFingerprint("index", staged.Fingerprint, "worktree", working.Fingerprint)
		}
	} else {
		stat, err = gitDiffStatWithEnv(ctx, workspacePath, env, index.HeadSHA)
		if err != nil {
			return DiffStat{}, err
		}
	}
	stat.HeadSHA = index.HeadSHA
	stat.HeadObservedAt = index.HeadObservedAt
	return stat, nil
}

func gitUnpushedCommitEvidence(ctx context.Context, workspacePath string, base string) (int, []string, error) {
	remoteRefs, err := runGitAt(ctx, workspacePath, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes")
	if err != nil {
		return 0, nil, fmt.Errorf("git list remote refs: %w", err)
	}
	if strings.TrimSpace(remoteRefs) == "" {
		return 0, nil, nil
	}
	output, err := runGitAt(ctx, workspacePath, "rev-list", "--count", "HEAD", "--not", "--remotes", base)
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
	refs, err := gitCommitEvidence(ctx, workspacePath, "HEAD", "--not", "--remotes", base)
	if err != nil {
		return 0, nil, fmt.Errorf("git list unpushed commits: %w", err)
	}
	return count, refs, nil
}

func gitTrackedPaths(ctx context.Context, workspacePath string) ([]string, error) {
	paths, err := gitDiffPaths(ctx, workspacePath, nil, []string{"--cached", "HEAD"}, nil)
	if err != nil {
		return nil, fmt.Errorf("git list tracked workspace changes: %w", err)
	}
	return paths[:min(len(paths), recoveryEvidenceLimit)], nil
}

func gitDiffPaths(ctx context.Context, workspacePath string, env []string, comparisons ...[]string) ([]string, error) {
	var paths []string
	for _, comparison := range comparisons {
		args := append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "-z"}, comparison...)
		output, err := runGitAtWithEnv(ctx, workspacePath, env, gitDiagnosticArgs(args...)...)
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

func gitUntrackedPaths(ctx context.Context, workspacePath string) ([]string, error) {
	output, err := runGitAt(ctx, workspacePath, gitDiagnosticArgs("ls-files", "--others", "--exclude-standard", "-z")...)
	if err != nil {
		return nil, fmt.Errorf("git list untracked workspace changes: %w", err)
	}
	return boundedNULFields(output, recoveryEvidenceLimit, false), nil
}

func gitCommitsNotInPullRequest(
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
	if _, err := runGitAt(ctx, workspacePath, "cat-file", "-e", pullRequestHead+"^{commit}"); err != nil {
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
	output, err := runGitAt(ctx, workspacePath, args...)
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

func gitDeliveryState(ctx context.Context, workspacePath string, branch string, baseRef string) (DeliverableState, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return DeliverableState{}, errors.New("inspect delivery state: workspace branch is required")
	}
	base := strings.TrimSpace(baseRef)
	if base == "" {
		branch, head, err := remoteDefaultBranchHead(ctx, workspacePath, defaultGitRemote)
		if err != nil {
			return DeliverableState{}, fmt.Errorf("inspect delivery base: %w", err)
		}
		if _, err := runGitAt(ctx, workspacePath, "fetch", "--no-tags", defaultGitRemote, "refs/heads/"+branch); err != nil {
			return DeliverableState{}, fmt.Errorf("fetch delivery base %s/%s: %w", defaultGitRemote, branch, err)
		}
		base = head
	}
	output, err := runGitAt(ctx, workspacePath, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return DeliverableState{}, fmt.Errorf("count local commits ahead: %w", err)
	}
	commitsAhead, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return DeliverableState{}, fmt.Errorf("parse local commits ahead: %w", err)
	}
	localHead, err := runGitAt(ctx, workspacePath, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return DeliverableState{}, fmt.Errorf("inspect local branch %s: %w", branch, err)
	}
	remoteHead, remoteBranchExists, err := remoteBranchHead(ctx, workspacePath, defaultGitRemote, branch)
	if err != nil {
		return DeliverableState{}, fmt.Errorf("inspect remote branch %s/%s: %w", defaultGitRemote, branch, err)
	}
	return DeliverableState{
		CommitsAhead:       commitsAhead,
		Remote:             defaultGitRemote,
		RemoteRef:          "refs/heads/" + branch,
		LocalHeadSHA:       strings.TrimSpace(localHead),
		RemoteHeadSHA:      strings.TrimSpace(remoteHead),
		RemoteBranchExists: remoteBranchExists,
	}, nil
}

func gitRecoveryBaseFingerprint(ctx context.Context, workspacePath string, baseRef string) string {
	if baseRef = strings.TrimSpace(baseRef); baseRef != "" {
		return baseRef
	}
	output, err := runGitAt(ctx, workspacePath, "rev-parse", "--verify", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func workspaceRecoveryFingerprint(parts ...string) string {
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
	output, err := runGitAtWithEnv(ctx, workspacePath, env, gitDiagnosticArgs(args...)...)
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
	args = gitDiagnosticArgs(args...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = append([]string{"git"}, args...)
	cmd.WaitDelay = workspaceCommandWaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	hash := sha256.New()
	cmd.Stdout = hash
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", &CommandError{
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
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		return "HEAD"
	}
	output, err := runGitAt(ctx, workspacePath, "merge-base", baseRef, "HEAD")
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
	return gitDiffArgsWithinLimit(ctx, workspacePath, env, maxBytes, gitDiagnosticArgs("diff", diffBase)...)
}

// gitDiffArgsWithinLimit streams one git diff invocation through a byte cap.
// The arguments are explicit so a caller that needs different diff options --
// the per-file diff of decisions section 18.5 pins --find-renames -- gets the
// same bounded read instead of a second copy of it.
func gitDiffArgsWithinLimit(ctx context.Context, workspacePath string, env []string, maxBytes int, args ...string) (string, bool, error) {
	gitArgs := append([]string{"git", "-C", workspacePath}, args...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = gitArgs
	cmd.WaitDelay = workspaceCommandWaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, fmt.Errorf("git diff stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", false, fmt.Errorf("git diff start: %w", err)
	}

	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := len(output) > maxBytes
	var killErr error
	if truncated && cmd.Process != nil {
		killErr = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()

	if readErr != nil {
		return "", false, fmt.Errorf("git diff read: %w", readErr)
	}
	if truncated {
		if err := gitDiffStopError(killErr, waitErr); err != nil {
			return "", false, err
		}
		return "", true, nil
	}
	if waitErr == nil {
		return string(output), false, nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		waitErr = ctx.Err()
	}
	combined := append(output, stderr.Bytes()...)
	return "", false, &CommandError{
		Command:  "git",
		Args:     gitArgs[1:],
		ExitCode: exitCode,
		Output:   string(combined),
		Err:      waitErr,
	}
}

func gitDiffStopError(killErr error, waitErr error) error {
	if killErr == nil || errors.Is(killErr, os.ErrProcessDone) || waitErr == nil {
		return nil
	}
	return fmt.Errorf("git diff stop after limit: %w", killErr)
}

// gitDiagnosticArgs applies runtime exclusions to one command without changing
// repository ignore rules. Top-level pathspecs also exclude tracked artifacts.
func gitDiagnosticArgs(args ...string) []string {
	args = append(args, "--", ".")
	for _, exclude := range detentHandoffDiffExcludes {
		args = append(args, ":(top,exclude)"+exclude)
	}
	return args
}

type gitIndexObservation struct {
	Path           string
	HeadSHA        string
	HeadObservedAt time.Time
}

func gitIndexPath(ctx context.Context, workspacePath string) (string, error) {
	index, err := gitIndexLookup(ctx, workspacePath, false)
	return index.Path, err
}

func gitIndexLookup(ctx context.Context, workspacePath string, observeHead bool) (gitIndexObservation, error) {
	args := []string{"rev-parse", "--git-path", "index"}
	if observeHead {
		args = append(args, "--verify", "HEAD")
	}
	output, err := runGitAt(ctx, workspacePath, args...)
	if err != nil {
		return gitIndexObservation{}, fmt.Errorf("git index path: %w", err)
	}
	index := gitIndexObservation{}
	indexPath := strings.TrimSuffix(output, "\n")
	if observeHead {
		separator := strings.LastIndexByte(indexPath, '\n')
		if separator < 0 || separator == len(indexPath)-1 {
			return gitIndexObservation{}, errors.New("git index head is empty")
		}
		index.HeadSHA = indexPath[separator+1:]
		index.HeadObservedAt = time.Now()
		indexPath = indexPath[:separator]
	}
	if indexPath == "" {
		return gitIndexObservation{}, errors.New("git index path is empty")
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
