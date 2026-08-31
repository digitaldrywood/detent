package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/project"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const runtimeUpdateDrainPollInterval = 500 * time.Millisecond

var ErrRuntimeUpdateDrainTimeout = errors.New("automatic update runtime drain timed out")

type RestartRequest struct {
	mu     sync.RWMutex
	binary string
}

func NewRestartRequest() *RestartRequest {
	return &RestartRequest{}
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
	cfg BootConfig,
	logger *slog.Logger,
	reserveIdle func(context.Context) (func(), bool),
	reserveDrain func(context.Context) (func(), error),
) (*detentupdate.Scheduler, error) {
	interval := time.Duration(cfg.Global.Update.NormalizedCheckIntervalHours()) * time.Hour
	schedulerConfig := detentupdate.SchedulerConfig{
		Enabled:          cfg.Global.Update.AutoCheckEnabled,
		AutoApplyEnabled: cfg.Global.Update.AutoApplyEnabled,
		CheckInterval:    interval,
		MaxDeferral:      time.Duration(cfg.Global.Update.NormalizedMaxDeferralHours()) * time.Hour,
		ReserveIdle:      reserveIdle,
		ReserveDrain:     reserveDrain,
		Logger:           logger,
		StatePath:        runtimeUpdateStatePath(cfg),
		ApplyOptions: detentupdate.ApplyOptions{
			Preflight:         candidateStartupPreflight(cfg),
			RecoveryStatePath: detentupdate.RecoveryStatePath(cfg.Global.Path),
		},
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
	if !schedulerConfig.Enabled {
		return detentupdate.NewScheduler(schedulerConfig)
	}
	version := runtimeUpdateVersion(cfg)
	schedulerConfig.Updater = newRuntimeUpdater(cfg, executable, version)
	schedulerConfig.RequestRestart = func(binary string) bool {
		if strings.TrimSpace(binary) == "" {
			binary = executable
		}
		return requestUpdateRestart(cfg.Shutdown, cfg.Restart, binary)
	}
	return detentupdate.NewScheduler(schedulerConfig)
}

func runtimeUpdateIdle(ctx context.Context, registry *project.Registry) bool {
	if registry == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	for _, trackedProject := range registry.List() {
		if !trackedProject.Running() {
			continue
		}
		orchestrator := trackedProject.Orchestrator()
		if orchestrator == nil {
			return false
		}
		state, err := orchestrator.State(ctx)
		if err != nil || len(state.Snapshot(now).Running) > 0 {
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
		waitForRuntimeUpdateDrain,
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
		wait = waitForRuntimeUpdateDrain
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

func waitForRuntimeUpdateDrain(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runtimeUpdateDrainTimeout(registry *project.Registry) time.Duration {
	defaultTimeout := max(
		time.Duration(workflowconfig.DefaultMaxSessionDurationMS)*time.Millisecond,
		time.Duration(workflowconfig.DefaultMergeWorkerMaxDurationMS)*time.Millisecond,
		time.Duration(workflowconfig.DefaultMergeFallbackMaxDurationMS)*time.Millisecond,
	)
	if registry == nil {
		return defaultTimeout
	}
	timeout := time.Duration(0)
	for _, trackedProject := range registry.List() {
		agent := trackedProject.Workflow().Config.Agent
		ceilings := []struct {
			configured int
			fallback   int
		}{
			{configured: agent.MaxSessionDurationMS, fallback: workflowconfig.DefaultMaxSessionDurationMS},
			{configured: agent.MergeWorkerMaxDurationMS, fallback: workflowconfig.DefaultMergeWorkerMaxDurationMS},
			{configured: agent.MergeFallbackMaxDurationMS, fallback: workflowconfig.DefaultMergeFallbackMaxDurationMS},
		}
		for _, ceiling := range ceilings {
			configured := ceiling.configured
			if configured <= 0 {
				configured = ceiling.fallback
			}
			timeout = max(timeout, time.Duration(configured)*time.Millisecond)
		}
	}
	if timeout == 0 {
		return defaultTimeout
	}
	return timeout
}

func requestUpdateRestart(controller *ShutdownController, restart *RestartRequest, binary string) bool {
	if controller == nil || restart == nil || strings.TrimSpace(binary) == "" {
		return false
	}
	return controller.RequestDrainIfIdle(func() {
		restart.set(binary)
	})
}
