package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

func TestSignalContextCancel(t *testing.T) {
	ctx, cancel := newSignalContext(context.Background())
	cancel()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal context was not canceled")
	}
}

func TestShadowRunCommandAllowsDiffAndWritesReport(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "shadow.json")
	input := `{
  "date": "2026-05-31",
  "now": "2026-05-31T12:00:00Z",
  "go": {
    "dispatch": {"dispatch_order": ["issue-go"]},
    "tokens": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}
  },
  "elixir": {
    "dispatch": {"dispatch_order": ["issue-elixir"]},
    "tokens": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 4}
  }
}`
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := newRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"shadow-run", "--input", inputPath, "--allow-diff"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(stdout.String(), `"has_differences": true`) {
		t.Fatalf("shadow report missing difference flag:\n%s", stdout.String())
	}
}

func TestShadowRunCommandFailsOnDiffByDefault(t *testing.T) {
	inputPath := filepath.Join(t.TempDir(), "shadow.json")
	input := `{
  "date": "2026-05-31",
  "go": {"dispatch": {"dispatch_order": ["issue-go"]}},
  "elixir": {"dispatch": {"dispatch_order": ["issue-elixir"]}}
}`
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := newRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"shadow-run", "--input", inputPath})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "shadow run differences found") {
		t.Fatalf("Execute() error = %v, want shadow run differences found", err)
	}
}
