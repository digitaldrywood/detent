//go:build unix

package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestHookDaemonHelper(t *testing.T) { testenv.RunHookDaemonHelper(t) }

type hookTeardownWorkspace struct {
	workspace.Backend
	cause error
}

func (w hookTeardownWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return w.cause
}

func TestRunnerStopsHookDaemonBeforeProvider(t *testing.T) {
	for _, reason := range []string{"cancelled", "runner.lease_expired", "runner.update_drain"} {
		t.Run(reason, func(t *testing.T) {
			command, identity := testenv.HookDaemonCommand(t)
			backend, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), Hooks: workspace.Hooks{AfterCreate: command}})
			if err != nil {
				t.Fatal(err)
			}
			cause := NewCancellationCause(context.Canceled, reason)
			var daemon procgroup.Identity
			r, err := NewRunner(Dependencies{
				Workflow:     config.Workflow{Config: config.Default(), Prompt: "Work"},
				Workspace:    hookTeardownWorkspace{Backend: backend, cause: cause},
				AgentBackend: &fakeCodexClient{},
				ReapWorkspaceProcesses: func(ctx context.Context, path string, grace time.Duration) (int, error) {
					daemon = identity()
					return workspace.ReapProcesses(workspace.WithProcessSource(ctx, func(context.Context, string) ([]int, error) {
						observations, err := procgroup.Observe([]procgroup.Identity{daemon})
						if err != nil {
							return nil, err
						}
						if observations[0].Alive {
							return []int{daemon.PID}, nil
						}
						return nil, nil
					}), path, grace)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "example", Identifier: "example-1"}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run error = %v", err)
			}
			if daemon.PID == 0 {
				t.Fatal("runner abandoned hook daemon without reaping")
			}
			observations, err := procgroup.Observe([]procgroup.Identity{daemon})
			if err != nil || observations[0].Alive {
				t.Fatalf("hook daemon remains: %v, %v", observations, err)
			}
		})
	}
}
