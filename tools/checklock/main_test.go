package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const validationSubprocessHelper = "DETENT_CHECKLOCK_HELPER"

func TestRunValidatesArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing lock", args: []string{"--", "go", "version"}, wantErr: "-lock is required"},
		{name: "invalid wait timeout", args: []string{"-lock", "gate.lock", "-wait-timeout", "0s", "--", "go", "version"}, wantErr: "-wait-timeout must be positive"},
		{name: "missing command", args: []string{"-lock", "gate.lock"}, wantErr: "command is required after --"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			if code := run(t.Context(), tt.args, strings.NewReader(""), io.Discard, &stderr); code != 2 {
				t.Fatalf("run() = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want containing %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestRunSerializesConcurrentSubprocesses(t *testing.T) {
	tempDir := t.TempDir()
	validationLock := filepath.Join(tempDir, "validation.lock")
	criticalLock := filepath.Join(tempDir, "critical.lock")
	tracePath := filepath.Join(tempDir, "trace")
	releasePath := filepath.Join(tempDir, "release")
	t.Setenv(validationSubprocessHelper, "1")
	t.Setenv("DETENT_CHECKLOCK_CRITICAL_LOCK", criticalLock)
	t.Setenv("DETENT_CHECKLOCK_TRACE", tracePath)
	t.Setenv("DETENT_CHECKLOCK_RELEASE", releasePath)
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, nil, 0o600)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	args := []string{
		"-lock", validationLock,
		"-wait-timeout", "5s",
		"--", os.Args[0], "-test.run=^TestValidationSubprocessHelper$",
	}

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- run(ctx, args, strings.NewReader(""), io.Discard, io.Discard)
	}()
	waitForTraceLines(t, ctx, tracePath, 1)

	secondWait := newNotifyingWriter()
	secondDone := make(chan int, 1)
	go func() {
		secondDone <- run(ctx, args, strings.NewReader(""), io.Discard, secondWait)
	}()
	select {
	case <-secondWait.notified:
	case code := <-secondDone:
		t.Fatalf("second run completed before first released lock with code %d", code)
	case <-ctx.Done():
		t.Fatalf("wait for second run contention: %v", ctx.Err())
	}
	if got := traceLineCount(t, tracePath); got != 1 {
		t.Fatalf("trace lines while first subprocess holds critical section = %d, want 1", got)
	}

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("write release signal: %v", err)
	}
	for name, done := range map[string]<-chan int{"first": firstDone, "second": secondDone} {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("%s run code = %d, want 0", name, code)
			}
		case <-ctx.Done():
			t.Fatalf("wait for %s run: %v", name, ctx.Err())
		}
	}
	if got := traceLineCount(t, tracePath); got != 2 {
		t.Fatalf("serialized subprocess trace lines = %d, want 2", got)
	}
}

func TestValidationSubprocessHelper(t *testing.T) {
	if os.Getenv(validationSubprocessHelper) == "" {
		t.Skip("helper process")
	}

	lock, err := instancelock.Acquire(os.Getenv("DETENT_CHECKLOCK_CRITICAL_LOCK"))
	if err != nil {
		t.Fatalf("acquire critical lock: %v", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Errorf("release critical lock: %v", err)
		}
	}()

	trace, err := os.OpenFile(os.Getenv("DETENT_CHECKLOCK_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	if _, err := fmt.Fprintln(trace, os.Getpid()); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	if err := trace.Close(); err != nil {
		t.Fatalf("close trace: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	waitForPath(t, ctx, os.Getenv("DETENT_CHECKLOCK_RELEASE"))
}

type notifyingWriter struct {
	mu       sync.Mutex
	data     bytes.Buffer
	notified chan struct{}
	once     sync.Once
}

func newNotifyingWriter() *notifyingWriter {
	return &notifyingWriter{notified: make(chan struct{})}
}

func (w *notifyingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.once.Do(func() { close(w.notified) })
	return w.data.Write(data)
}

func waitForTraceLines(t *testing.T, ctx context.Context, path string, want int) {
	t.Helper()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if traceLineCount(t, path) >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %d trace lines: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func traceLineCount(t *testing.T, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read trace: %v", err)
	}
	return len(strings.Fields(string(data)))
}

func waitForPath(t *testing.T, ctx context.Context, path string) {
	t.Helper()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat release signal: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for release signal: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
