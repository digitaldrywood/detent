package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

const validationLockPollInterval = 100 * time.Millisecond

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("checklock", flag.ContinueOnError)
	flags.SetOutput(stderr)

	lockPath := flags.String("lock", "", "validation lock path")
	waitTimeout := flags.Duration("wait-timeout", 15*time.Minute, "maximum wait without an owner handoff or unheld queue advancement")
	maxWaitTimeout := flags.Duration("max-wait-timeout", 4*time.Hour, "maximum total registration and queue wait")
	eventsPath := flags.String("events", "", "append local gate timing events as JSON lines")

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
	if *maxWaitTimeout <= 0 {
		fmt.Fprintln(stderr, "-max-wait-timeout must be positive")
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, "command is required after --")
		return 2
	}
	events := io.Discard
	if *eventsPath != "" {
		file, err := os.OpenFile(*eventsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(stderr, "open validation events: %v\n", err)
			return 1
		}
		defer func() {
			if err := file.Close(); err != nil {
				fmt.Fprintf(stderr, "close validation events: %v\n", err)
			}
		}()
		events = file
	}
	started := time.Now()
	encodedCommand := []byte(strings.Join(command, "\x00"))
	event := validationEvent{Schema: 1, PID: os.Getpid(), StartedAt: started, CommandHash: fmt.Sprintf("%x", sha256.Sum256(encodedCommand))}
	event.write(events, stderr, "waiting")

	lock, waited, err := acquireValidationLockWithTimeouts(ctx, *lockPath, stderr, *waitTimeout, *maxWaitTimeout, validationPosition)
	if err != nil {
		event.WaitSeconds = time.Since(started).Seconds()
		event.write(events, stderr, "wait_failed")
		fmt.Fprintf(stderr, "acquire validation lock: %v\n", err)
		return 1
	}
	event.WaitSeconds = time.Since(started).Seconds()
	if err := ctx.Err(); err != nil {
		event.write(events, stderr, "canceled")
		fmt.Fprintf(stderr, "validation canceled before command start: %v\n", errors.Join(err, lock.Close()))
		return 1
	}
	if waited {
		fmt.Fprintln(stderr, "validation gate acquired shared lock; starting validation (wait timeout no longer applies)")
	}
	fmt.Fprintf(stderr, "validation gate running: owner_pid=%d waited=%s\n", os.Getpid(), time.Since(started).Round(time.Millisecond))
	event.write(events, stderr, "running")

	commandPath, commandPathErr := exec.LookPath(command[0])
	if commandPathErr != nil {
		event.write(events, stderr, "failed")
		fmt.Fprintln(stderr, "validation gate finished: result=failed; command could not start")
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
	runStarted := time.Now()
	commandErr := runValidationCommand(ctx, cmd)
	event.RunSeconds = time.Since(runStarted).Seconds()
	if cmd.ProcessState != nil {
		event.CommandUserSeconds = cmd.ProcessState.UserTime().Seconds()
		event.CommandSystemSeconds = cmd.ProcessState.SystemTime().Seconds()
	}
	closeErr := lock.Close()
	phase := "passed"
	if commandErr != nil || closeErr != nil || ctx.Err() != nil {
		phase = "failed"
	}
	event.write(events, stderr, phase)
	fmt.Fprintf(stderr, "validation gate finished: result=%s waited=%s ran=%s\n", phase, time.Duration(event.WaitSeconds*float64(time.Second)).Round(time.Millisecond), time.Since(runStarted).Round(time.Millisecond))
	if closeErr != nil {
		fmt.Fprintf(stderr, "release validation lock: %v\n", closeErr)
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(stderr, "validation command canceled: %v\n", errors.Join(err, commandErr))
		return 1
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

func runValidationCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	procgroup.Configure(ctx, cmd)
	terminate := cmd.Cancel
	cmd.Cancel = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	groupID := procgroup.GroupID(cmd)
	terminated := make(chan struct{})
	var terminationErr error
	stop := context.AfterFunc(ctx, func() {
		terminationErr = terminate()
		close(terminated)
	})
	waitErr := cmd.Wait()
	if !stop() {
		<-terminated
	}
	return errors.Join(waitErr, terminationErr, procgroup.Cleanup(groupID))
}
