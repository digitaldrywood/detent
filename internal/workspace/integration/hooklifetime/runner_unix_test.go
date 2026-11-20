//go:build darwin || linux

package hooklifetime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
)

type hookTestWorkspace struct {
	*workspace.Filesystem
	created func(workspace.Issue)
}

func (w hookTestWorkspace) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	info, err := w.Filesystem.Create(ctx, issue)
	if err == nil {
		w.created(issue)
	}
	return info, err
}

type serviceAgent func(runner.AgentUpdateHandler) error

func (a serviceAgent) RunTurn(ctx context.Context, req runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return runner.AgentTurnResult{}, a(update)
}

type leaseExecution struct{ validateErr error }

func (e *leaseExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, e.validateErr
}
func (e *leaseExecution) Validate(context.Context) error                             { return e.validateErr }
func (*leaseExecution) Start(context.Context, tracker.NativeExecutionIdentity) error { return nil }
func (*leaseExecution) Checkpoint(context.Context, tracker.NativeCheckpoint) error   { return nil }
func (*leaseExecution) Finish(context.Context, string) error                         { return nil }
func (*leaseExecution) Recovery() tracker.NativeRecovery                             { return tracker.NativeRecovery{} }

func TestRunTeardownStopsDetachedHook(t *testing.T) {
	for _, mode := range []string{"success", "cancellation", "drain", "lease loss", "lease loss with incomplete cleanup"} {
		t.Run(mode, func(t *testing.T) {
			daemon := detachedHook(t)
			useDaemonCandidates(t, daemon)
			fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: daemon.Command, BeforeRun: ":"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ex := &leaseExecution{}
			scan := hookproc.ReapProcessScanner
			cleanupFailure := errors.New("fixture ownership unavailable")
			var created workspace.Issue
			served := false
			agent := serviceAgent(func(update runner.AgentUpdateHandler) error {
				daemon.Request(t)
				served = true
				if mode == "cancellation" {
					cancel(context.Canceled)
					return context.Canceled
				}
				return update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: "result", Delta: "Complete"})
			})
			backend := hookTestWorkspace{fs, func(issue workspace.Issue) {
				created = issue
				if !daemon.Alive(t) {
					t.Fatal("service exited during after_create")
				}
				switch mode {
				case "drain":
					cancel(runner.NewCancellationCause(context.Canceled, "runner_update"))
				case "lease loss", "lease loss with incomplete cleanup":
					if mode == "lease loss with incomplete cleanup" {
						hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, cleanupFailure }
					}
					ex.validateErr = runner.ErrExecutionAuthorityUnavailable
				}
			}}
			r, err := runner.NewRunner(runner.Dependencies{ProcessSource: func(context.Context, string) ([]int, error) { return nil, nil }, Workspace: backend, Workflow: config.Workflow{Prompt: "Work"}, AgentBackend: agent})
			if err != nil {
				t.Fatal(err)
			}
			req := runner.RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example/repository#1"}}
			if mode == "lease loss" || mode == "lease loss with incomplete cleanup" {
				req.Execution = ex
			}
			_, err = r.Run(ctx, req)
			var want error
			switch mode {
			case "lease loss", "lease loss with incomplete cleanup":
				want = runner.ErrExecutionAuthorityUnavailable
			case "drain", "cancellation":
				want = context.Canceled
			}
			if (mode == "success" || mode == "cancellation") && !served {
				t.Fatal("work did not use the service")
			}
			if !errors.Is(err, want) {
				t.Errorf("Run error = %v, want %v", err, want)
			}
			if mode == "lease loss with incomplete cleanup" {
				var unverified *workspace.HookExitUnverifiedError
				if !daemon.Alive(t) || !errors.As(err, &unverified) || !errors.Is(err, cleanupFailure) || errors.Is(err, runner.ErrWorkerProcessReap) {
					t.Fatal("incomplete cleanup changed the primary failure or lost the service")
				}
				hookproc.ReapProcessScanner = scan
				if _, err := fs.CleanupIssue(t.Context(), created); err != nil {
					t.Fatal(err)
				}
			}
			if daemon.Alive(t) {
				t.Fatal("detached service still holds its lock after teardown")
			}
		})
	}
}
