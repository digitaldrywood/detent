package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	var helper func() error
	cancellation := slices.Index(os.Args, "checklock-cancel-child")
	inherited := slices.Index(os.Args, "checklock-inherited-output")
	switch {
	case os.Getenv(validationSubprocessHelper) != "":
		helper = validationSubprocessHelperProcess
	case os.Getenv("DETENT_CHECKLOCK_QUEUE_HELPER") != "":
		helper = validationQueueProcessHelper
	case cancellation >= 0 && cancellation+1 < len(os.Args):
		helper = func() error { return validationCancellationHelper(os.Args[cancellation+1]) }
	case inherited >= 0 && inherited+1 < len(os.Args):
		helper = func() error { return validationInheritedOutputHelper(os.Args[inherited+1]) }
	}
	if helper != nil {
		for _, argument := range os.Args {
			if directory, ok := strings.CutPrefix(argument, "checklock-coverdir="); ok {
				if err := os.Setenv("GOCOVERDIR", directory); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			}
		}
		if err := helper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func validationSubprocessHelperProcess() (err error) {
	lock, err := instancelock.Acquire(os.Getenv("DETENT_CHECKLOCK_CRITICAL_LOCK"))
	if err != nil {
		return fmt.Errorf("acquire critical lock: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	trace, err := os.OpenFile(os.Getenv("DETENT_CHECKLOCK_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open trace: %w", err)
	}
	_, writeErr := fmt.Fprintln(trace, os.Getpid())
	if err := errors.Join(writeErr, trace.Close()); err != nil {
		return fmt.Errorf("write trace: %w", err)
	}
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		return fmt.Errorf("write readiness: %w", err)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		return fmt.Errorf("wait for exit: %w", err)
	}
	return nil
}

func validationCancellationHelper(path string) (err error) {
	lock, err := instancelock.Acquire(path)
	if err != nil {
		return fmt.Errorf("acquire child lock: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		return fmt.Errorf("write readiness: %w", err)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		return fmt.Errorf("wait for exit: %w", err)
	}
	return nil
}

func validationInheritedOutputHelper(path string) error {
	cmd := exec.Command(os.Args[0], "--", "checklock-cancel-child", path)
	coverageDir := filepath.Join(os.Getenv("GOCOVERDIR"), "child")
	if err := os.Mkdir(coverageDir, 0o700); err != nil {
		return fmt.Errorf("create child coverage directory: %w", err)
	}
	cmd.Env = append(os.Environ(), "GOCOVERDIR="+coverageDir)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	output, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open child output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start inherited child: %w", err)
	}
	releaseChild := false
	defer func() {
		if !releaseChild {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	if _, err := io.ReadFull(output, make([]byte, len("ready\n"))); err != nil {
		return fmt.Errorf("read child readiness: %w", err)
	}
	if release := os.Getenv("DETENT_CHECKLOCK_PARENT_RELEASE"); release != "" {
		if _, err := fmt.Fprintln(os.Stdout, os.Getpid()); err != nil {
			return fmt.Errorf("write parent readiness: %w", err)
		}
		for {
			if _, err := os.Stat(release); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read parent release: %w", err)
			}
			if err := waitValidationPoll(context.Background()); err != nil {
				return fmt.Errorf("wait for parent release: %w", err)
			}
		}
	}
	releaseChild = true
	return nil
}

func validationQueueProcessHelper() error {
	path := os.Getenv("DETENT_CHECKLOCK_QUEUE_HELPER")
	mode := os.Getenv("DETENT_CHECKLOCK_QUEUE_MODE")
	var lock *instancelock.Lock
	var err error
	switch mode {
	case "waiter":
		var waiter validationWaiter
		waiter, err = registerValidationWaiter(context.Background(), path)
		lock = waiter.lock
	case "registration":
		lock, err = instancelock.Acquire(path + ".queue.lock")
	default:
		lock, err = instancelock.Acquire(path)
	}
	if err != nil {
		return fmt.Errorf("acquire queue process lock: %w", err)
	}
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		return fmt.Errorf("write readiness: %w", err)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, len("exit\n"))); err != nil {
		return fmt.Errorf("wait for exit: %w", err)
	}
	if mode == "holder" {
		if err := lock.Close(); err != nil {
			return fmt.Errorf("close holder lock: %w", err)
		}
	}
	return nil
}
