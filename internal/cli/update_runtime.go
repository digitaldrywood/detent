package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/cli/remoterunner"
	"github.com/digitaldrywood/detent/internal/runnerauth"

	"github.com/digitaldrywood/detent/internal/project"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const runtimeUpdateDrainPollInterval = 500 * time.Millisecond

var ErrRuntimeUpdateDrainTimeout = errors.New("automatic update runtime drain timed out")

type RestartRequest struct {
	mu           sync.RWMutex
	binary       string
	execute      func(context.Context, string) error
	control      *controllerConnection
	reserve      func() func()
	retire       func()
	artifactRoot string
}

func NewRestartRequest() *RestartRequest {
	return &RestartRequest{}
}

func NewProcessRestartRequest(execute func(string) error) *RestartRequest {
	if execute == nil {
		return &RestartRequest{}
	}
	return &RestartRequest{execute: func(_ context.Context, binary string) error { return execute(binary) }}
}

func (r *RestartRequest) Binary() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.binary
}

func (r *RestartRequest) set(binary string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.binary = strings.TrimSpace(binary)
}

func newRuntimeUpdateScheduler(
	ctx context.Context,
	cfg BootConfig,
	logger *slog.Logger,
	reserveIdle func(context.Context) (func(), bool),
	reserveDrain func(context.Context) (func(), error),
	receiptStores ...detentupdate.ReceiptStore,
) (*detentupdate.Scheduler, error) {
	interval := time.Duration(cfg.Global.Update.NormalizedCheckIntervalHours()) * time.Hour
	if cfg.Global.Client.IdentityFile != "" {
		interval = cfg.Global.Client.HeartbeatInterval()
	}
	schedulerConfig := detentupdate.SchedulerConfig{
		Enabled:          cfg.Global.Update.AutoCheckEnabled,
		AutoApplyEnabled: cfg.Global.Update.AutoApplyEnabled,
		CheckInterval:    interval,
		MaxDeferral:      time.Duration(cfg.Global.Update.NormalizedMaxDeferralHours()) * time.Hour,
		ReserveIdle:      reserveIdle,
		ReserveDrain:     reserveDrain,
		Logger:           logger,
		StatePath:        runtimeUpdateStatePath(cfg),
		RunningBuild:     runnerRunningBuild(ctx, cfg.Build, cfg.Version),
		ApplyOptions: detentupdate.ApplyOptions{
			Preflight:         candidateStartupPreflight(cfg),
			RecoveryStatePath: detentupdate.RecoveryStatePath(cfg.Global.Path),
		},
	}
	if cfg.Global.Client.Configured() {
		schedulerConfig.StatePath = ""
		schedulerConfig.ApplyOptions.RecoveryStatePath = ""
		if len(receiptStores) > 0 {
			schedulerConfig.ReceiptStore = receiptStores[0]
		}
	}
	if cfg.Isolated != nil {
		schedulerConfig.Enabled = false
		schedulerConfig.AutoApplyEnabled = false
		return detentupdate.NewScheduler(schedulerConfig)
	}
	if cfg.Global.Client.IdentityFile != "" {
		schedulerConfig.Enabled = false
		schedulerConfig.AutoApplyEnabled = false
	}
	executable, err := os.Executable()
	if err != nil {
		if !schedulerConfig.Enabled {
			return detentupdate.NewScheduler(schedulerConfig)
		}
		return nil, fmt.Errorf("resolve executable for automatic update checks: %w", err)
	}
	schedulerConfig.LastAppliedVersion = detentupdate.InstalledReleaseVersion(detentupdate.DetectionOptions{
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
	})
	version := runtimeUpdateVersion(cfg)
	schedulerConfig.Updater, err = newRuntimeUpdater(cfg, executable, version)
	if err != nil {
		return nil, err
	}
	if !detentupdate.IsDevelopmentVersion(version) {
		service, ok := schedulerConfig.Updater.(*detentupdate.Service)
		if !ok {
			return nil, errors.New("worker release service is unavailable")
		}
		artifactRoot := filepath.Join(filepath.Dir(runtimeUpdateStatePath(cfg)), "workers")
		if cfg.Restart != nil && cfg.Restart.control != nil {
			artifactRoot = filepath.Join(artifactRoot, fmt.Sprintf("controller-%d", os.Getpid()))
		}
		schedulerConfig.WorkerArtifacts, err = detentupdate.NewWorkerArtifacts(service, artifactRoot)
		if err != nil {
			return nil, err
		}
		if cfg.Restart != nil && cfg.Restart.control != nil {
			cfg.Restart.artifactRoot = artifactRoot
		}
		schedulerConfig.WorkerPreflight = workerProtocolPreflight
	}
	if cfg.Shutdown != nil && cfg.Restart != nil {
		schedulerConfig.RequestRestart = func(binary string) bool {
			if strings.TrimSpace(binary) == "" {
				binary = executable
			}
			accepted, err := requestRuntimeUpdateRestart(ctx, cfg.Shutdown, cfg.Restart, binary, reserveIdle)
			if err != nil {
				restartLogger := logger
				if restartLogger == nil {
					restartLogger = slog.Default()
				}
				restartLogger.Error("restart applied controller update failed", "error", err)
			}
			return accepted
		}
	}
	return detentupdate.NewScheduler(schedulerConfig)
}

func requestRuntimeUpdateRestart(ctx context.Context, controller *ShutdownController, restart *RestartRequest, binary string, reserveIdle func(context.Context) (func(), bool)) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if restart == nil || strings.TrimSpace(binary) == "" {
		return false, nil
	}
	if restart.execute == nil {
		return requestUpdateRestart(controller, restart, binary), nil
	}
	if restart.control != nil {
		if restart.reserve == nil {
			return false, errors.New("controller handoff reservation is unavailable")
		}
		release := restart.reserve()
		accepted, err := controller.executeUpdateRestart(ctx, func(ctx context.Context) error {
			return restart.execute(ctx, binary)
		}, func() {
			restart.control.retired.Store(true)
			if restart.retire != nil {
				restart.retire()
			}
			if !controller.shutdownRequested.Load() {
				controller.updateRestart.Store(true)
				controller.shutdownRequested.Store(true)
				controller.request(ShutdownRequestDrain)
			}
		})
		if !accepted {
			release()
		}
		return accepted, err
	}
	if reserveIdle == nil {
		return false, nil
	}
	release, idle := reserveIdle(ctx)
	if release != nil {
		defer release()
	}
	if !idle {
		return false, nil
	}
	return controller.executeUpdateRestart(ctx, func(ctx context.Context) error { return restart.execute(ctx, binary) }, nil)
}

func runtimeUpdateIdle(ctx context.Context, registry *project.Registry) bool {
	if registry == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orchestrator := trackedProject.Orchestrator()
		if orchestrator == nil {
			return false
		}
		state, err := orchestrator.State(ctx)
		if err != nil || state.UnsettledWork() > 0 {
			return false
		}
	}
	return true
}

type dispatchPauser interface {
	PauseDispatch() func()
}

func runtimeUpdateIdleReservation(ctx context.Context, registry *project.Registry, gate dispatchPauser) (func(), bool) {
	if gate == nil {
		return nil, false
	}
	release := gate.PauseDispatch()
	if !runtimeUpdateIdle(ctx, registry) {
		release()
		return nil, false
	}
	return release, true
}

func runtimeUpdateDrainReservation(ctx context.Context, registry *project.Registry, gate dispatchPauser) (func(), error) {
	if gate == nil {
		return nil, errors.New("runtime dispatch pauser is unavailable")
	}
	release := gate.PauseDispatch()
	err := waitForRuntimeUpdateIdle(
		ctx,
		func(waitCtx context.Context) bool { return runtimeUpdateIdle(waitCtx, registry) },
		runtimeUpdateDrainTimeout(registry),
		time.Now,
		remoterunner.WaitForUpdateDrain,
	)
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func waitForRuntimeUpdateIdle(
	ctx context.Context,
	idle func(context.Context) bool,
	timeout time.Duration,
	now func() time.Time,
	wait func(context.Context, time.Duration) bool,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if idle == nil {
		return errors.New("runtime idle check is unavailable")
	}
	if timeout <= 0 {
		return fmt.Errorf("%w: invalid ceiling %s", ErrRuntimeUpdateDrainTimeout, timeout)
	}
	if now == nil {
		now = time.Now
	}
	if wait == nil {
		wait = remoterunner.WaitForUpdateDrain
	}
	deadline := now().Add(timeout)
	for !idle(ctx) {
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return fmt.Errorf("%w after %s", ErrRuntimeUpdateDrainTimeout, timeout)
		}
		if !wait(ctx, min(runtimeUpdateDrainPollInterval, remaining)) {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("%w after %s", ErrRuntimeUpdateDrainTimeout, timeout)
		}
	}
	return nil
}

func runtimeUpdateDrainTimeout(registry *project.Registry) time.Duration {
	return remoterunner.UpdateDrainTimeout(registry)
}

func requestUpdateRestart(controller *ShutdownController, restart *RestartRequest, binary string) bool {
	if controller == nil || restart == nil || strings.TrimSpace(binary) == "" {
		return false
	}
	return controller.RequestDrainIfIdle(func() {
		restart.set(binary)
		controller.updateRestart.Store(true)
	})
}

func runnerRunningBuild(ctx context.Context, info buildinfo.Info, version string) runnerauth.BuildEvidence {
	return remoterunner.RunningBuild(ctx, info, version)
}

func enrolledUpdateOwner(runtimeCtx context.Context, scheduler *detentupdate.Scheduler, running runnerauth.BuildEvidence) func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
	return func(ctx context.Context, request *runnerauth.UpdateRequest) *runnerauth.UpdateObservation {
		if request != nil && (request.Urgent || request.FollowHub) {
			copy := *request
			go scheduler.EnrolledUpdate(runtimeCtx, running, &copy)
			return scheduler.EnrolledUpdate(ctx, running, nil)
		}
		return scheduler.EnrolledUpdate(ctx, running, request)
	}
}
