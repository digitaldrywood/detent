package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRootCommandHelp(t *testing.T) {
	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{"symphony", "agent orchestrator", "Usage:"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestRootCommandWithoutArgsBootsFromGlobalConfig(t *testing.T) {
	t.Setenv("SYMPHONY_HOME", t.TempDir())

	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want missing global config")
	}
	if !strings.Contains(err.Error(), "read global config") {
		t.Fatalf("Execute() error = %v, want missing global config", err)
	}
}

func TestSignalContextCancel(t *testing.T) {
	ctx, cancel := newSignalContext(context.Background())
	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal context was not canceled")
	}
}
