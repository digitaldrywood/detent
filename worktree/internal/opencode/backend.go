package opencode

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
	TurnTimeout     time.Duration
	StallTimeout    time.Duration
}

type AgentBackend struct {
	options     Options
	toolServers []map[string]any
}

var _ runner.AgentToolBackend = (*AgentBackend)(nil)

var _ runner.AgentBackend = (*AgentBackend)(nil)
var _ runner.AgentResumeVerifier = (*AgentBackend)(nil)

func NewAgentBackend(options Options) (*AgentBackend, error) {
	if options.CommandFactory == nil {
		options.CommandFactory = func(ctx context.Context, _ []string) *exec.Cmd {
			return exec.CommandContext(ctx, "opencode", "acp")
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
	reason  string
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
	details := backendcapacity.Details{Type: backendcapacity.ErrorTypeTransientOverload, Reason: "OpenCode backend transport or protocol unavailable"}
	if failure.reason != "" {
		details.Reason = failure.reason
	}
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
		return nil, &infrastructureError{err: errors.New("opencode backend cannot enforce read-only execution")}
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
		return nil, &infrastructureError{err: fmt.Errorf("opencode backend requires native-trusted isolation: %w", isolation.ErrSandboxUnavailable)}
	}
	if req.RequireSubscriptionAuth {
		return nil, runner.ErrSubscriptionAuthRequired
	}
	if req.SupplementalTools && len(b.toolServers) == 0 {
		return nil, &infrastructureError{err: errors.New("opencode dynamic tools require RunTurnWithTools")}
	}
	if !strings.Contains(req.Model, "/") {
		return nil, &infrastructureError{err: errors.New("opencode requires an explicit provider/model route")}
	}
	return []string{"acp"}, nil
}

func providerRequestError(method string, code int, message string) error {
	failure := &infrastructureError{err: fmt.Errorf("OpenCode %s failed (%d)", method, code), startup: method != "session/prompt"}
	if strings.Contains(message, "This Go model requires Global regions") {
		failure.startup = true
		failure.reason = "OpenCode workspace region policy prevents this model from running"
		failure.err = fmt.Errorf("%s: open https://opencode.ai/console, select the authenticated workspace, then Settings > Privacy > Regions > Global if permitted by your data policy; otherwise select an allowed model. If Privacy is hidden, append /privacy to the workspace settings URL. No code correction or new API key is required: %w", failure.reason, failure.err)
	}
	return failure
}
