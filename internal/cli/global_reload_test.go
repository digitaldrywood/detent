package cli

import (
	"context"
	"errors"
	"reflect"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestGlobalConfigReloaderApply(t *testing.T) {
	t.Parallel()

	reloadErr := errors.New("invalid global config")
	buildErr := globalconfig.ValidationError{
		Path:     "global.yaml",
		Problems: []string{"projects[0].workflow: expand path: home directory is not available"},
	}
	reconcileErr := errors.New("reconcile failed")
	current := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next := reloadTestConfig("global.yaml", 4, []globalconfig.Project{
		{ID: "alpha", Weight: 1},
		{ID: "bravo", Weight: 2},
	})

	tests := []struct {
		name        string
		update      configwatcher.FileUpdate[globalconfig.Config]
		managerErr  error
		wantCurrent globalconfig.Config
		wantCalls   int
		wantErr     error
		wantErrText string
	}{
		{
			name:        "valid update reconciles and retains next config",
			update:      configwatcher.FileUpdate[globalconfig.Config]{Path: next.Path, Value: next},
			wantCurrent: next,
			wantCalls:   1,
		},
		{
			name:        "invalid update keeps current config",
			update:      configwatcher.FileUpdate[globalconfig.Config]{Path: current.Path, Err: reloadErr},
			wantCurrent: current,
			wantErr:     reloadErr,
		},
		{
			name:        "build error keeps current config",
			update:      configwatcher.FileUpdate[globalconfig.Config]{Path: current.Path, Err: buildErr},
			wantCurrent: current,
			wantErrText: buildErr.Error(),
		},
		{
			name:        "reconcile error keeps current config",
			update:      configwatcher.FileUpdate[globalconfig.Config]{Path: next.Path, Value: next},
			managerErr:  reconcileErr,
			wantCurrent: current,
			wantCalls:   1,
			wantErr:     reconcileErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := &globalReloadManager{err: tt.managerErr}
			reloader := &globalConfigReloader{
				current: current,
				manager: manager,
			}

			_, err := reloader.apply(context.Background(), tt.update)
			if tt.wantErrText != "" {
				if err == nil || err.Error() != tt.wantErrText {
					t.Fatalf("apply() error = %v, want %v", err, tt.wantErrText)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("apply() error = %v, want %v", err, tt.wantErr)
			}
			if manager.calls != tt.wantCalls {
				t.Fatalf("manager calls = %d, want %d", manager.calls, tt.wantCalls)
			}
			if manager.calls > 0 {
				wantConfig := project.ManagerConfigFromGlobal(tt.update.Value)
				if !reflect.DeepEqual(manager.config, wantConfig) {
					t.Fatalf("manager config = %#v, want %#v", manager.config, wantConfig)
				}
			}
			if !reflect.DeepEqual(reloader.current, tt.wantCurrent) {
				t.Fatalf("current = %#v, want %#v", reloader.current, tt.wantCurrent)
			}
		})
	}
}

func TestGlobalConfigReloaderUpdatesRuntimeGitHubToken(t *testing.T) {
	t.Parallel()

	current := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next.GitHubToken = "next-token"
	token := newRuntimeGitHubTokenState("current-token")
	manager := &globalReloadManager{}
	reloader := &globalConfigReloader{
		current:     current,
		manager:     manager,
		githubToken: token,
		resolveGitHubToken: func(_ context.Context, cfg globalconfig.Config) (string, error) {
			return cfg.GitHubToken, nil
		},
	}

	_, err := reloader.apply(context.Background(), configwatcher.FileUpdate[globalconfig.Config]{
		Path:  next.Path,
		Value: next,
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if got := token.get(); got != "next-token" {
		t.Fatalf("runtime GitHub token = %q, want next-token", got)
	}
	if got, want := manager.config.RuntimeCredentialVersion, runtimeGitHubTokenVersion("next-token"); got != want {
		t.Fatalf("RuntimeCredentialVersion = %q, want %q", got, want)
	}
}

func TestGlobalConfigReloaderRestoresRuntimeGitHubTokenOnError(t *testing.T) {
	t.Parallel()

	reconcileErr := errors.New("reconcile failed")
	current := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next.GitHubToken = "next-token"
	token := newRuntimeGitHubTokenState("current-token")
	reloader := &globalConfigReloader{
		current:     current,
		manager:     &globalReloadManager{err: reconcileErr},
		githubToken: token,
		resolveGitHubToken: func(_ context.Context, cfg globalconfig.Config) (string, error) {
			return cfg.GitHubToken, nil
		},
	}

	_, err := reloader.apply(context.Background(), configwatcher.FileUpdate[globalconfig.Config]{
		Path:  next.Path,
		Value: next,
	})
	if !errors.Is(err, reconcileErr) {
		t.Fatalf("apply() error = %v, want %v", err, reconcileErr)
	}
	if got := token.get(); got != "current-token" {
		t.Fatalf("runtime GitHub token = %q, want current-token", got)
	}
}

type globalReloadManager struct {
	calls  int
	config project.ManagerConfig
	err    error
}

func (m *globalReloadManager) Reconcile(
	_ context.Context,
	cfg project.ManagerConfig,
) (project.ReconcileResult, error) {
	m.calls++
	m.config = cfg
	return project.ReconcileResult{Added: []project.ProjectID{"bravo"}}, m.err
}

func reloadTestConfig(path string, maxConcurrentAgents int, projects []globalconfig.Project) globalconfig.Config {
	return globalconfig.Config{
		Path:       path,
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: maxConcurrentAgents,
			Scheduling:          globalconfig.SchedulingWeighted,
			FairShare:           map[string]any{"half_life": "1h"},
			Startup:             map[string]any{"jitter_seconds": 0, "max_spawn_per_second": 1},
		},
		Projects: projects,
	}
}
