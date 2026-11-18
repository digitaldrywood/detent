package commandtest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type commandDeadlineContext struct {
	deadline time.Time
	done     chan struct{}
}

func (c *commandDeadlineContext) Value(any) any { return nil }

func (c *commandDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *commandDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *commandDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func Fixture(t *testing.T, delay, timeout time.Duration, expire bool) (context.Context, func(context.Context, *exec.Cmd) error) {
	ctx := t.Context()
	var cancel context.CancelFunc = func() {}
	if !expire {
		ctx, cancel = context.WithTimeout(t.Context(), timeout)
	}
	t.Cleanup(cancel)
	var controlled *commandDeadlineContext
	if expire {
		controlled = &commandDeadlineContext{deadline: time.Now().Add(timeout), done: make(chan struct{})}
		ctx = controlled
	}
	deadline, _ := ctx.Deadline()
	run := func(commandCtx context.Context, command *exec.Cmd) error {
		if got, ok := commandCtx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("command deadline = %v, %t, want caller deadline %v", got, ok, deadline)
		}
		if controlled != nil {
			ready, signal, err := os.Pipe()
			if err != nil {
				return err
			}
			defer closeFile(t, ready)
			defer closeFile(t, signal)
			input, hold, err := os.Pipe()
			if err != nil {
				return err
			}
			defer closeFile(t, input)
			defer closeFile(t, hold)
			cmd := command
			cmd.ExtraFiles = []*os.File{signal}
			cmd.Stdin = input
			if err := cmd.Start(); err != nil {
				return err
			}
			if err := signal.Close(); err != nil {
				close(controlled.done)
				return errors.Join(err, cmd.Wait())
			}
			var marker [1]byte
			_, readErr := io.ReadFull(ready, marker[:])
			close(controlled.done)
			return errors.Join(readErr, cmd.Wait())
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-commandCtx.Done():
			return commandCtx.Err()
		case <-timer.C:
			return nil
		}
	}
	if controlled != nil {
		binDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(binDir, "git"), []byte("#!/bin/sh\nprintf r >&3\nread value\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	return ctx, run
}

func closeFile(t *testing.T, file *os.File) {
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close %s: %v", file.Name(), err)
	}
}
