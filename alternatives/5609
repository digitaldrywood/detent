package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const validationSubprocessHelper = "DETENT_CHECKLOCK_HELPER"

// These deadlines only detect deadlocked OS subprocess and file-lock handshakes.
// They are deliberately generous enough for loaded Windows and macOS runners.
const (
	validationIntegrationTimeout = 60 * time.Second
	validationLockWaitTimeout    = 45 * time.Second
)

func TestRunValidatesArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "missing lock", args: []string{"--", "go", "version"}, wantErr: "-lock is required"},
		{name: "invalid wait timeout", args: []string{"-lock", "gate.lock", "-wait-timeout", "0s", "--", "go", "version"}, wantErr: "-wait-timeout must be positive"},
		{name: "invalid total wait timeout", args: []string{"-lock", "gate.lock", "-max-wait-timeout", "0s", "--", "go", "version"}, wantErr: "-max-wait-timeout must be positive"},
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
	t.Setenv(validationSubprocessHelper, "1")
	t.Setenv("DETENT_CHECKLOCK_CRITICAL_LOCK", criticalLock)
	t.Setenv("DETENT_CHECKLOCK_TRACE", tracePath)

	ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
	defer cancel()
	args := []string{
		"-lock", validationLock,
		"-wait-timeout", validationLockWaitTimeout.String(),
		"--", os.Args[0],
	}
	firstReader, firstWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	secondReader, secondWriter, err := os.Pipe()
	if err != nil {
		firstReader.Close()
		firstWriter.Close()
		t.Fatal(err)
	}
	firstReady := newNotifyingWriter()
	firstDone := make(chan struct{})
	var firstCode int
	firstArgs := append(slices.Clone(args), "checklock-coverdir="+t.TempDir())
	go func() {
		firstCode = run(ctx, firstArgs, firstReader, firstReady, io.Discard)
		close(firstDone)
	}()
	t.Cleanup(func() {
		firstWriter.Close()
		secondWriter.Close()
		cancel()
		<-firstDone
		firstReader.Close()
		secondReader.Close()
	})
	select {
	case <-firstReady.notified:
	case <-firstDone:
		t.Fatalf("first run completed before readiness with code %d", firstCode)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	secondWait := newNotifyingWriter()
	secondDone := make(chan struct{})
	var secondCode int
	secondArgs := append(slices.Clone(args), "checklock-coverdir="+t.TempDir())
	go func() {
		secondCode = run(ctx, secondArgs, secondReader, io.Discard, secondWait)
		close(secondDone)
	}()
	t.Cleanup(func() {
		secondWriter.Close()
		cancel()
		<-secondDone
	})
	select {
	case <-secondWait.notified:
	case <-secondDone:
		t.Fatalf("second run completed before first released lock with code %d", secondCode)
	case <-ctx.Done():
		t.Fatalf("wait for second run contention: %v", ctx.Err())
	}
	if got := traceLineCount(t, tracePath); got != 1 {
		t.Fatalf("trace lines while first subprocess holds critical section = %d, want 1", got)
	}

	for _, writer := range []*os.File{firstWriter, secondWriter} {
		if _, err := writer.Write([]byte{1}); err != nil {
			t.Fatalf("write release signal: %v", err)
		}
	}
	for name, done := range map[string]<-chan struct{}{"first": firstDone, "second": secondDone} {
		select {
		case <-done:
			var code int
			if name == "second" {
				code = secondCode
			} else {
				code = firstCode
			}
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

func TestValidationWaiterCannotBeOvertaken(t *testing.T) {
	t.Parallel()
	for _, arrivals := range []int{1, 8} {
		t.Run(fmt.Sprintf("%d new arrivals", arrivals), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
			defer cancel()
			path := filepath.Join(t.TempDir(), "validation.lock")
			holder, err := instancelock.Acquire(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { holder.Close() })
			paused := &pausedWriter{ready: make(chan struct{}), resume: make(chan struct{})}
			t.Cleanup(func() { paused.release() })
			older := startValidationWaiter(ctx, path, paused)
			select {
			case <-paused.ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := holder.Close(); err != nil {
				t.Fatal(err)
			}
			newcomers := make([]<-chan validationResult, 0, arrivals)
			for range arrivals {
				writer := newNotifyingWriter()
				newcomer := startValidationWaiter(ctx, path, writer)
				select {
				case <-writer.notified:
				case result := <-newcomer:
					if result.lock != nil {
						result.lock.Close()
					}
					t.Fatalf("new arrival overtook a registered older waiter: %v", result.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				newcomers = append(newcomers, newcomer)
			}
			paused.release()
			for _, waiter := range append([]<-chan validationResult{older}, newcomers...) {
				select {
				case result := <-waiter:
					if result.err != nil {
						t.Fatal(result.err)
					}
					if _, err := instancelock.Acquire(path); !errors.Is(err, instancelock.ErrHeld) {
						t.Fatalf("active owner lost exclusivity: %v", err)
					}
					if err := result.lock.Close(); err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		})
	}
}

type validationResult struct {
	lock *instancelock.Lock
	err  error
}

func startValidationWaiter(ctx context.Context, path string, writer io.Writer) <-chan validationResult {
	done := make(chan validationResult, 1)
	go func() {
		lock, _, err := acquireValidationLock(ctx, path, writer)
		done <- validationResult{lock: lock, err: err}
	}()
	return done
}

type pausedWriter struct {
	ready  chan struct{}
	resume chan struct{}
	once   sync.Once
	closed sync.Once
}

func (w *pausedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.ready)
		<-w.resume
	})
	return len(data), nil
}

func (w *pausedWriter) release() {
	w.closed.Do(func() { close(w.resume) })
}

func TestRunCancellationStopsActiveValidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), validationIntegrationTimeout)
	defer cancel()
	path := filepath.Join(t.TempDir(), "validation.lock")
	criticalPath := filepath.Join(t.TempDir(), "critical.lock")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	ready := newNotifyingWriter()
	var stderr bytes.Buffer
	commandCtx, stopCommand := context.WithCancel(ctx)
	defer stopCommand()
	done := make(chan int, 1)
	coverageDir := t.TempDir()
	go func() {
		done <- run(commandCtx, []string{
			"-lock", path, "--", os.Args[0],
			"--", "checklock-cancel-child", criticalPath, "checklock-coverdir=" + coverageDir,
		}, reader, ready, &stderr)
	}()
	select {
	case <-ready.notified:
	case code := <-done:
		t.Fatalf("command exited before readiness: %d: %s", code, &stderr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, heldPath := range []string{path, criticalPath} {
		inspection, err := instancelock.Inspect(heldPath)
		if err != nil || inspection.Status != instancelock.StatusHeld {
			t.Fatalf("validation was not active before cancellation: %+v, %v", inspection, err)
		}
	}
	stopCommand()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatalf("canceled command returned success: %s", &stderr)
		}
	case <-ctx.Done():
		t.Fatal("active validation ignored cancellation")
	}
	for _, releasedPath := range []string{path, criticalPath} {
		acquireTestLock(t, releasedPath)
	}
}

func TestRunRetainsWaitAcrossHandoffs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		path := filepath.Join(t.TempDir(), "validation.lock")
		holder := acquireTestLock(t, path)
		older, err := registerValidationWaiter(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { older.lock.Close() })
		ownersDone := make(chan struct{})
		go func() {
			defer close(ownersDone)
			defer older.lock.Close()
			for i := range 4 {
				time.Sleep(400 * time.Millisecond)
				if err := holder.Close(); err != nil {
					t.Error(err)
					return
				}
				if i < 3 {
					var err error
					for {
						holder, err = instancelock.Acquire(path)
						if !errors.Is(err, instancelock.ErrHeld) {
							break
						}
						if err = waitValidationPoll(ctx); err != nil {
							break
						}
					}
					if err != nil {
						t.Error(err)
						return
					}
				}
			}
		}()
		var stderr bytes.Buffer
		code := run(ctx, []string{"-lock", path, "-wait-timeout", "1s", "--", filepath.Join(t.TempDir(), "missing-command")}, nil, io.Discard, &stderr)
		<-ownersDone
		if code != 1 || !strings.Contains(stderr.String(), "resolve validation command") || strings.Count(stderr.String(), "reason=owner_handoff") != 3 {
			t.Fatalf("run failed to reach command after healthy handoffs: code=%d stderr=%s", code, &stderr)
		}
	})
}
