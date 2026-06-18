package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	if !strings.HasPrefix(output, "Examples:\n") {
		t.Fatalf("help output does not lead with examples:\n%s", output)
	}
	for _, want := range []string{"detent", "agent orchestrator", "Usage:", "Exit codes:", "2  auth", "4  not found"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestRootCommandHelpCatalogJSON(t *testing.T) {
	cmd := newRootCommand(context.Background())

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Commands []struct {
			Name    string `json:"name"`
			Short   string `json:"short"`
			Example string `json:"example"`
			Flags   []struct {
				Name string `json:"name"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("help catalog is not JSON: %v\n%s", err, stdout.String())
	}

	wantCommands := map[string]bool{
		"detent":      false,
		"add-project": false,
		"config path": false,
		"doctor":      false,
		"update":      false,
		"version":     false,
		"shadow-run":  false,
	}
	for _, command := range got.Commands {
		if _, ok := wantCommands[command.Name]; ok {
			wantCommands[command.Name] = true
			if strings.TrimSpace(command.Short) == "" {
				t.Fatalf("command %q short description is empty", command.Name)
			}
			if strings.TrimSpace(command.Example) == "" {
				t.Fatalf("command %q example is empty", command.Name)
			}
		}
	}
	for command, found := range wantCommands {
		if !found {
			t.Fatalf("help catalog missing %q:\n%s", command, stdout.String())
		}
	}
}

func TestRegisteredCommandsHaveExamples(t *testing.T) {
	cmd := newRootCommand(context.Background())
	var missing []string
	collectCommandsWithoutExamples(cmd, &missing)
	if len(missing) > 0 {
		t.Fatalf("commands missing examples: %s", strings.Join(missing, ", "))
	}
}

func TestRunCLIWritesProblemJSONForUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runCLI(context.Background(), []string{"--format", "json", "paues"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3\nstderr:\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	var got struct {
		Type         string   `json:"type"`
		Title        string   `json:"title"`
		Detail       string   `json:"detail"`
		ExitCode     int      `json:"exit_code"`
		DidYouMean   []string `json:"did_you_mean"`
		SuggestedFix string   `json:"suggested_fix"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not problem JSON: %v\n%s", err, stderr.String())
	}
	if got.Type != "https://detent.dev/errors/unknown_command" {
		t.Fatalf("type = %q, want unknown_command", got.Type)
	}
	if got.Title != "Unknown command" {
		t.Fatalf("title = %q, want Unknown command", got.Title)
	}
	if got.ExitCode != code {
		t.Fatalf("exit_code = %d, want %d", got.ExitCode, code)
	}
	if !strings.Contains(got.Detail, "paues") {
		t.Fatalf("detail = %q, want typo", got.Detail)
	}
	if len(got.DidYouMean) != 1 || got.DidYouMean[0] != "pause" {
		t.Fatalf("did_you_mean = %#v, want pause", got.DidYouMean)
	}
	if !strings.Contains(got.SuggestedFix, "detent pause") {
		t.Fatalf("suggested_fix = %q, want detent pause", got.SuggestedFix)
	}
}

func TestRunCLIWritesProblemJSONForUnknownFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runCLI(context.Background(), []string{"--format", "json", "--hedless"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3\nstderr:\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	var got struct {
		Code       string   `json:"code"`
		ExitCode   int      `json:"exit_code"`
		DidYouMean []string `json:"did_you_mean"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not problem JSON: %v\n%s", err, stderr.String())
	}
	if got.Code != "unknown_flag" {
		t.Fatalf("code = %q, want unknown_flag", got.Code)
	}
	if got.ExitCode != code {
		t.Fatalf("exit_code = %d, want %d", got.ExitCode, code)
	}
	if len(got.DidYouMean) != 1 || got.DidYouMean[0] != "--headless" {
		t.Fatalf("did_you_mean = %#v, want --headless", got.DidYouMean)
	}
}

func TestRunCLIWritesProblemJSONForProjectNotFound(t *testing.T) {
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	workdirPath := filepath.Join(root, "repo")
	configPath := filepath.Join(root, "global.yaml")
	if err := os.Mkdir(workdirPath, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(workflowPath, []byte("name: test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(workflow) error = %v", err)
	}
	config := fmt.Sprintf(`apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 1
  scheduling: weighted
projects:
  - id: api
    workflow: %s
    workdir: %s
    weight: 1
    priority: 0
  - id: web
    workflow: %s
    workdir: %s
    weight: 1
    priority: 0
  - id: infra
    workflow: %s
    workdir: %s
    weight: 1
    priority: 0
`, workflowPath, workdirPath, workflowPath, workdirPath, workflowPath, workdirPath)
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runCLI(context.Background(), []string{"--config", configPath, "--format", "json", "pause", "ap"}, &stdout, &stderr)
	if code != cli.ExitNotFoundOrConfig {
		t.Fatalf("exit code = %d, want %d\nstderr:\n%s", code, cli.ExitNotFoundOrConfig, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}

	var got struct {
		Type         string   `json:"type"`
		Code         string   `json:"code"`
		Title        string   `json:"title"`
		Detail       string   `json:"detail"`
		ExitCode     int      `json:"exit_code"`
		SuggestedFix string   `json:"suggested_fix"`
		DidYouMean   []string `json:"did_you_mean"`
		DocsURL      string   `json:"docs_url"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not problem JSON: %v\n%s", err, stderr.String())
	}
	if got.Type != "https://detent.dev/errors/project_not_found" {
		t.Fatalf("type = %q, want project_not_found", got.Type)
	}
	if got.Code != "project_not_found" {
		t.Fatalf("code = %q, want project_not_found", got.Code)
	}
	if got.Title != "Project not found" {
		t.Fatalf("title = %q, want Project not found", got.Title)
	}
	if got.Detail != `project "ap" not found` {
		t.Fatalf("detail = %q, want project not found", got.Detail)
	}
	if got.ExitCode != code {
		t.Fatalf("exit_code = %d, want %d", got.ExitCode, code)
	}
	for _, want := range []string{"available: api, web, infra", `did you mean "api"?`} {
		if !strings.Contains(got.SuggestedFix, want) {
			t.Fatalf("suggested_fix missing %q:\n%s", want, got.SuggestedFix)
		}
	}
	if len(got.DidYouMean) != 1 || got.DidYouMean[0] != "api" {
		t.Fatalf("did_you_mean = %#v, want api", got.DidYouMean)
	}
	if got.DocsURL != "https://detent.dev/docs/cli#project-not-found" {
		t.Fatalf("docs_url = %q, want project-not-found docs", got.DocsURL)
	}
}

func TestRunCLIPrettyUnknownCommandPrintsHint(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := runCLI(context.Background(), []string{"paues", "--format", "pretty"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, want 3\nstderr:\n%s", code, stderr.String())
	}
	for _, want := range []string{"Error: unknown command", "Hint: Run detent pause instead."} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestRenderCommandErrorUsesSemanticExitCodes(t *testing.T) {
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
			if got := renderCommandError(cmd, tt.err, io.Discard, cli.OutputFormatPretty, nil); got != tt.want {
				t.Fatalf("renderCommandError(%v) = %d, want %d", tt.err, got, tt.want)
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

func TestWriteSignalShutdownNotice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request cli.ShutdownRequest
		want    string
	}{
		{
			name:    "drain",
			request: cli.ShutdownRequestDrain,
			want:    "shutdown requested; draining sessions, press Ctrl+C again to force quit",
		},
		{
			name:    "force",
			request: cli.ShutdownRequestForce,
			want:    "force quit requested; interrupting sessions",
		},
		{
			name: "stopping",
			want: "shutdown requested; stopping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			writeSignalShutdownNotice(&output, tt.request)
			if got := strings.TrimSpace(output.String()); got != tt.want {
				t.Fatalf("notice = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleShutdownSignalHardExitsOnForceInterrupt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupts := &shutdownInterruptStub{
		requests: []cli.ShutdownRequest{
			cli.ShutdownRequestDrain,
			cli.ShutdownRequestForce,
		},
	}
	var output bytes.Buffer
	var exits []int

	if done := handleShutdownSignal(interrupts, cancel, &output, func(code int) {
		exits = append(exits, code)
	}); done {
		t.Fatal("first interrupt stopped signal loop")
	}
	if len(exits) != 0 {
		t.Fatalf("first interrupt exit codes = %v, want none", exits)
	}
	select {
	case <-ctx.Done():
		t.Fatal("first interrupt canceled root context")
	default:
	}

	if done := handleShutdownSignal(interrupts, cancel, &output, func(code int) {
		exits = append(exits, code)
	}); !done {
		t.Fatal("force interrupt did not stop signal loop")
	}
	if len(exits) != 1 || exits[0] != cli.ExitGeneral {
		t.Fatalf("force interrupt exit codes = %v, want [%d]", exits, cli.ExitGeneral)
	}

	notice := output.String()
	for _, want := range []string{
		"shutdown requested; draining sessions, press Ctrl+C again to force quit",
		"force quit requested; interrupting sessions",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q:\n%s", want, notice)
		}
	}
}

func TestHandleShutdownSignalSuppressesHandledNotices(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupts := &shutdownInterruptStub{
		requests: []cli.ShutdownRequest{
			cli.ShutdownRequestDrain,
			cli.ShutdownRequestForce,
		},
		suppressNotices: true,
	}
	var output bytes.Buffer
	var exits []int

	if done := handleShutdownSignal(interrupts, cancel, &output, func(code int) {
		exits = append(exits, code)
	}); done {
		t.Fatal("first interrupt stopped signal loop")
	}
	if got := output.String(); got != "" {
		t.Fatalf("first interrupt wrote suppressed notice:\n%s", got)
	}
	select {
	case <-ctx.Done():
		t.Fatal("first interrupt canceled root context")
	default:
	}

	if done := handleShutdownSignal(interrupts, cancel, &output, func(code int) {
		exits = append(exits, code)
	}); !done {
		t.Fatal("force interrupt did not stop signal loop")
	}
	if got := output.String(); got != "" {
		t.Fatalf("force interrupt wrote suppressed notice:\n%s", got)
	}
	if len(exits) != 1 || exits[0] != cli.ExitGeneral {
		t.Fatalf("force interrupt exit codes = %v, want [%d]", exits, cli.ExitGeneral)
	}
}

type shutdownInterruptStub struct {
	requests        []cli.ShutdownRequest
	suppressNotices bool
	calls           int
}

func (s *shutdownInterruptStub) RequestInterruptKind() (cli.ShutdownRequest, bool) {
	if s.calls >= len(s.requests) {
		s.calls++
		return 0, false
	}
	request := s.requests[s.calls]
	s.calls++
	return request, request != 0
}

func (s *shutdownInterruptStub) SignalNoticesSuppressed() bool {
	return s.suppressNotices
}

func collectCommandsWithoutExamples(cmd *cobra.Command, missing *[]string) {
	name := cmd.CommandPath()
	if name == "" {
		name = cmd.Name()
	}
	switch cmd.Name() {
	case "completion":
		return
	}
	if strings.TrimSpace(cmd.Example) == "" {
		*missing = append(*missing, name)
	}
	for _, child := range cmd.Commands() {
		collectCommandsWithoutExamples(child, missing)
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
