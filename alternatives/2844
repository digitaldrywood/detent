package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func nativeTerminalFailure(err error, at time.Time, turnStartRefused bool) *tracker.NativeTerminalFailure {
	if err == nil {
		return nil
	}
	failure := tracker.NativeTerminalFailure{}
	var provider interface {
		NativeTerminalFailure() tracker.NativeTerminalFailure
	}
	if errors.As(err, &provider) {
		failure = provider.NativeTerminalFailure()
	}
	failure.Decision = certainFailureDecision(err)
	if decision, ok := FailureDecision(err); ok {
		failure.Decision = &decision
	}
	failure.Error = err.Error()
	var command *workspace.CommandError
	if errors.As(err, &command) && command.Command == "git" && isGitPush(command.Args) {
		failure.ErrorClass = "git_push"
		failure.Summary = command.Output
	}
	var carrier backendErrorCarrier
	if errors.As(err, &carrier) {
		failure.Error = strings.TrimSpace(failure.Provider + " " + strings.ReplaceAll(failure.Operation, "/", "_") + ": " + carrier.BackendErrorMessage())
	}
	var hook *workspace.HookError
	var issueConfiguration *IssueConfigurationError
	if capacity, ok := backendcapacity.As(err); ok && backendcapacity.IsStartupFailureKind(capacity.Details.Kind) {
		failure.ErrorClass = "backend_startup"
	}
	switch {
	case errors.As(err, &issueConfiguration):
		failure.ErrorClass = "issue_configuration"
	case failure.Stage == "" && (errors.As(err, &hook) || errors.Is(err, ErrWorkspacePreparation)):
		failure.ErrorClass = "workspace_hook"
		failure.Stage = "workspace_preparation"
		if hook != nil {
			failure.Error = fmt.Sprintf("workspace hook %s failed (exit status %d)", hook.Hook, hook.ExitCode)
			var unavailable *shell.UnavailableError
			if errors.As(err, &unavailable) {
				failure.Error += ": " + unavailable.Error()
			}
		}
	case failure.Stage == "" && errors.Is(err, ErrNativeRecoveryRequired):
		failure.ErrorClass = "source_recovery"
	case errors.Is(err, ErrMergeWorkerStartupTimeout), failure.Operation == "initialize", failure.ErrorClass == "backend_startup":
		failure.ErrorClass = "backend_startup"
	case failure.RPCCode != nil:
		failure.ErrorClass = "protocol"
	}
	failure.ObservedAt = at
	failure.TurnStartRefused = turnStartRefused
	public := failure.Public()
	return &public
}

func isGitPush(args []string) bool {
	if len(args) >= 2 && args[0] == "-C" {
		args = args[2:]
	}
	return len(args) > 0 && args[0] == "push"
}
