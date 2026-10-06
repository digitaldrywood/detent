package runner

import (
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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
	failure.ObservedAt = at
	failure.TurnStartRefused = turnStartRefused
	public := failure.Public()
	return &public
}
