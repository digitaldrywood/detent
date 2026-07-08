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
	writer := &asyncStoreWriter{
		jobs:   make(chan func(context.Context), buffer),
		done:   make(chan struct{}),
		logger: logger,
	}
	go writer.run()
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

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *asyncStoreWriter) run() {
	defer close(w.done)
	for job := range w.jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		job(ctx)
		cancel()
	}
}
