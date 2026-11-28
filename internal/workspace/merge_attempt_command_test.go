package workspace

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

type fakeFencedProcess struct {
	startedErr error
	started    bool
	finished   bool
}

func (f *fakeFencedProcess) Started(context.Context, procgroup.Identity) error {
	f.started = true
	return f.startedErr
}

func (f *fakeFencedProcess) Finished(context.Context, gate.CommandResult, error) error {
	f.finished = true
	return nil
}

func TestStartAttemptCommandFencesUnknownGitSubcommand(t *testing.T) {
	t.Parallel()
	attempt := AttemptWorkspace{
		Organization: "org_example",
		Project:      "prj_example",
		Attempt:      "attempt_11111111111111111111111111111111",
	}
	t.Run("missing owner fails closed", func(t *testing.T) {
		t.Parallel()
		ctx := WithAttemptWorkspace(t.Context(), attempt)
		cmd := exec.CommandContext(ctx, "git", "custom-helper")
		if _, err := commandshell.StartCommand(ctx, cmd); err == nil {
			t.Fatal("unknown git subcommand started without fenced session owner")
		}
	})
	t.Run("requires start acknowledgement before release", func(t *testing.T) {
		t.Parallel()
		ackErr := errors.New("acknowledgement refused")
		proc := &fakeFencedProcess{startedErr: ackErr}
		ctx := WithValidationProcessOwner(t.Context(), func(context.Context, Info, Issue, string) (ValidationProcess, error) {
			return proc, nil
		})
		ctx = WithAttemptWorkspace(ctx, attempt)
		cmd := exec.CommandContext(ctx, "git", "custom-helper")
		_, err := commandshell.StartCommand(ctx, cmd)
		if !errors.Is(err, ackErr) {
			t.Fatalf("StartCommand() error = %v, want %v", err, ackErr)
		}
		if !proc.started || !proc.finished {
			t.Fatalf("started=%v finished=%v, want both true", proc.started, proc.finished)
		}
	})
}
