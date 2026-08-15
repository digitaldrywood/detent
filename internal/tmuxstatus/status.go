package tmuxstatus

import (
	"context"
	"errors"
	"fmt"
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
	closeOnce    sync.Once
	closeErr     error
}

func Enabled(tmux string, configured *bool) bool {
	if strings.TrimSpace(tmux) == "" {
		return false
	}
	return configured == nil || *configured
}

func New(ctx context.Context) (*Status, error) {
	return newStatus(ctx, execCommandRunner{})
}

func newStatus(ctx context.Context, runner commandRunner) (*Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runner == nil {
		return nil, errors.New("tmux command runner is required")
	}

	current, err := runner.Output(ctx, "display-message", "-p", "#{window_id}\t#{window_name}")
	if err != nil {
		return nil, fmt.Errorf("read current tmux window: %w", err)
	}
	target, originalName, ok := strings.Cut(strings.TrimSuffix(current, "\n"), "\t")
	if !ok || strings.TrimSpace(target) == "" {
		return nil, errors.New("read current tmux window: unexpected response")
	}

	return &Status{
		runner:       runner,
		target:       strings.TrimSpace(target),
		originalName: originalName,
	}, nil
}

func Format(counts telemetry.Counts) string {
	return fmt.Sprintf("detent %dr/%dq/%db", counts.Running, counts.Queue, counts.Blocked)
}

func (s *Status) Update(ctx context.Context, snapshot telemetry.Snapshot) error {
	if s == nil {
		return nil
	}
	name := Format(snapshot.EffectiveCounts())
	if name == s.lastName {
		return nil
	}
	if err := s.runner.Run(ctx, "rename-window", "-t", s.target, name); err != nil {
		return fmt.Errorf("rename tmux window: %w", err)
	}
	s.lastName = name
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
