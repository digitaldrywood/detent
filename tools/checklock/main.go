package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const validationLockPollInterval = 100 * time.Millisecond

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("checklock", flag.ContinueOnError)
	flags.SetOutput(stderr)

	lockPath := flags.String("lock", "", "validation lock path")
	waitTimeout := flags.Duration("wait-timeout", 15*time.Minute, "maximum time to wait for another validation gate")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *lockPath == "" {
		fmt.Fprintln(stderr, "-lock is required")
		return 2
	}
	if *waitTimeout <= 0 {
		fmt.Fprintln(stderr, "-wait-timeout must be positive")
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, "command is required after --")
		return 2
	}

	waitCtx, cancel := context.WithTimeout(ctx, *waitTimeout)
	defer cancel()

	lock, waited, err := acquireValidationLock(waitCtx, *lockPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "acquire validation lock: %v\n", err)
		return 1
	}
	if waited {
		fmt.Fprintf(stderr, "validation gate acquired shared lock: %s\n", *lockPath)
	}

	commandPath, commandPathErr := exec.LookPath(command[0])
	if commandPathErr != nil {
		fmt.Fprintf(stderr, "resolve validation command: %v\n", commandPathErr)
		if err := lock.Close(); err != nil {
			fmt.Fprintf(stderr, "release validation lock: %v\n", err)
		}
		return 1
	}
	cmd := &exec.Cmd{
		Path:   commandPath,
		Args:   command,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}
	commandErr := cmd.Run()
	closeErr := lock.Close()
	if closeErr != nil {
		fmt.Fprintf(stderr, "release validation lock: %v\n", closeErr)
	}
	if commandErr != nil {
		var exitErr *exec.ExitError
		if errors.As(commandErr, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "run validation command: %v\n", commandErr)
		return 1
	}
	if closeErr != nil {
		return 1
	}
	return 0
}

func acquireValidationLock(ctx context.Context, path string, stderr io.Writer) (*instancelock.Lock, bool, error) {
	waited := false
	for {
		lock, err := instancelock.Acquire(path)
		if err == nil {
			return lock, waited, nil
		}
		if !errors.Is(err, instancelock.ErrHeld) {
			return nil, waited, err
		}
		if !waited {
			fmt.Fprintf(stderr, "validation gate waiting for another worktree: %s\n", path)
			waited = true
		}

		timer := time.NewTimer(validationLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, waited, ctx.Err()
		case <-timer.C:
		}
	}
}
