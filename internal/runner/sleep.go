package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

func sleepInhibitorCommand(platform string) (string, []string) {
	switch platform {
	case "darwin":
		return "caffeinate", []string{"-i", "-w", strconv.Itoa(os.Getpid())}
	case "linux":
		return "systemd-inhibit", []string{"--what=sleep", "--mode=block", "--who=Detent", "--why=Runner job", "cat"}
	default:
		return "", nil
	}
}

func inhibitSleep(ctx context.Context) (func(), error) {
	return startSleepInhibitor(ctx, runtime.GOOS, exec.CommandContext)
}

func startSleepInhibitor(ctx context.Context, platform string, command func(context.Context, string, ...string) *exec.Cmd) (func(), error) {
	name, args := sleepInhibitorCommand(platform)
	if name == "" {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	held, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := command(held, name, args...)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		closeErr := input.Close()
		cancel()
		return nil, errors.Join(err, closeErr)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil && held.Err() == nil {
			slog.Default().Warn("runner sleep inhibitor exited", "error", err)
		}
	}()
	return func() {
		if err := input.Close(); err != nil && held.Err() == nil {
			slog.Default().Warn("runner sleep inhibitor input not closed", "error", err)
		}
		cancel()
		<-done
	}, nil
}

func (r *Runner) keepAwake(ctx context.Context) func() {
	if r.sleepInhibitor == nil {
		return func() {}
	}
	release, err := r.sleepInhibitor(ctx)
	if err != nil {
		r.logger.Warn("runner sleep inhibition unavailable", "error", err)
		return func() {}
	}
	return release
}
