package runner

import (
	"errors"
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
	case errors.As(err, &hook), errors.Is(err, ErrWorkspacePreparation):
		failure.ErrorClass = "workspace_hook"
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
