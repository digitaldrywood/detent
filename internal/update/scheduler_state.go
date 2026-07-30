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
	Schema      int       `json:"schema"`
	LastCheckAt time.Time `json:"last_check_at"`
}

func loadSchedulerState(path string) (time.Time, bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return time.Time{}, false, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read scheduler state: %w", err)
	}
	var state schedulerState
	if err := json.Unmarshal(raw, &state); err != nil {
		return time.Time{}, false, fmt.Errorf("decode scheduler state: %w", err)
	}
	if state.Schema != schedulerStateSchema {
		return time.Time{}, false, fmt.Errorf("decode scheduler state: unsupported schema %d", state.Schema)
	}
	if state.LastCheckAt.IsZero() {
		return time.Time{}, false, errors.New("decode scheduler state: last check timestamp is required")
	}
	return state.LastCheckAt, true, nil
}

func saveSchedulerState(path string, lastCheckAt time.Time) (saveErr error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if lastCheckAt.IsZero() {
		return errors.New("save scheduler state: last check timestamp is required")
	}
	raw, err := json.Marshal(schedulerState{
		Schema:      schedulerStateSchema,
		LastCheckAt: lastCheckAt.UTC(),
	})
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
