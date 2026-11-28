package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

const WaitDelay = time.Second

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

func Run(ctx context.Context, dir string, args ...string) (string, error) {
	return WithEnv(ctx, dir, nil, args...)
}

func WithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return WithEnvCapture(ctx, dir, env, true, args...)
}

func WithEnvCapture(ctx context.Context, dir string, env []string, fileOutput bool, args ...string) (string, error) {
	return WithInput(ctx, dir, env, fileOutput, "", args...)
}

func WithInput(ctx context.Context, dir string, env []string, fileOutput bool, input string, args ...string) (string, error) {
	return withInputCommand(ctx, dir, env, fileOutput, input, RunCommand, args...)
}

func withInputCommand(ctx context.Context, dir string, env []string, fileOutput bool, input string, run func(context.Context, *exec.Cmd) error, args ...string) (string, error) {
	if len(env) > 0 {
		env = append(os.Environ(), env...)
	}
	return withCommandEnvironment(ctx, dir, env, fileOutput, input, run, args...)
}

func withCommandEnvironment(ctx context.Context, dir string, env []string, fileOutput bool, input string, run func(context.Context, *exec.Cmd) error, args ...string) (string, error) {
	initializing := len(args) != 0 && args[0] == "init"
	if initializing {
		if !filepath.IsAbs(dir) {
			return "", errors.New("git init requires an absolute target directory")
		}
		env = IsolatedEnvironment()
		args = append(slices.Clone(args), "--", dir)
	}
	gitArgs := append([]string{"git", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git")
	if initializing {
		cmd.Dir = dir
	}
	cmd.Args = gitArgs
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.WaitDelay = WaitDelay
	cmd.Env = env
	var output []byte
	var err error
	if fileOutput {
		file, createErr := os.CreateTemp("", "detent-git-output-*")
		if createErr != nil {
			return "", fmt.Errorf("create git output file: %w", createErr)
		}
		cmd.Stdout = file
		cmd.Stderr = file
		err = run(ctx, cmd)
		_, seekErr := file.Seek(0, io.SeekStart)
		var readErr error
		if seekErr == nil {
			output, readErr = io.ReadAll(file)
		}
		closeErr := file.Close()
		removeErr := os.Remove(file.Name())
		if seekErr != nil {
			return "", fmt.Errorf("rewind git output file: %w", seekErr)
		}
		if readErr != nil {
			return "", fmt.Errorf("read git output file: %w", readErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close git output file: %w", closeErr)
		}
		if removeErr != nil {
			// On Windows a descendant can retain this handle without delete sharing.
			// Report the leftover file without invalidating a successful query.
			slog.Warn("remove git output file", slog.String("path", file.Name()), slog.Any("error", removeErr))
		}
	} else {
		var captured bytes.Buffer
		cmd.Stdout, cmd.Stderr = &captured, &captured
		err = run(ctx, cmd)
		output = captured.Bytes()
	}
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

func RunCommand(ctx context.Context, cmd *exec.Cmd) error {
	if commandshell.HasCommandStarter(ctx) {
		return commandshell.RunCommand(ctx, cmd)
	}
	if IsLocalGitCommand(cmd) {
		ApplyLocalIsolation(cmd)
	}
	return runOwnedGitCommand(ctx, cmd)
}

func runOwnedGitCommand(ctx context.Context, cmd *exec.Cmd) error {
	runErr, cleanupErr := procgroup.RunOwned(ctx, cmd)
	if runErr == exec.ErrWaitDelay && ctx.Err() == nil {
		runErr = nil
	}
	return errors.Join(runErr, cleanupErr)
}

func IsLocalGitCommand(cmd *exec.Cmd) bool {
	if cmd == nil || len(cmd.Args) < 2 || hasRawCommandLine(cmd) {
		return false
	}
	if !isTrustedGitExecutable(cmd.Path) || !isGitExecutableName(cmd.Args[0]) {
		return false
	}
	subIdx, ok := parseLocalGitSubcommand(cmd.Args[1:])
	if !ok {
		return false
	}
	sub := cmd.Args[subIdx+1]
	rest := cmd.Args[subIdx+2:]
	for _, a := range rest {
		if a == "--" {
			break
		}
		if isHelpOrPaginateFlag(a) {
			return false
		}
	}
	switch sub {
	case "merge-base", "show-ref", "check-ref-format",
		"ls-tree", "patch-id":
		return true
	case "rev-parse":
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if a == "--" {
				break
			}
			if !strings.HasPrefix(a, "-") {
				continue
			}
			if a == "--git-path" {
				i++
				if i >= len(rest) {
					return false
				}
				continue
			}
			switch a {
			case "--verify", "--quiet", "-q", "--short", "--abbrev-ref",
				"--symbolic-full-name", "--git-dir", "--git-common-dir",
				"--shared-index-path", "--show-toplevel",
				"--is-inside-work-tree", "--is-bare-repository",
				"--path-format=absolute", "--end-of-options",
				"--show-object-format", "--absolute-git-dir":
				continue
			default:
				return false
			}
		}
		return len(rest) > 0
	case "ls-files":
		for _, a := range rest {
			if a == "--" {
				break
			}
			if !strings.HasPrefix(a, "-") {
				continue
			}
			switch a {
			case "--stage", "-s", "--cached", "-c", "--others", "-o",
				"--exclude-standard", "--unmerged", "-u", "-z", "-v",
				"--full-name", "--error-unmatch":
				continue
			default:
				return false
			}
		}
		return true
	case "rev-list":
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") || a == "--" {
				continue
			}
			switch a {
			case "--count", "--reverse", "--first-parent", "--topo-order",
				"--no-merges", "--left-right", "--ancestry-path", "--parents", "-n":
				continue
			default:
				if strings.HasPrefix(a, "--max-count=") {
					continue
				}
				return false
			}
		}
		return len(rest) > 0
	case "diff-tree":
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				continue
			}
			switch a {
			case "--root", "--no-commit-id", "--raw", "--no-abbrev", "--no-renames",
				"--name-only", "--name-status", "-r", "-m", "-z",
				"--no-ext-diff", "--no-textconv":
				continue
			default:
				return false
			}
		}
		return len(rest) > 0
	case "cat-file":
		if len(rest) == 2 && (rest[0] == "blob" || rest[0] == "-t" || rest[0] == "-s" || rest[0] == "-e") {
			return true
		}
		return false
	case "symbolic-ref":
		positional := 0
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				if a != "--quiet" && a != "-q" && a != "--short" {
					return false
				}
				continue
			}
			positional++
		}
		return positional == 1
	default:
		return false
	}
}

func ApplyLocalIsolation(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	base := cmd.Environ()
	filtered := make([]string, 0, len(base)+3)
	for _, entry := range base {
		if strings.HasPrefix(entry, "GIT_ALLOW_PROTOCOL=") ||
			strings.HasPrefix(entry, "GIT_NO_LAZY_FETCH=") ||
			strings.HasPrefix(entry, "GIT_OPTIONAL_LOCKS=") ||
			strings.HasPrefix(entry, "GIT_EXTERNAL_DIFF=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	cmd.Env = append(filtered, "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
	if len(cmd.Args) >= 2 {
		subIdx := gitSubcommandIndex(cmd.Args[1:])
		if subIdx >= 0 {
			pos := subIdx + 1
			sub := cmd.Args[pos]
			prefix := append([]string{}, cmd.Args[:pos]...)
			prefix = append(prefix, "--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath="+os.DevNull, "-c", "log.showSignature=false")
			if sub == "diff-tree" {
				prefix = append(prefix, sub, "--no-ext-diff", "--no-textconv")
				cmd.Args = append(prefix, cmd.Args[pos+1:]...)
			} else {
				cmd.Args = append(prefix, cmd.Args[pos:]...)
			}
		}
	}
}

func isGitExecutableName(name string) bool {
	if name == "" {
		return false
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	return base == "git"
}

func isTrustedGitExecutable(path string) bool {
	if !isGitExecutableName(path) || !filepath.IsAbs(path) {
		return false
	}
	resolved, err := exec.LookPath("git")
	if err != nil || !filepath.IsAbs(resolved) {
		return false
	}
	return path == resolved
}

func isHelpOrPaginateFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "--help-all" || strings.HasPrefix(arg, "--help=") || arg == "--paginate"
}

func parseLocalGitSubcommand(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-C" {
			i++
			if i >= len(args) {
				return -1, false
			}
			continue
		}
		if arg == "--no-pager" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return -1, false
		}
		return i, true
	}
	return -1, false
}

func gitSubcommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env":
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return i
	}
	return -1
}

func prepareLocalInspection(cmd *exec.Cmd) {
	args := append([]string{}, cmd.Args[:3]...)
	args = append(args, "--no-pager", "-c", "core.fsmonitor=false", "-c", "log.showSignature=false")
	if cmd.Args[3] == "diff" {
		args = append(args, "diff", "--no-ext-diff", "--no-textconv", "--submodule=short")
		cmd.Args = append(args, cmd.Args[4:]...)
	} else {
		cmd.Args = append(args, cmd.Args[3:]...)
	}
	cmd.Env = append(cmd.Environ(), "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
}

func runLocalInspection(ctx context.Context, cmd *exec.Cmd) error {
	prepareLocalInspection(cmd)
	wait, err := procgroup.StartLocalInspection(ctx, cmd)
	if err != nil {
		return err
	}
	return wait()
}

func WithDiffInspection(ctx context.Context, path, index, head string, inspect func(context.Context) error) (err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	objects, err := ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return err
	}
	objects = strings.TrimSuffix(objects, "\n")
	shared, err := ReadPublicationSource(ctx, path, "rev-parse", "--path-format=absolute", "--shared-index-path")
	if err != nil {
		return err
	}
	scratch, err := Scratch()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	format := "sha1"
	if len(head) == 64 {
		format = "sha256"
	}
	if _, err := withCommandEnvironment(ctx, scratch, nil, false, "", runLocalInspection, "init", "--bare", "--template=", "--object-format="+format); err != nil {
		return err
	}
	if shared = strings.TrimSuffix(shared, "\n"); shared != "" {
		data, err := os.ReadFile(shared)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(scratch, filepath.Base(shared)), data, 0o600); err != nil {
			return err
		}
	}
	env := append(IsolatedEnvironment(), "GIT_DIR="+scratch, "GIT_WORK_TREE="+path, "GIT_INDEX_FILE="+index,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+strconv.Quote(objects), "GIT_ATTR_NOSYSTEM=1")
	options := []string{"-c", "core.excludesFile=" + filepath.Join(filepath.Dir(objects), "info", "exclude")}
	return inspect(commandshell.WithCommandStarter(ctx, func(ctx context.Context, cmd *exec.Cmd) (func() error, error) {
		cmd.Env = env
		if cmd.Args[3] == "diff" {
			cmd.Args = slices.Insert(cmd.Args, 4, "--ignore-submodules=dirty")
		}
		prepareLocalInspection(cmd)
		cmd.Args = slices.Insert(cmd.Args, 3, options...)
		return procgroup.StartLocalInspection(ctx, cmd)
	}))
}

const DefaultRemote = "origin"

func PathFor(ctx context.Context, workspacePath string, gitPath string) (string, error) {
	output, err := Run(ctx, workspacePath, "rev-parse", "--git-path", gitPath)
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(output)
	if path == "" {
		return "", errors.New("git path is empty")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspacePath, path)
	}
	return filepath.Clean(path), nil
}

func ErrorOutput(err error) string {
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return strings.TrimSpace(commandErr.Output)
	}
	return strings.TrimSpace(err.Error())
}

func InitScratch(ctx context.Context, dir string, options ...string) error {
	args := append([]string{"init", "--template="}, options...)
	_, err := withCommandEnvironment(ctx, dir, nil, false, "", runLocalInspection, args...)
	return err
}

func Scratch() (string, error) {
	root := os.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value := os.Getenv(key); value != "" {
			root = value
			break
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git scratch root: %w", err)
	}
	return os.MkdirTemp(root, "detent-change-source-*")
}

func ValidHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	for _, character := range head {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

var HandoffDiffExcludes = []string{
	".detent/tmp/",
	".detent/worker-tmp/",
}

type boundedLimitWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	limit      int
	truncated  bool
	onTruncate func()
}

func (w *boundedLimitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if len(p) > remaining {
		if remaining > 0 {
			_, _ = w.buf.Write(p[:remaining])
		}
		w.truncated = true
		if w.onTruncate != nil {
			w.onTruncate()
		}
		return len(p), nil
	}
	_, _ = w.buf.Write(p)
	return len(p), nil
}

func (w *boundedLimitWriter) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.truncated
}

// WithinLimit streams one git invocation through a byte cap on stdout and
// caps captured stderr so an untrusted remote cannot exhaust runner memory.
func WithinLimit(ctx context.Context, workspacePath string, env []string, maxBytes int, args ...string) (string, bool, error) {
	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()

	gitArgs := append([]string{"git", "-C", workspacePath}, args...)
	cmd := exec.CommandContext(cmdCtx, "git")
	cmd.Args = gitArgs
	cmd.WaitDelay = WaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdout := &boundedLimitWriter{
		limit:      maxBytes,
		onTruncate: cancelCmd,
	}
	stderr := &boundedLimitWriter{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	waitErr := RunCommand(cmdCtx, cmd)

	output, truncated := stdout.snapshot()
	if truncated {
		ownerErr := nonTruncationCommandError(waitErr)
		if ctx.Err() != nil {
			return "", false, errors.Join(ctx.Err(), ownerErr)
		}
		if ownerErr != nil {
			return "", false, ownerErr
		}
		return "", true, nil
	}
	if waitErr == nil || (waitErr == exec.ErrWaitDelay && ctx.Err() == nil) {
		return output, false, nil
	}

	exitCode := -1
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if ctx.Err() != nil && !errors.Is(waitErr, ctx.Err()) {
		waitErr = errors.Join(ctx.Err(), waitErr)
	}
	stderrText, _ := stderr.snapshot()
	return "", false, &CommandError{
		Command:  "git",
		Args:     gitArgs[1:],
		ExitCode: exitCode,
		Output:   output + stderrText,
		Err:      waitErr,
	}
}

func nonTruncationCommandError(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, sub := range joined.Unwrap() {
			if rem := nonTruncationCommandError(sub); rem != nil {
				kept = append(kept, rem)
			}
		}
		return errors.Join(kept...)
	}
	if err == exec.ErrWaitDelay || err == context.Canceled {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if rem := nonTruncationCommandError(wrapped.Unwrap()); rem != nil {
			return err
		}
		return nil
	}
	return err
}

func gitDiffStopError(killErr error, waitErr error) error {
	if killErr == nil || errors.Is(killErr, os.ErrProcessDone) || waitErr == nil {
		return nil
	}
	return fmt.Errorf("git diff stop after limit: %w", killErr)
}
