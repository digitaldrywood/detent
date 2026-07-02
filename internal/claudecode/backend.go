package claudecode

import (
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

const (
	defaultMaxScanTokenSize = 10 * 1024 * 1024
	defaultStderrTailBytes  = 64 * 1024
)

var (
	ErrNilCommand     = errors.New("claude command factory returned nil command")
	ErrMissingResult  = errors.New("claude result event missing")
	ErrStreamStalled  = errors.New("claude stream stalled")
	ErrTurnFailed     = errors.New("claude turn failed")
	ErrUpdateRejected = errors.New("claude update rejected")
)

type CommandFactory func(context.Context) *exec.Cmd

type CommandFactoryWithArgs func(context.Context, []string) *exec.Cmd

type Options struct {
	CommandFactory         CommandFactory
	CommandFactoryWithArgs CommandFactoryWithArgs
	PermissionMode         string
	AllowedTools           []string
	DisallowedTools        []string
	IncludePartialMessages bool
	ExtraArgs              []string
	TurnTimeout            time.Duration
	StallTimeout           time.Duration
	MaxScanTokenSize       int
	StderrTailBytes        int
}

type AgentBackend struct {
	options Options
}

var _ runner.AgentBackend = (*AgentBackend)(nil)

func NewAgentBackend(options Options) (*AgentBackend, error) {
	if options.CommandFactory == nil && options.CommandFactoryWithArgs == nil {
		options.CommandFactory = defaultCommandFactory
	}
	if options.MaxScanTokenSize <= 0 {
		options.MaxScanTokenSize = defaultMaxScanTokenSize
	}
	if options.StderrTailBytes <= 0 {
		options.StderrTailBytes = defaultStderrTailBytes
	}
	return &AgentBackend{options: options}, nil
}

func defaultCommandFactory(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(contextOrBackground(ctx), "claude")
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
