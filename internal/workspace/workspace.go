package workspace

import (
	"context"
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
)

const KindLocalGit = "local_git"

const defaultHookTimeout = time.Minute

var (
	ErrHookFailed         = errors.New("workspace hook failed")
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

type Issue struct {
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
	ExitCode int
	Output   string
	Err      error
}

func (e *HookError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", ErrHookFailed, e.Hook, e.Err)
	}
	return fmt.Sprintf("%s: %s exited with status %d", ErrHookFailed, e.Hook, e.ExitCode)
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
	if key == "" || key == "." || key == ".." {
		return "issue"
	}
	return key
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
	info, err := l.infoForIssue(Issue{Identifier: identifier})
	if err != nil {
		return err
	}

	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return err
	}
	if !exists {
		_, pruneErr := l.runGit(ctx, "worktree", "prune")
		return pruneErr
	}
	if isDir && l.isSourceWorktree(ctx, info.Path) {
		if err := l.runHook(ctx, "before_remove", l.hooks.BeforeRemove, info, Issue{Identifier: identifier}); err != nil {
			l.logger.Warn("workspace before_remove hook failed", slog.String("path", info.Path), slog.Any("error", err))
		}
	}

	if err := l.removePath(ctx, info.Path); err != nil {
		return err
	}
	_, err = l.runGit(ctx, "worktree", "prune")
	return err
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
	key := SafeKey(issue.Identifier)
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
		key = SafeKey(issue.Identifier)
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

	cmd := exec.CommandContext(hookCtx, "sh", "-c", command)
	cmd.Dir = info.Path
	cmd.Env = hookEnv(info, issue)

	l.logger.Info("running workspace hook", slog.String("hook", name), slog.String("path", info.Path))

	output, err := cmd.CombinedOutput()
	if err == nil {
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

	return &HookError{
		Hook:     name,
		ExitCode: exitCode,
		Output:   string(output),
		Err:      err,
	}
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
	values := map[string]string{
		"DETENT_WORKSPACE":        info.Path,
		"DETENT_WORKSPACE_KEY":    info.Key,
		"DETENT_BRANCH":           info.Branch,
		"DETENT_ISSUE_ID":         issue.ID,
		"DETENT_ISSUE_IDENTIFIER": issue.Identifier,
	}
	for key, value := range values {
		env = append(env, key+"="+value)
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
