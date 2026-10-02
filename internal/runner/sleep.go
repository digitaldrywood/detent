package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"

	"github.com/digitaldrywood/detent/internal/runnerauth"
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

func inhibitSleep(ctx context.Context, failed func()) (func(), error) {
	if runtime.GOOS == "linux" && spriteSocketPresent() {
		return holdSpriteTask(ctx, failed)
	}
	return startSleepInhibitor(ctx, runtime.GOOS, exec.CommandContext, failed)
}

func startSleepInhibitor(ctx context.Context, platform string, command func(context.Context, string, ...string) *exec.Cmd, failures ...func()) (func(), error) {
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
			for _, failed := range failures {
				failed()
			}
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
	failed := false
	report := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !failed {
			failed = true
			r.sleepFailures++
		}
	}
	release, err := r.sleepInhibitor(ctx, report)
	if err != nil {
		report()
		r.logger.Warn("runner sleep inhibition unavailable", "error", err)
		release = func() {}
	}
	return func() {
		release()
		r.mu.Lock()
		defer r.mu.Unlock()
		if failed {
			failed = false
			r.sleepFailures--
		}
	}
}

func (r *Runner) Problems() []runnerauth.Problem {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sleepFailures > 0 {
		return []runnerauth.Problem{runnerauth.NewProblem("keep_awake_failed")}
	}
	return nil
}
