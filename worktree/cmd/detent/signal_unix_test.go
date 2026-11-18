//go:build unix

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestShutdownSignalsUnix(t *testing.T) {
	signals := shutdownSignals()
	if len(signals) != 2 {
		t.Fatalf("shutdownSignals() length = %d, want 2", len(signals))
	}
	if signals[0] != os.Interrupt {
		t.Fatalf("shutdownSignals()[0] = %v, want %v", signals[0], os.Interrupt)
	}
	if signals[1] != syscall.SIGTERM {
		t.Fatalf("shutdownSignals()[1] = %v, want %v", signals[1], syscall.SIGTERM)
	}
}

func TestSignalContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := newSignalContext(context.Background())
	defer stop()

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess() error = %v", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal() error = %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal context was not canceled by SIGTERM")
	}
}
