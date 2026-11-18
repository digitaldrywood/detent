package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	WorkerGenerationStateActive    = "active"
	WorkerGenerationStateExited    = "exited"
	WorkerGenerationStateAbandoned = "abandoned"
)

type WorkerGenerationStore interface {
	LiveForeignGenerationReader
	StartWorkerGeneration(context.Context, WorkerGenerationStart) (int64, error)
	ExitWorkerGeneration(context.Context, time.Time) error
	WorkerGeneration() int64
	LiveWorkerGenerations(context.Context) ([]int64, error)
}

type LiveForeignGenerationReader interface {
	LiveForeignWorkerGenerations(context.Context) ([]int64, error)
}

type WorkerGenerationStart struct {
	PID          int
	ProcessStart string
	Version      string
	StartedAt    time.Time
}

// WorkerGenerationLivenessError reports generations whose process could not be
// inspected. They are treated as live, so recovery skips their rows until a
// later inspection proves the process gone or replaced.
type WorkerGenerationLivenessError struct {
	Unknown []UnknownWorkerGeneration
}

type UnknownWorkerGeneration struct {
	Generation int64
	PID        int
	Err        error
}

func (e *WorkerGenerationLivenessError) Error() string {
	parts := make([]string, 0, len(e.Unknown))
	for _, unknown := range e.Unknown {
		parts = append(parts, fmt.Sprintf("generation %d pid %d: %v", unknown.Generation, unknown.PID, unknown.Err))
	}
	return "worker generation liveness unknown; treating as live and skipping its rows: " + strings.Join(parts, "; ")
}

func (e *WorkerGenerationLivenessError) Unwrap() []error {
	errs := make([]error, 0, len(e.Unknown))
	for _, unknown := range e.Unknown {
		errs = append(errs, unknown.Err)
	}
	return errs
}

func (e *WorkerGenerationLivenessError) Log(logger *slog.Logger) {
	for _, unknown := range e.Unknown {
		logger.Warn("worker generation liveness unknown; recovery skips its rows",
			"operation", "worker_generation_liveness",
			"worker_generation", unknown.Generation,
			"pid", unknown.PID,
			"error", unknown.Err,
		)
	}
}
