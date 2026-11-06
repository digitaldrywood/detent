package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store"
)

const workerGenerationExitTimeout = 5 * time.Second

func startWorkerGeneration(ctx context.Context, generations store.WorkerGenerationStore, version string, now time.Time) (int64, error) {
	pid := os.Getpid()
	processStart, err := procstart.Identity(pid)
	if err != nil {
		return 0, fmt.Errorf("inspect worker process start: %w", err)
	}
	generation, err := generations.StartWorkerGeneration(ctx, store.WorkerGenerationStart{
		PID:          pid,
		ProcessStart: processStart,
		Version:      version,
		StartedAt:    now,
	})
	if err != nil {
		return 0, fmt.Errorf("allocate worker generation: %w", err)
	}
	return generation, nil
}

func exitWorkerGeneration(generations store.WorkerGenerationStore, logger *slog.Logger, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), workerGenerationExitTimeout)
	defer cancel()
	if err := generations.ExitWorkerGeneration(ctx, now); err != nil {
		logger.Warn("mark worker generation exited failed", "worker_generation", generations.WorkerGeneration(), "error", err)
	}
}
