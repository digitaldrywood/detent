package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func TestGitDiffStopErrorIgnoresCompletedProcess(t *testing.T) {
	t.Parallel()

	waitErr := errors.New("wait")

	tests := []struct {
		name    string
		killErr error
		waitErr error
		wantErr bool
	}{
		{
			name: "no kill error",
		},
		{
			name:    "already done",
			killErr: os.ErrProcessDone,
			waitErr: waitErr,
		},
		{
			name:    "kill denied after successful wait",
			killErr: os.ErrPermission,
		},
		{
			name:    "kill denied before failed wait",
			killErr: os.ErrPermission,
			waitErr: waitErr,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := gitDiffStopError(tt.killErr, tt.waitErr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("gitDiffStopError() error = nil, want error")
				}
				if !errors.Is(err, tt.killErr) {
					t.Fatalf("gitDiffStopError() error = %v, want wrapped %v", err, tt.killErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitDiffStopError() error = %v, want nil", err)
			}
		})
	}
}

func TestWithinLimitPreservesOwnerErrors(t *testing.T) {
	t.Parallel()

	ownerErr := errors.New("owner cleanup failed")
	tests := []struct {
		name           string
		maxBytes       int
		writeOutput    string
		waitErr        error
		cancelParent   bool
		wantOwnerErr   bool
		wantParentErr  bool
		wantTruncated  bool
	}{
		{
			name:        "wait delay alone succeeds",
			maxBytes:    64,
			writeOutput: "ok\n",
			waitErr:     exec.ErrWaitDelay,
		},
		{
			name:         "wait delay joined with owner error fails",
			maxBytes:     64,
			writeOutput:  "ok\n",
			waitErr:      errors.Join(exec.ErrWaitDelay, ownerErr),
			wantOwnerErr: true,
		},
		{
			name:          "overflow with cancel alone truncates",
			maxBytes:      4,
			writeOutput:   "0123456789\n",
			waitErr:       context.Canceled,
			wantTruncated: true,
		},
		{
			name:         "overflow joined with owner error fails",
			maxBytes:     4,
			writeOutput:  "0123456789\n",
			waitErr:      errors.Join(context.Canceled, exec.ErrWaitDelay, ownerErr),
			wantOwnerErr: true,
		},
		{
			name:         "overflow with wrapped join of exit and owner error fails",
			maxBytes:     4,
			writeOutput:  "0123456789\n",
			waitErr:      fmt.Errorf("wait: %w", errors.Join(&exec.ExitError{}, ownerErr)),
			wantOwnerErr: true,
		},
		{
			name:          "parent deadline with owner error preserves both without overflow",
			maxBytes:      64,
			writeOutput:   "ok\n",
			waitErr:       errors.Join(exec.ErrWaitDelay, ownerErr),
			cancelParent:  true,
			wantOwnerErr:  true,
			wantParentErr: true,
		},
		{
			name:          "parent deadline with owner error preserves both on overflow",
			maxBytes:      4,
			writeOutput:   "0123456789\n",
			waitErr:       fmt.Errorf("wait: %w", errors.Join(&exec.ExitError{}, ownerErr)),
			cancelParent:  true,
			wantOwnerErr:  true,
			wantParentErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parentCtx, cancelParent := context.WithCancelCause(t.Context())
			defer cancelParent(nil)
			ctx := commandshell.WithCommandStarter(parentCtx, func(_ context.Context, cmd *exec.Cmd) (func() error, error) {
				if cmd.Stdout != nil {
					_, _ = cmd.Stdout.Write([]byte(tt.writeOutput))
				}
				return func() error {
					if tt.cancelParent {
						cancelParent(context.DeadlineExceeded)
					}
					return tt.waitErr
				}, nil
			})
			got, truncated, err := WithinLimit(ctx, t.TempDir(), nil, tt.maxBytes, "ls-remote", "--heads", "--refs", "origin")
			if tt.wantOwnerErr || tt.wantParentErr {
				if tt.wantOwnerErr && !errors.Is(err, ownerErr) {
					t.Fatalf("WithinLimit() = (%q, %t, %v), want wrapped %v", got, truncated, err, ownerErr)
				}
				if tt.wantParentErr && !errors.Is(err, context.Canceled) {
					t.Fatalf("WithinLimit() = (%q, %t, %v), want wrapped %v", got, truncated, err, context.Canceled)
				}
				return
			}
			if err != nil {
				t.Fatalf("WithinLimit() unexpected error = %v", err)
			}
			if truncated != tt.wantTruncated {
				t.Fatalf("WithinLimit() truncated = %t, want %t", truncated, tt.wantTruncated)
			}
			if !tt.wantTruncated && got != tt.writeOutput {
				t.Fatalf("WithinLimit() output = %q, want %q", got, tt.writeOutput)
			}
		})
	}
}
