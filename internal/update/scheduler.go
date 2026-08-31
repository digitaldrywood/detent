package update

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
)

const defaultJitterFraction = 10

const defaultIdlePollInterval = 5 * time.Second

const defaultMaxDeferral = 6 * time.Hour

var ErrNoPendingUpdate = errors.New("no Detent update is pending")

type Updater interface {
	Check(context.Context) (Status, error)
	Apply(context.Context, ApplyOptions) (Status, error)
}

type AutoStatus struct {
	Enabled            bool
	AutoApplyEnabled   bool
	CheckInterval      time.Duration
	MaxDeferral        time.Duration
	State              string
	LastCheckAt        *time.Time
	LastAppliedVersion string
	NextCheckAt        *time.Time
	AvailableVersion   string
	PendingSince       *time.Time
	Critical           bool
	LastError          string
}

type SchedulerConfig struct {
	Enabled            bool
	AutoApplyEnabled   bool
	CheckInterval      time.Duration
	IdlePollInterval   time.Duration
	MaxDeferral        time.Duration
	LastAppliedVersion string
	StatePath          string
	Updater            Updater
	ReserveIdle        func(context.Context) (func(), bool)
	ReserveDrain       func(context.Context) (func(), error)
	RequestRestart     func(string) bool
	ApplyOptions       ApplyOptions
	Logger             *slog.Logger
	Now                func() time.Time
	NextDelay          func(time.Duration) time.Duration
	Wait               func(context.Context, time.Duration) bool
}

type Scheduler struct {
	cfg         SchedulerConfig
	operationMu sync.Mutex
	mu          sync.RWMutex
	status      AutoStatus
}

func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.CheckInterval <= 0 {
		return nil, errors.New("update check interval must be positive")
	}
	if cfg.Enabled && cfg.Updater == nil {
		return nil, errors.New("update checker is required when automatic checks are enabled")
	}
	if cfg.Enabled && cfg.AutoApplyEnabled && cfg.ReserveIdle == nil {
		return nil, errors.New("runtime idle reservation is required when automatic update apply is enabled")
	}
	if cfg.MaxDeferral < 0 {
		return nil, errors.New("update maximum deferral must not be negative")
	}
	if cfg.Enabled && cfg.AutoApplyEnabled && cfg.ReserveDrain == nil {
		return nil, errors.New("runtime drain reservation is required when automatic update apply is enabled")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NextDelay == nil {
		cfg.NextDelay = jitteredDelay
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForUpdateCheck
	}
	if cfg.IdlePollInterval <= 0 {
		cfg.IdlePollInterval = defaultIdlePollInterval
	}
	if cfg.AutoApplyEnabled && cfg.MaxDeferral == 0 {
		cfg.MaxDeferral = defaultMaxDeferral
	}
	loadedState, stateFound, err := loadSchedulerState(cfg.StatePath)
	if err != nil {
		cfg.Logger.Warn("load automatic update state failed", "path", cfg.StatePath, "error", err)
	}
	var lastCheckAt *time.Time
	if stateFound {
		lastCheckAt = &loadedState.LastCheckAt
	}
	state := "disabled"
	if cfg.Enabled {
		state = "scheduled"
	}
	var pendingSince *time.Time
	availableVersion := ""
	critical := false
	if cfg.Enabled && cfg.AutoApplyEnabled && stateFound && loadedState.PendingSince != nil {
		state = "pending_idle"
		pendingSince = loadedState.PendingSince
		availableVersion = loadedState.AvailableVersion
		critical = loadedState.Critical
	}
	return &Scheduler{
		cfg: cfg,
		status: AutoStatus{
			Enabled:            cfg.Enabled,
			AutoApplyEnabled:   cfg.AutoApplyEnabled,
			CheckInterval:      cfg.CheckInterval,
			MaxDeferral:        cfg.MaxDeferral,
			State:              state,
			LastCheckAt:        lastCheckAt,
			LastAppliedVersion: strings.TrimSpace(cfg.LastAppliedVersion),
			AvailableVersion:   availableVersion,
			PendingSince:       pendingSince,
			Critical:           critical,
		},
	}, nil
}

func (s *Scheduler) Run(ctx context.Context) {
	if s == nil || !s.cfg.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		checkDelay, next := s.nextCheck()
		pendingDelay, pending := s.nextPendingAction()
		delay := checkDelay
		if pending && pendingDelay < delay {
			delay = pendingDelay
		}
		s.updateStatus(func(status *AutoStatus) {
			if !pending {
				status.State = "scheduled"
			}
			status.NextCheckAt = &next
		})
		if delay > 0 && !s.cfg.Wait(ctx, delay) {
			return
		}
		if checkDelay <= delay {
			if _, err := s.CheckNow(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Logger.Warn("automatic update check failed", "error", err)
			}
		}
		if pending && pendingDelay <= delay && s.pendingIdle() {
			if _, err := s.applyWhenIdle(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Logger.Warn("automatic update apply failed", "error", err)
			}
		}
	}
}

func (s *Scheduler) CheckNow(ctx context.Context) (Status, error) {
	if s == nil || !s.cfg.Enabled {
		return Status{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	s.updateStatus(func(status *AutoStatus) {
		status.State = "checking"
		status.NextCheckAt = nil
		status.LastError = ""
	})

	checked, err := s.cfg.Updater.Check(ctx)
	checkedAt := s.cfg.Now()
	if err != nil {
		if ctx.Err() != nil {
			s.updateStatus(func(status *AutoStatus) {
				status.State = "failed"
				status.LastError = err.Error()
			})
			return checked, fmt.Errorf("check for Detent update: %w", err)
		}
		persistErr := s.recordCheckFailure(checkedAt, err)
		return checked, fmt.Errorf("check for Detent update: %w", errors.Join(err, persistErr))
	}
	if !checked.UpdateAvailable {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "up_to_date"
			status.LastCheckAt = &checkedAt
			status.AvailableVersion = ""
			clearPending(status)
		})
		persistErr := s.persistLastCheck(checkedAt)
		s.cfg.Logger.Info("automatic update check completed", "current_version", checked.CurrentVersion, "update_available", false)
		return checked, persistErr
	}

	s.updateStatus(func(status *AutoStatus) {
		status.State = "available"
		status.LastCheckAt = &checkedAt
		status.AvailableVersion = checked.LatestVersion
		status.Critical = checked.Critical
	})
	s.cfg.Logger.Info("Detent update available", "current_version", checked.CurrentVersion, "latest_version", checked.LatestVersion, "auto_apply_enabled", s.cfg.AutoApplyEnabled)
	if !s.cfg.AutoApplyEnabled {
		s.updateStatus(clearPending)
		return checked, s.persistLastCheck(checkedAt)
	}
	if checked.InstallSource != InstallSourceRelease {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "available_manual_apply"
			clearPending(status)
		})
		s.cfg.Logger.Warn("automatic update apply skipped for non-release install", "install_source", checked.InstallSource, "latest_version", checked.LatestVersion)
		return checked, s.persistLastCheck(checkedAt)
	}
	if checked.Critical {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "pending_idle"
			setPendingSince(status, checkedAt)
		})
		persistErr := s.persistLastCheck(checkedAt)
		s.cfg.Logger.Warn("critical Detent update bypassing idle wait", "version", checked.LatestVersion)
		applied, applyErr := s.drainAndApplyLocked(ctx)
		return applied, errors.Join(persistErr, applyErr)
	}
	releaseIdle, idle := s.cfg.ReserveIdle(ctx)
	if !idle {
		if releaseIdle != nil {
			releaseIdle()
		}
		s.updateStatus(func(status *AutoStatus) {
			status.State = "pending_idle"
			setPendingSince(status, checkedAt)
		})
		persistErr := s.persistLastCheck(checkedAt)
		s.cfg.Logger.Info("automatic update apply deferred until runtime is idle", "version", checked.LatestVersion)
		return checked, persistErr
	}

	return s.applyLocked(ctx, releaseIdle)
}

func (s *Scheduler) ApplyPending(ctx context.Context) (Status, error) {
	if s == nil || !s.cfg.Enabled {
		return Status{}, ErrNoPendingUpdate
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if !s.pendingIdle() {
		return Status{}, ErrNoPendingUpdate
	}
	return s.applyLocked(ctx, nil)
}

func (s *Scheduler) applyWhenIdle(ctx context.Context) (Status, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if !s.pendingIdle() {
		return Status{}, nil
	}
	if s.Status().Critical || s.deferralExpired() {
		return s.drainAndApplyLocked(ctx)
	}
	releaseIdle, idle := s.cfg.ReserveIdle(ctx)
	if !idle {
		if releaseIdle != nil {
			releaseIdle()
		}
		return Status{}, nil
	}
	return s.applyLocked(ctx, releaseIdle)
}

func (s *Scheduler) drainAndApplyLocked(ctx context.Context) (Status, error) {
	s.updateStatus(func(status *AutoStatus) {
		status.State = "draining"
		status.LastError = ""
	})
	releaseDrain, err := s.cfg.ReserveDrain(ctx)
	if err != nil {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "pending_idle"
			status.LastError = err.Error()
		})
		return Status{}, fmt.Errorf("drain runtime for automatic update: %w", err)
	}
	return s.applyLocked(ctx, releaseDrain)
}

func (s *Scheduler) applyLocked(ctx context.Context, releaseIdle func()) (Status, error) {
	keepIdleReserved := false
	defer func() {
		if !keepIdleReserved && releaseIdle != nil {
			releaseIdle()
		}
	}()
	s.updateStatus(func(status *AutoStatus) {
		status.State = "applying"
		status.LastError = ""
	})

	applyOptions := s.cfg.ApplyOptions
	applyOptions.AssumeYes = true
	applyOptions.Stdout = io.Discard
	applyOptions.Stderr = io.Discard
	applied, err := s.cfg.Updater.Apply(ctx, applyOptions)
	if err != nil {
		persistErr := s.recordFailure(s.cfg.Now(), err)
		return applied, fmt.Errorf("apply Detent update: %w", errors.Join(err, persistErr))
	}
	if applied.Action != ActionUpdated {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "available"
			status.LastError = strings.TrimSpace(applied.Message)
			clearPending(status)
		})
		return applied, s.persistCurrentState()
	}

	s.updateStatus(func(status *AutoStatus) {
		status.State = "applied"
		status.LastAppliedVersion = applied.LatestVersion
		status.AvailableVersion = ""
		status.LastError = ""
		clearPending(status)
	})
	persistErr := s.persistCurrentState()
	restartRequested := s.cfg.RequestRestart != nil && s.cfg.RequestRestart(applied.Binary)
	if !restartRequested {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "applied_restart_deferred"
		})
		s.cfg.Logger.Warn("Detent update applied; restart deferred because shutdown is already in progress or restart is unavailable", "version", applied.LatestVersion)
		return applied, persistErr
	}
	s.updateStatus(func(status *AutoStatus) {
		status.State = "restart_requested"
	})
	keepIdleReserved = true
	s.cfg.Logger.Info("Detent update applied; graceful restart requested", "version", applied.LatestVersion)
	return applied, persistErr
}

func (s *Scheduler) pendingIdle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status.State == "pending_idle" && strings.TrimSpace(s.status.AvailableVersion) != ""
}

func (s *Scheduler) nextPendingAction() (time.Duration, bool) {
	status := s.Status()
	if status.State != "pending_idle" || strings.TrimSpace(status.AvailableVersion) == "" {
		return 0, false
	}
	if status.PendingSince == nil || status.MaxDeferral <= 0 {
		return s.cfg.IdlePollInterval, true
	}
	remaining := status.PendingSince.Add(status.MaxDeferral).Sub(s.cfg.Now())
	if remaining <= 0 {
		return 0, true
	}
	return min(s.cfg.IdlePollInterval, remaining), true
}

func (s *Scheduler) deferralExpired() bool {
	status := s.Status()
	return status.State == "pending_idle" &&
		status.PendingSince != nil &&
		status.MaxDeferral > 0 &&
		!s.cfg.Now().Before(status.PendingSince.Add(status.MaxDeferral))
}

func (s *Scheduler) Status() AutoStatus {
	if s == nil {
		return AutoStatus{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAutoStatus(s.status)
}

func (s *Scheduler) recordFailure(checkedAt time.Time, err error) error {
	s.updateStatus(func(status *AutoStatus) {
		status.State = "failed"
		status.LastCheckAt = &checkedAt
		status.LastError = err.Error()
	})
	persistErr := s.persistLastCheck(checkedAt)
	if persistErr != nil {
		s.updateStatus(func(status *AutoStatus) {
			status.LastError = errors.Join(err, persistErr).Error()
		})
	}
	return persistErr
}

func (s *Scheduler) recordCheckFailure(checkedAt time.Time, err error) error {
	s.updateStatus(func(status *AutoStatus) {
		if status.PendingSince != nil && strings.TrimSpace(status.AvailableVersion) != "" {
			status.State = "pending_idle"
		} else {
			status.State = "failed"
		}
		status.LastCheckAt = &checkedAt
		status.LastError = err.Error()
	})
	persistErr := s.persistLastCheck(checkedAt)
	if persistErr != nil {
		s.updateStatus(func(status *AutoStatus) {
			status.LastError = errors.Join(err, persistErr).Error()
		})
	}
	return persistErr
}

func (s *Scheduler) persistLastCheck(checkedAt time.Time) error {
	status := s.Status()
	state := schedulerState{LastCheckAt: checkedAt}
	if status.State == "pending_idle" && status.PendingSince != nil && strings.TrimSpace(status.AvailableVersion) != "" {
		pendingSince := *status.PendingSince
		state.AvailableVersion = status.AvailableVersion
		state.PendingSince = &pendingSince
		state.Critical = status.Critical
	}
	if err := saveSchedulerState(s.cfg.StatePath, state); err != nil {
		wrapped := fmt.Errorf("persist automatic update state: %w", err)
		s.updateStatus(func(status *AutoStatus) {
			status.LastError = wrapped.Error()
		})
		return wrapped
	}
	return nil
}

func (s *Scheduler) persistCurrentState() error {
	status := s.Status()
	if status.LastCheckAt == nil {
		return nil
	}
	return s.persistLastCheck(*status.LastCheckAt)
}

func (s *Scheduler) nextCheck() (time.Duration, time.Time) {
	jittered := s.cfg.NextDelay(s.cfg.CheckInterval)
	if jittered <= 0 || jittered > s.cfg.CheckInterval {
		jittered = s.cfg.CheckInterval
	}
	now := s.cfg.Now()
	status := s.Status()
	if status.LastCheckAt == nil {
		if strings.TrimSpace(s.cfg.StatePath) != "" {
			return 0, now
		}
		return jittered, now.Add(jittered)
	}
	next := status.LastCheckAt.Add(jittered)
	remaining := next.Sub(now)
	if remaining <= 0 {
		return 0, now
	}
	if remaining > s.cfg.CheckInterval {
		return s.cfg.CheckInterval, now.Add(s.cfg.CheckInterval)
	}
	return remaining, next
}

func (s *Scheduler) updateStatus(update func(*AutoStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(&s.status)
}

func cloneAutoStatus(status AutoStatus) AutoStatus {
	if status.LastCheckAt != nil {
		lastCheckAt := *status.LastCheckAt
		status.LastCheckAt = &lastCheckAt
	}
	if status.NextCheckAt != nil {
		nextCheckAt := *status.NextCheckAt
		status.NextCheckAt = &nextCheckAt
	}
	if status.PendingSince != nil {
		pendingSince := *status.PendingSince
		status.PendingSince = &pendingSince
	}
	return status
}

func setPendingSince(status *AutoStatus, pendingSince time.Time) {
	if status.PendingSince == nil {
		status.PendingSince = &pendingSince
	}
}

func clearPending(status *AutoStatus) {
	status.PendingSince = nil
	status.Critical = false
}

func jitteredDelay(interval time.Duration) time.Duration {
	window := interval / defaultJitterFraction
	if window <= 0 {
		return interval
	}
	offset, err := rand.Int(rand.Reader, big.NewInt(int64(window)+1))
	if err != nil {
		return interval
	}
	return interval - time.Duration(offset.Int64())
}

func waitForUpdateCheck(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
