package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
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
	failure.Error = err.Error()
	var carrier backendErrorCarrier
	if errors.As(err, &carrier) {
		failure.Error = strings.TrimSpace(failure.Provider + " " + strings.ReplaceAll(failure.Operation, "/", "_") + ": " + carrier.BackendErrorMessage())
	}
	var hook *workspace.HookError
	switch {
	case errors.As(err, &hook), errors.Is(err, ErrWorkspacePreparation), errors.Is(err, ErrNativeRecoveryRequired):
		failure.ErrorClass = "workspace_hook"
		failure.Stage = "workspace_preparation"
		if hook != nil {
			output, _, _ := strings.Cut(strings.TrimSpace(hook.Output), "\n")
			failure.Error = fmt.Sprintf("workspace hook %s failed (exit status %d)", hook.Hook, hook.ExitCode)
			if output != "" {
				failure.Error += ": " + output
			}
		}
	case errors.Is(err, ErrMergeWorkerStartupTimeout), failure.Operation == "initialize":
		failure.ErrorClass = "backend_startup"
	case failure.RPCCode != nil:
		failure.ErrorClass = "protocol"
	}
	if capacity, ok := backendcapacity.As(err); ok && backendcapacity.IsStartupFailureKind(capacity.Details.Kind) {
		failure.ErrorClass = "backend_startup"
	}
	failure.ObservedAt = at
	failure.TurnStartRefused = turnStartRefused
	public := failure.Public()
	return &public
}
