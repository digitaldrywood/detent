//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestCloneRunnerRepositoryFailure(t *testing.T) {

	for _, test := range []struct {
		name, stderr, fix string
		timeout           bool
		oversized         bool
	}{
		{name: "authentication", stderr: "fatal: Authentication failed for https://user:password@github.com/example-org/project.git", fix: "gh auth login"},
		{name: "not found", stderr: "fatal: repository not found", fix: "Verify the repository binding"},
		{name: "network", stderr: "fatal: Could not resolve host: github.com", fix: "Check repository connectivity"},
		{name: "timeout", stderr: "transfer stalled", timeout: true, fix: "Repository checkout timed out"},
		{name: "excessive diagnostics", stderr: "fatal: Authentication failed for https://user:password@github.com/example-org/project.git", fix: "gh auth login", oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\nexit 128\n"
			if test.oversized {
				script = "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\ni=0\nwhile [ \"$i\" -lt 1024 ]; do printf '%01024d' 0 >&2; i=$((i+1)); done\nprintf discarded-suffix >&2\nexit 128\n"
			}
			if test.timeout {
				script = "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\nprintf ready > \"$CHECKOUT_STARTED\"\nwhile :; do :; done\n"
			}
			if err := os.WriteFile(filepath.Join(root, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root)
			t.Setenv("CHECKOUT_STDERR", test.stderr)
			started := filepath.Join(root, "started")
			t.Setenv("CHECKOUT_STARTED", started)
			contexts := newControlledRuntimeCommandContexts()
			contexts.AllowCommand()
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- cloneRunnerRepositoryWithCommandContext(t.Context(), "https://github.com/example-org/project.git", root, contexts.New)
			}()
			<-contexts.ready
			t.Cleanup(func() {
				contexts.Expire()
				<-done
			})
			if test.timeout {
				testenv.WaitUntil(t, t.Context(), func() error {
					_, err := os.Stat(started)
					return err
				})
				contexts.Expire()
			}
			err := <-done
			var checkoutErr *runnerCheckoutError
			if !errors.As(err, &checkoutErr) || !strings.Contains(checkoutErr.fix, test.fix) {
				t.Fatalf("checkout failure: %v", err)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("lost git exit status: %v", err)
			}
			if errors.Is(err, context.DeadlineExceeded) != test.timeout {
				t.Fatalf("deadline cause: %v", err)
			}
			if !test.timeout && exitErr.ExitCode() != 128 {
				t.Fatalf("exit status = %d", exitErr.ExitCode())
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "user:") {
				t.Fatalf("credentials leaked: %v", err)
			}
			checks := runnerauth.LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending", CheckoutMessage: checkoutErr.guidance(), CheckoutFix: checkoutErr.fix}
			if err := checks.Validate(); err != nil {
				t.Fatalf("Hub rejected checkout guidance: %v", err)
			}
			if test.timeout && !strings.Contains(err.Error(), "transfer stalled") {
				t.Fatalf("lost stderr: %v", err)
			}
			if test.oversized {
				if len(err.Error()) > 9*1024 || !strings.Contains(err.Error(), "[stderr truncated]") || strings.Contains(err.Error(), "discarded-suffix") {
					t.Fatalf("stderr was not bounded with a truncation marker: %d bytes", len(err.Error()))
				}
				if !strings.Contains(err.Error(), "Authentication failed") {
					t.Fatal("lost retained stderr")
				}
			} else if strings.Contains(err.Error(), "[stderr truncated]") {
				t.Fatal("short stderr was marked truncated")
			}
		})
	}
}
