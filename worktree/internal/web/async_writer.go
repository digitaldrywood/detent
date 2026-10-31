package web

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type asyncStoreWriter struct {
	jobs   chan func(context.Context)
	done   chan struct{}
	logger *slog.Logger
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
}

func newAsyncStoreWriter(buffer int, logger *slog.Logger) *asyncStoreWriter {
	if buffer < 0 {
		buffer = 0
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &asyncStoreWriter{
		jobs:   make(chan func(context.Context), buffer),
		done:   make(chan struct{}),
		logger: logger,
		cancel: cancel,
	}
	go writer.run(ctx)
	return writer
}

func (w *asyncStoreWriter) Enqueue(job func(context.Context)) bool {
	if w == nil || job == nil {
		return false
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return false
	}
	select {
	case w.jobs <- job:
		return true
	default:
		w.logger.Warn("dropping async store write because queue is full", "buffer", cap(w.jobs))
		return false
	}
}

func (w *asyncStoreWriter) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.jobs)
	}
	w.mu.Unlock()

	drainCtx := ctx
	cancelDrain := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 {
			joinGrace := min(time.Second, remaining/5)
			drainCtx, cancelDrain = context.WithDeadline(ctx, deadline.Add(-joinGrace))
		}
	}
	defer cancelDrain()
	stopCancel := context.AfterFunc(drainCtx, w.cancel)
	defer stopCancel()

	<-w.done
	return nil
}

func (w *asyncStoreWriter) run(ctx context.Context) {
	defer close(w.done)
	defer w.cancel()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case job, ok := <-w.jobs:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			jobCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			job(jobCtx)
			cancel()
		}
	}
}
