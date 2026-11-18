//go:build darwin || linux

package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
	"github.com/digitaldrywood/detent/internal/workspace/workspacetest"
)

func TestDetachedHookHelper(t *testing.T) { workspacetest.RunDetachedHook(t) }

type hookTestWorkspace struct {
	*workspace.Filesystem
	created func()
}

func (w hookTestWorkspace) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	info, err := w.Filesystem.Create(ctx, issue)
	if err == nil {
		w.created()
	}
	return info, err
}

type hookServiceBackend struct {
	AgentBackend
	work func()
}

func (b hookServiceBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.work()
	return b.AgentBackend.RunTurn(ctx, req, update)
}

func TestRunTeardownStopsDetachedHook(t *testing.T) {
	for _, mode := range []string{"success", "cancellation", "drain", "lease loss", "lease loss with incomplete cleanup"} {
		t.Run(mode, func(t *testing.T) {
			daemon := workspacetest.DetachedHook(t)
			workspacetest.UseDaemonCandidates(t, daemon)
			fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: daemon.Command, BeforeRun: ":"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ex := &testExecution{}
			scan := hookproc.ReapProcessScanner
			cleanupFailure := errors.New("fixture ownership unavailable")
			agent := AgentBackend(&fakeCodexClient{updates: []AgentUpdate{{Type: AgentUpdateMessageDelta, ItemID: "result", Delta: "Complete"}}})
			if mode == "cancellation" {
				agent = &cancelingCodexClient{cancel: func() { cancel(context.Canceled) }}
			}
			backend := hookTestWorkspace{fs, func() {
				if !daemon.Alive(t) {
					t.Fatal("service exited during after_create")
				}
				switch mode {
				case "drain":
					cancel(NewCancellationCause(context.Canceled, "runner_update"))
				case "lease loss", "lease loss with incomplete cleanup":
					if mode == "lease loss with incomplete cleanup" {
						hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, cleanupFailure }
					}
					ex.validateErr = ErrExecutionAuthorityUnavailable
				}
			}}
			served := false
			r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Workspace: backend, Workflow: config.Workflow{Prompt: "Work"}, AgentBackend: hookServiceBackend{agent, func() { daemon.Request(t); served = true }}})
			if err != nil {
				t.Fatal(err)
			}
			req := RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example/repository#1"}}
			if mode == "lease loss" || mode == "lease loss with incomplete cleanup" {
				req.Execution = ex
			}
			_, err = r.Run(ctx, req)
			var want error
			switch mode {
			case "lease loss", "lease loss with incomplete cleanup":
				want = ErrExecutionAuthorityUnavailable
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
				if !daemon.Alive(t) || errors.Is(err, cleanupFailure) {
					t.Fatal("incomplete cleanup changed the primary failure or lost the service")
				}
				hookproc.ReapProcessScanner = scan
				if _, err := fs.CleanupIssue(t.Context(), workspaceIssue(r.projectID, req.Issue)); err != nil {
					t.Fatal(err)
				}
			}
			if daemon.Alive(t) {
				t.Fatal("detached service still holds its lock after teardown")
			}
		})
	}
}

type progressHookWorkspace struct {
	*workspace.Filesystem
	headErr   error
	onRelease func()
}

func (w progressHookWorkspace) Use(ctx context.Context, issue workspace.Issue) (func(), error) {
	release, err := w.Filesystem.Use(ctx, issue)
	if err != nil {
		return nil, err
	}
	return func() { w.onRelease(); release() }, nil
}

func (w progressHookWorkspace) Head(context.Context, workspace.Info, workspace.Issue) (string, error) {
	return "head", w.headErr
}

func (progressHookWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func TestProgressHookStopsBeforeCheckoutRelease(t *testing.T) {
	for _, test := range []struct {
		name    string
		headErr error
	}{
		{"completed work", nil}, {"preparation failed", errors.New("fixture head unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := workspacetest.DetachedHook(t)
			workspacetest.UseDaemonCandidates(t, service)
			fs, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: service.Command}})
			if err != nil {
				t.Fatal(err)
			}
			released := false
			backend := progressHookWorkspace{fs, test.headErr, func() {
				if service.Alive(t) {
					t.Error("checkout released with live hook service")
				}
				released = true
			}}
			r, err := NewRunner(Dependencies{Workspace: backend, ProcessSource: emptyProcessSource, AgentBackend: &fakeCodexClient{}})
			if err != nil {
				t.Fatal(err)
			}
			execution := &progressSourceExecution{source: workspace.ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: "head", Source: &tracker.ChangeSource{}}}}}
			_, finish, err := r.progressWorkspace(t.Context(), RunRequest{Issue: connector.Issue{ID: "example", Identifier: "EXAMPLE-1"}, Execution: execution}, progresscheck.Request{SourceSHA: "head"})
			if finish != nil {
				service.Request(t)
				finish(&err)
			}
			if !errors.Is(err, test.headErr) || !released {
				t.Fatalf("checkout released = %t, error = %v", released, err)
			}
		})
	}
}
