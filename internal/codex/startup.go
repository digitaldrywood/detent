package codex

import (
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
)

type StartupError struct {
	Evidence backendcapacity.StartupEvidence
	Err      error
}

func (e *StartupError) Error() string {
	if e == nil || e.Err == nil {
		return "codex app-server startup failed"
	}
	return e.Err.Error()
}

func (e *StartupError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func startupStageError(err error, stage string, startedAt time.Time, failedAt time.Time, deadline time.Duration) error {
	if err == nil {
		return nil
	}
	var existing *StartupError
	if errors.As(err, &existing) {
		return err
	}
	startedAt = startedAt.UTC()
	failedAt = failedAt.UTC()
	return &StartupError{
		Evidence: backendcapacity.StartupEvidence{
			Stage:          stage,
			StageStartedAt: startedAt,
			FailedAt:       failedAt,
			ElapsedMS:      max(failedAt.Sub(startedAt).Milliseconds(), 0),
			DeadlineMS:     max(deadline.Milliseconds(), 0),
		},
		Err: err,
	}
}

func startupEvidence(err error) (backendcapacity.StartupEvidence, bool) {
	var startupErr *StartupError
	if !errors.As(err, &startupErr) || startupErr == nil {
		return backendcapacity.StartupEvidence{}, false
	}
	return startupErr.Evidence, true
}

func attachStartupProcessEvidence(err error, transport Transport) {
	var startupErr *StartupError
	if !errors.As(err, &startupErr) || startupErr == nil {
		return
	}
	provider, ok := transport.(interface {
		StartupProcessEvidence() backendcapacity.StartupProcessEvidence
	})
	if !ok {
		return
	}
	startupErr.Evidence.Process = provider.StartupProcessEvidence()
}

func markTransportReady(transport Transport, readyAt time.Time) {
	marker, ok := transport.(interface {
		MarkStartupReady(time.Time)
	})
	if ok {
		marker.MarkStartupReady(readyAt.UTC())
	}
}
