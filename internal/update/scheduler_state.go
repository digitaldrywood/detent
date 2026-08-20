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

const schedulerStateSchema = 1

type schedulerState struct {
	Schema           int        `json:"schema"`
	LastCheckAt      time.Time  `json:"last_check_at"`
	AvailableVersion string     `json:"available_version,omitempty"`
	PendingSince     *time.Time `json:"pending_since,omitempty"`
	Critical         bool       `json:"critical,omitempty"`
}

func loadSchedulerState(path string) (schedulerState, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return schedulerState{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return schedulerState{}, false, nil
	}
	if err != nil {
		return schedulerState{}, false, fmt.Errorf("read scheduler state: %w", err)
	}
	var state schedulerState
	if err := json.Unmarshal(raw, &state); err != nil {
		return schedulerState{}, false, fmt.Errorf("decode scheduler state: %w", err)
	}
	if state.Schema != schedulerStateSchema {
		return schedulerState{}, false, fmt.Errorf("decode scheduler state: unsupported schema %d", state.Schema)
	}
	if state.LastCheckAt.IsZero() {
		return schedulerState{}, false, errors.New("decode scheduler state: last check timestamp is required")
	}
	state.AvailableVersion = strings.TrimSpace(state.AvailableVersion)
	if state.PendingSince != nil {
		if state.PendingSince.IsZero() || state.AvailableVersion == "" {
			return schedulerState{}, false, errors.New("decode scheduler state: pending update requires timestamp and version")
		}
		pendingSince := state.PendingSince.UTC()
		state.PendingSince = &pendingSince
	} else {
		state.AvailableVersion = ""
		state.Critical = false
	}
	state.LastCheckAt = state.LastCheckAt.UTC()
	return state, true, nil
}

func saveSchedulerState(path string, state schedulerState) (saveErr error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if state.LastCheckAt.IsZero() {
		return errors.New("save scheduler state: last check timestamp is required")
	}
	state.Schema = schedulerStateSchema
	state.LastCheckAt = state.LastCheckAt.UTC()
	state.AvailableVersion = strings.TrimSpace(state.AvailableVersion)
	if state.PendingSince != nil {
		if state.PendingSince.IsZero() || state.AvailableVersion == "" {
			return errors.New("save scheduler state: pending update requires timestamp and version")
		}
		pendingSince := state.PendingSince.UTC()
		state.PendingSince = &pendingSince
	} else {
		state.AvailableVersion = ""
		state.Critical = false
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode scheduler state: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create scheduler state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".update-scheduler-*")
	if err != nil {
		return fmt.Errorf("create temporary scheduler state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath == "" {
			return
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			saveErr = errors.Join(saveErr, fmt.Errorf("remove temporary scheduler state: %w", err))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return closeSchedulerState(temporary, fmt.Errorf("secure temporary scheduler state: %w", err))
	}
	if _, err := temporary.Write(raw); err != nil {
		return closeSchedulerState(temporary, fmt.Errorf("write temporary scheduler state: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return closeSchedulerState(temporary, fmt.Errorf("sync temporary scheduler state: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary scheduler state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace scheduler state: %w", err)
	}
	temporaryPath = ""
	return nil
}

func closeSchedulerState(file *os.File, operationErr error) error {
	if err := file.Close(); err != nil {
		return errors.Join(operationErr, fmt.Errorf("close temporary scheduler state: %w", err))
	}
	return operationErr
}
