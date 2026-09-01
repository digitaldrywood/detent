package update

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const startupCrashLoopThreshold = 3

type CrashLoopEvent struct {
	Event         string     `json:"event"`
	Instance      string     `json:"instance,omitempty"`
	Host          string     `json:"host,omitempty"`
	Version       string     `json:"version"`
	FailureCount  int        `json:"failure_count"`
	FirstFailedAt time.Time  `json:"first_failed_at"`
	LastFailedAt  time.Time  `json:"last_failed_at"`
	NextRetryAt   *time.Time `json:"next_retry_at,omitempty"`
	Error         string     `json:"error"`
	StatePath     string     `json:"state_path"`
	Action        string     `json:"action,omitempty"`
}

type StartupRecoveryConfig struct {
	StatePath      string
	CurrentVersion string
	ExecutablePath string
	GOOS           string
	HomeDir        string
	Env            map[string]string
	AutoUpdate     bool
	Updater        Updater
	ApplyOptions   ApplyOptions
	Instance       string
	Host           string
	Logger         *slog.Logger
	Now            func() time.Time
	Wait           func(context.Context, time.Duration) bool
	Notify         func(context.Context, CrashLoopEvent) error
	Rollback       func(context.Context, PendingUpdate) error
	Remove         func(string) error
}

type StartupRecovery struct {
	cfg   StartupRecoveryConfig
	mu    sync.Mutex
	state startupRecoveryState
}

func NewStartupRecovery(cfg StartupRecoveryConfig) (*StartupRecovery, error) {
	cfg.StatePath = strings.TrimSpace(cfg.StatePath)
	if cfg.StatePath == "" {
		return nil, errors.New("startup recovery state path is required")
	}
	cfg.CurrentVersion = strings.TrimSpace(cfg.CurrentVersion)
	if cfg.CurrentVersion == "" {
		return nil, errors.New("startup recovery current version is required")
	}
	cfg.ExecutablePath = strings.TrimSpace(cfg.ExecutablePath)
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForStartupRetry
	}
	if cfg.Remove == nil {
		cfg.Remove = os.Remove
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	state, _, err := loadStartupRecoveryState(cfg.StatePath)
	if err != nil {
		cfg.Logger.Warn("load startup recovery state failed", "path", cfg.StatePath, "error", err)
		state = startupRecoveryState{}
	}
	return &StartupRecovery{cfg: cfg, state: state}, nil
}

func (r *StartupRecovery) MarkHealthy(context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now().UTC()
	if failure := r.state.ActiveFailure; failure != nil && failure.CrashLoop {
		archived := *failure
		r.state.LastCrashLoop = &archived
	}
	if pending := r.state.PendingUpdate; pending != nil {
		switch r.cfg.CurrentVersion {
		case strings.TrimSpace(pending.ToVersion):
			if err := r.removePreviousBinary(pending.PreviousBinaryPath); err != nil {
				return err
			}
			r.state.PendingUpdate = nil
		case strings.TrimSpace(pending.FromVersion):
			if pending.RollbackRequestedAt != nil {
				r.state.LastRollback = &StartupRollback{
					FromVersion:  pending.ToVersion,
					ToVersion:    pending.FromVersion,
					RolledBackAt: now,
				}
			}
			if err := r.removePreviousBinary(pending.PreviousBinaryPath); err != nil {
				return err
			}
			r.state.PendingUpdate = nil
		}
	}
	r.state.ActiveFailure = nil
	r.state.LastHealthyAt = &now
	return saveStartupRecoveryState(r.cfg.StatePath, r.state)
}

func (r *StartupRecovery) HandleFailure(ctx context.Context, cause error) {
	if r == nil || cause == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	failure, pending, shouldUpdate, saveErr := r.recordFailure(cause)
	if saveErr != nil {
		r.cfg.Logger.Error("persist startup failure failed", "path", r.cfg.StatePath, "error", saveErr)
	}
	if pending != nil && failure.Count >= startupCrashLoopThreshold {
		r.rollbackPendingUpdate(ctx, *pending)
	} else if shouldUpdate {
		r.applyRecoveryUpdate(ctx)
	}
	r.signalCrashLoop(ctx)
	r.waitBeforeRetry(ctx)
}

func (r *StartupRecovery) recordFailure(cause error) (StartupFailure, *PendingUpdate, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now().UTC()
	message := strings.TrimSpace(cause.Error())
	signature := startupFailureSignature(r.cfg.CurrentVersion, message)
	failure := r.state.ActiveFailure
	if failure == nil || failure.Version != r.cfg.CurrentVersion || failure.Signature != signature {
		failure = &StartupFailure{
			Version:       r.cfg.CurrentVersion,
			Signature:     signature,
			Message:       message,
			FirstFailedAt: now,
		}
		r.state.ActiveFailure = failure
	}
	failure.Count++
	failure.Message = message
	failure.LastFailedAt = now
	failure.Backoff = startupFailureBackoff(failure.Count)
	failure.CrashLoop = failure.Count >= startupCrashLoopThreshold
	if failure.Backoff > 0 {
		next := now.Add(failure.Backoff)
		failure.NextRetryAt = &next
	} else {
		failure.NextRetryAt = nil
	}
	if failure.CrashLoop {
		archived := *failure
		r.state.LastCrashLoop = &archived
	}

	var pending *PendingUpdate
	if candidate := r.state.PendingUpdate; candidate != nil && strings.TrimSpace(candidate.ToVersion) == r.cfg.CurrentVersion {
		copy := *candidate
		pending = &copy
	}
	shouldUpdate := pending == nil && r.cfg.AutoUpdate && r.cfg.Updater != nil && r.state.LastRecoveryUpdateAttemptVersion != r.cfg.CurrentVersion
	if shouldUpdate {
		r.state.LastRecoveryUpdateAttemptVersion = r.cfg.CurrentVersion
		failure.RecoveryAction = "checking_for_update"
	}
	err := saveStartupRecoveryState(r.cfg.StatePath, r.state)
	return *failure, pending, shouldUpdate, err
}

func (r *StartupRecovery) applyRecoveryUpdate(ctx context.Context) {
	status, err := r.cfg.Updater.Check(ctx)
	manualInstall := false
	if err == nil {
		switch {
		case !status.UpdateAvailable:
			status.Action = ActionUpToDate
		case status.InstallSource != InstallSourceRelease:
			manualInstall = true
			status.Action = ActionDelegate
			status.Message = "startup recovery cannot safely replace a non-release-managed Detent binary"
		default:
			status, err = r.cfg.Updater.Apply(ctx, r.cfg.ApplyOptions)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloadStateLocked()
	failure := r.state.ActiveFailure
	if failure == nil {
		return
	}
	if err != nil {
		failure.RecoveryAction = "update_failed"
		failure.LastRecoveryError = err.Error()
		r.cfg.Logger.Error("startup recovery update failed", "version", r.cfg.CurrentVersion, "error", err)
	} else if manualInstall {
		failure.RecoveryAction = "update_requires_manual_install"
		failure.LastRecoveryError = status.Message
		r.cfg.Logger.Warn("startup recovery update requires a release-managed binary", "version", r.cfg.CurrentVersion, "install_source", status.InstallSource)
	} else if status.Action == ActionUpdated {
		failure.RecoveryAction = "update_applied"
		failure.LastRecoveryError = ""
		r.cfg.Logger.Warn("startup recovery update applied", "from_version", status.CurrentVersion, "to_version", status.LatestVersion)
	} else {
		failure.RecoveryAction = "update_unavailable"
		failure.LastRecoveryError = strings.TrimSpace(status.Message)
		r.cfg.Logger.Warn("startup recovery found no applicable update", "version", r.cfg.CurrentVersion, "action", status.Action)
	}
	if saveErr := saveStartupRecoveryState(r.cfg.StatePath, r.state); saveErr != nil {
		r.cfg.Logger.Error("persist startup recovery update outcome failed", "path", r.cfg.StatePath, "error", saveErr)
	}
}

func (r *StartupRecovery) rollbackPendingUpdate(ctx context.Context, pending PendingUpdate) {
	r.mu.Lock()
	if r.state.PendingUpdate != nil {
		now := r.cfg.Now().UTC()
		r.state.PendingUpdate.RollbackRequestedAt = &now
		if r.state.ActiveFailure != nil {
			r.state.ActiveFailure.RecoveryAction = "rolling_back_update"
		}
		if err := saveStartupRecoveryState(r.cfg.StatePath, r.state); err != nil {
			r.cfg.Logger.Error("persist startup rollback intent failed", "path", r.cfg.StatePath, "error", err)
		}
	}
	r.mu.Unlock()

	rollback := r.cfg.Rollback
	if rollback == nil {
		rollback = func(ctx context.Context, pending PendingUpdate) error {
			if err := restorePreviousBinary(ctx, pending, r.cfg.GOOS); err != nil {
				return err
			}
			return restorePendingInstallLock(pending, r.cfg.GOOS, DetectionOptions{
				HomeDir: r.cfg.HomeDir,
				Env:     r.cfg.Env,
			})
		}
	}
	err := rollback(ctx, pending)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloadStateLocked()
	failure := r.state.ActiveFailure
	if err != nil {
		if failure != nil {
			failure.RecoveryAction = "rollback_failed"
			failure.LastRecoveryError = err.Error()
		}
		r.cfg.Logger.Error("startup recovery rollback failed", "from_version", pending.ToVersion, "to_version", pending.FromVersion, "error", err)
	} else {
		now := r.cfg.Now().UTC()
		if failure != nil {
			failure.RecoveryAction = "rolled_back_update"
			failure.LastRecoveryError = ""
		}
		r.state.LastRollback = &StartupRollback{FromVersion: pending.ToVersion, ToVersion: pending.FromVersion, RolledBackAt: now}
		if r.cfg.GOOS != "windows" {
			r.state.PendingUpdate = nil
		}
		r.cfg.Logger.Error("rolled back Detent after repeated startup failure", "from_version", pending.ToVersion, "to_version", pending.FromVersion)
	}
	if saveErr := saveStartupRecoveryState(r.cfg.StatePath, r.state); saveErr != nil {
		r.cfg.Logger.Error("persist startup rollback outcome failed", "path", r.cfg.StatePath, "error", saveErr)
	}
}

func restorePendingInstallLock(pending PendingUpdate, goos string, opts DetectionOptions) error {
	path := strings.TrimSpace(pending.InstallLockPath)
	if path != "" {
		if pending.PreviousInstallLockFound {
			return writeInstallLockContents(path, pending.PreviousInstallLock)
		}
		return removePendingReleaseInstallLock(path, pending, goos)
	}
	if pending.InstallSource == InstallSourceRelease {
		return writeReleaseInstallLock(goos, opts, pending.ExecutablePath, pending.FromVersion)
	}
	resolved, ok := installLockPath(goos, opts)
	if !ok {
		return nil
	}
	return removePendingReleaseInstallLock(resolved, pending, goos)
}

func removePendingReleaseInstallLock(path string, pending PendingUpdate, goos string) error {
	metadata, ok := readInstallLock(path)
	if !ok || strings.TrimSpace(metadata.version) != strings.TrimSpace(pending.ToVersion) ||
		!samePath(cleanPath(metadata.binary, goos), cleanPath(pending.ExecutablePath, goos), goos) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rolled back release install lock: %w", err)
	}
	return nil
}

func (r *StartupRecovery) signalCrashLoop(ctx context.Context) {
	r.mu.Lock()
	failure := r.state.ActiveFailure
	if failure == nil || !failure.CrashLoop || failure.Notified {
		r.mu.Unlock()
		return
	}
	event := CrashLoopEvent{
		Event:         "detent_startup_crash_loop",
		Instance:      r.cfg.Instance,
		Host:          r.cfg.Host,
		Version:       failure.Version,
		FailureCount:  failure.Count,
		FirstFailedAt: failure.FirstFailedAt,
		LastFailedAt:  failure.LastFailedAt,
		NextRetryAt:   failure.NextRetryAt,
		Error:         failure.Message,
		StatePath:     r.cfg.StatePath,
		Action:        failure.RecoveryAction,
	}
	r.mu.Unlock()

	r.cfg.Logger.Error(
		"Detent startup crash loop detected",
		"version", event.Version,
		"failure_count", event.FailureCount,
		"error", event.Error,
		"state_path", event.StatePath,
		"next_retry_at", event.NextRetryAt,
		"action", event.Action,
	)
	if r.cfg.Notify == nil {
		return
	}
	if err := r.cfg.Notify(ctx, event); err != nil {
		r.cfg.Logger.Error("deliver startup crash loop notification failed", "error", err)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if failure := r.state.ActiveFailure; failure != nil && failure.Signature == eventSignature(event) {
		failure.Notified = true
		if err := saveStartupRecoveryState(r.cfg.StatePath, r.state); err != nil {
			r.cfg.Logger.Error("persist startup crash loop notification failed", "path", r.cfg.StatePath, "error", err)
		}
	}
}

func (r *StartupRecovery) waitBeforeRetry(ctx context.Context) {
	r.mu.Lock()
	failure := r.state.ActiveFailure
	if failure == nil || failure.Backoff <= 0 {
		r.mu.Unlock()
		return
	}
	delay := failure.Backoff
	nextRetryAt := failure.NextRetryAt
	count := failure.Count
	r.mu.Unlock()

	r.cfg.Logger.Warn("delaying restart after repeated startup failure", "failure_count", count, "delay", delay, "next_retry_at", nextRetryAt)
	r.cfg.Wait(ctx, delay)
}

func (r *StartupRecovery) reloadStateLocked() {
	state, found, err := loadStartupRecoveryState(r.cfg.StatePath)
	if err != nil {
		r.cfg.Logger.Error("reload startup recovery state failed", "path", r.cfg.StatePath, "error", err)
		return
	}
	if found {
		r.state = state
	}
}

func (r *StartupRecovery) removePreviousBinary(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := r.cfg.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove healthy update rollback binary: %w", err)
	}
	return nil
}

func startupFailureSignature(version string, message string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(version)+"\x00"+strings.TrimSpace(message))))
}

func eventSignature(event CrashLoopEvent) string {
	return startupFailureSignature(event.Version, event.Error)
}

func startupFailureBackoff(count int) time.Duration {
	switch count {
	case 0, 1:
		return 0
	case 2:
		return 5 * time.Second
	case 3:
		return 30 * time.Second
	case 4:
		return 2 * time.Minute
	case 5:
		return 10 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func waitForStartupRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
