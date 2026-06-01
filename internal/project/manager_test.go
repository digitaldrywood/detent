package project_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestManagerStartsProjectsWithStartupLimits(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	var slept []time.Duration
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
			{ID: "charlie", Weight: 1, Paused: true},
		},
		Startup: project.StartupConfig{
			Jitter:            time.Second,
			MaxSpawnPerSecond: 2,
		},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return newManagerTestProject(t, cfg, events)
		},
		Jitter: func(time.Duration) time.Duration {
			return 100 * time.Millisecond
		},
		Sleep: func(_ context.Context, delay time.Duration) error {
			slept = append(slept, delay)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	wantSleeps := []time.Duration{100 * time.Millisecond, 600 * time.Millisecond}
	if !reflect.DeepEqual(slept, wantSleeps) {
		t.Fatalf("sleep delays = %v, want %v", slept, wantSleeps)
	}

	started := []project.ProjectID{
		receiveEvent(t, sub.C()).ProjectID,
		receiveEvent(t, sub.C()).ProjectID,
	}
	if !reflect.DeepEqual(started, []project.ProjectID{"alpha", "bravo"}) {
		t.Fatalf("started projects = %v, want [alpha bravo]", started)
	}
	if manager.Registry().Len() != 3 {
		t.Fatalf("Registry().Len() = %d, want 3", manager.Registry().Len())
	}
}

func TestManagerLiveAddRemovePauseUnpause(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Startup: project.StartupConfig{MaxSpawnPerSecond: 10},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return newManagerTestProject(t, cfg, events)
		},
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := manager.Add(context.Background(), globalconfig.Project{ID: "alpha", Weight: 1}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if event := receiveEvent(t, sub.C()); event.ProjectID != "alpha" || event.Kind != project.EventStarted {
		t.Fatalf("add event = %#v, want alpha started", event)
	}
	if err := manager.Add(context.Background(), globalconfig.Project{ID: "alpha", Weight: 1}); !errors.Is(err, project.ErrProjectExists) {
		t.Fatalf("Add() duplicate error = %v, want %v", err, project.ErrProjectExists)
	}

	if err := manager.Pause(context.Background(), "alpha"); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	stopped := receiveEvent(t, sub.C())
	paused := receiveEvent(t, sub.C())
	if stopped.Kind != project.EventStopped || paused.Kind != project.EventPaused {
		t.Fatalf("pause events = %#v %#v, want stopped then paused", stopped, paused)
	}
	got, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Get(alpha) ok = false, want true")
	}
	if !got.Paused() {
		t.Fatal("Paused() = false, want true")
	}

	if err := manager.Unpause(context.Background(), "alpha"); err != nil {
		t.Fatalf("Unpause() error = %v", err)
	}
	unpaused := receiveEvent(t, sub.C())
	restarted := receiveEvent(t, sub.C())
	if unpaused.Kind != project.EventStarted || restarted.Kind != project.EventUnpaused {
		t.Fatalf("unpause events = %#v %#v, want started then unpaused", unpaused, restarted)
	}
	if got.Paused() {
		t.Fatal("Paused() = true, want false")
	}

	if err := manager.Remove(context.Background(), "alpha"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	removed := receiveEvent(t, sub.C())
	if removed.Kind != project.EventStopped {
		t.Fatalf("remove event = %#v, want stopped", removed)
	}
	if _, ok := manager.Registry().Get("alpha"); ok {
		t.Fatal("Get(alpha) ok = true after Remove, want false")
	}
	if err := manager.Remove(context.Background(), "alpha"); !errors.Is(err, project.ErrProjectNotFound) {
		t.Fatalf("Remove() missing error = %v, want %v", err, project.ErrProjectNotFound)
	}
}

func TestManagerReconcileProjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		initial     []globalconfig.Project
		next        []globalconfig.Project
		want        project.ReconcileResult
		wantEvents  []project.Event
		wantConfigs map[project.ProjectID]globalconfig.Project
		wantErr     error
	}{
		{
			name:    "unchanged",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			next:    []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			want: project.ReconcileResult{
				Unchanged: []project.ProjectID{"alpha"},
			},
			wantConfigs: map[project.ProjectID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 1, Workdir: "/repo/alpha"},
			},
		},
		{
			name:    "added",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1}},
			next: []globalconfig.Project{
				{ID: "alpha", Weight: 1},
				{ID: "bravo", Weight: 2, Priority: 3, Workdir: "/repo/bravo"},
			},
			want: project.ReconcileResult{
				Added:     []project.ProjectID{"bravo"},
				Unchanged: []project.ProjectID{"alpha"},
			},
			wantEvents: []project.Event{{ProjectID: "bravo", Kind: project.EventStarted}},
			wantConfigs: map[project.ProjectID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 1},
				"bravo": {ID: "bravo", Weight: 2, Priority: 3, Workdir: "/repo/bravo"},
			},
		},
		{
			name: "removed",
			initial: []globalconfig.Project{
				{ID: "alpha", Weight: 1},
				{ID: "bravo", Weight: 1},
			},
			next: []globalconfig.Project{{ID: "alpha", Weight: 1}},
			want: project.ReconcileResult{
				Removed:   []project.ProjectID{"bravo"},
				Unchanged: []project.ProjectID{"alpha"},
			},
			wantEvents: []project.Event{{ProjectID: "bravo", Kind: project.EventStopped}},
			wantConfigs: map[project.ProjectID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 1},
			},
		},
		{
			name:    "changed",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/old"}},
			next:    []globalconfig.Project{{ID: "alpha", Weight: 2, Priority: 1, Workdir: "/repo/new"}},
			want: project.ReconcileResult{
				Changed: []project.ProjectID{"alpha"},
			},
			wantEvents: []project.Event{
				{ProjectID: "alpha", Kind: project.EventStopped},
				{ProjectID: "alpha", Kind: project.EventStarted},
			},
			wantConfigs: map[project.ProjectID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 2, Priority: 1, Workdir: "/repo/new"},
			},
		},
		{
			name:    "invalid config retention",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			next:    []globalconfig.Project{{ID: "  ", Weight: 1, Workdir: "/repo/invalid"}},
			wantConfigs: map[project.ProjectID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 1, Workdir: "/repo/alpha"},
			},
			wantErr: project.ErrMissingProjectID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events := hub.New[project.Event](hub.WithBuffer(16))
			sub, err := events.Subscribe(context.Background())
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}

			manager, err := project.NewManager(project.ManagerConfig{
				Projects: tt.initial,
			}, project.ManagerDependencies{
				Events: events,
				ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
					return newManagerTestProject(t, cfg, events)
				},
				Sleep: func(context.Context, time.Duration) error {
					return nil
				},
			})
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}
			if err := manager.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			drainProjectEvents(t, sub.C(), startedProjectCount(tt.initial))

			got, err := manager.Reconcile(context.Background(), project.ManagerConfig{Projects: tt.next})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reconcile() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Reconcile() = %#v, want %#v", got, tt.want)
			}
			assertProjectEvents(t, sub.C(), tt.wantEvents)
			assertNoProjectEvent(t, sub.C())
			assertManagerProjectConfigs(t, manager, tt.wantConfigs)
		})
	}
}

func TestManagerReconcileKeepsRegistryWhenNewProjectCannotBeCreated(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("invalid workflow")
	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			if cfg.ID == "bravo" {
				return nil, factoryErr
			}
			return newManagerTestProject(t, cfg, events)
		},
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	drainProjectEvents(t, sub.C(), 1)

	_, err = manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, factoryErr)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ProjectID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 1},
	})
}

func TestManagerReconcileKeepsChangedProjectWhenReplacementProvisionFails(t *testing.T) {
	t.Parallel()

	provisionErr := errors.New("provision failed")
	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			var provision func(context.Context) error
			if cfg.ID == "alpha" && cfg.Weight == 2 {
				provision = func(context.Context) error {
					return provisionErr
				}
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: provisioningConnector{provision: provision},
				Events:    events,
				Runner:    blockingRunner{},
			})
		},
		Sleep: func(context.Context, time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	drainProjectEvents(t, sub.C(), 1)

	_, err = manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 2}},
	})
	if !errors.Is(err, provisionErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, provisionErr)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ProjectID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 1},
	})

	got, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want true")
	}
	if !got.Running() {
		t.Fatal("alpha Running() = false, want true")
	}
}

func TestManagerSharedGlobalSchedulerGate(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(16))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	global := scheduler.NewWeightedFair(scheduler.Config{Capacity: 2})
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 5},
			{ID: "bravo", Weight: 3},
			{ID: "charlie", Weight: 2},
		},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return project.New(project.Config{Project: cfg}, project.Dependencies{
				Events:    events,
				Runner:    blockingRunner{},
				Scheduler: global,
			})
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	assertStartedProjects(t, sub.C(), []project.ProjectID{"alpha", "bravo", "charlie"})

	for _, id := range []project.ProjectID{"alpha", "bravo", "charlie"} {
		got, ok := manager.Registry().Get(id)
		if !ok {
			t.Fatalf("Registry().Get(%q) ok = false, want true", id)
		}
		if got.Scheduler() != global {
			t.Fatalf("project %q scheduler is not the shared global scheduler", id)
		}
	}

	slots := requestProjectSlots(t, manager, []project.ProjectID{"alpha", "bravo"})
	assertGlobalUsage(t, global, 2)
	if _, err := requestProjectSlot(manager, "charlie"); !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("charlie RequestSlot() error = %v, want ErrNoSlots", err)
	}
	assertGlobalUsage(t, global, 2)
	releaseProjectSlot(t, global, slots[0])
	charlie, err := requestProjectSlot(manager, "charlie")
	if err != nil {
		t.Fatalf("charlie RequestSlot() after release error = %v", err)
	}
	assertGlobalUsage(t, global, 2)
	releaseProjectSlot(t, global, slots[1])
	releaseProjectSlot(t, global, charlie)
	assertGlobalUsage(t, global, 0)

	assertWeightedFairCounts(t, global, manager, 100, map[string]int{
		"alpha":   50,
		"bravo":   30,
		"charlie": 20,
	})

	if err := manager.Pause(context.Background(), "bravo"); err != nil {
		t.Fatalf("Pause() error = %v", err)
	}
	stopped := receiveEvent(t, sub.C())
	paused := receiveEvent(t, sub.C())
	if stopped.ProjectID != "bravo" || stopped.Kind != project.EventStopped ||
		paused.ProjectID != "bravo" || paused.Kind != project.EventPaused {
		t.Fatalf("pause events = %#v %#v, want bravo stopped then paused", stopped, paused)
	}

	assertWeightedFairCounts(t, global, manager, 14, map[string]int{
		"alpha":   10,
		"bravo":   0,
		"charlie": 4,
	})

	if err := manager.Unpause(context.Background(), "bravo"); err != nil {
		t.Fatalf("Unpause() error = %v", err)
	}
	started := receiveEvent(t, sub.C())
	unpaused := receiveEvent(t, sub.C())
	if started.ProjectID != "bravo" || started.Kind != project.EventStarted ||
		unpaused.ProjectID != "bravo" || unpaused.Kind != project.EventUnpaused {
		t.Fatalf("unpause events = %#v %#v, want bravo started then unpaused", started, unpaused)
	}

	assertWeightedFairCounts(t, global, manager, 100, map[string]int{
		"alpha":   50,
		"bravo":   30,
		"charlie": 20,
	})
}

func TestManagerConfigFromGlobal(t *testing.T) {
	t.Parallel()

	cfg := globalconfig.Config{
		Global: globalconfig.Settings{
			Startup: map[string]any{
				"jitter_seconds":       3,
				"max_spawn_per_second": 4,
			},
		},
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}

	got := project.ManagerConfigFromGlobal(cfg)
	if got.Startup.Jitter != 3*time.Second {
		t.Fatalf("Startup.Jitter = %s, want 3s", got.Startup.Jitter)
	}
	if got.Startup.MaxSpawnPerSecond != 4 {
		t.Fatalf("Startup.MaxSpawnPerSecond = %d, want 4", got.Startup.MaxSpawnPerSecond)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "alpha" {
		t.Fatalf("Projects = %#v, want alpha", got.Projects)
	}
}

func newManagerTestProject(t *testing.T, cfg globalconfig.Project, events *hub.Hub[project.Event]) (*project.Project, error) {
	t.Helper()

	if cfg.Weight == 0 {
		cfg.Weight = 1
	}
	return project.New(project.Config{
		Project: cfg,
	}, project.Dependencies{
		Events: events,
		Runner: blockingRunner{},
	})
}

func startedProjectCount(configs []globalconfig.Project) int {
	count := 0
	for _, cfg := range configs {
		if !cfg.Paused {
			count++
		}
	}
	return count
}

func drainProjectEvents(t *testing.T, ch <-chan project.Event, count int) {
	t.Helper()

	for range count {
		receiveEvent(t, ch)
	}
}

func assertProjectEvents(t *testing.T, ch <-chan project.Event, want []project.Event) {
	t.Helper()

	for _, expected := range want {
		got := receiveEvent(t, ch)
		if got.ProjectID != expected.ProjectID || got.Kind != expected.Kind {
			t.Fatalf("event = %#v, want project_id=%q kind=%q", got, expected.ProjectID, expected.Kind)
		}
	}
}

func assertNoProjectEvent(t *testing.T, ch <-chan project.Event) {
	t.Helper()

	select {
	case event := <-ch:
		t.Fatalf("unexpected project event = %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertManagerProjectConfigs(t *testing.T, manager *project.Manager, want map[project.ProjectID]globalconfig.Project) {
	t.Helper()

	for id, expected := range want {
		got, ok := manager.Registry().Get(id)
		if !ok {
			t.Fatalf("Registry().Get(%q) ok = false, want true", id)
		}
		if cfg := got.Config(); !reflect.DeepEqual(cfg, expected) {
			t.Fatalf("project %q config = %#v, want %#v", id, cfg, expected)
		}
	}
	if got := manager.Registry().Len(); got != len(want) {
		t.Fatalf("Registry().Len() = %d, want %d", got, len(want))
	}
}

func assertStartedProjects(t *testing.T, ch <-chan project.Event, want []project.ProjectID) {
	t.Helper()

	got := make([]project.ProjectID, 0, len(want))
	for range want {
		event := receiveEvent(t, ch)
		if event.Kind != project.EventStarted {
			t.Fatalf("event = %#v, want project started", event)
		}
		got = append(got, event.ProjectID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("started projects = %v, want %v", got, want)
	}
}

func requestProjectSlots(t *testing.T, manager *project.Manager, ids []project.ProjectID) []scheduler.Slot {
	t.Helper()

	slots := make([]scheduler.Slot, 0, len(ids))
	for _, id := range ids {
		slot, err := requestProjectSlot(manager, id)
		if err != nil {
			t.Fatalf("%s RequestSlot() error = %v", id, err)
		}
		slots = append(slots, slot)
	}
	return slots
}

func requestProjectSlot(manager *project.Manager, id project.ProjectID) (scheduler.Slot, error) {
	got, ok := manager.Registry().Get(id)
	if !ok {
		return scheduler.Slot{}, project.ErrProjectNotFound
	}
	return got.Scheduler().RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  string(id),
	})
}

func releaseProjectSlot(t *testing.T, global scheduler.GlobalScheduler, slot scheduler.Slot) {
	t.Helper()

	if err := global.ReleaseSlot(slot); err != nil {
		t.Fatalf("ReleaseSlot() error = %v", err)
	}
}

func assertGlobalUsage(t *testing.T, global scheduler.GlobalScheduler, want int) {
	t.Helper()

	countersProvider, ok := global.(interface{ Counters() scheduler.Counters })
	if !ok {
		t.Fatalf("global scheduler %T does not expose Counters()", global)
	}
	if got := countersProvider.Counters().Used; got != want {
		t.Fatalf("global scheduler used slots = %d, want %d", got, want)
	}
}

func assertWeightedFairCounts(
	t *testing.T,
	global scheduler.GlobalScheduler,
	manager *project.Manager,
	iterations int,
	want map[string]int,
) {
	t.Helper()

	counts := map[string]int{
		"alpha":   0,
		"bravo":   0,
		"charlie": 0,
	}
	for range iterations {
		selection, err := global.SelectProject(context.Background(), scheduler.ProjectSelectionRequest{
			Projects: projectCandidates(manager),
		})
		if err != nil {
			t.Fatalf("SelectProject() error = %v", err)
		}
		counts[selection.Project.ID]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("weighted-fair counts after %d selections = %#v, want %#v", iterations, counts, want)
	}
}

func projectCandidates(manager *project.Manager) []scheduler.ProjectCandidate {
	projects := manager.Registry().List()
	candidates := make([]scheduler.ProjectCandidate, 0, len(projects))
	for _, item := range projects {
		cfg := item.Config()
		candidates = append(candidates, scheduler.ProjectCandidate{
			ID:       cfg.ID,
			Weight:   cfg.Weight,
			Priority: cfg.Priority,
			Paused:   cfg.Paused,
		})
	}
	return candidates
}
