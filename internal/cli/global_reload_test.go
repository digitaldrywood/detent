package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
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

func TestGlobalConfigReloaderRestoresRuntimeConfigOnReconcileError(t *testing.T) {
	t.Parallel()

	reconcileErr := errors.New("reconcile failed")
	current := reloadTestConfig("global.yaml", 2, nil)
	next := reloadTestConfig("global.yaml", 4, nil)
	applied := []int{}
	reloader := &globalConfigReloader{
		current: current,
		manager: &globalReloadManager{err: reconcileErr},
		applyRuntime: func(cfg globalconfig.Config) error {
			applied = append(applied, cfg.Global.MaxConcurrentAgents)
			return nil
		},
	}

	_, err := reloader.apply(context.Background(), configwatcher.FileUpdate[globalconfig.Config]{
		Path:  next.Path,
		Value: next,
	})
	if !errors.Is(err, reconcileErr) {
		t.Fatalf("apply() error = %v, want %v", err, reconcileErr)
	}
	if want := []int{4, 2}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("runtime capacities = %v, want %v", applied, want)
	}
}

func TestGlobalConfigReloaderAppliesIdentityReload(t *testing.T) {
	t.Parallel()

	current := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	current.Global.Identity = globalconfig.Identity{Name: "old-worker", GitHubLogin: "old-bot"}
	next := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	next.Global.Identity = globalconfig.Identity{Name: "new-worker", GitHubLogin: "new-bot"}
	manager := &globalReloadManager{}
	reloader := &globalConfigReloader{
		current: current,
		manager: manager,
	}

	_, err := reloader.apply(context.Background(), configwatcher.FileUpdate[globalconfig.Config]{
		Path:  next.Path,
		Value: next,
	})
	if err != nil {
		t.Fatalf("apply() error = %v", err)
	}
	if manager.config.Identity.Name != "new-worker" {
		t.Fatalf("manager identity name = %q, want new-worker", manager.config.Identity.Name)
	}
	if len(manager.config.Projects) != 1 || manager.config.Projects[0].Identity.Name != "new-worker" {
		t.Fatalf("manager project identity = %#v, want new-worker", manager.config.Projects)
	}
}

func TestChangedGlobalConfigFieldsReloadClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		field           string
		requiresRestart bool
		mutate          func(*globalconfig.Config)
	}{
		{name: "environment", field: "env", requiresRestart: true, mutate: func(cfg *globalconfig.Config) { cfg.Env = "dev" }},
		{name: "log level", field: "log_level", mutate: func(cfg *globalconfig.Config) { cfg.LogLevel = "debug" }},
		{name: "log max size", field: "log_max_size_bytes", requiresRestart: true, mutate: func(cfg *globalconfig.Config) { value := 2048; cfg.LogMaxSizeBytes = &value }},
		{name: "log backups", field: "log_max_backups", requiresRestart: true, mutate: func(cfg *globalconfig.Config) { value := 2; cfg.LogMaxBackups = &value }},
		{name: "GitHub token", field: "github_token", mutate: func(cfg *globalconfig.Config) { cfg.GitHubToken = "gh" }},
		{name: "dashboard access mode", field: "dashboard_access.mode", mutate: func(cfg *globalconfig.Config) {
			cfg.DashboardAccess.Mode = globalconfig.DashboardAccessModePrivateToken
		}},
		{name: "dashboard write access", field: "dashboard_access.allow_write", mutate: func(cfg *globalconfig.Config) { cfg.DashboardAccess.AllowWrite = true }},
		{name: "port", field: "port", requiresRestart: true, mutate: func(cfg *globalconfig.Config) { value := 4101; cfg.Port = &value }},
		{name: "instance name", field: "instance_name", mutate: func(cfg *globalconfig.Config) { cfg.InstanceName = "buildbox" }},
		{name: "projects", field: "projects", mutate: func(cfg *globalconfig.Config) { cfg.Projects = []globalconfig.Project{{ID: "bravo", Weight: 2}} }},
		{name: "project pool", field: "projects", mutate: func(cfg *globalconfig.Config) { cfg.Projects[0].Pool = "video" }},
		{name: "maximum concurrent agents", field: "global.max_concurrent_agents", mutate: func(cfg *globalconfig.Config) { cfg.Global.MaxConcurrentAgents = 4 }},
		{name: "scheduling", field: "global.scheduling", mutate: func(cfg *globalconfig.Config) { cfg.Global.Scheduling = globalconfig.SchedulingRoundRobin }},
		{name: "agent pools", field: "global.agent_pools", mutate: func(cfg *globalconfig.Config) {
			cfg.Global.AgentPools = []globalconfig.AgentPool{{Name: "video", MaxConcurrentAgents: 4}}
		}},
		{name: "active hours", field: "global.active_hours", mutate: func(cfg *globalconfig.Config) {
			cfg.Global.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Sun 22:00-06:00"}}
		}},
		{name: "identity", field: "global.identity", mutate: func(cfg *globalconfig.Config) {
			cfg.Global.Identity = globalconfig.Identity{Name: "new-worker", GitHubLogin: "new-bot"}
		}},
		{name: "fair share", field: "global.fair_share", mutate: func(cfg *globalconfig.Config) { cfg.Global.FairShare = map[string]any{"half_life": "2h"} }},
		{name: "startup", field: "global.startup", mutate: func(cfg *globalconfig.Config) { cfg.Global.Startup = map[string]any{"jitter_seconds": 1} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
			next := reloadTestConfig("global.yaml", 2, []globalconfig.Project{{ID: "alpha", Weight: 1}})
			tt.mutate(&next)

			got := changedGlobalConfigFields(base, next)
			if len(got) != 1 {
				t.Fatalf("changedGlobalConfigFields() = %#v, want one change", got)
			}
			if got[0].Field != tt.field || got[0].RequiresRestart != tt.requiresRestart {
				t.Fatalf("changedGlobalConfigFields() = %#v, want field %q restart %t", got[0], tt.field, tt.requiresRestart)
			}
		})
	}
}

func TestGlobalConfigReloaderHotAppliesSchedulerCapacityWithoutInterruptingWorkers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	alpha := scheduler.ProjectCandidate{ID: "alpha", Weight: 1, Priority: 3}
	bravo := scheduler.ProjectCandidate{ID: "bravo", Weight: 1, Priority: 2}
	charlie := scheduler.ProjectCandidate{ID: "charlie", Weight: 1, Priority: 0}
	initial := reloadTestConfig("global.yaml", 2, []globalconfig.Project{
		{ID: alpha.ID, Weight: alpha.Weight, Priority: alpha.Priority},
		{ID: bravo.ID, Weight: bravo.Weight, Priority: bravo.Priority},
		{ID: charlie.ID, Weight: charlie.Weight, Priority: charlie.Priority},
	})
	initial.Global.Scheduling = globalconfig.SchedulingStrict
	gate, err := buildGlobalDispatchPools(initial, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}
	gate.MarkIdle(bravo)
	gate.MarkIdle(charlie)
	alphaSlot, ok, err := gate.TryAcquire(ctx, alpha, scheduler.SlotRequest{State: "Todo"}, time.Time{})
	if err != nil || !ok {
		t.Fatalf("alpha TryAcquire() = ok %t error %v, want granted", ok, err)
	}
	bravoSlot, ok, err := gate.TryAcquire(ctx, bravo, scheduler.SlotRequest{State: "Todo"}, time.Time{})
	if err != nil || !ok {
		t.Fatalf("bravo TryAcquire() = ok %t error %v, want granted", ok, err)
	}
	interrupts := 0
	gate.SetPreempt(alphaSlot, func() { interrupts++ })
	gate.SetPreempt(bravoSlot, func() { interrupts++ })

	next := reloadTestConfig("global.yaml", 1, nil)
	next.Global.Scheduling = globalconfig.SchedulingStrict
	if err := applyGlobalRuntimeConfig(gate, nil, nil, next); err != nil {
		t.Fatalf("applyGlobalRuntimeConfig() error = %v", err)
	}
	if _, ok, decision, err := gate.TryAcquireWithDecision(ctx, charlie, scheduler.SlotRequest{State: "Merging"}, time.Time{}); err != nil {
		t.Fatalf("charlie TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("charlie TryAcquireWithDecision() ok = true above lowered capacity")
	} else if decision.GlobalCapacity != 1 || decision.GlobalUsed != 2 {
		t.Fatalf("capacity decision = %#v, want capacity 1 used 2", decision)
	}
	if interrupts != 0 {
		t.Fatalf("worker interrupts = %d, want 0", interrupts)
	}
	if err := gate.Release(alphaSlot); err != nil {
		t.Fatalf("Release(alpha) error = %v", err)
	}
	if _, ok, _, err := gate.TryAcquireWithDecision(ctx, charlie, scheduler.SlotRequest{State: "Merging"}, time.Time{}); err != nil {
		t.Fatalf("charlie second TryAcquireWithDecision() error = %v", err)
	} else if ok {
		t.Fatal("charlie second TryAcquireWithDecision() ok = true before usage fell below the lowered capacity")
	}
	if interrupts != 0 {
		t.Fatalf("worker interrupts after attrition to capacity = %d, want 0", interrupts)
	}
	if err := gate.Release(bravoSlot); err != nil {
		t.Fatalf("Release(bravo) error = %v", err)
	}
	charlieSlot, ok, err := gate.TryAcquire(ctx, charlie, scheduler.SlotRequest{State: "Merging"}, time.Time{})
	if err != nil || !ok {
		t.Fatalf("charlie TryAcquire() after attrition = ok %t error %v, want granted", ok, err)
	}
	next.Global.MaxConcurrentAgents = 2
	if err := applyGlobalRuntimeConfig(gate, nil, nil, next); err != nil {
		t.Fatalf("raise applyGlobalRuntimeConfig() error = %v", err)
	}
	delta := scheduler.ProjectCandidate{ID: "delta", Weight: 1, Priority: 0}
	deltaSlot, ok, err := gate.TryAcquire(ctx, delta, scheduler.SlotRequest{State: "Todo"}, time.Time{})
	if err != nil || !ok {
		t.Fatalf("delta TryAcquire() after raise = ok %t error %v, want granted", ok, err)
	}
	if err := gate.Release(charlieSlot); err != nil {
		t.Fatalf("Release(charlie) error = %v", err)
	}
	if err := gate.Release(deltaSlot); err != nil {
		t.Fatalf("Release(delta) error = %v", err)
	}
}

func TestGlobalConfigReloaderHotAppliesSchedulingAndFairShare(t *testing.T) {
	t.Parallel()

	store := &globalReloadFairShareStore{}
	next := reloadTestConfig("global.yaml", 2, nil)
	gate, err := buildGlobalDispatchPools(next, store)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}
	next.Global.Scheduling = globalconfig.SchedulingFairShare
	next.Global.FairShare = map[string]any{"half_life": "2h"}

	if err := applyGlobalRuntimeConfig(gate, store, nil, next); err != nil {
		t.Fatalf("applyGlobalRuntimeConfig() error = %v", err)
	}
	if mode := gate.PoolSnapshotFor("").Mode; mode != scheduler.ModeFairShare {
		t.Fatalf("Mode() = %q, want %q", mode, scheduler.ModeFairShare)
	}
}

func TestGlobalConfigReloaderHotReconfiguresAgentPools(t *testing.T) {
	t.Parallel()

	initial := reloadTestConfig("global.yaml", 1, []globalconfig.Project{{ID: "alpha", Weight: 1}})
	initial.Global.Scheduling = globalconfig.SchedulingStrict
	gate, err := buildGlobalDispatchPools(initial, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}

	added := initial
	added.Global.AgentPools = []globalconfig.AgentPool{{
		Name:                "video",
		MaxConcurrentAgents: 2,
		BurstTo:             4,
		Scheduling:          globalconfig.SchedulingRoundRobin,
	}}
	added.Projects = []globalconfig.Project{{ID: "alpha", Pool: "video", Weight: 1}}
	if err := applyGlobalRuntimeConfig(gate, nil, nil, added); err != nil {
		t.Fatalf("add applyGlobalRuntimeConfig() error = %v", err)
	}
	if snapshot := gate.PoolSnapshotFor("alpha"); snapshot.Name != "video" ||
		snapshot.Capacity != 4 || snapshot.Guaranteed != 2 ||
		snapshot.BurstTo != 4 || snapshot.Mode != scheduler.ModeRoundRobin {
		t.Fatalf("PoolSnapshotFor(alpha) after add = %#v", snapshot)
	}

	changed := added
	changed.Global.AgentPools = []globalconfig.AgentPool{{
		Name:                "video",
		MaxConcurrentAgents: 3,
		BurstTo:             5,
		Scheduling:          globalconfig.SchedulingStrict,
	}}
	if err := applyGlobalRuntimeConfig(gate, nil, nil, changed); err != nil {
		t.Fatalf("change applyGlobalRuntimeConfig() error = %v", err)
	}
	if snapshot := gate.PoolSnapshotFor("alpha"); snapshot.Capacity != 5 ||
		snapshot.Guaranteed != 3 || snapshot.BurstTo != 5 ||
		snapshot.Mode != scheduler.ModeStrictPriority {
		t.Fatalf("PoolSnapshotFor(alpha) after change = %#v", snapshot)
	}

	removed := initial
	if err := applyGlobalRuntimeConfig(gate, nil, nil, removed); err != nil {
		t.Fatalf("remove applyGlobalRuntimeConfig() error = %v", err)
	}
	if snapshot := gate.PoolSnapshotFor("alpha"); snapshot.Name != scheduler.DefaultPoolName || snapshot.Capacity != 1 {
		t.Fatalf("PoolSnapshotFor(alpha) after removal = %#v", snapshot)
	}
	if snapshots := gate.PoolSnapshots(); len(snapshots) != 1 {
		t.Fatalf("PoolSnapshots() after removal = %#v, want default only", snapshots)
	}
}

func TestGlobalConfigReloaderHotAppliesLogLevel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelInfo)
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level}))
	logger.Debug("before reload")

	next := reloadTestConfig("global.yaml", 2, nil)
	gate, err := buildGlobalDispatchPools(next, nil)
	if err != nil {
		t.Fatalf("buildGlobalDispatchPools() error = %v", err)
	}
	next.LogLevel = "debug"
	if err := applyGlobalRuntimeConfig(gate, nil, level, next); err != nil {
		t.Fatalf("applyGlobalRuntimeConfig() error = %v", err)
	}
	logger.Debug("after reload")

	if strings.Contains(logs.String(), "before reload") || !strings.Contains(logs.String(), "after reload") {
		t.Fatalf("captured logs = %q, want only post-reload debug record", logs.String())
	}
}

func TestLogGlobalConfigChangesIncludesOldAndNewValues(t *testing.T) {
	t.Parallel()

	previous := reloadTestConfig("global.yaml", 2, nil)
	next := reloadTestConfig("global.yaml", 1, nil)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	logGlobalConfigChanges(logger, previous, next)

	for _, want := range []string{"global config setting reloaded", "field=global.max_concurrent_agents", "old=2", "new=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("reload log missing %q: %s", want, logs.String())
		}
	}
}

func TestLogGlobalConfigChangesPreservesRestartWarning(t *testing.T) {
	t.Parallel()

	previous := reloadTestConfig("global.yaml", 2, nil)
	next := reloadTestConfig("global.yaml", 2, nil)
	next.Env = "production"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	logGlobalConfigChanges(logger, previous, next)

	for _, want := range []string{"level=WARN", `msg="global config setting change requires restart"`, "field=env"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("restart warning missing %q: %s", want, logs.String())
		}
	}
}

func TestLogGlobalConfigChangesRedactsDashboardToken(t *testing.T) {
	t.Parallel()

	previous := reloadTestConfig("global.yaml", 2, nil)
	previous.DashboardAccess.Token = "private-old-token"
	next := reloadTestConfig("global.yaml", 2, nil)
	next.DashboardAccess.Token = "private-new-token"
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	logGlobalConfigChanges(logger, previous, next)

	if !strings.Contains(logs.String(), "field=dashboard_access.token") || !strings.Contains(logs.String(), "<redacted>") {
		t.Fatalf("reload log missing redacted dashboard token change: %s", logs.String())
	}
	for _, token := range []string{previous.DashboardAccess.Token, next.DashboardAccess.Token} {
		if strings.Contains(logs.String(), token) {
			t.Fatalf("reload log leaked dashboard token %q: %s", token, logs.String())
		}
	}
}

type globalReloadManager struct {
	calls  int
	config project.ManagerConfig
	err    error
}

type globalReloadFairShareStore struct{}

func (s *globalReloadFairShareStore) ListFairShareUsage(context.Context) ([]store.FairShareUsage, error) {
	return nil, nil
}

func (s *globalReloadFairShareStore) RecordFairShareDispatch(context.Context, store.FairShareDispatch) error {
	return nil
}

func (m *globalReloadManager) Reconcile(
	_ context.Context,
	cfg project.ManagerConfig,
) (project.ReconcileResult, error) {
	m.calls++
	m.config = cfg
	return project.ReconcileResult{Added: []project.ID{"bravo"}}, m.err
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
