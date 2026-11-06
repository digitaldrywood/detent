package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func startRemoteRunner(ctx context.Context, cfg BootConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := slog.Default()
	usage := &runner.ProviderUsage{}
	gate, err := buildGlobalDispatchPools(cfg.Global)
	if err != nil {
		return err
	}
	selected := newGlobalConfigState(cfg.Global)
	var manager *project.Manager
	var scheduling orchestrator.SchedulingSource
	applyProjects := func(ctx context.Context, current globalconfig.Config) error {
		if manager == nil {
			return errors.New("runner project manager is unavailable")
		}
		if _, err := manager.Reconcile(ctx, project.ManagerConfigFromGlobal(current)); err != nil {
			return fmt.Errorf("reconcile runner projects: %w", err)
		}
		selected.set(current)
		return nil
	}
	scheduling, err = newHubScheduling(ctx, cfg.Global, cfg.Version, hubSchedulingOptions{
		hubProcessReceipts: true,
		providerUsage:      usage,
		logger:             logger,
		applyProjects:      applyProjects,
		runtimeConfig:      selected.get,
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
			return errors.New("Hub runner receipt transport is unavailable")
		}
		receipts = &hubUpdateReceipts{ctx: ctx, client: transport.Transport(), running: runnerRunningBuild(ctx, cfg.Build, cfg.Version)}
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
			return resolveRunnerProjectGitHubToken(ctx, selected.get(), projectConfig, workflow, runtimeDeps{})
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
	if err := startRunnerProjects(ctx, manager, scheduling); err != nil {
		return err
	}
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
		if heartbeat, ok := scheduling.(interface {
			Heartbeat(context.Context) error
			HeartbeatInterval() time.Duration
		}); ok {
			workers.Go(func() {
				ticker := time.NewTicker(heartbeat.HeartbeatInterval())
				defer ticker.Stop()
				for {
					select {
					case <-runCtx.Done():
						return
					case <-ticker.C:
						if err := heartbeat.Heartbeat(runCtx); err != nil && runCtx.Err() == nil {
							logger.Warn("Hub runner heartbeat failed", "error", err)
						}
					}
				}
			})
		}
		<-runCtx.Done()
		return context.Cause(runCtx)
	})
}

type hubUpdateReceipts struct {
	ctx     context.Context
	client  *api.Client
	running runnerauth.BuildEvidence
}

func (s *hubUpdateReceipts) Load() (*runnerauth.UpdateReceipt, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return s.client.RunnerUpdateReceipt(ctx)
}

func (s *hubUpdateReceipts) Save(receipt *runnerauth.UpdateReceipt) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 10*time.Second)
	defer cancel()
	return s.client.StoreRunnerUpdateReceipt(ctx, receipt, s.running)
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
