package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestServerShutdownDrainsAsyncWritesAfterActiveHandlers(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	server := &Server{
		echo:        e,
		logger:      logger,
		asyncWrites: newAsyncStoreWriter(1, logger),
	}

	started := make(chan struct{})
	release := make(chan struct{})
	shutdownStarted := make(chan struct{})
	enqueued := make(chan bool, 1)
	ran := make(chan struct{})
	e.Server.RegisterOnShutdown(func() {
		close(shutdownStarted)
	})
	e.GET("/block", func(c echo.Context) error {
		close(started)
		<-release
		enqueued <- server.asyncWrites.Enqueue(func(context.Context) {
			close(ran)
		})
		return c.NoContent(http.StatusAccepted)
	})

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.StartListener(listener)
	}()

	clientResult := make(chan shutdownHTTPResult, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String()+"/block", nil)
		if err != nil {
			clientResult <- shutdownHTTPResult{err: err}
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			clientResult <- shutdownHTTPResult{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		clientResult <- shutdownHTTPResult{status: resp.StatusCode}
	}()
	waitForAsyncWriterSignal(t, started)

	shutdownErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownErr <- server.Shutdown(ctx)
	}()
	waitForAsyncWriterSignal(t, shutdownStarted)
	close(release)

	result := waitForShutdownHTTPResult(t, clientResult)
	if result.err != nil {
		t.Fatalf("GET /block error = %v", result.err)
	}
	if result.status != http.StatusAccepted {
		t.Fatalf("GET /block status = %d, want %d", result.status, http.StatusAccepted)
	}
	if err := waitForAsyncWriterClose(t, shutdownErr); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if ok := waitForShutdownEnqueueResult(t, enqueued); !ok {
		t.Fatal("Enqueue() after shutdown started = false, want true")
	}
	waitForAsyncWriterSignal(t, ran)
	if err := waitForAsyncWriterClose(t, serveErr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("StartListener() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func TestServerShutdownJoinsAsyncWriterBeforeStoreClose(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{
		echo:        echo.New(),
		logger:      logger,
		asyncWrites: newAsyncStoreWriter(1, logger),
	}

	var storeClosed atomic.Bool
	var accessAfterClose atomic.Int64
	accessStore := func() {
		if storeClosed.Load() {
			accessAfterClose.Add(1)
		}
	}
	started := make(chan struct{})
	release := make(chan struct{})
	jobCanceled := make(chan struct{})
	if !server.asyncWrites.Enqueue(func(ctx context.Context) {
		close(started)
		select {
		case <-ctx.Done():
			close(jobCanceled)
		case <-release:
		}
		accessStore()
	}) {
		t.Fatal("Enqueue(active) = false, want true")
	}
	waitForAsyncWriterSignal(t, started)
	if !server.asyncWrites.Enqueue(func(context.Context) {
		accessStore()
	}) {
		t.Fatal("Enqueue(queued) = false, want true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownErr := make(chan error, 1)
	go func() {
		shutdownErr <- server.Shutdown(ctx)
	}()
	if err := waitForAsyncWriterClose(t, shutdownErr); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	storeClosed.Store(true)
	close(release)
	waitForAsyncWriterSignal(t, server.asyncWrites.done)
	if got := accessAfterClose.Load(); got != 0 {
		t.Fatalf("store accesses after Shutdown returned = %d, want 0", got)
	}
	select {
	case <-jobCanceled:
	default:
		t.Fatal("active job did not observe shutdown cancellation")
	}
}

type shutdownHTTPResult struct {
	status int
	err    error
}

func waitForShutdownHTTPResult(t *testing.T, ch <-chan shutdownHTTPResult) shutdownHTTPResult {
	t.Helper()

	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown HTTP result")
		return shutdownHTTPResult{}
	}
}

func waitForShutdownEnqueueResult(t *testing.T, ch <-chan bool) bool {
	t.Helper()

	select {
	case ok := <-ch:
		return ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown enqueue result")
		return false
	}
}
