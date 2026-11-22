package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/cli/remoterunner"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func startRemoteRunner(ctx context.Context, cfg BootConfig) (result error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := slog.Default()
	usage := &runner.ProviderUsage{}
	var gate *scheduler.PoolRegistry
	var err error
	logLevel := runtimeLogLevelForReload(cfg)
	selected := remoterunner.NewConfigOwner(cfg.Global, func(next globalconfig.Config) error { return applyGlobalRuntimeConfig(gate, logLevel, next) })
	var manager *project.Manager
	var scheduling orchestrator.SchedulingSource
	lifecycle := cfg.lifecycle
	if lifecycle == nil && cfg.Global.Client.IdentityFile != "" {
		lifecycle, err = remoterunner.StartLifecycle(ctx, cfg.Global.Client, cfg.Build, cfg.Version)
		if err != nil {
			return err
		}
	}
	ready := false
	defer func() {
		if lifecycle == nil {
			return
		}
		if !ready {
			lifecycle.ReportStartupFailure(ctx)
			return
		}
		reportCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		reason := "signal"
		if cfg.Shutdown != nil && cfg.Shutdown.updateRestart.Load() {
			reason = "update"
		} else if cfg.Shutdown != nil && cfg.Shutdown.forceRequested.Load() {
			reason = "operator_stop"
		} else if cfg.Shutdown != nil && cfg.Shutdown.shutdownRequested.Load() {
			reason = "drain_complete"
		}
		if err := lifecycle.Report(reportCtx, runnerauth.LifecycleShuttingDown, reason, ""); err != nil {
			logger.Warn("report runner shutdown", "error", err)
		}
	}()
	gate, err = buildGlobalDispatchPools(cfg.Global)
	if err != nil {
		return err
	}
	scheduling, err = newHubScheduling(ctx, cfg.Global, cfg.Version, hubSchedulingOptions{
		hubProcessReceipts: true,
		lifecycle:          lifecycle,
		applyProjectConfiguration: func(ctx context.Context, id string, desired runnerauth.ProjectConfiguration) runnerauth.ProjectConfiguration {
			if manager == nil {
				view := project.MissingConfigurationOwner(desired.ProjectID)
				view.Authority = "hub_configuration"
				return view
			}
			return manager.ApplyHubConfiguration(ctx, id, desired)
		},
		providerUsage: usage,
		logger:        logger,
		applyProjects: selected.ApplyProjects,
		runtimeConfig: selected.Current,
		releasedStopped: func(ctx context.Context, item tracker.NativeWorkItemID, attempt tracker.NativeAttempt) (bool, error) {
			return remoteReleasedStopped(ctx, item, attempt)
		},
	})
	if err != nil {
		return fmt.Errorf("configure Hub runner: %w", err)
	}
	if admission, ok := scheduling.(interface{ SetClaimAdmission(func() (func(), bool)) }); ok {
		admission.SetClaimAdmission(gate.ReserveDispatch)
	}
	var receipts detentupdate.ReceiptStore
	if cfg.Global.Client.IdentityFile != "" {
		transport, ok := scheduling.(interface{ Transport() *api.Client })
		if !ok {
			return errors.New("hub runner receipt transport is unavailable")
		}
		receipts = remoterunner.NewUpdateReceipts(ctx, transport.Transport(), runnerRunningBuild(ctx, cfg.Build, cfg.Version))
	}
	update, err := newRuntimeUpdateScheduler(ctx, cfg, logger,
		func(ctx context.Context) (func(), bool) {
			if manager == nil {
				return nil, false
			}
			return runtimeUpdateIdleReservation(ctx, manager.Registry(), gate)
		}, func(ctx context.Context) (func(), error) {
			if manager == nil {
				return nil, errors.New("runner project manager is unavailable")
			}
			return runtimeUpdateDrainReservation(ctx, manager.Registry(), gate)
		}, receipts)
	if err != nil {
		return fmt.Errorf("configure runner worker artifacts: %w", err)
	}
	if lifecycle != nil {
		update.SetLifecycleReporter(lifecycle.Report)
	}
	if err := update.PrepareWorkers(ctx); err != nil {
		return fmt.Errorf("prepare runner worker artifacts: %w", err)
	}
	if owner, ok := scheduling.(interface {
		SetUpdateOwner(func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation)
	}); ok {
		owner.SetUpdateOwner(func(ctx context.Context, request *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
			return update.EnrolledUpdate(ctx, update.RunningBuild(), request)
		})
	}
	provider := project.Dependencies{
		Scheduling: scheduling, Logger: logger, GlobalDispatchGate: gate,
		GitHubToken:      runtimeGlobalGitHubToken(cfg.Runtime.GitHubToken),
		ConnectorFactory: cfg.ConnectorFactory, Runner: cfg.Runner,
	}
	factory := withRunnerFactoryWithProjectTokens(ctx, provider, nil, nil, serviceapi.Connection{}, nil,
		runnerIsolationPolicy(cfg.Global.Client.IdentityFile),
		func(ctx context.Context, projectConfig globalconfig.Project, workflow workflowconfig.Config) (string, error) {
			return resolveRunnerProjectGitHubToken(ctx, selected.Current(), projectConfig, workflow, runtimeDeps{})
		}, update.WorkerArtifacts(), usage)
	manager, err = project.NewManager(project.ManagerConfigFromGlobal(cfg.Global), project.ManagerDependencies{
		ProjectFactory: factory, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("create runner project manager: %w", err)
	}
	defer func() {
		cancel()
		manager.Wait()
		for _, runtimeProject := range manager.Registry().List() {
			if err := runtimeProject.Close(); err != nil {
				logger.Warn("close runner project failed", "project_id", runtimeProject.ID(), "error", err)
			}
		}
	}()
	syncDispatch := func(projects []globalconfig.Project) { syncGlobalDispatchProjects(gate, projects, manager.Registry()) }
	selected.Attach(manager, syncDispatch)
	syncDispatch(cfg.Global.Projects)
	reloadDone := startGlobalConfigWatcher(ctx, cfg.Global, manager, logger, nil, nil, nil, func(r *globalConfigReloader) {
		r.applyConfiguration = selected.ApplyFile
	})
	defer func() {
		cancel()
		waitGlobalConfigWatcher(reloadDone)
	}()
	if err := startRunnerProjects(ctx, manager, scheduling); err != nil {
		return err
	}
	if lifecycle != nil {
		if err := lifecycle.Report(ctx, runnerauth.LifecycleReady, "", ""); err != nil {
			return err
		}
	}
	ready = true
	return runWithShutdown(ctx, runningShutdownConfig{
		Controller: cfg.Shutdown, Registry: manager.Registry(), Logger: logger,
	}, func(parent context.Context) error {
		runCtx, stop := context.WithCancel(parent)
		var workers sync.WaitGroup
		defer func() {
			stop()
			workers.Wait()
		}()
		workers.Go(func() { update.Run(runCtx) })
		if heartbeat, ok := scheduling.(remoterunner.Heartbeater); ok {
			workers.Go(func() { remoterunner.RunHeartbeats(runCtx, heartbeat, cfg.remoteHeartbeatTicker, logger) })
		}
		<-runCtx.Done()
		return context.Cause(runCtx)
	})
}

func remoteReleasedStopped(_ context.Context, _ tracker.NativeWorkItemID, attempt tracker.NativeAttempt) (bool, error) {
	if attempt.Runtime == nil || attempt.Runtime.WorkerLaunch == nil {
		return false, nil
	}
	launch := attempt.Runtime.WorkerLaunch
	if launch.PID <= 0 || launch.GroupID <= 0 || launch.ProcessStart.IsZero() {
		return false, nil
	}
	observations, err := procgroup.Observe([]procgroup.Identity{{PID: launch.PID, GroupID: launch.GroupID, StartedAt: launch.ProcessStart}})
	if err != nil {
		return false, fmt.Errorf("observe released execution process: %w", err)
	}
	return len(observations) == 1 && observations[0].GroupObserved && !observations[0].Alive && !observations[0].Stale, nil
}
