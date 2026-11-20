package runner

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
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
	var timeout *sessiontimeout.Error
	if capacity, ok := backendcapacity.As(err); ok && backendcapacity.IsStartupFailureKind(capacity.Details.Kind) {
		failure.ErrorClass = "backend_startup"
	}
	switch {
	case errors.As(err, &timeout):
		failure.ErrorClass = string(timeout.Kind)
		failure.Stage = "provider_session"
		failure.Error = timeout.Error()
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
		if class := WorkspaceResourceClass(err); class != "" {
			failure.ErrorClass = class
			failure.Error = tracker.WorkspaceResourceMessage(class)
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

func WorkspaceResourceClass(err error) string {
	var hook *workspace.HookError
	if !errors.Is(err, ErrWorkspacePreparation) && !errors.As(err, &hook) {
		return ""
	}
	var command *workspace.CommandError
	var detail string
	switch {
	case errors.As(err, &hook):
		detail, err = hook.Output, hook.Err
	case errors.As(err, &command):
		detail, err = command.Output, command.Err
	default:
		detail = err.Error()
	}
	var operation *os.SyscallError
	if errors.As(err, &operation) && operation.Syscall == "shmget" && errors.Is(operation, syscall.ENOSPC) {
		return tracker.WorkspaceSharedMemoryExhausted
	}
	for line := range strings.SplitSeq(strings.ToLower(detail), "\n") {
		switch {
		case strings.Contains(line, "no space left on device") && (strings.Contains(line, "shared memory") || strings.Contains(line, "shmget")):
			return tracker.WorkspaceSharedMemoryExhausted
		case strings.Contains(line, "too many open files"):
			return tracker.WorkspaceFileDescriptorsExhausted
		case strings.Contains(line, "address already in use"):
			return tracker.WorkspacePortUnavailable
		}
	}
	switch {
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return tracker.WorkspaceFileDescriptorsExhausted
	case errors.Is(err, syscall.EADDRINUSE):
		return tracker.WorkspacePortUnavailable
	case errors.Is(err, syscall.ENOSPC), strings.Contains(strings.ToLower(detail), "no space left on device"):
		return tracker.WorkspaceDiskFull
	}
	return ""
}
