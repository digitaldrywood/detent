package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const startupRecoveryStateSchema = 1

const startupRecoveryStateName = "detent-startup-recovery.json"

type StartupFailure struct {
	Version           string        `json:"version"`
	Signature         string        `json:"signature"`
	Message           string        `json:"message"`
	Count             int           `json:"count"`
	FirstFailedAt     time.Time     `json:"first_failed_at"`
	LastFailedAt      time.Time     `json:"last_failed_at"`
	NextRetryAt       *time.Time    `json:"next_retry_at,omitempty"`
	Backoff           time.Duration `json:"backoff"`
	CrashLoop         bool          `json:"crash_loop"`
	Notified          bool          `json:"notified"`
	RecoveryAction    string        `json:"recovery_action,omitempty"`
	LastRecoveryError string        `json:"last_recovery_error,omitempty"`
}

type PendingUpdate struct {
	FromVersion              string        `json:"from_version"`
	ToVersion                string        `json:"to_version"`
	InstallSource            InstallSource `json:"install_source"`
	ExecutablePath           string        `json:"executable_path"`
	PreviousBinaryPath       string        `json:"previous_binary_path"`
	InstallLockPath          string        `json:"install_lock_path,omitempty"`
	PreviousInstallLock      string        `json:"previous_install_lock,omitempty"`
	PreviousInstallLockFound bool          `json:"previous_install_lock_found,omitempty"`
	AppliedAt                time.Time     `json:"applied_at"`
	RollbackRequestedAt      *time.Time    `json:"rollback_requested_at,omitempty"`
}

type StartupRollback struct {
	FromVersion  string    `json:"from_version"`
	ToVersion    string    `json:"to_version"`
	RolledBackAt time.Time `json:"rolled_back_at"`
}

type startupRecoveryState struct {
	Schema                           int              `json:"schema"`
	ActiveFailure                    *StartupFailure  `json:"active_failure,omitempty"`
	PendingUpdate                    *PendingUpdate   `json:"pending_update,omitempty"`
	LastCrashLoop                    *StartupFailure  `json:"last_crash_loop,omitempty"`
	LastRollback                     *StartupRollback `json:"last_rollback,omitempty"`
	LastHealthyAt                    *time.Time       `json:"last_healthy_at,omitempty"`
	LastRecoveryUpdateAttemptVersion string           `json:"last_recovery_update_attempt_version,omitempty"`
}

func RecoveryStatePath(configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), startupRecoveryStateName)
}

func loadStartupRecoveryState(path string) (startupRecoveryState, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return startupRecoveryState{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return startupRecoveryState{}, false, nil
	}
	if err != nil {
		return startupRecoveryState{}, false, fmt.Errorf("read startup recovery state: %w", err)
	}
	var state startupRecoveryState
	if err := json.Unmarshal(raw, &state); err != nil {
		return startupRecoveryState{}, false, fmt.Errorf("decode startup recovery state: %w", err)
	}
	if state.Schema != startupRecoveryStateSchema {
		return startupRecoveryState{}, false, fmt.Errorf("decode startup recovery state: unsupported schema %d", state.Schema)
	}
	return state, true, nil
}

func saveStartupRecoveryState(path string, state startupRecoveryState) (saveErr error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	state.Schema = startupRecoveryStateSchema
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode startup recovery state: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create startup recovery state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".startup-recovery-*")
	if err != nil {
		return fmt.Errorf("create temporary startup recovery state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath == "" {
			return
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			saveErr = errors.Join(saveErr, fmt.Errorf("remove temporary startup recovery state: %w", err))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return closeStartupRecoveryState(temporary, fmt.Errorf("secure temporary startup recovery state: %w", err))
	}
	if _, err := temporary.Write(raw); err != nil {
		return closeStartupRecoveryState(temporary, fmt.Errorf("write temporary startup recovery state: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return closeStartupRecoveryState(temporary, fmt.Errorf("sync temporary startup recovery state: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary startup recovery state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace startup recovery state: %w", err)
	}
	temporaryPath = ""
	return nil
}

func closeStartupRecoveryState(file *os.File, operationErr error) error {
	if err := file.Close(); err != nil {
		return errors.Join(operationErr, fmt.Errorf("close temporary startup recovery state: %w", err))
	}
	return operationErr
}

func recordPendingUpdate(path string, pending PendingUpdate) error {
	state, _, err := loadStartupRecoveryState(path)
	if err != nil {
		return err
	}
	pending.FromVersion = strings.TrimSpace(pending.FromVersion)
	pending.ToVersion = strings.TrimSpace(pending.ToVersion)
	pending.ExecutablePath = strings.TrimSpace(pending.ExecutablePath)
	pending.PreviousBinaryPath = strings.TrimSpace(pending.PreviousBinaryPath)
	pending.InstallLockPath = strings.TrimSpace(pending.InstallLockPath)
	if pending.FromVersion == "" || pending.ToVersion == "" || pending.ExecutablePath == "" || pending.PreviousBinaryPath == "" || pending.AppliedAt.IsZero() {
		return errors.New("record pending update: versions, binary paths, and applied timestamp are required")
	}
	state.PendingUpdate = &pending
	return saveStartupRecoveryState(path, state)
}

func clearPendingUpdate(path string, toVersion string) error {
	state, found, err := loadStartupRecoveryState(path)
	if err != nil || !found {
		return err
	}
	if state.PendingUpdate == nil || strings.TrimSpace(state.PendingUpdate.ToVersion) != strings.TrimSpace(toVersion) {
		return nil
	}
	state.PendingUpdate = nil
	return saveStartupRecoveryState(path, state)
}
