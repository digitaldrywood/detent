package project_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/symphony-go/internal/config/global"
	"github.com/digitaldrywood/symphony-go/internal/hub"
	"github.com/digitaldrywood/symphony-go/internal/project"
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
	if unpaused.Kind != project.EventUnpaused || restarted.Kind != project.EventStarted {
		t.Fatalf("unpause events = %#v %#v, want unpaused then started", unpaused, restarted)
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
