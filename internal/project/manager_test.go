package project_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
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

	wantSleeps := []time.Duration{600 * time.Millisecond}
	if !reflect.DeepEqual(slept, wantSleeps) {
		t.Fatalf("sleep delays = %v, want %v", slept, wantSleeps)
	}

	started := []project.ID{
		receiveEvent(t, sub.C()).ProjectID,
		receiveEvent(t, sub.C()).ProjectID,
	}
	slices.Sort(started)
	if !reflect.DeepEqual(started, []project.ID{"alpha", "bravo"}) {
		t.Fatalf("started projects = %v, want [alpha bravo]", started)
	}
	if manager.Registry().Len() != 3 {
		t.Fatalf("Registry().Len() = %d, want 3", manager.Registry().Len())
	}
}

func TestManagerStartKeepsHealthyProjectRunningWhenProvisioningIsTransientlyUnavailable(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	attempts := 0
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			workflowCfg.Tracker.AutoProvision = true
			projectConnector := provisioningConnector{}
			if cfg.ID == "alpha" {
				projectConnector.provision = func(context.Context) error {
					mu.Lock()
					defer mu.Unlock()
					attempts++
					if attempts == 1 {
						return githubconnector.ErrTransient
					}
					return nil
				}
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				Connector: projectConnector,
				Runner:    blockingRunner{},
			})
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v, want degraded project startup", err)
	}

	alpha, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want degraded project retained")
	}
	if alpha.Running() {
		t.Fatal("alpha Running() = true before provisioning retry, want false")
	}
	if alpha.RuntimeError().Message == "" {
		t.Fatal("alpha RuntimeError().Message = empty, want transient provisioning error")
	}
	if alpha.RuntimeError().NextRetryAt.IsZero() {
		t.Fatal("alpha RuntimeError().NextRetryAt is zero, want scheduled retry")
	}
	bravo, ok := manager.Registry().Get("bravo")
	if !ok {
		t.Fatal("Registry().Get(bravo) ok = false, want healthy project")
	}
	if !bravo.Running() {
		t.Fatal("bravo Running() = false, want healthy project running")
	}
}

func TestManagerRetriesTransientProvisioningWithBoundedExponentialBackoff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	baseTime := time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC)
	attempted := make(chan int, 4)
	delays := make(chan time.Duration)
	release := make(chan struct{})
	var mu sync.Mutex
	attempts := 0

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			workflowCfg.Tracker.AutoProvision = true
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				Connector: provisioningConnector{provision: func(context.Context) error {
					mu.Lock()
					attempts++
					attempt := attempts
					mu.Unlock()
					attempted <- attempt
					if attempt < 4 {
						return githubconnector.ErrTransient
					}
					return nil
				}},
				Runner: blockingRunner{},
			})
		},
		ConnectorRetry: project.ConnectorRetryConfig{
			InitialBackoff: 2 * time.Second,
			MaxBackoff:     5 * time.Second,
			Jitter:         time.Second,
		},
		RetrySleep: func(ctx context.Context, delay time.Duration) error {
			select {
			case delays <- delay:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		RetryJitter: func(time.Duration) time.Duration {
			return 500 * time.Millisecond
		},
		Now: func() time.Time {
			return baseTime
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if attempt := receiveProvisionAttempt(t, attempted); attempt != 1 {
		t.Fatalf("initial attempt = %d, want 1", attempt)
	}

	alpha, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want degraded project")
	}
	initialError := alpha.RuntimeError()
	if !initialError.NextRetryAt.Equal(baseTime.Add(2500 * time.Millisecond)) {
		t.Fatalf("initial NextRetryAt = %v, want %v", initialError.NextRetryAt, baseTime.Add(2500*time.Millisecond))
	}

	wantDelays := []time.Duration{2500 * time.Millisecond, 4500 * time.Millisecond, 5 * time.Second}
	for index, wantDelay := range wantDelays {
		if delay := receiveRetryDelay(t, delays); delay != wantDelay {
			t.Fatalf("retry %d delay = %v, want %v", index+1, delay, wantDelay)
		}
		release <- struct{}{}
		if attempt := receiveProvisionAttempt(t, attempted); attempt != index+2 {
			t.Fatalf("retry %d attempt = %d, want %d", index+1, attempt, index+2)
		}
	}

	waitForProjectRunning(t, alpha)
	manager.Wait()
	if runtimeErr := alpha.RuntimeError(); runtimeErr.Message != "" || !runtimeErr.NextRetryAt.IsZero() {
		t.Fatalf("RuntimeError() after recovery = %#v, want cleared", runtimeErr)
	}
	cancel()
	if err := alpha.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestManagerRetriesTransientConnectorFactoryFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	retryStarted := make(chan time.Duration)
	releaseRetry := make(chan struct{})
	var mu sync.Mutex
	factoryCalls := 0

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					mu.Lock()
					defer mu.Unlock()
					factoryCalls++
					if factoryCalls == 1 {
						return nil, githubconnector.ErrTransient
					}
					return provisioningConnector{}, nil
				},
				Runner: blockingRunner{},
			})
		},
		ConnectorRetry: project.ConnectorRetryConfig{
			InitialBackoff: time.Second,
			MaxBackoff:     2 * time.Second,
			Jitter:         time.Millisecond,
		},
		RetrySleep: func(ctx context.Context, delay time.Duration) error {
			select {
			case retryStarted <- delay:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-releaseRetry:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		RetryJitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if manager.Registry().Len() != 0 {
		t.Fatalf("Registry().Len() = %d, want no initialized project", manager.Registry().Len())
	}
	pending, ok := manager.Registry().Pending("alpha")
	if !ok {
		t.Fatal("Registry().Pending(alpha) ok = false, want degraded factory state")
	}
	if pending.Status != project.HealthStatusDegraded || pending.NextRetryAt.IsZero() || pending.RetryStopped {
		t.Fatalf("pending health = %#v, want retrying degraded state", pending)
	}

	if delay := receiveRetryDelay(t, retryStarted); delay != time.Second {
		t.Fatalf("retry delay = %v, want %v", delay, time.Second)
	}
	releaseRetry <- struct{}{}
	manager.Wait()
	alpha, ok := manager.Registry().Get("alpha")
	if !ok || !alpha.Running() {
		t.Fatalf("Registry().Get(alpha) = %#v, %t, want running project", alpha, ok)
	}
	if _, ok := manager.Registry().Pending("alpha"); ok {
		t.Fatal("Registry().Pending(alpha) ok = true after recovery, want false")
	}
	cancel()
	if err := alpha.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestManagerKeepsPermanentConnectorFactoryFailureTerminal(t *testing.T) {
	t.Parallel()

	retryStarted := make(chan time.Duration, 1)
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					return nil, errors.New("connector credentials are invalid")
				},
				Runner: blockingRunner{},
			})
		},
		RetrySleep: func(context.Context, time.Duration) error {
			retryStarted <- time.Second
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want terminal project degradation", err)
	}
	health, ok := manager.Registry().Pending("alpha")
	if !ok {
		t.Fatal("Registry().Pending(alpha) ok = false, want terminal health")
	}
	if health.Status != project.HealthStatusDegraded || !health.RetryStopped || !health.NextRetryAt.IsZero() {
		t.Fatalf("pending health = %#v, want terminal degraded state", health)
	}
	if !strings.Contains(health.LastError, "connector credentials are invalid") {
		t.Fatalf("LastError = %q, want credential error", health.LastError)
	}
	select {
	case delay := <-retryStarted:
		t.Fatalf("retry scheduled for permanent failure with delay %v", delay)
	default:
	}
}

func TestManagerReconcileRemovesPendingConnectorRetry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	retryStarted := make(chan struct{})
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					return nil, githubconnector.ErrTransient
				},
				Runner: blockingRunner{},
			})
		},
		RetrySleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-retryStarted:
			default:
				close(retryStarted)
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending retry")
	}
	result, err := manager.Reconcile(ctx, project.ManagerConfig{})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(result.Removed, []project.ID{"alpha"}) {
		t.Fatalf("Reconcile().Removed = %v, want [alpha]", result.Removed)
	}
	if _, ok := manager.Registry().Pending("alpha"); ok {
		t.Fatal("Registry().Pending(alpha) ok = true after removal, want false")
	}
	manager.Wait()
}

func TestManagerReconcileRecoversTerminalConnectorFactoryFailureAfterConfigChange(t *testing.T) {
	t.Parallel()

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					if cfg.Weight == 1 {
						return nil, errors.New("connector credentials are invalid")
					}
					return provisioningConnector{}, nil
				},
				Runner: blockingRunner{},
			})
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if health, ok := manager.Registry().Pending("alpha"); !ok || !health.RetryStopped {
		t.Fatalf("Registry().Pending(alpha) = %#v, %t, want terminal state", health, ok)
	}

	result, err := manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 2}},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(result.Changed, []project.ID{"alpha"}) {
		t.Fatalf("Reconcile().Changed = %v, want [alpha]", result.Changed)
	}
	alpha, ok := manager.Registry().Get("alpha")
	if !ok || !alpha.Running() {
		t.Fatalf("Registry().Get(alpha) = %#v, %t, want recovered running project", alpha, ok)
	}
	if _, ok := manager.Registry().Pending("alpha"); ok {
		t.Fatal("Registry().Pending(alpha) ok = true after config recovery, want false")
	}
	if err := alpha.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestManagerStartRejectsNilProjectFactoryResult(t *testing.T) {
	t.Parallel()

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		ProjectFactory: func(globalconfig.Project) (*project.Project, error) {
			return nil, nil //nolint:nilnil // Test verifies manager rejects a nil project without a factory error.
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(context.Background()); !errors.Is(err, project.ErrMissingProject) {
		t.Fatalf("Start() error = %v, want %v", err, project.ErrMissingProject)
	}
	if manager.Registry().Len() != 0 {
		t.Fatalf("Registry().Len() = %d, want 0", manager.Registry().Len())
	}
}

func TestManagerStartsProjectsWithBoundedConcurrency(t *testing.T) {
	t.Parallel()

	const maxConcurrentStarts = 2

	events := hub.New[project.Event](hub.WithBuffer(8))
	release := make(chan struct{})
	started := make(chan project.ID, 4)
	var active int
	var maxActive int
	var mu sync.Mutex

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
			{ID: "charlie", Weight: 1},
			{ID: "delta", Weight: 1},
		},
		Startup: project.StartupConfig{
			MaxConcurrentStarts: maxConcurrentStarts,
		},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			projectConnector := provisioningConnector{
				provision: func(ctx context.Context) error {
					mu.Lock()
					active++
					if active > maxActive {
						maxActive = active
					}
					mu.Unlock()

					started <- project.ID(cfg.ID)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
					}

					mu.Lock()
					active--
					mu.Unlock()
					return nil
				},
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: projectConnector,
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

	done := make(chan error, 1)
	go func() {
		done <- manager.Start(context.Background())
	}()

	for range maxConcurrentStarts {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent project startup")
		}
	}
	select {
	case id := <-started:
		t.Fatalf("project %s started beyond concurrency limit", id)
	default:
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager.Start")
	}

	mu.Lock()
	gotMaxActive := maxActive
	mu.Unlock()
	if gotMaxActive != maxConcurrentStarts {
		t.Fatalf("max active starts = %d, want %d", gotMaxActive, maxConcurrentStarts)
	}
	if manager.Registry().Len() != 4 {
		t.Fatalf("Registry().Len() = %d, want 4", manager.Registry().Len())
	}
}

func TestManagerStartCancellationWaitsForStartedProjectCleanup(t *testing.T) {
	t.Parallel()

	fixture := newBlockingStartupManager(t, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fixture.manager.Start(ctx)
	}()

	select {
	case <-fixture.refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial project refresh")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Manager.Start() returned before project refresh cleanup completed: %v", err)
	default:
	}

	close(fixture.releaseRefresh)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Manager.Start() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manager startup rollback")
	}
	assertConnectorClosed(t, fixture.alphaConnector.closeTracker)
	if fixture.manager.Registry().Len() != 0 {
		t.Fatalf("Registry().Len() = %d, want 0", fixture.manager.Registry().Len())
	}
}

func TestManagerStartCancellationBoundsStalledProjectCleanup(t *testing.T) {
	t.Parallel()

	fixture := newBlockingStartupManager(t, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fixture.manager.Start(ctx)
	}()

	select {
	case <-fixture.refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial project refresh")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Manager.Start() error = %v, want %v", err, context.Canceled)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Manager.Start() error = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bounded manager startup rollback")
	}
	if fixture.manager.Registry().Len() != 2 {
		t.Fatalf("Registry().Len() = %d, want 2 retained projects", fixture.manager.Registry().Len())
	}

	close(fixture.releaseRefresh)
	for _, runtimeProject := range fixture.manager.Registry().List() {
		if err := runtimeProject.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}
	assertConnectorClosed(t, fixture.alphaConnector.closeTracker)
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

func TestManagerRemoveClosesProjectConnector(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	projectConnector := newCloseTrackingConnector()
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: projectConnector,
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
	receiveEvent(t, sub.C())

	if err := manager.Remove(context.Background(), "alpha"); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	receiveEvent(t, sub.C())
	assertConnectorClosed(t, projectConnector)
}

func TestManagerRemoveKeepsProjectOpenWhenStopContextExpires(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	release := make(chan struct{})
	projectConnector := newCloseTrackingConnector()
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: projectConnector,
				Events:    events,
				Runner:    releaseBlockingRunner{release: release},
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
	receiveEvent(t, sub.C())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Remove(ctx, "alpha"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove() error = %v, want %v", err, context.Canceled)
	}
	if _, ok := manager.Registry().Get("alpha"); !ok {
		t.Fatal("Get(alpha) ok = false after failed Remove, want true")
	}
	assertConnectorOpen(t, projectConnector)

	close(release)
	if err := manager.Remove(context.Background(), "alpha"); err != nil {
		t.Fatalf("Remove() after release error = %v", err)
	}
	assertConnectorClosed(t, projectConnector)
}

func TestManagerReconcileProjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		initial     []globalconfig.Project
		next        []globalconfig.Project
		want        project.ReconcileResult
		wantEvents  []project.Event
		wantConfigs map[project.ID]globalconfig.Project
		wantErr     error
	}{
		{
			name:    "unchanged",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			next:    []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			want: project.ReconcileResult{
				Unchanged: []project.ID{"alpha"},
			},
			wantConfigs: map[project.ID]globalconfig.Project{
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
				Added:     []project.ID{"bravo"},
				Unchanged: []project.ID{"alpha"},
			},
			wantEvents: []project.Event{{ProjectID: "bravo", Kind: project.EventStarted}},
			wantConfigs: map[project.ID]globalconfig.Project{
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
				Removed:   []project.ID{"bravo"},
				Unchanged: []project.ID{"alpha"},
			},
			wantEvents: []project.Event{{ProjectID: "bravo", Kind: project.EventStopped}},
			wantConfigs: map[project.ID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 1},
			},
		},
		{
			name:    "changed",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/old"}},
			next:    []globalconfig.Project{{ID: "alpha", Weight: 2, Priority: 1, Workdir: "/repo/new"}},
			want: project.ReconcileResult{
				Changed: []project.ID{"alpha"},
			},
			wantEvents: []project.Event{
				{ProjectID: "alpha", Kind: project.EventStopped},
				{ProjectID: "alpha", Kind: project.EventStarted},
			},
			wantConfigs: map[project.ID]globalconfig.Project{
				"alpha": {ID: "alpha", Weight: 2, Priority: 1, Workdir: "/repo/new"},
			},
		},
		{
			name:    "invalid config retention",
			initial: []globalconfig.Project{{ID: "alpha", Weight: 1, Workdir: "/repo/alpha"}},
			next:    []globalconfig.Project{{ID: "  ", Weight: 1, Workdir: "/repo/invalid"}},
			wantConfigs: map[project.ID]globalconfig.Project{
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

func TestManagerReconcileAddedProjectBeginsPolling(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	alphaRunner := newProjectBlockingRunner()
	bravoRunner := newProjectBlockingRunner()
	workflowWithIssue := func(issueID string, title string) workflowconfig.Workflow {
		workflowCfg := workflowConfigWithMemoryIssue(issueID)
		workflowCfg.Tracker.Issues[0].State = "Todo"
		workflowCfg.Tracker.Issues[0].Title = title
		workflowCfg.Tracker.Issues[0].AssignedToWorker = true
		return workflowconfig.Workflow{Config: workflowCfg, Prompt: title + "."}
	}
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			switch cfg.ID {
			case "alpha":
				return project.New(project.Config{
					Project:  cfg,
					Workflow: workflowWithIssue("issue-alpha", "Run alpha"),
				}, project.Dependencies{
					Events: events,
					Runner: alphaRunner,
				})
			case "bravo":
				return project.New(project.Config{
					Project:  cfg,
					Workflow: workflowWithIssue("issue-bravo", "Run bravo"),
				}, project.Dependencies{
					Events: events,
					Runner: bravoRunner,
				})
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
	alphaRequest := receiveRunRequest(t, alphaRunner.started)
	if alphaRequest.Issue.ID != "issue-alpha" {
		t.Fatalf("alpha dispatched issue ID = %q, want issue-alpha", alphaRequest.Issue.ID)
	}
	t.Cleanup(func() {
		close(alphaRunner.release)
		close(bravoRunner.release)
		for _, item := range manager.Registry().List() {
			if !item.Running() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := item.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
				cancel()
				t.Fatalf("Stop(%s) error = %v", item.ID(), err)
			}
			cancel()
		}
	})

	got, err := manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	want := project.ReconcileResult{
		Added:     []project.ID{"bravo"},
		Unchanged: []project.ID{"alpha"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reconcile() = %#v, want %#v", got, want)
	}
	assertProjectEvents(t, sub.C(), []project.Event{{ProjectID: "bravo", Kind: project.EventStarted}})
	request := receiveRunRequest(t, bravoRunner.started)
	if request.Issue.ID != "issue-bravo" {
		t.Fatalf("dispatched issue ID = %q, want issue-bravo", request.Issue.ID)
	}
	alpha, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want true")
	}
	alphaState, err := alpha.Orchestrator().State(context.Background())
	if err != nil {
		t.Fatalf("alpha State() error = %v", err)
	}
	if _, ok := alphaState.Running["issue-alpha"]; !ok {
		t.Fatalf("alpha running agents = %#v, want issue-alpha still running", alphaState.Running)
	}
}

func TestManagerReconcileRecordsAddedProjectStartFailure(t *testing.T) {
	t.Parallel()

	provisionErr := errors.New("provision failed")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		Logger: logger,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			if cfg.ID == "bravo" {
				workflowCfg := workflowConfig("memory")
				workflowCfg.Tracker.AutoProvision = true
				return project.New(project.Config{
					Project:  cfg,
					Workflow: workflowconfig.Workflow{Config: workflowCfg},
				}, project.Dependencies{
					Connector: provisioningConnector{provision: func(context.Context) error {
						return provisionErr
					}},
					Events: events,
					Runner: blockingRunner{},
				})
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
	t.Cleanup(func() {
		for _, item := range manager.Registry().List() {
			if !item.Running() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := item.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
				cancel()
				t.Fatalf("Stop(%s) error = %v", item.ID(), err)
			}
			cancel()
		}
	})

	got, err := manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	want := project.ReconcileResult{
		Added:     []project.ID{"bravo"},
		Unchanged: []project.ID{"alpha"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reconcile() = %#v, want %#v", got, want)
	}
	assertNoProjectEvent(t, sub.C())

	alpha, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want true")
	}
	if !alpha.Running() {
		t.Fatal("alpha Running() = false after failed bravo start, want true")
	}
	bravo, ok := manager.Registry().Get("bravo")
	if !ok {
		t.Fatal("Registry().Get(bravo) ok = false, want failed project retained")
	}
	if bravo.Running() {
		t.Fatal("bravo Running() = true after failed start, want false")
	}

	gotLogs := logs.String()
	for _, want := range []string{"project startup failed", "bravo", "provision failed"} {
		if !strings.Contains(gotLogs, want) {
			t.Fatalf("logs missing %q:\n%s", want, gotLogs)
		}
	}
}

func TestManagerReconcilePropagatesAddedProjectContextCancellation(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(4))
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
		Startup:  project.StartupConfig{MaxSpawnPerSecond: 1},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			return newManagerTestProject(t, cfg, events)
		},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		for _, item := range manager.Registry().List() {
			if !item.Running() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := item.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
				cancel()
				t.Fatalf("Stop(%s) error = %v", item.ID(), err)
			}
			cancel()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = manager.Reconcile(ctx, project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
		Startup: project.StartupConfig{MaxSpawnPerSecond: 1},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error = %v, want %v", err, context.Canceled)
	}
	if _, ok := manager.Registry().Get("bravo"); ok {
		t.Fatal("Registry().Get(bravo) ok = true after canceled reconcile, want false")
	}
}

func TestManagerReconcileClosesPreparedProjectWhenLaterCreateFails(t *testing.T) {
	t.Parallel()

	factoryErr := errors.New("invalid workflow")
	preparedConnector := newCloseTrackingConnector()
	manager, err := project.NewManager(project.ManagerConfig{}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			if cfg.ID == "bravo" {
				return nil, factoryErr
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: preparedConnector,
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

	_, err = manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, factoryErr)
	}
	assertConnectorClosed(t, preparedConnector)
}

func TestManagerReconcileChangesProjectsWhenRuntimeCredentialVersionChanges(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Projects:                 []globalconfig.Project{{ID: "alpha", Weight: 1}},
		RuntimeCredentialVersion: "old",
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
	drainProjectEvents(t, sub.C(), 1)

	got, err := manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects:                 []globalconfig.Project{{ID: "alpha", Weight: 1}},
		RuntimeCredentialVersion: "new",
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	want := project.ReconcileResult{Changed: []project.ID{"alpha"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reconcile() = %#v, want %#v", got, want)
	}
	assertProjectEvents(t, sub.C(), []project.Event{
		{ProjectID: "alpha", Kind: project.EventStopped},
		{ProjectID: "alpha", Kind: project.EventStarted},
	})
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
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 1},
	})
}

func TestManagerReconcileKeepsRegistryWhenNewProjectFactoryReturnsNil(t *testing.T) {
	t.Parallel()

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
				return nil, nil //nolint:nilnil // Test verifies reconcile keeps the registry when a new project factory returns nil.
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
	if !errors.Is(err, project.ErrMissingProject) {
		t.Fatalf("Reconcile() error = %v, want %v", err, project.ErrMissingProject)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 1},
	})
}

func TestManagerReconcileKeepsChangedProjectWhenReplacementCannotBeCreated(t *testing.T) {
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
			if cfg.ID == "alpha" && cfg.Weight == 2 {
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
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 2}},
	})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("Reconcile() error = %v, want %v", err, factoryErr)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
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

func TestManagerReconcileStopsChangedProjectBeforeStartingReplacement(t *testing.T) {
	t.Parallel()

	oldStillRunningErr := errors.New("old project still running")
	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	var manager *project.Manager
	manager, err = project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 1}},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			if cfg.ID == "alpha" && cfg.Weight == 2 {
				return project.New(project.Config{
					Project:  cfg,
					Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
				}, project.Dependencies{
					Connector: provisioningConnector{provision: func(context.Context) error {
						current, ok := manager.Registry().Get("alpha")
						if !ok {
							return project.ErrProjectNotFound
						}
						if current.Running() {
							return oldStillRunningErr
						}
						return nil
					}},
					Events: events,
					Runner: blockingRunner{},
				})
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

	got, err := manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 2}},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	want := project.ReconcileResult{Changed: []project.ID{"alpha"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Reconcile() = %#v, want %#v", got, want)
	}
	assertProjectEvents(t, sub.C(), []project.Event{
		{ProjectID: "alpha", Kind: project.EventStopped},
		{ProjectID: "alpha", Kind: project.EventStarted},
	})
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 2},
	})
}

func TestManagerReconcileKeepsChangedProjectWhenReplacementProvisionFails(t *testing.T) {
	t.Parallel()

	provisionErr := errors.New("provision failed")
	replacementConnector := newCloseTrackingProvisioningConnector()
	replacementConnector.provision = func(context.Context) error {
		return provisionErr
	}
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
			var projectConnector connector.Connector = provisioningConnector{}
			if cfg.ID == "alpha" && cfg.Weight == 2 {
				projectConnector = replacementConnector
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
			}, project.Dependencies{
				Connector: projectConnector,
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
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
		"alpha": {ID: "alpha", Weight: 1},
	})

	got, ok := manager.Registry().Get("alpha")
	if !ok {
		t.Fatal("Registry().Get(alpha) ok = false, want true")
	}
	if !got.Running() {
		t.Fatal("alpha Running() = false, want true")
	}
	assertConnectorClosed(t, replacementConnector.closeTrackingConnector)
}

func TestManagerReconcileKeepsChangedProjectWhenReplacementStartFails(t *testing.T) {
	t.Parallel()

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
			if cfg.ID == "alpha" && cfg.Weight == 2 {
				return newStoppedManagerTestProject(t, cfg)
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
		Projects: []globalconfig.Project{{ID: "alpha", Weight: 2}},
	})
	if !errors.Is(err, project.ErrProjectStopped) {
		t.Fatalf("Reconcile() error = %v, want %v", err, project.ErrProjectStopped)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
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

func TestManagerReconcileKeepsRegistryWhenAddedProjectStartFailsAfterRemoval(t *testing.T) {
	t.Parallel()

	events := hub.New[project.Event](hub.WithBuffer(8))
	sub, err := events.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "charlie", Weight: 1},
		},
	}, project.ManagerDependencies{
		Events: events,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			if cfg.ID == "bravo" {
				return newStoppedManagerTestProject(t, cfg)
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
	drainProjectEvents(t, sub.C(), 2)

	_, err = manager.Reconcile(context.Background(), project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "charlie", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	})
	if !errors.Is(err, project.ErrProjectStopped) {
		t.Fatalf("Reconcile() error = %v, want %v", err, project.ErrProjectStopped)
	}
	assertNoProjectEvent(t, sub.C())
	assertManagerProjectConfigs(t, manager, map[project.ID]globalconfig.Project{
		"alpha":   {ID: "alpha", Weight: 1},
		"charlie": {ID: "charlie", Weight: 1},
	})
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
	assertStartedProjects(t, sub.C(), []project.ID{"alpha", "bravo", "charlie"})

	for _, id := range []project.ID{"alpha", "bravo", "charlie"} {
		got, ok := manager.Registry().Get(id)
		if !ok {
			t.Fatalf("Registry().Get(%q) ok = false, want true", id)
		}
		if got.Scheduler() != global {
			t.Fatalf("project %q scheduler is not the shared global scheduler", id)
		}
	}

	slots := requestProjectSlots(t, manager, []project.ID{"alpha", "bravo"})
	if _, err := requestProjectSlot(manager, "charlie"); !errors.Is(err, scheduler.ErrNoSlots) {
		t.Fatalf("charlie RequestSlot() error = %v, want ErrNoSlots", err)
	}
	releaseProjectSlot(t, global, slots[0])
	charlie, err := requestProjectSlot(manager, "charlie")
	if err != nil {
		t.Fatalf("charlie RequestSlot() after release error = %v", err)
	}
	releaseProjectSlot(t, global, slots[1])
	releaseProjectSlot(t, global, charlie)

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

func TestManagerProjectsShareGlobalDispatchCap(t *testing.T) {
	t.Parallel()

	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	runners := map[project.ID]*projectBlockingRunner{
		"alpha": newProjectBlockingRunner(),
		"bravo": newProjectBlockingRunner(),
	}
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
	}, project.ManagerDependencies{
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			runner := runners[project.ID(cfg.ID)]
			if runner == nil {
				t.Fatalf("runner for project %q = nil, want runner", cfg.ID)
				return nil, project.ErrMissingProject
			}
			workflowCfg := workflowConfigWithMemoryIssue("issue-" + cfg.ID)
			workflowCfg.Agent.MaxConcurrentAgents = 2
			workflowCfg.Tracker.Issues[0].State = "Todo"
			workflowCfg.Tracker.Issues[0].Title = "Run " + cfg.ID
			workflowCfg.Tracker.Issues[0].AssignedToWorker = true
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg, Prompt: "Run test issue."},
			}, project.Dependencies{
				Runner:             runner,
				GlobalDispatchGate: globalGate,
			})
		},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		for _, item := range manager.Registry().List() {
			if !item.Running() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if err := item.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
				cancel()
				t.Fatalf("Stop(%s) error = %v", item.ID(), err)
			}
			cancel()
		}
	})

	alphaRunner := runners["alpha"]
	if alphaRunner == nil {
		t.Fatal("runner for project alpha = nil, want runner")
		return
	}
	bravoRunner := runners["bravo"]
	if bravoRunner == nil {
		t.Fatal("runner for project bravo = nil, want runner")
		return
	}

	first := receiveFirstProjectRun(t, alphaRunner.started, bravoRunner.started)
	var blocked <-chan orchestrator.RunRequest
	if first == "alpha" {
		blocked = bravoRunner.started
	} else {
		blocked = alphaRunner.started
	}
	select {
	case request := <-blocked:
		t.Fatalf("unexpected second project dispatch while global slot is full = %#v", request)
	default:
	}

	if got := runningProjects(t, manager); got != 1 {
		t.Fatalf("combined running agents = %d, want 1", got)
	}
}

func TestManagerConfigFromGlobal(t *testing.T) {
	t.Parallel()

	cfg := globalconfig.Config{
		Global: globalconfig.Settings{
			Identity: globalconfig.Identity{
				Name:        "release-captain",
				GitHubLogin: "detent-bot",
			},
			Knowledge: workflowconfig.Knowledge{
				Enabled: true,
				Sources: []workflowconfig.KnowledgeSource{{
					Name: "Global",
					Path: "/shared/global.md",
				}},
			},
			Startup: map[string]any{
				"jitter_seconds":        3,
				"max_spawn_per_second":  4,
				"max_concurrent_starts": 2,
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
	if got.Startup.MaxConcurrentStarts != 2 {
		t.Fatalf("Startup.MaxConcurrentStarts = %d, want 2", got.Startup.MaxConcurrentStarts)
	}
	if got.Identity.Name != "release-captain" {
		t.Fatalf("Identity.Name = %q, want release-captain", got.Identity.Name)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "alpha" {
		t.Fatalf("Projects = %#v, want alpha", got.Projects)
	}
	if got.Projects[0].Identity.GitHubLogin != "detent-bot" {
		t.Fatalf("Projects[0].Identity.GitHubLogin = %q, want detent-bot", got.Projects[0].Identity.GitHubLogin)
	}
	if len(got.Projects[0].GlobalKnowledge.Sources) != 1 || got.Projects[0].GlobalKnowledge.Sources[0].Name != "Global" {
		t.Fatalf("Projects[0].GlobalKnowledge = %#v, want global source", got.Projects[0].GlobalKnowledge)
	}
}

func receiveProvisionAttempt(t *testing.T, attempts <-chan int) int {
	t.Helper()

	select {
	case attempt := <-attempts:
		return attempt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for connector provisioning attempt")
		return 0
	}
}

func receiveRetryDelay(t *testing.T, delays <-chan time.Duration) time.Duration {
	t.Helper()

	select {
	case delay := <-delays:
		return delay
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for connector retry delay")
		return 0
	}
}

func waitForProjectRunning(t *testing.T, trackedProject *project.Project) {
	t.Helper()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if trackedProject.Running() {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("timed out waiting for retried project to run")
		}
	}
}

type blockingStartupConnector struct {
	provisioningConnector
	closeTracker   *closeTrackingConnector
	refreshStarted chan struct{}
	releaseRefresh <-chan struct{}
	startOnce      sync.Once
}

func (c *blockingStartupConnector) Name() string {
	return "blocking-startup"
}

func (c *blockingStartupConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.startOnce.Do(func() {
		close(c.refreshStarted)
	})
	<-c.releaseRefresh
	return nil, nil
}

func (c *blockingStartupConnector) Close() error {
	return c.closeTracker.Close()
}

type blockingStartupManager struct {
	manager        *project.Manager
	alphaConnector *blockingStartupConnector
	refreshStarted <-chan struct{}
	releaseRefresh chan struct{}
}

func newBlockingStartupManager(t *testing.T, rollbackTimeout time.Duration) blockingStartupManager {
	t.Helper()

	events := hub.New[project.Event](hub.WithBuffer(4))
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	alphaConnector := &blockingStartupConnector{
		provisioningConnector: provisioningConnector{},
		closeTracker:          newCloseTrackingConnector(),
		refreshStarted:        refreshStarted,
		releaseRefresh:        releaseRefresh,
	}
	manager, err := project.NewManager(project.ManagerConfig{
		Projects: []globalconfig.Project{
			{ID: "alpha", Weight: 1},
			{ID: "bravo", Weight: 1},
		},
		Startup: project.StartupConfig{MaxConcurrentStarts: 2},
	}, project.ManagerDependencies{
		Events:                 events,
		StartupRollbackTimeout: rollbackTimeout,
		ProjectFactory: func(cfg globalconfig.Project) (*project.Project, error) {
			workflowCfg := workflowConfig("memory")
			workflowCfg.Tracker.AutoProvision = true
			var currentConnector connector.Connector = provisioningConnector{
				provision: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}
			if cfg.ID == "alpha" {
				currentConnector = alphaConnector
			}
			return project.New(project.Config{
				Project:  cfg,
				Workflow: workflowconfig.Workflow{Config: workflowCfg},
			}, project.Dependencies{
				Connector: currentConnector,
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

	return blockingStartupManager{
		manager:        manager,
		alphaConnector: alphaConnector,
		refreshStarted: refreshStarted,
		releaseRefresh: releaseRefresh,
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

func newStoppedManagerTestProject(t *testing.T, cfg globalconfig.Project) (*project.Project, error) {
	t.Helper()

	if cfg.Weight == 0 {
		cfg.Weight = 1
	}
	got, err := project.New(project.Config{
		Project:  cfg,
		Workflow: workflowconfig.Workflow{Config: workflowConfig("memory")},
	}, project.Dependencies{
		Events: hub.New[project.Event](hub.WithBuffer(4)),
		Runner: blockingRunner{},
	})
	if err != nil {
		return nil, err
	}
	if err := got.Start(context.Background()); err != nil {
		t.Fatalf("replacement Start() error = %v", err)
	}
	if err := got.Stop(context.Background()); err != nil {
		t.Fatalf("replacement Stop() error = %v", err)
	}
	return got, nil
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

func assertManagerProjectConfigs(t *testing.T, manager *project.Manager, want map[project.ID]globalconfig.Project) {
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

func assertStartedProjects(t *testing.T, ch <-chan project.Event, want []project.ID) {
	t.Helper()

	got := make([]project.ID, 0, len(want))
	for range want {
		event := receiveEvent(t, ch)
		if event.Kind != project.EventStarted {
			t.Fatalf("event = %#v, want project started", event)
		}
		got = append(got, event.ProjectID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("started projects = %v, want %v", got, want)
	}
}

func requestProjectSlots(t *testing.T, manager *project.Manager, ids []project.ID) []scheduler.Slot {
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

func requestProjectSlot(manager *project.Manager, id project.ID) (scheduler.Slot, error) {
	got, ok := manager.Registry().Get(id)
	if !ok {
		return scheduler.Slot{}, project.ErrProjectNotFound
	}
	return got.Scheduler().RequestSlot(context.Background(), scheduler.SlotRequest{
		State: "Todo",
		Host:  string(id),
	})
}

func receiveFirstProjectRun(
	t *testing.T,
	alpha <-chan orchestrator.RunRequest,
	bravo <-chan orchestrator.RunRequest,
) project.ID {
	t.Helper()

	select {
	case <-alpha:
		return "alpha"
	case <-bravo:
		return "bravo"
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first project dispatch")
	}
	return ""
}

func receiveRunRequest(t *testing.T, requests <-chan orchestrator.RunRequest) orchestrator.RunRequest {
	t.Helper()

	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner request")
	}
	return orchestrator.RunRequest{}
}

func runningProjects(t *testing.T, manager *project.Manager) int {
	t.Helper()

	total := 0
	for _, item := range manager.Registry().List() {
		state, err := item.Orchestrator().State(context.Background())
		if err != nil {
			t.Fatalf("State(%s) error = %v", item.ID(), err)
		}
		total += len(state.Running)
	}
	return total
}

func releaseProjectSlot(t *testing.T, global scheduler.GlobalScheduler, slot scheduler.Slot) {
	t.Helper()

	if err := global.ReleaseSlot(slot); err != nil {
		t.Fatalf("ReleaseSlot() error = %v", err)
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
