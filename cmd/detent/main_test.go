package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cli"
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
	for _, want := range []string{"detent", "agent orchestrator", "Usage:", "Exit codes:", "2  auth", "4  not found"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestRootCommandWritesSuggestionErrorsAsJSON(t *testing.T) {
	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--format", "json", "paues"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want error")
	}
	if exitCode := handleCommandError(cmd, err); exitCode != cli.ExitValidation {
		t.Fatalf("handleCommandError() exit code = %d, want %d", exitCode, cli.ExitValidation)
	}

	var got struct {
		Error struct {
			Code       string   `json:"code"`
			ExitCode   int      `json:"exit_code"`
			Input      string   `json:"input"`
			DidYouMean []string `json:"did_you_mean"`
		} `json:"error"`
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if decodeErr := json.Unmarshal(stderr.Bytes(), &got); decodeErr != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", decodeErr, stderr.String())
	}
	if got.Error.Code != "unknown_command" {
		t.Fatalf("error.code = %q, want unknown_command", got.Error.Code)
	}
	if got.Error.ExitCode != cli.ExitValidation {
		t.Fatalf("error.exit_code = %d, want %d", got.Error.ExitCode, cli.ExitValidation)
	}
	if got.Error.Input != "paues" {
		t.Fatalf("error.input = %q, want paues", got.Error.Input)
	}
	if len(got.Error.DidYouMean) != 1 || got.Error.DidYouMean[0] != "pause" {
		t.Fatalf("error.did_you_mean = %#v, want [pause]", got.Error.DidYouMean)
	}
}

func TestHandleCommandErrorUsesSemanticExitCodes(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "context canceled", err: context.Canceled, want: cli.ExitSuccess},
		{name: "general", err: errors.New("boom"), want: cli.ExitGeneral},
		{name: "shutdown forced", err: cli.ErrShutdownForced, want: cli.ExitGeneral},
		{name: "auth", err: fmt.Errorf("wrapped: %w", cli.ErrGitHubAuth), want: cli.ExitAuth},
		{name: "validation", err: fmt.Errorf("wrapped: %w", cli.ErrValidation), want: cli.ExitValidation},
		{name: "not found", err: fmt.Errorf("wrapped: %w", cli.ErrProjectNotFound), want: cli.ExitNotFoundOrConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCommand(context.Background())
			cmd.SetErr(io.Discard)
			if got := handleCommandError(cmd, tt.err); got != tt.want {
				t.Fatalf("handleCommandError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestCommandErrorsUseSemanticExitCodes(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		cmd := newRootCommand(context.Background())
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"shadow-run"})

		err := cmd.Execute()
		if err == nil {
			t.Fatal("Execute() error = nil, want error")
		}
		if got := cli.ExitCode(err); got != cli.ExitValidation {
			t.Fatalf("ExitCode(%v) = %d, want %d", err, got, cli.ExitValidation)
		}
	})

	t.Run("not found", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "global.yaml")
		if err := os.WriteFile(configPath, []byte(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 1
  scheduling: weighted
projects: []
`), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		cmd := newRootCommand(context.Background())
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", configPath, "pause", "missing"})

		err := cmd.Execute()
		if err == nil {
			t.Fatal("Execute() error = nil, want error")
		}
		if got := cli.ExitCode(err); got != cli.ExitNotFoundOrConfig {
			t.Fatalf("ExitCode(%v) = %d, want %d", err, got, cli.ExitNotFoundOrConfig)
		}
	})

	t.Run("invalid global config", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "global.yaml")
		if err := os.WriteFile(configPath, []byte(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 1
  scheduling: weighted
projects:
  - id: detent
    workflow: WORKFLOW.md
    workdir: .
    weight: 1
`), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		cmd := newRootCommand(context.Background())
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", configPath})

		err := cmd.Execute()
		if err == nil {
			t.Fatal("Execute() error = nil, want error")
		}
		if got := cli.ExitCode(err); got != cli.ExitValidation {
			t.Fatalf("ExitCode(%v) = %d, want %d", err, got, cli.ExitValidation)
		}
	})
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
