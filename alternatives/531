package remoterunner

import (
	"context"
	"errors"
	"reflect"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

type recordingManager struct {
	configs []project.ManagerConfig
	err     error
}

func (m *recordingManager) Reconcile(_ context.Context, cfg project.ManagerConfig) (project.ReconcileResult, error) {
	m.configs = append(m.configs, cfg)
	return project.ReconcileResult{}, m.err
}

func runnerTestConfig(capacity int, logLevel string, projectID, nativeID string) globalconfig.Config {
	return globalconfig.Config{
		Path:     "global.yaml",
		LogLevel: logLevel,
		Global:   globalconfig.Settings{MaxConcurrentAgents: capacity},
		Projects: []globalconfig.Project{{ID: projectID, Weight: 1, Pool: "default"}},
		Client:   globalconfig.HubClient{Capacity: capacity, NativeProjects: map[string]string{projectID: nativeID}},
	}
}

func TestConfigOwnerMergesHubRoutingWithFileSettings(t *testing.T) {
	t.Parallel()

	reconcileErr := errors.New("reconcile failed")
	runtimeErr := errors.New("reconfigure failed")
	initial := runnerTestConfig(2, "info", "routed", "prj_example")
	fileReload := runnerTestConfig(4, "debug", "file-only", "prj_file")
	wantFileReload := fileReload
	wantFileReload.Projects, wantFileReload.Client.NativeProjects = initial.Projects, initial.Client.NativeProjects
	hubRouting := runnerTestConfig(9, "error", "moved", "prj_moved")
	wantHubRouting := initial
	wantHubRouting.Projects = []globalconfig.Project{{ID: "moved", Weight: 1, Pool: "default"}}
	wantHubRouting.Client.NativeProjects = map[string]string{"moved": "prj_moved"}

	tests := []struct {
		name         string
		detached     bool
		runtimeErr   error
		reconcileErr error
		apply        func(context.Context, *ConfigOwner) error
		wantErr      error
		wantErrText  string
		wantCurrent  globalconfig.Config
		wantRuntime  []globalconfig.Config
		wantManager  []globalconfig.Config
		wantSynced   [][]globalconfig.Project
	}{
		{
			name: "file reload applies capacity and keeps Hub routing",
			apply: func(ctx context.Context, owner *ConfigOwner) error {
				_, err := owner.ApplyFile(ctx, fileReload)
				return err
			},
			wantCurrent: wantFileReload,
			wantRuntime: []globalconfig.Config{wantFileReload},
			wantManager: []globalconfig.Config{wantFileReload},
			wantSynced:  [][]globalconfig.Project{initial.Projects},
		},
		{
			name: "Hub routing keeps reloaded file settings and owns its copy",
			apply: func(ctx context.Context, owner *ConfigOwner) error {
				routed := hubRouting
				routed.Projects = []globalconfig.Project{{ID: "moved", Weight: 1, Pool: "default"}}
				routed.Client.NativeProjects = map[string]string{"moved": "prj_moved"}
				err := owner.ApplyProjects(ctx, routed)
				routed.Projects[0].Pool = "mutated"
				routed.Client.NativeProjects["moved"] = "mutated"
				return err
			},
			wantCurrent: wantHubRouting,
			wantRuntime: []globalconfig.Config{wantHubRouting},
			wantManager: []globalconfig.Config{wantHubRouting},
			wantSynced:  [][]globalconfig.Project{wantHubRouting.Projects},
		},
		{
			name:         "reconcile failure restores previous runtime",
			reconcileErr: reconcileErr,
			apply: func(ctx context.Context, owner *ConfigOwner) error {
				_, err := owner.ApplyFile(ctx, fileReload)
				return err
			},
			wantErr:     reconcileErr,
			wantCurrent: initial,
			wantRuntime: []globalconfig.Config{wantFileReload, initial},
			wantManager: []globalconfig.Config{wantFileReload},
		},
		{
			name:       "runtime failure skips reconcile",
			runtimeErr: runtimeErr,
			apply: func(ctx context.Context, owner *ConfigOwner) error {
				return owner.ApplyProjects(ctx, hubRouting)
			},
			wantErr:     runtimeErr,
			wantCurrent: initial,
			wantRuntime: []globalconfig.Config{wantHubRouting},
		},
		{
			name:     "owner without a project manager refuses",
			detached: true,
			apply: func(ctx context.Context, owner *ConfigOwner) error {
				_, err := owner.ApplyFile(ctx, fileReload)
				return err
			},
			wantErrText: "runner project manager is unavailable",
			wantCurrent: initial,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var runtime []globalconfig.Config
			var synced [][]globalconfig.Project
			manager := &recordingManager{err: tt.reconcileErr}
			owner := NewConfigOwner(initial, func(cfg globalconfig.Config) error {
				runtime = append(runtime, cfg)
				if len(runtime) == 1 {
					return tt.runtimeErr
				}
				return nil
			})
			if !tt.detached {
				owner.Attach(manager, func(projects []globalconfig.Project) { synced = append(synced, projects) })
			}

			err := tt.apply(t.Context(), owner)
			switch {
			case tt.wantErrText != "":
				if err == nil || err.Error() != tt.wantErrText {
					t.Fatalf("apply error = %v, want %q", err, tt.wantErrText)
				}
			case !errors.Is(err, tt.wantErr):
				t.Fatalf("apply error = %v, want %v", err, tt.wantErr)
			}
			if got := owner.Current(); !reflect.DeepEqual(got, tt.wantCurrent) {
				t.Fatalf("current = %#v, want %#v", got, tt.wantCurrent)
			}
			if !reflect.DeepEqual(runtime, tt.wantRuntime) {
				t.Fatalf("runtime applications = %#v, want %#v", runtime, tt.wantRuntime)
			}
			var wantManager []project.ManagerConfig
			for _, cfg := range tt.wantManager {
				wantManager = append(wantManager, project.ManagerConfigFromGlobal(cfg))
			}
			if !reflect.DeepEqual(manager.configs, wantManager) {
				t.Fatalf("reconciled = %#v, want %#v", manager.configs, wantManager)
			}
			if !reflect.DeepEqual(synced, tt.wantSynced) {
				t.Fatalf("dispatch syncs = %#v, want %#v", synced, tt.wantSynced)
			}
		})
	}
}
