//go:build !windows

package gitexec

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/internal/commandtest"
)

func TestRunGitAtWithEnvCancellationReturnsPromptly(t *testing.T) {
	for _, test := range []struct {
		name    string
		owned   bool
		delay   time.Duration
		timeout time.Duration
		cancel  bool
	}{
		{name: "Git caller cancellation", delay: 4 * time.Second, timeout: 20 * time.Millisecond, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			execute := func(t *testing.T) {
				ctx, run := commandtest.Fixture(t, test.delay, test.timeout, test.cancel)
				started := time.Now()
				var err error

				_, err = withInputCommand(ctx, t.TempDir(), nil, true, "", run, "status")

				elapsed := time.Since(started)
				if !test.cancel {
					if err != nil {
						t.Fatalf("rewrite with an unexpired caller deadline failed: %v", err)
					}
					return
				}
				if err == nil {
					t.Fatal("runGitAtWithEnv() error = nil, want cancellation error")
				}
				var commandErr *CommandError
				if !errors.As(err, &commandErr) {
					t.Fatalf("runGitAtWithEnv() error = %T, want *CommandError", err)
				}
				if !errors.Is(commandErr.Err, context.DeadlineExceeded) {
					t.Fatalf("CommandError.Err = %v, want context deadline exceeded", commandErr.Err)
				}
				if elapsed > 2*time.Second {
					t.Fatalf("runGitAtWithEnv() elapsed = %s, want cancellation within wait delay", elapsed)
				}
			}
			if test.cancel {
				execute(t)
			} else {
				synctest.Test(t, execute)
			}
		})
	}
}
