package buildinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Drift struct {
	Comparable bool
	Detected   bool
}

type BinaryRunner func(context.Context, string, ...string) (string, error)

func DetectDrift(running Info, installed Info) Drift {
	if IsZero(running) || IsZero(installed) {
		return Drift{}
	}
	running = Normalize(running)
	installed = Normalize(installed)
	if !placeholderCommit(running.Commit) && !placeholderCommit(installed.Commit) {
		return Drift{Comparable: true, Detected: !sameCommit(running.Commit, installed.Commit)}
	}
	if !placeholderVersion(running.Version) && !placeholderVersion(installed.Version) {
		return Drift{Comparable: true, Detected: running.Version != installed.Version}
	}
	return Drift{}
}

func ReadBinary(ctx context.Context, path string, run BinaryRunner) (Info, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Info{}, errors.New("binary path is required")
	}
	if run == nil {
		return Info{}, errors.New("binary runner is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	output, err := run(ctx, path, "--format", "json", "version")
	if err != nil {
		return Info{}, fmt.Errorf("read installed Detent build: %w", err)
	}
	var payload struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"build_date"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return Info{}, fmt.Errorf("decode installed Detent build: %w", err)
	}
	info := Info{Version: payload.Version, Commit: payload.Commit, Date: payload.Date}
	if IsZero(info) {
		return Info{}, errors.New("installed Detent binary did not report build metadata")
	}
	return Normalize(info), nil
}

func sameCommit(left string, right string) bool {
	left = strings.ToLower(strings.TrimSpace(left))
	right = strings.ToLower(strings.TrimSpace(right))
	if left == right {
		return true
	}
	if min(len(left), len(right)) < shortCommitWidth {
		return false
	}
	return strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}
