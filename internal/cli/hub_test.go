package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestHubServeCommandPassesConfiguration(t *testing.T) {
	t.Parallel()

	wantContext := context.WithValue(context.Background(), hubContextKey{}, "request")
	wantDatabase := filepath.Join(t.TempDir(), "hub.db")
	var gotContextValue string
	var gotConfig hubserver.Config
	run := func(ctx context.Context, cfg hubserver.Config) error {
		gotContextValue, _ = ctx.Value(hubContextKey{}).(string)
		gotConfig = cfg
		return nil
	}
	cmd := newHubCommandWithRun("v-test", run)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"serve",
		"--database", wantDatabase,
		"--listen", "127.0.0.1:0",
		"--busy-timeout", "2s",
		"--shutdown-timeout", "3s",
	})
	if err := cmd.ExecuteContext(wantContext); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if gotContextValue != "request" {
		t.Fatal("runner did not receive command context")
	}
	if gotConfig.DatabasePath != wantDatabase {
		t.Fatalf("database path = %q, want %q", gotConfig.DatabasePath, wantDatabase)
	}
	if gotConfig.ListenAddress != "127.0.0.1:0" {
		t.Fatalf("listen address = %q, want ephemeral loopback", gotConfig.ListenAddress)
	}
	if gotConfig.BusyTimeout != 2*time.Second {
		t.Fatalf("busy timeout = %s, want 2s", gotConfig.BusyTimeout)
	}
	if gotConfig.ShutdownTimeout != 3*time.Second {
		t.Fatalf("shutdown timeout = %s, want 3s", gotConfig.ShutdownTimeout)
	}
	if gotConfig.Version != "v-test" {
		t.Fatalf("version = %q, want v-test", gotConfig.Version)
	}
}

func TestHubServeCommandValidatesDatabase(t *testing.T) {
	t.Parallel()

	runErr := errors.New("runner should not be called")
	run := func(context.Context, hubserver.Config) error {
		return runErr
	}
	cmd := newHubCommandWithRun("v-test", run)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"serve"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want validation error")
	}
	if errors.Is(err, runErr) {
		t.Fatalf("ExecuteContext() error = %v, runner was called", err)
	}
}

type hubContextKey struct{}
