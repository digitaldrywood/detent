package cli

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
)

type workerProcessStore interface {
	ListActiveWorkerProcesses(context.Context) ([]store.WorkerProcess, error)
	MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error
}

type workerProcessReapFunc func(context.Context, procgroup.Identity, time.Duration) (procgroup.TerminationOutcome, error)

func reapWorkerProcesses(
	ctx context.Context,
	processStore workerProcessStore,
	logger *slog.Logger,
	reason string,
	grace time.Duration,
	now func() time.Time,
	reap workerProcessReapFunc,
) error {
	if processStore == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if reap == nil {
		reap = procgroup.Terminate
	}
	processes, err := processStore.ListActiveWorkerProcesses(ctx)
	if err != nil {
		return err
	}
	var result error
	for _, process := range processes {
		identity := procgroup.Identity{
			PID:       process.PID,
			GroupID:   process.GroupID,
			StartedAt: process.StartedAt,
		}
		outcome, reapErr := reap(ctx, identity, grace)
		attrs := []any{
			"operation", "worker_process_reap",
			"reason", strings.TrimSpace(reason),
			"decision", string(outcome),
			"detent_session_id", process.SessionID,
			"issue_id", strings.TrimSpace(process.IssueID),
			"issue_identifier", strings.TrimSpace(process.Identifier),
			"pid", process.PID,
			"pgid", process.GroupID,
		}
		if reapErr != nil {
			attrs = append(attrs, "error", reapErr)
			logger.Info("worker process lifecycle decision", attrs...)
			result = errors.Join(result, reapErr)
			continue
		}
		logger.Info("worker process lifecycle decision", attrs...)
		if err := processStore.MarkSessionWorkerProcessReaped(ctx, process.SessionID, store.WorkerProcessReap{
			ReapedAt: now().UTC(),
			Outcome:  string(outcome),
		}); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
