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
	gitArgs := append([]string{"git", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git")
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
	return commandshell.RunCommand(ctx, cmd)
}

func PrepareLocalInspection(cmd *exec.Cmd) bool {
	if len(cmd.Args) <= 3 || cmd.Args[0] != "git" || cmd.Args[1] != "-C" || !localInspection(cmd.Args[3:]) {
		return false
	}
	prepareLocalInspection(cmd)
	return true
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
	env := publicationEnvironment()
	if _, err := withCommandEnvironment(ctx, scratch, env, false, "", runLocalInspection, "init", "--bare", "--template=", "--object-format="+format); err != nil {
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
	env = append(env, "GIT_DIR="+scratch, "GIT_WORK_TREE="+path, "GIT_INDEX_FILE="+index,
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

func localInspection(args []string) bool {
	switch args[0] {
	case "rev-parse", "merge-base", "show-ref", "check-ref-format":
		return true
	case "config":
		return len(args) == 3 && args[1] == "--get"
	case "remote":
		return len(args) > 2 && args[1] == "get-url"
	case "worktree":
		return len(args) > 1 && args[1] == "list"
	case "cat-file":
		return len(args) == 3 && (args[1] == "-e" || args[1] == "-t" || args[1] == "-s" || args[1] == "blob") ||
			len(args) == 2 && args[1] == "--batch-check=%(objectname) %(objecttype)"
	case "rev-list":
		for _, arg := range args[1:] {
			switch arg {
			case "--count", "--parents", "-n", "--not", "--branches", "--tags", "--remotes", "--single-worktree", "--all", "--objects", "--no-object-names", "--disk-usage":
			default:
				if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--exclude=refs/") {
					return false
				}
			}
		}
		return true
	case "for-each-ref":
		for _, arg := range args[1:] {
			switch arg {
			case "--count=1", "--contains", "--format=%(objectname)", "--format=%(refname)", "--format=update %(refname) %(objectname)":
			default:
				if strings.HasPrefix(arg, "-") {
					return false
				}
			}
		}
		return true
	case "ls-files":
		for _, arg := range args[1:] {
			switch arg {
			case "--others", "--exclude-standard", "--cached", "--stage", "-z":
			case "--":
				return true
			default:
				return false
			}
		}
		return true
	case "diff":
		cached := false
		for _, arg := range args[1:] {
			switch arg {
			case "--cached", "--staged":
				cached = true
			case "--no-ext-diff", "--no-textconv", "--name-only", "--stat", "--numstat", "--raw", "--no-renames", "-z":
			case "--":
				return cached
			default:
				if strings.HasPrefix(arg, "-") {
					return false
				}
			}
		}
		return cached
	}
	return false
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

func Scratch() (string, error) {
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if root := os.Getenv(key); root != "" {
			return os.MkdirTemp(root, "detent-change-source-*")
		}
	}
	return os.MkdirTemp("", "detent-change-source-*")
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

// WithinLimit streams one git diff invocation through a byte cap.
// The arguments are explicit so a caller that needs different diff options --
// the per-file attempt diff pins --find-renames -- gets the
// same bounded read instead of a second copy of it.
func WithinLimit(ctx context.Context, workspacePath string, env []string, maxBytes int, args ...string) (string, bool, error) {
	gitArgs := append([]string{"git", "-C", workspacePath}, args...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = gitArgs
	cmd.WaitDelay = WaitDelay
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, fmt.Errorf("git diff stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	wait, err := commandshell.StartCommand(ctx, cmd)
	if err != nil {
		return "", false, fmt.Errorf("git diff start: %w", err)
	}

	output, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := len(output) > maxBytes
	var killErr error
	if truncated && cmd.Process != nil {
		killErr = cmd.Process.Kill()
	}
	waitErr := wait()

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
