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

var ErrNoPendingUpdate = errors.New("no Detent update is pending")

type Updater interface {
	Check(context.Context) (Status, error)
	Apply(context.Context, ApplyOptions) (Status, error)
}

type AutoStatus struct {
	Enabled            bool
	AutoApplyEnabled   bool
	CheckInterval      time.Duration
	State              string
	LastCheckAt        *time.Time
	LastAppliedVersion string
	NextCheckAt        *time.Time
	AvailableVersion   string
	LastError          string
}

type SchedulerConfig struct {
	Enabled            bool
	AutoApplyEnabled   bool
	CheckInterval      time.Duration
	IdlePollInterval   time.Duration
	LastAppliedVersion string
	Updater            Updater
	IsIdle             func(context.Context) bool
	RequestRestart     func(string) bool
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
	if cfg.Enabled && cfg.AutoApplyEnabled && cfg.IsIdle == nil {
		return nil, errors.New("runtime idle check is required when automatic update apply is enabled")
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
	state := "disabled"
	if cfg.Enabled {
		state = "scheduled"
	}
	return &Scheduler{
		cfg: cfg,
		status: AutoStatus{
			Enabled:            cfg.Enabled,
			AutoApplyEnabled:   cfg.AutoApplyEnabled,
			CheckInterval:      cfg.CheckInterval,
			State:              state,
			LastAppliedVersion: strings.TrimSpace(cfg.LastAppliedVersion),
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
		if s.pendingIdle() {
			if !s.cfg.Wait(ctx, min(s.cfg.IdlePollInterval, s.cfg.CheckInterval)) {
				return
			}
			if _, err := s.applyWhenIdle(ctx); err != nil && ctx.Err() == nil {
				s.cfg.Logger.Warn("automatic update apply failed", "error", err)
			}
			continue
		}
		delay := s.cfg.NextDelay(s.cfg.CheckInterval)
		if delay <= 0 || delay > s.cfg.CheckInterval {
			delay = s.cfg.CheckInterval
		}
		next := s.cfg.Now().Add(delay)
		s.updateStatus(func(status *AutoStatus) {
			status.State = "scheduled"
			status.NextCheckAt = &next
		})
		if !s.cfg.Wait(ctx, delay) {
			return
		}
		if _, err := s.CheckNow(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("automatic update check failed", "error", err)
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
		s.recordFailure(checkedAt, err)
		return checked, fmt.Errorf("check for Detent update: %w", err)
	}
	if !checked.UpdateAvailable {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "up_to_date"
			status.LastCheckAt = &checkedAt
			status.AvailableVersion = ""
		})
		s.cfg.Logger.Info("automatic update check completed", "current_version", checked.CurrentVersion, "update_available", false)
		return checked, nil
	}

	s.updateStatus(func(status *AutoStatus) {
		status.State = "available"
		status.LastCheckAt = &checkedAt
		status.AvailableVersion = checked.LatestVersion
	})
	s.cfg.Logger.Info("Detent update available", "current_version", checked.CurrentVersion, "latest_version", checked.LatestVersion, "auto_apply_enabled", s.cfg.AutoApplyEnabled)
	if !s.cfg.AutoApplyEnabled {
		return checked, nil
	}
	if checked.InstallSource != InstallSourceRelease {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "available_manual_apply"
		})
		s.cfg.Logger.Warn("automatic update apply skipped for non-release install", "install_source", checked.InstallSource, "latest_version", checked.LatestVersion)
		return checked, nil
	}
	if !s.cfg.IsIdle(ctx) {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "pending_idle"
		})
		s.cfg.Logger.Info("automatic update apply deferred until runtime is idle", "version", checked.LatestVersion)
		return checked, nil
	}

	return s.applyLocked(ctx)
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
	return s.applyLocked(ctx)
}

func (s *Scheduler) applyWhenIdle(ctx context.Context) (Status, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if !s.pendingIdle() || !s.cfg.IsIdle(ctx) {
		return Status{}, nil
	}
	return s.applyLocked(ctx)
}

func (s *Scheduler) applyLocked(ctx context.Context) (Status, error) {
	s.updateStatus(func(status *AutoStatus) {
		status.State = "applying"
		status.LastError = ""
	})

	applied, err := s.cfg.Updater.Apply(ctx, ApplyOptions{
		AssumeYes: true,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		s.recordFailure(s.cfg.Now(), err)
		return applied, fmt.Errorf("apply Detent update: %w", err)
	}
	if applied.Action != ActionUpdated {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "available"
			status.LastError = strings.TrimSpace(applied.Message)
		})
		return applied, nil
	}

	s.updateStatus(func(status *AutoStatus) {
		status.State = "applied"
		status.LastAppliedVersion = applied.LatestVersion
		status.AvailableVersion = ""
		status.LastError = ""
	})
	restartRequested := s.cfg.RequestRestart != nil && s.cfg.RequestRestart(applied.Binary)
	if !restartRequested {
		s.updateStatus(func(status *AutoStatus) {
			status.State = "applied_restart_deferred"
		})
		s.cfg.Logger.Warn("Detent update applied; restart deferred because shutdown is already in progress or restart is unavailable", "version", applied.LatestVersion)
		return applied, nil
	}
	s.updateStatus(func(status *AutoStatus) {
		status.State = "restart_requested"
	})
	s.cfg.Logger.Info("Detent update applied; graceful restart requested", "version", applied.LatestVersion)
	return applied, nil
}

func (s *Scheduler) pendingIdle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status.State == "pending_idle" && strings.TrimSpace(s.status.AvailableVersion) != ""
}

func (s *Scheduler) Status() AutoStatus {
	if s == nil {
		return AutoStatus{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneAutoStatus(s.status)
}

func (s *Scheduler) recordFailure(checkedAt time.Time, err error) {
	s.updateStatus(func(status *AutoStatus) {
		status.State = "failed"
		status.LastCheckAt = &checkedAt
		status.LastError = err.Error()
	})
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
	return status
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
