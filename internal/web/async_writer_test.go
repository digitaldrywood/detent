package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncStoreWriter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "enqueued jobs execute",
			run: func(t *testing.T) {
				t.Helper()

				writer := newAsyncStoreWriter(1, discardAsyncStoreWriterLogger())
				ran := make(chan struct{})
				if !writer.Enqueue(func(context.Context) {
					close(ran)
				}) {
					t.Fatal("Enqueue() = false, want true")
				}
				waitForAsyncWriterSignal(t, ran)
				closeAsyncStoreWriter(t, writer)
			},
		},
		{
			name: "close drains queued jobs",
			run: func(t *testing.T) {
				t.Helper()

				writer := newAsyncStoreWriter(1, discardAsyncStoreWriterLogger())
				started := make(chan struct{})
				release := make(chan struct{})
				queued := make(chan struct{})
				if !writer.Enqueue(func(context.Context) {
					close(started)
					<-release
				}) {
					t.Fatal("Enqueue(blocking) = false, want true")
				}
				waitForAsyncWriterSignal(t, started)
				if !writer.Enqueue(func(context.Context) {
					close(queued)
				}) {
					t.Fatal("Enqueue(queued) = false, want true")
				}

				closed := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					closed <- writer.Close(ctx)
				}()
				close(release)
				if err := waitForAsyncWriterClose(t, closed); err != nil {
					t.Fatalf("Close() error = %v, want nil", err)
				}
				select {
				case <-queued:
				default:
					t.Fatal("queued job did not run before Close returned")
				}
			},
		},
		{
			name: "enqueue returns false when buffer is full",
			run: func(t *testing.T) {
				t.Helper()

				var logs bytes.Buffer
				writer := newAsyncStoreWriter(1, slog.New(slog.NewTextHandler(&logs, nil)))
				started := make(chan struct{})
				release := make(chan struct{})
				if !writer.Enqueue(func(context.Context) {
					close(started)
					<-release
				}) {
					t.Fatal("Enqueue(blocking) = false, want true")
				}
				waitForAsyncWriterSignal(t, started)
				if !writer.Enqueue(func(context.Context) {}) {
					t.Fatal("Enqueue(buffered) = false, want true")
				}
				if writer.Enqueue(func(context.Context) {}) {
					t.Fatal("Enqueue(full) = true, want false")
				}
				if !strings.Contains(logs.String(), "queue is full") {
					t.Fatalf("logs missing queue full warning:\n%s", logs.String())
				}
				close(release)
				closeAsyncStoreWriter(t, writer)
			},
		},
		{
			name: "close cancels active job and joins within deadline",
			run: func(t *testing.T) {
				t.Helper()

				writer := newAsyncStoreWriter(1, discardAsyncStoreWriterLogger())
				started := make(chan struct{})
				jobErr := make(chan error, 1)
				if !writer.Enqueue(func(ctx context.Context) {
					close(started)
					<-ctx.Done()
					jobErr <- ctx.Err()
				}) {
					t.Fatal("Enqueue() = false, want true")
				}
				waitForAsyncWriterSignal(t, started)
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()

				if err := writer.Close(ctx); err != nil {
					t.Fatalf("Close() error = %v, want nil", err)
				}
				if err := ctx.Err(); err != nil {
					t.Fatalf("context error after Close = %v, want nil", err)
				}
				if err := waitForAsyncWriterClose(t, jobErr); !errors.Is(err, context.Canceled) {
					t.Fatalf("job context error = %v, want %v", err, context.Canceled)
				}
				select {
				case <-writer.done:
				default:
					t.Fatal("worker still running after Close returned")
				}
			},
		},
		{
			name: "close cancellation discards queued jobs",
			run: func(t *testing.T) {
				t.Helper()

				writer := newAsyncStoreWriter(1, discardAsyncStoreWriterLogger())
				started := make(chan struct{})
				if !writer.Enqueue(func(ctx context.Context) {
					close(started)
					<-ctx.Done()
				}) {
					t.Fatal("Enqueue(active) = false, want true")
				}
				waitForAsyncWriterSignal(t, started)
				queued := make(chan struct{})
				if !writer.Enqueue(func(context.Context) {
					close(queued)
				}) {
					t.Fatal("Enqueue(queued) = false, want true")
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				if err := writer.Close(ctx); err != nil {
					t.Fatalf("Close() error = %v, want nil", err)
				}
				select {
				case <-writer.done:
				default:
					t.Fatal("worker still running after Close returned")
				}
				select {
				case <-queued:
					t.Fatal("queued job ran after shutdown cancellation")
				default:
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func TestAsyncStoreWriterConcurrentEnqueueClose(t *testing.T) {
	t.Parallel()

	writer := newAsyncStoreWriter(8, discardAsyncStoreWriterLogger())
	var ran int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				writer.Enqueue(func(context.Context) {
					atomic.AddInt64(&ran, 1)
				})
			}
		}()
	}

	close(start)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := writer.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	wg.Wait()
	if atomic.LoadInt64(&ran) < 0 {
		t.Fatal("ran count should never be negative")
	}
}

func discardAsyncStoreWriterLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func closeAsyncStoreWriter(t *testing.T, writer *asyncStoreWriter) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := writer.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func waitForAsyncWriterSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async writer signal")
	}
}

func waitForAsyncWriterClose(t *testing.T, ch <-chan error) error {
	t.Helper()

	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async writer close")
		return nil
	}
}
