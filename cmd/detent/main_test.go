package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/cli"
)

func TestRootCommandHelp(t *testing.T) {
	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{"detent", "agent orchestrator", "Usage:"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestSubcommandHelpShowsExampleBeforeUsage(t *testing.T) {
	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "update", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	examplesAt := strings.Index(output, "Examples:")
	usageAt := strings.Index(output, "Usage:")
	if examplesAt < 0 {
		t.Fatalf("help output missing Examples:\n%s", output)
	}
	if usageAt < 0 {
		t.Fatalf("help output missing Usage:\n%s", output)
	}
	if examplesAt > usageAt {
		t.Fatalf("Examples section appears after Usage:\n%s", output)
	}
	if !strings.Contains(output, "detent update --check --json") {
		t.Fatalf("help output missing update example:\n%s", output)
	}
}

func TestRegisteredCommandsHaveExamples(t *testing.T) {
	cmd := newRootCommand(context.Background())

	missing := commandsMissingExamples(cmd)
	if len(missing) > 0 {
		t.Fatalf("commands missing examples: %s", strings.Join(missing, ", "))
	}
}

func TestHandleCommandErrorReturnsSemanticExitCode(t *testing.T) {
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "context canceled", err: context.Canceled, want: cli.ExitSuccess},
		{name: "validation", err: cli.ValidationError("bad input"), want: cli.ExitValidation},
		{name: "not found", err: errors.Join(cli.ErrProjectNotFound, errors.New("missing")), want: cli.ExitNotFoundOrConfig},
		{name: "general", err: errors.New("boom"), want: cli.ExitGeneral},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := handleCommandError(tt.err); got != tt.want {
				t.Fatalf("handleCommandError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func commandsMissingExamples(cmd *cobra.Command) []string {
	var missing []string
	walkCommands(cmd, func(command *cobra.Command) {
		if strings.TrimSpace(command.Example) == "" {
			missing = append(missing, command.CommandPath())
		}
	})
	return missing
}

func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	if isGeneratedCommand(cmd) {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, visit)
	}
}

func isGeneratedCommand(cmd *cobra.Command) bool {
	return cmd.Name() == "help" || cmd.Name() == "completion" || strings.HasPrefix(cmd.Name(), cobra.ShellCompRequestCmd)
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
