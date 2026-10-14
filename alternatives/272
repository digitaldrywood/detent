//go:build windows

package main

import (
	"os"
	"testing"
)

func TestShutdownSignalsWindows(t *testing.T) {
	signals := shutdownSignals()
	if len(signals) != 1 {
		t.Fatalf("shutdownSignals() length = %d, want 1", len(signals))
	}
	if signals[0] != os.Interrupt {
		t.Fatalf("shutdownSignals()[0] = %v, want %v", signals[0], os.Interrupt)
	}
}
