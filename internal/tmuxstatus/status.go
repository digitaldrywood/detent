package tmuxstatus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type commandRunner interface {
	Output(context.Context, ...string) (string, error)
	Run(context.Context, ...string) error
}

type Status struct {
	runner       commandRunner
	target       string
	originalName string
	lastName     string
	logger       *slog.Logger
	renameOnce   sync.Once
	closeOnce    sync.Once
	closeErr     error
}

func Enabled(tmux, pane string, configured *bool) bool {
	if strings.TrimSpace(tmux) == "" || strings.TrimSpace(pane) == "" {
		return false
	}
	return configured == nil || *configured
}

func New(ctx context.Context, pane string, logger *slog.Logger) (*Status, error) {
	return newStatus(ctx, execCommandRunner{}, pane, logger)
}

func newStatus(ctx context.Context, runner commandRunner, pane string, logger *slog.Logger) (*Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		return nil, errors.New("tmux command runner is required")
	}
	target := strings.TrimSpace(pane)
	if target == "" {
		return nil, errors.New("tmux pane target is required")
	}

	current, err := runner.Output(ctx, "display-message", "-t", target, "-p", "#{window_id}\t#{window_name}")
	if err != nil {
		return nil, fmt.Errorf("read current tmux window: %w", err)
	}
	windowID, originalName, ok := strings.Cut(strings.TrimSuffix(current, "\n"), "\t")
	if !ok || strings.TrimSpace(windowID) == "" {
		return nil, errors.New("read current tmux window: unexpected response")
	}

	status := &Status{
		runner:       runner,
		target:       strings.TrimSpace(windowID),
		originalName: originalName,
		logger:       logger,
	}
	logger.Info("initialized tmux window status", "window_id", status.target, "original_name", status.originalName)
	return status, nil
}

func Format(counts telemetry.Counts) string {
	return fmt.Sprintf("detent %dr/%dq/%db", counts.Running, counts.Queue, counts.Blocked)
}

func (s *Status) Update(ctx context.Context, snapshot telemetry.Snapshot) error {
	if s == nil {
		return nil
	}
	counts := snapshot.EffectiveCounts()
	counts.Blocked = telemetry.BoardWorkload(snapshot).Blocked
	name := Format(counts)
	if name == s.lastName {
		currentName, err := s.runner.Output(ctx, "display-message", "-t", s.target, "-p", "#{window_name}")
		if err != nil {
			return fmt.Errorf("read current tmux window name: %w", err)
		}
		if strings.TrimSuffix(currentName, "\n") == name {
			return nil
		}
	}
	if err := s.runner.Run(ctx, "rename-window", "-t", s.target, name); err != nil {
		return fmt.Errorf("rename tmux window: %w", err)
	}
	s.lastName = name
	s.renameOnce.Do(func() {
		s.logger.Info("tmux window status renamed", "window_id", s.target, "name", name)
	})
	return nil
}

func (s *Status) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if err := s.runner.Run(ctx, "rename-window", "-t", s.target, s.originalName); err != nil {
			s.closeErr = fmt.Errorf("restore tmux window name: %w", err)
		}
	})
	return s.closeErr
}

type execCommandRunner struct{}

func (execCommandRunner) Output(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, "tmux", args...).Output() // #nosec G204 -- arguments are fixed tmux operations assembled within this package.
	return string(output), err
}

func (execCommandRunner) Run(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "tmux", args...).Run() // #nosec G204 -- arguments are fixed tmux operations assembled within this package.
}
