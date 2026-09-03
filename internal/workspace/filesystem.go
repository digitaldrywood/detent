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
	"strconv"
	"strings"
	"time"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

type FilesystemOptions struct {
	Root       string
	SourceRoot string
	OutputRoot string
	Hooks      Hooks
	Logger     *slog.Logger
}

type Filesystem struct {
	root       string
	sourceRoot string
	outputRoot string
	hooks      Hooks
	logger     *slog.Logger
}

func NewFilesystem(opts FilesystemOptions) (*Filesystem, error) {
	root, err := prepareRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	sourceRoot := strings.TrimSpace(opts.SourceRoot)
	if sourceRoot != "" {
		sourceRoot, err = canonicalExistingPath(sourceRoot)
		if err != nil {
			return nil, fmt.Errorf("source root: %w", err)
		}
	}
	outputRoot := strings.TrimSpace(opts.OutputRoot)
	if outputRoot != "" {
		outputRoot, err = prepareRoot(outputRoot)
		if err != nil {
			return nil, fmt.Errorf("output root: %w", err)
		}
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
	return &Filesystem{
		root:       root,
		sourceRoot: sourceRoot,
		outputRoot: outputRoot,
		hooks:      hooks,
		logger:     logger,
	}, nil
}

func (f *Filesystem) Create(ctx context.Context, issue Issue) (Info, error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return Info{}, err
	}
	root, err := os.OpenRoot(f.root)
	if err != nil {
		return Info{}, fmt.Errorf("open filesystem workspace root: %w", err)
	}
	defer f.closeRoot("workspace", root)
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return Info{}, err
	}
	if exists && !isDir {
		return Info{}, fmt.Errorf("workspace path exists and is not a directory: %s", info.Path)
	}
	created := false
	if !exists {
		if err := root.MkdirAll(info.Key, 0o700); err != nil {
			return Info{}, fmt.Errorf("create filesystem workspace: %w", err)
		}
		created = true
	}
	if err := root.MkdirAll(filepath.Join(info.Key, "artifacts"), 0o700); err != nil {
		return Info{}, fmt.Errorf("create artifact directory: %w", err)
	}
	if f.outputRoot != "" {
		if _, err := validateWorkspacePath(f.outputRoot, filepath.Join(f.outputRoot, info.Key)); err != nil {
			return Info{}, err
		}
		outputRoot, err := os.OpenRoot(f.outputRoot)
		if err != nil {
			return Info{}, fmt.Errorf("open filesystem output root: %w", err)
		}
		defer f.closeRoot("output", outputRoot)
		if err := outputRoot.MkdirAll(info.Key, 0o700); err != nil {
			return Info{}, fmt.Errorf("create output workspace: %w", err)
		}
	}
	info.Created = created
	if created {
		if err := f.runHook(ctx, "after_create", f.hooks.AfterCreate, info, issue); err != nil {
			if cleanupErr := removeWorkspacePath(f.root, info.Path); cleanupErr != nil {
				f.logger.Warn("failed to clean filesystem workspace after after_create hook error", slog.String("path", info.Path), slog.Any("error", cleanupErr))
			}
			return Info{}, err
		}
	}
	return info, nil
}

func (f *Filesystem) Cleanup(ctx context.Context, identifier string) error {
	_, err := f.CleanupIssue(ctx, Issue{Identifier: identifier})
	return err
}

func (f *Filesystem) CleanupIssue(ctx context.Context, issue Issue) (CleanupResult, error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return CleanupResult{}, err
	}
	result := CleanupResult{}
	exists, _, err := pathExists(info.Path)
	if err != nil {
		return CleanupResult{}, err
	}
	if !exists {
		return result, nil
	}
	result.Processes = reapWorkspaceProcesses(ctx, info.Path, f.logger)
	if err := f.runHook(ctx, "before_remove", f.hooks.BeforeRemove, info, issue); err != nil {
		f.logger.Warn("filesystem workspace before_remove hook failed", slog.String("path", info.Path), slog.Any("error", err))
	}
	if err := removeWorkspacePath(f.root, info.Path); err != nil {
		return CleanupResult{}, fmt.Errorf("remove filesystem workspace: %w", err)
	}
	result.Worktrees = 1
	return result, nil
}

func (f *Filesystem) BeforeRun(ctx context.Context, info Info, issue Issue) error {
	normalized, err := f.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	return f.runHook(ctx, "before_run", f.hooks.BeforeRun, normalized, issue)
}

func (f *Filesystem) AfterRun(ctx context.Context, info Info, issue Issue) {
	normalized, err := f.normalizeInfo(info, issue)
	if err != nil {
		f.logger.Warn("filesystem workspace after_run path validation failed", slog.String("path", info.Path), slog.Any("error", err))
		return
	}
	if err := f.runHook(ctx, "after_run", f.hooks.AfterRun, normalized, issue); err != nil {
		f.logger.Warn("filesystem workspace after_run hook failed", slog.String("path", normalized.Path), slog.Any("error", err))
	}
}

func (f *Filesystem) DiffStat(ctx context.Context, info Info, issue Issue) (DiffStat, error) {
	normalized, err := f.normalizeInfo(info, issue)
	if err != nil {
		return DiffStat{}, err
	}
	exists, isDir, err := pathExists(normalized.Path)
	if err != nil {
		return DiffStat{}, err
	}
	if !exists || !isDir {
		return DiffStat{}, ErrMissingWorkspace
	}
	root, err := os.OpenRoot(normalized.Path)
	if err != nil {
		return DiffStat{}, fmt.Errorf("open filesystem recovery root: %w", err)
	}
	defer f.closeRoot("recovery", root)
	filesChanged := 0
	hash := sha256.New()
	err = filepath.WalkDir(normalized.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".detent" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		filesChanged++
		relativePath, err := filepath.Rel(normalized.Path, path)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(hash, filepath.ToSlash(relativePath)+"\x00"); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := root.Readlink(relativePath)
			if err != nil {
				return err
			}
			_, err = io.WriteString(hash, "symlink\x00"+target+"\x00")
			return err
		}
		file, err := root.Open(relativePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		_, err = io.WriteString(hash, "\x00")
		return err
	})
	if err != nil {
		return DiffStat{}, err
	}
	if filesChanged == 0 {
		return DiffStat{}, nil
	}
	return DiffStat{Files: filesChanged, Fingerprint: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (f *Filesystem) infoForIssue(issue Issue) (Info, error) {
	key := issueKey(issue)
	path, err := validateWorkspacePath(f.root, filepath.Join(f.root, key))
	if err != nil {
		return Info{}, err
	}
	return Info{
		Path: path,
		Key:  key,
	}, nil
}

func (f *Filesystem) IssueRecoveryState(ctx context.Context, issue Issue) (RecoveryState, error) {
	info, err := f.infoForIssue(issue)
	if err != nil {
		return RecoveryState{}, err
	}
	diffStat, err := f.DiffStat(ctx, info, issue)
	if err != nil {
		return RecoveryState{}, err
	}
	return RecoveryState{
		DiffStat:             diffStat,
		WorkspaceFingerprint: diffStat.Fingerprint,
	}, nil
}

func (f *Filesystem) ArtifactEvidence(ctx context.Context, info Info, issue Issue) (ArtifactEvidence, error) {
	if f == nil || strings.TrimSpace(f.outputRoot) == "" {
		return ArtifactEvidence{}, nil
	}
	normalized, err := f.normalizeInfo(info, issue)
	if err != nil {
		return ArtifactEvidence{}, err
	}
	outputPath, err := validateWorkspacePath(f.outputRoot, filepath.Join(f.outputRoot, normalized.Key))
	if err != nil {
		return ArtifactEvidence{}, err
	}
	exists, isDir, err := pathExists(outputPath)
	if err != nil {
		return ArtifactEvidence{}, err
	}
	if !exists || !isDir {
		return ArtifactEvidence{Available: true}, nil
	}
	root, err := os.OpenRoot(outputPath)
	if err != nil {
		return ArtifactEvidence{}, fmt.Errorf("open artifact output root: %w", err)
	}
	defer f.closeRoot("artifact output", root)

	hash := sha256.New()
	files := 0
	err = filepath.WalkDir(outputPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relativePath, err := filepath.Rel(outputPath, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		values := []string{
			filepath.ToSlash(relativePath),
			info.Mode().String(),
			strconv.FormatInt(info.Size(), 10),
			strconv.FormatInt(info.ModTime().UTC().UnixNano(), 10),
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := root.Readlink(relativePath)
			if err != nil {
				return err
			}
			values = append(values, target)
		}
		_, err = io.WriteString(hash, strings.Join(values, "\x00")+"\x00")
		return err
	})
	if err != nil {
		return ArtifactEvidence{}, err
	}
	return ArtifactEvidence{
		Available:   true,
		Files:       files,
		Fingerprint: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func (f *Filesystem) normalizeInfo(info Info, issue Issue) (Info, error) {
	key := info.Key
	if key == "" {
		key = issueKey(issue)
	}
	path := info.Path
	if path == "" {
		var err error
		path, err = validateWorkspacePath(f.root, filepath.Join(f.root, key))
		if err != nil {
			return Info{}, err
		}
	} else {
		var err error
		path, err = validateWorkspacePath(f.root, path)
		if err != nil {
			return Info{}, err
		}
	}
	info.Path = path
	info.Key = key
	info.Branch = ""
	return info, nil
}

func (f *Filesystem) runHook(ctx context.Context, name string, command string, info Info, issue Issue) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	timeout := f.hooks.Timeout
	if timeout == 0 {
		timeout = defaultHookTimeout
	}
	hookCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		hookCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := commandshell.Command(hookCtx, command, f.hooks.Shell)
	cmd.Dir = info.Path
	cmd.Env = hookEnv(info, issue)
	cmd.WaitDelay = workspaceCommandWaitDelay
	f.logger.Info("running filesystem workspace hook", slog.String("hook", name), slog.String("path", info.Path), slog.String("command", command))

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
	logPath, logErr := f.writeHookLog(name, command, info, exitCode, err, output)
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
		f.logger.Warn("filesystem workspace hook log write failed", slog.String("hook", name), slog.String("path", info.Path), slog.String("command", command), slog.Any("error", logErr))
	}
	f.logger.Warn(
		"filesystem workspace hook failed",
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

func (f *Filesystem) writeHookLog(name string, command string, info Info, exitCode int, err error, output []byte) (string, error) {
	root, rootErr := os.OpenRoot(f.root)
	if rootErr != nil {
		return "", fmt.Errorf("open filesystem workspace root: %w", rootErr)
	}
	defer f.closeRoot("workspace", root)
	dir := filepath.Join(".detent", "hook-logs", SafeKey(info.Key))
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	relPath := filepath.Join(dir, hookLogFileName(name, time.Now().UTC(), os.Getpid()))
	path := filepath.Join(f.root, relPath)
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
	if err := root.WriteFile(relPath, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (f *Filesystem) closeRoot(name string, root *os.Root) {
	if err := root.Close(); err != nil {
		f.logger.Warn("filesystem root close failed", slog.String("root", name), slog.Any("error", err))
	}
}
