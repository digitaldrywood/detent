// Package antigravity adapts the Antigravity CLI print-mode stream to Detent's worker interface.
package antigravity

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const BackendKind = "antigravity"

type Options struct {
	BackendID       string
	CommandFactory  func(context.Context, []string) *exec.Cmd
	IsolationPolicy func() (isolation.Policy, error)
	Provider        string
	Effort          string
	TurnTimeout     time.Duration
	StallTimeout    time.Duration
}

type AgentBackend struct{ options Options }

var _ runner.AgentBackend = (*AgentBackend)(nil)
var _ runner.AgentResumeVerifier = (*AgentBackend)(nil)

func NewAgentBackend(options Options) (*AgentBackend, error) {
	if options.CommandFactory == nil {
		options.CommandFactory = func(ctx context.Context, args []string) *exec.Cmd {
			return exec.CommandContext(ctx, "agy", args...) // #nosec G204 -- the fixed Antigravity executable receives backend-built arguments directly without a shell.
		}
	}
	return &AgentBackend{options: options}, nil
}

func (*AgentBackend) VerifyResume(context.Context, runner.AgentProcessRequest, runner.AgentResume) error {
	return runner.ErrAgentResumeUnsupported
}

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
	details := backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Reason: "Antigravity backend transport or stream unavailable"}
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
	if req.ReadOnly || req.TextOnly {
		return nil, &infrastructureError{err: errors.New("antigravity backend cannot enforce read-only execution")}
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
		return nil, &infrastructureError{err: fmt.Errorf("antigravity backend requires native-trusted isolation: %w", isolation.ErrSandboxUnavailable)}
	}
	if req.RequireSubscriptionAuth {
		return nil, runner.ErrSubscriptionAuthRequired
	}
	if req.SupplementalTools {
		return nil, &infrastructureError{err: errors.New("antigravity backend does not support Detent dynamic tools")}
	}
	if len(runner.AttachmentImages(req.Attachments)) > 0 {
		return nil, errors.New("antigravity backend does not support image attachments")
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--dangerously-skip-permissions", "--disable-slash-commands"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if effort := b.effort(req); effort != "" {
		args = append(args, "--effort", effort)
	}
	return append(args, "--print="), nil
}

func (b *AgentBackend) effort(req runner.AgentTurnRequest) string {
	if req.ReasoningEffort != "" {
		return req.ReasoningEffort
	}
	return b.options.Effort
}

func (b *AgentBackend) provider(req runner.AgentTurnRequest) string {
	if req.ModelProvider != "" {
		return req.ModelProvider
	}
	return b.options.Provider
}
