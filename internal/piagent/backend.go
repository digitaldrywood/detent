// Package piagent adapts Pi's JSONL RPC subprocess to Detent's worker interface.
package piagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type Options struct {
	BackendID       string
	CommandFactory  func(context.Context, []string) *exec.Cmd
	IsolationPolicy func() (isolation.Policy, error)
	Provider        string
	ThinkingLevel   string
	Tools           []string
	SessionDir      string
	TurnTimeout     time.Duration
	StallTimeout    time.Duration
}

type AgentBackend struct{ options Options }

var _ runner.AgentBackend = (*AgentBackend)(nil)
var _ runner.AgentResumeVerifier = (*AgentBackend)(nil)

func NewAgentBackend(options Options) (*AgentBackend, error) {
	if options.CommandFactory == nil {
		options.CommandFactory = func(ctx context.Context, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, "pi", args...) // #nosec G204 -- the fixed Pi executable receives backend-built RPC arguments directly without a shell.
		}
	}
	return &AgentBackend{options: options}, nil
}

// Session IDs are recorded for observability. Until persisted Pi session
// context can be verified, neither verification nor direct turns accept resume.
func (*AgentBackend) VerifyResume(context.Context, runner.AgentProcessRequest, runner.AgentResume) error {
	return runner.ErrAgentResumeUnsupported
}

// infrastructureError lets the existing instance capacity path attribute RPC
// and launch failures without parsing provider prose or introducing a reason.
type infrastructureError struct {
	err     error
	startup bool
}

func (e *infrastructureError) Error() string { return e.err.Error() }
func (e *infrastructureError) Unwrap() error { return e.err }

func (*AgentBackend) ClassifyCapacityError(err error, _ *telemetry.RateLimits, _ time.Time) (backendcapacity.Details, bool) {
	var failure *infrastructureError
	if errors.Is(err, context.Canceled) || !errors.As(err, &failure) {
		return backendcapacity.Details{}, false
	}
	if errors.Is(err, context.DeadlineExceeded) && !failure.startup {
		return backendcapacity.Details{}, false
	}
	details := backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Reason: "Pi backend transport or protocol unavailable"}
	if failure.startup {
		details.Kind = backendcapacity.StartupFailureKind
		if errors.Is(err, context.DeadlineExceeded) {
			details.Kind = backendcapacity.StartupTimeoutKind
		}
	}
	return details, true
}

func (b *AgentBackend) argv(ctx context.Context, req runner.AgentTurnRequest) ([]string, error) {
	if req.Resume.SessionID != "" || req.Resume.ThreadID != "" {
		return nil, runner.ErrAgentResumeUnsupported
	}
	if req.ReadOnly {
		return nil, &infrastructureError{err: errors.New("Pi backend cannot enforce read-only execution")}
	}
	policy, pinned := isolation.FromContext(ctx)
	if !pinned && b.options.IsolationPolicy != nil {
		var err error
		policy, err = b.options.IsolationPolicy()
		if err != nil {
			return nil, &infrastructureError{err: err}
		}
		pinned = true
	}
	if pinned && policy.Tier != isolation.NativeTrusted {
		return nil, &infrastructureError{err: fmt.Errorf("Pi backend requires native-trusted isolation: %w", isolation.ErrSandboxUnavailable)}
	}
	if req.RequireSubscriptionAuth {
		return nil, runner.ErrSubscriptionAuthRequired
	}
	if req.SupplementalTools {
		return nil, &infrastructureError{err: errors.New("Pi backend does not support Detent dynamic tools")}
	}
	args := []string{"--mode", "rpc", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-approve"}
	provider := b.options.Provider
	if req.ModelProvider != "" {
		provider = req.ModelProvider
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	thinking := b.options.ThinkingLevel
	if req.ReasoningEffort != "" {
		thinking = req.ReasoningEffort
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	tools := b.options.Tools
	if len(tools) == 0 {
		tools = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}
	}
	args = append(args, "--tools", strings.Join(tools, ","))
	if b.options.SessionDir != "" {
		args = append(args, "--session-dir", b.options.SessionDir)
	}
	return args, nil
}
