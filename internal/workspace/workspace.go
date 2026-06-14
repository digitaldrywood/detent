package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

const KindLocalGit = "local_git"

const defaultHookTimeout = time.Minute
const workspaceCommandWaitDelay = time.Second
const hookOutputTailBytes = 16 * 1024

var (
	ErrHookFailed         = errors.New("workspace hook failed")
	ErrMissingWorkspace   = errors.New("workspace missing")
	ErrUnsafePath         = errors.New("unsafe workspace path")
	ErrUnsupportedBackend = errors.New("unsupported workspace backend")
)

var unsafeKeyPattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Backend interface {
	Create(context.Context, Issue) (Info, error)
	Cleanup(context.Context, string) error
	BeforeRun(context.Context, Info, Issue) error
	AfterRun(context.Context, Info, Issue)
	DiffStat(context.Context, Info, Issue) (DiffStat, error)
}

type CleanupResult struct {
	Worktrees int
	Branches  int
	Processes int
}

type IssueCleaner interface {
	CleanupIssue(context.Context, Issue) (CleanupResult, error)
}

type Issue struct {
	ProjectID  string
	ID         string
	Identifier string
	BranchName string
}

type Info struct {
	Path    string
	Key     string
	Branch  string
	Created bool
}

type Hooks struct {
	Shell        string
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

type LocalGitOptions struct {
	Root       string
	SourceRoot string
	AutoBranch bool
	Hooks      Hooks
	Logger     *slog.Logger
}

type LocalGit struct {
	root       string
	sourceRoot string
	autoBranch bool
	hooks      Hooks
	logger     *slog.Logger
}

type PathError struct {
	Path   string
	Root   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("%s: %s is not safe under %s: %s", ErrUnsafePath, e.Path, e.Root, e.Reason)
}

func (e *PathError) Unwrap() error {
	return ErrUnsafePath
}

type HookError struct {
	Hook     string
	Command  string
	Dir      string
	ExitCode int
	LogPath  string
	Output   string
	Err      error
}

func (e *HookError) Error() string {
	parts := []string{}
	if e.Command != "" {
		parts = append(parts, fmt.Sprintf("command %q", e.Command))
	}
	if e.Dir != "" {
		parts = append(parts, fmt.Sprintf("working directory %q", e.Dir))
	}
	if e.ExitCode >= 0 {
		parts = append(parts, fmt.Sprintf("exit status %d", e.ExitCode))
	}
	if e.LogPath != "" {
		parts = append(parts, fmt.Sprintf("hook log %q", e.LogPath))
	}

	detail := ""
	if len(parts) > 0 {
		detail = " (" + strings.Join(parts, "; ") + ")"
	}

	output := hookOutputTail(e.Output, hookOutputTailBytes)
	if output != "" {
		detail += fmt.Sprintf("\noutput (last %d KiB):\n%s", hookOutputTailBytes/1024, output)
	}

	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v%s", ErrHookFailed, e.Hook, e.Err, detail)
	}
	return fmt.Sprintf("%s: %s exited with status %d%s", ErrHookFailed, e.Hook, e.ExitCode, detail)
}

func (e *HookError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return ErrHookFailed
}

func (e *HookError) Is(target error) bool {
	return target == ErrHookFailed
}

type CommandError struct {
	Command  string
	Args     []string
	ExitCode int
	Output   string
	Err      error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s %s failed: %v", e.Command, strings.Join(e.Args, " "), e.Err)
}

func (e *CommandError) Unwrap() error {
	return e.Err
}

func IsMissingWorkspaceError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrMissingWorkspace) {
		return true
	}

	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	output := strings.ToLower(commandErr.Output)
	return strings.Contains(output, "cannot change to") && strings.Contains(output, "no such file or directory")
}

func NewBackend(kind string, opts LocalGitOptions) (Backend, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindLocalGit:
		return NewLocalGit(opts)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedBackend, kind)
	}
}

func NewLocalGit(opts LocalGitOptions) (*LocalGit, error) {
	root, err := prepareRoot(opts.Root)
	if err != nil {
		return nil, err
	}

	sourceRoot, err := canonicalExistingPath(opts.SourceRoot)
	if err != nil {
		return nil, fmt.Errorf("source root: %w", err)
	}

	hooks := opts.Hooks
	if hooks.Timeout == 0 {
		hooks.Timeout = defaultHookTimeout
	}
	if hooks.Timeout < 0 {
		return nil, errors.New("hooks timeout must be greater than or equal to 0")
	}
	hooks.Shell = commandshell.Normalize(hooks.Shell)

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &LocalGit{
		root:       root,
		sourceRoot: sourceRoot,
		autoBranch: opts.AutoBranch,
		hooks:      hooks,
		logger:     logger,
	}, nil
}

func SafeKey(identifier string) string {
	key := unsafeKeyPattern.ReplaceAllString(strings.TrimSpace(identifier), "_")
	if key == "" || key == "." || key == ".." || key == ".detent" {
		return "issue"
	}
	return key
}

func issueKey(issue Issue) string {
	identifierKey := SafeKey(issue.Identifier)
	projectKey := SafeKey(issue.ProjectID)
	if strings.TrimSpace(issue.ProjectID) == "" {
		return identifierKey
	}
	return projectKey + "-" + identifierKey + "-" + issueKeyDigest(issue)
}

func issueKeyDigest(issue Issue) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(issue.ProjectID) + "\x00" + strings.TrimSpace(issue.Identifier)))
	return hex.EncodeToString(sum[:])[:12]
}

func (l *LocalGit) Create(ctx context.Context, issue Issue) (Info, error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return Info{}, err
	}

	created, err := l.ensureWorktree(ctx, info.Path, info.Branch)
	if err != nil {
		return Info{}, err
	}
	info.Created = created

	if created {
		if err := l.runHook(ctx, "after_create", l.hooks.AfterCreate, info, issue); err != nil {
			if cleanupErr := l.removePath(ctx, info.Path); cleanupErr != nil {
				l.logger.Warn("failed to clean workspace after after_create hook error", slog.String("path", info.Path), slog.Any("error", cleanupErr))
			}
			return Info{}, err
		}
	}

	return info, nil
}

func (l *LocalGit) Cleanup(ctx context.Context, identifier string) error {
	_, err := l.CleanupIssue(ctx, Issue{Identifier: identifier})
	return err
}

func (l *LocalGit) CleanupIssue(ctx context.Context, issue Issue) (CleanupResult, error) {
	info, err := l.infoForIssue(issue)
	if err != nil {
		return CleanupResult{}, err
	}

	result := CleanupResult{}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return CleanupResult{}, err
	}
	if !exists {
		_, pruneErr := l.runGit(ctx, "worktree", "prune")
		if pruneErr != nil {
			return CleanupResult{}, pruneErr
		}
		branchRemoved, branchErr := l.deleteBranch(ctx, info.Branch)
		if branchErr != nil {
			return CleanupResult{}, branchErr
		}
		if branchRemoved {
			result.Branches = 1
		}
		return result, nil
	}
	result.Processes = reapWorkspaceProcesses(ctx, info.Path, l.logger)
	if isDir && l.isSourceWorktree(ctx, info.Path) {
		if err := l.runHook(ctx, "before_remove", l.hooks.BeforeRemove, info, issue); err != nil {
			l.logger.Warn("workspace before_remove hook failed", slog.String("path", info.Path), slog.Any("error", err))
		}
	}

	if err := l.removePath(ctx, info.Path); err != nil {
		return CleanupResult{}, err
	}
	result.Worktrees = 1
	if _, err := l.runGit(ctx, "worktree", "prune"); err != nil {
		return CleanupResult{}, err
	}
	branchRemoved, err := l.deleteBranch(ctx, info.Branch)
	if err != nil {
		return CleanupResult{}, err
	}
	if branchRemoved {
		result.Branches = 1
	}
	return result, nil
}

func (l *LocalGit) BeforeRun(ctx context.Context, info Info, issue Issue) error {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	return l.runHook(ctx, "before_run", l.hooks.BeforeRun, normalized, issue)
}

func (l *LocalGit) AfterRun(ctx context.Context, info Info, issue Issue) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		l.logger.Warn("workspace after_run path validation failed", slog.String("path", info.Path), slog.Any("error", err))
		return
	}
	if err := l.runHook(ctx, "after_run", l.hooks.AfterRun, normalized, issue); err != nil {
		l.logger.Warn("workspace after_run hook failed", slog.String("path", normalized.Path), slog.Any("error", err))
	}
}

func (l *LocalGit) infoForIssue(issue Issue) (Info, error) {
	key := issueKey(issue)
	path, err := l.workspacePath(key)
	if err != nil {
		return Info{}, err
	}

	return Info{
		Path:   path,
		Key:    key,
		Branch: l.branchName(issue, key),
	}, nil
}

func (l *LocalGit) normalizeInfo(info Info, issue Issue) (Info, error) {
	key := info.Key
	if key == "" {
		key = issueKey(issue)
	}
	path := info.Path
	if path == "" {
		var err error
		path, err = l.workspacePath(key)
		if err != nil {
			return Info{}, err
		}
	} else {
		var err error
		path, err = validateWorkspacePath(l.root, path)
		if err != nil {
			return Info{}, err
		}
	}
	branch := info.Branch
	if branch == "" {
		branch = l.branchName(issue, key)
	}

	info.Path = path
	info.Key = key
	info.Branch = branch
	return info, nil
}

func (l *LocalGit) workspacePath(key string) (string, error) {
	return validateWorkspacePath(l.root, filepath.Join(l.root, key))
}

func (l *LocalGit) branchName(issue Issue, key string) string {
	if !l.autoBranch {
		return ""
	}
	if strings.TrimSpace(issue.BranchName) != "" {
		return strings.TrimSpace(issue.BranchName)
	}
	return "detent/" + strings.ToLower(key)
}

func (l *LocalGit) ensureWorktree(ctx context.Context, path string, branch string) (bool, error) {
	exists, isDir, err := pathExists(path)
	if err != nil {
		return false, err
	}
	if exists {
		if isDir {
			if l.isSourceWorktree(ctx, path) {
				return false, nil
			}
			if l.isGitWorkspace(ctx, path) {
				return false, fmt.Errorf("workspace path is a git worktree not managed by source: %s", path)
			}
			empty, err := dirIsEmpty(path)
			if err != nil {
				return false, err
			}
			if !empty {
				return false, fmt.Errorf("workspace path exists but is not a git worktree: %s", path)
			}
		}
		if err := os.RemoveAll(path); err != nil {
			return false, fmt.Errorf("remove stale workspace path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create workspace parent: %w", err)
	}

	if l.autoBranch {
		if err := l.addBranchedWorktree(ctx, path, branch); err != nil {
			return false, err
		}
		return true, nil
	}

	_, err = l.runGit(ctx, "worktree", "add", "--detach", path, "HEAD")
	return err == nil, err
}

func (l *LocalGit) addBranchedWorktree(ctx context.Context, path string, branch string) error {
	exists, err := l.branchExists(ctx, branch)
	if err != nil {
		return err
	}
	if exists {
		_, err = l.runGit(ctx, "worktree", "add", path, branch)
		return err
	}

	_, err = l.runGit(ctx, "worktree", "add", "-b", branch, path, "HEAD")
	return err
}

func (l *LocalGit) branchExists(ctx context.Context, branch string) (bool, error) {
	_, err := l.runGit(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}

	var cmdErr *CommandError
	if errors.As(err, &cmdErr) && cmdErr.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

func (l *LocalGit) deleteBranch(ctx context.Context, branch string) (bool, error) {
	branch = strings.TrimSpace(branch)
	if !l.autoBranch || branch == "" || !strings.HasPrefix(branch, "detent/") {
		return false, nil
	}
	exists, err := l.branchExists(ctx, branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	_, err = l.runGit(ctx, "branch", "-D", branch)
	return err == nil, err
}

func (l *LocalGit) isGitWorkspace(ctx context.Context, path string) bool {
	output, err := runGitAt(ctx, path, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(output) == "true"
}

func (l *LocalGit) isSourceWorktree(ctx context.Context, path string) bool {
	workspaceCommon, err := gitCommonDir(ctx, path)
	if err != nil {
		return false
	}
	sourceCommon, err := gitCommonDir(ctx, l.sourceRoot)
	if err != nil {
		return false
	}
	return workspaceCommon == sourceCommon
}

func (l *LocalGit) removePath(ctx context.Context, path string) error {
	exists, _, err := pathExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	if _, err := l.runGit(ctx, "worktree", "remove", "--force", path); err != nil {
		if l.isSourceWorktree(ctx, path) {
			return err
		}
		if l.isGitWorkspace(ctx, path) {
			return fmt.Errorf("refusing to remove git workspace not managed by source: %s", path)
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove workspace path: %w", err)
		}
	}
	return nil
}

func (l *LocalGit) runHook(ctx context.Context, name string, command string, info Info, issue Issue) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}

	timeout := l.hooks.Timeout
	if timeout == 0 {
		timeout = defaultHookTimeout
	}

	hookCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		hookCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := commandshell.Command(hookCtx, command, l.hooks.Shell)
	cmd.Dir = info.Path
	cmd.Env = hookEnv(info, issue)
	cmd.WaitDelay = workspaceCommandWaitDelay

	l.logger.Info(
		"running workspace hook",
		slog.String("hook", name),
		slog.String("path", info.Path),
		slog.String("command", command),
	)

	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrWaitDelay) && hookCtx.Err() == nil {
		return nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if hookCtx.Err() != nil {
		err = hookCtx.Err()
	}

	logPath, logErr := l.writeHookLog(name, command, info, exitCode, err, output)
	hookErr := &HookError{
		Hook:     name,
		Command:  command,
		Dir:      info.Path,
		ExitCode: exitCode,
		LogPath:  logPath,
		Output:   string(output),
		Err:      err,
	}
	if logErr != nil {
		l.logger.Warn(
			"workspace hook log write failed",
			slog.String("hook", name),
			slog.String("path", info.Path),
			slog.String("command", command),
			slog.Any("error", logErr),
		)
	}
	l.logger.Warn(
		"workspace hook failed",
		slog.String("hook", name),
		slog.String("path", info.Path),
		slog.String("command", command),
		slog.Int("exit_code", exitCode),
		slog.String("log_path", logPath),
		slog.String("output_tail", hookOutputTail(string(output), hookOutputTailBytes)),
		slog.Any("error", err),
	)
	return hookErr
}

func (l *LocalGit) writeHookLog(
	name string,
	command string,
	info Info,
	exitCode int,
	err error,
	output []byte,
) (string, error) {
	root := strings.TrimSpace(l.root)
	if root == "" {
		root = strings.TrimSpace(info.Path)
	}
	if root == "" {
		return "", errors.New("workspace hook log root is empty")
	}
	dir := filepath.Join(root, ".detent", "hook-logs", SafeKey(info.Key))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hookLogFileName(name, time.Now().UTC(), os.Getpid()))

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "hook: %s\n", name)
	fmt.Fprintf(&buf, "command: %s\n", command)
	fmt.Fprintf(&buf, "working_directory: %s\n", info.Path)
	fmt.Fprintf(&buf, "exit_status: %d\n", exitCode)
	if err != nil {
		fmt.Fprintf(&buf, "error: %s\n", err)
	}
	fmt.Fprint(&buf, "\noutput:\n")
	fmt.Fprint(&buf, string(output))
	if len(output) > 0 && output[len(output)-1] != '\n' {
		fmt.Fprint(&buf, "\n")
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func hookLogFileName(name string, at time.Time, pid int) string {
	return at.Format("20060102T150405.000000000Z") + "-" + SafeKey(name) + fmt.Sprintf("-%d.log", pid)
}

func hookOutputTail(output string, limit int) string {
	if limit <= 0 || output == "" {
		return ""
	}
	if len(output) <= limit {
		return output
	}
	return "[truncated to last " + fmt.Sprint(limit/1024) + " KiB]\n" + output[len(output)-limit:]
}

func (l *LocalGit) runGit(ctx context.Context, args ...string) (string, error) {
	return runGitAt(ctx, l.sourceRoot, args...)
}

func runGitAt(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitAtWithEnv(ctx, dir, nil, args...)
}

func runGitAtWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	gitArgs := append([]string{"git", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = gitArgs
	cmd.WaitDelay = workspaceCommandWaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), nil
	}

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
		Args:     gitArgs[1:],
		ExitCode: exitCode,
		Output:   string(output),
		Err:      err,
	}
}

func gitCommonDir(ctx context.Context, dir string) (string, error) {
	output, err := runGitAt(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(output)
	if commonDir == "" {
		return "", errors.New("git common dir is empty")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}
	return canonicalExistingPath(commonDir)
}

func hookEnv(info Info, issue Issue) []string {
	env := append([]string{}, os.Environ()...)
	values := []struct {
		key   string
		value string
	}{
		{"DETENT_WORKSPACE", info.Path},
		{"DETENT_WORKSPACE_KEY", info.Key},
		{"DETENT_BRANCH", info.Branch},
		{"DETENT_ISSUE_ID", issue.ID},
		{"DETENT_ISSUE_IDENTIFIER", issue.Identifier},
		{"WORKSPACE", info.Path},
		{"WORKSPACE_KEY", info.Key},
		{"BRANCH", info.Branch},
		{"ISSUE_ID", issue.ID},
		{"ISSUE_IDENTIFIER", issue.Identifier},
	}
	for _, value := range values {
		env = append(env, value.key+"="+value.value)
	}
	return env
}

func prepareRoot(path string) (string, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(expanded) == "" {
		return "", errors.New("workspace root is required")
	}
	if err := os.MkdirAll(expanded, 0o700); err != nil {
		return "", fmt.Errorf("create workspace root: %w", err)
	}
	return canonicalExistingPath(expanded)
}

func canonicalExistingPath(path string) (string, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(expanded) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("absolute path: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize %s: %w", abs, err)
	}
	return filepath.Clean(canonical), nil
}

func expandPath(path string) (string, error) {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func validateWorkspacePath(root string, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("absolute workspace path: %w", err)
	}
	clean := filepath.Clean(abs)

	if info, err := os.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", &PathError{Path: clean, Root: root, Reason: "workspace path is a symlink"}
		}
		canonical, err := filepath.EvalSymlinks(clean)
		if err != nil {
			return "", fmt.Errorf("canonicalize workspace path: %w", err)
		}
		clean = filepath.Clean(canonical)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect workspace path: %w", err)
	}

	if clean == root {
		return "", &PathError{Path: clean, Root: root, Reason: "workspace path equals root"}
	}

	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return "", fmt.Errorf("relative workspace path: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", &PathError{Path: clean, Root: root, Reason: "workspace path escapes root"}
	}

	return clean, nil
}

func pathExists(path string) (bool, bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		return true, info.IsDir(), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	return false, false, fmt.Errorf("inspect path: %w", err)
}

func dirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read workspace directory: %w", err)
	}
	return len(entries) == 0, nil
}
