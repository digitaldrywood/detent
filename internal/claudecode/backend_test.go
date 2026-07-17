package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestAgentBackendRunTurnSuccess(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory: fixtureCommand(t, "success.jsonl", "", 0),
	})

	var updates []runner.AgentUpdate
	result, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "ship it",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ThreadID != "session-success" || result.TurnID != "session-success" || result.SessionID != "session-success" {
		t.Fatalf("RunTurn() result = %#v, want session-success IDs", result)
	}

	wantTypes := []runner.AgentUpdateType{
		runner.AgentUpdateProcessStarted,
		runner.AgentUpdateTurnStarted,
		runner.AgentUpdateMessageDelta,
		runner.AgentUpdateMessageDelta,
		runner.AgentUpdateTokenUsage,
		runner.AgentUpdateTokenUsage,
		runner.AgentUpdateTurnCompleted,
	}
	if got := updateTypes(updates); !reflect.DeepEqual(got, wantTypes) {
		t.Fatalf("update types = %#v, want %#v", got, wantTypes)
	}
	firstDelta := requireUpdate(t, updates, 2)
	secondDelta := requireUpdate(t, updates, 3)
	if firstDelta.Delta != "hello " || secondDelta.Delta != "world" {
		t.Fatalf("message deltas = %q %q, want hello/world", firstDelta.Delta, secondDelta.Delta)
	}
	if got := requireUpdate(t, updates, 1).Model; got != "fable" {
		t.Fatalf("TurnStarted model = %q, want fable", got)
	}
	if got := requireUpdate(t, updates, 1).RuntimeIdentity; got.ResolvedModel != (agentidentity.Value{Value: "fable", Provenance: agentidentity.ProvenanceRuntime}) || !got.ReasoningEffort.IsZero() {
		t.Fatalf("TurnStarted runtime identity = %#v, want observed model and unavailable effort", got)
	}
	if got := requireUpdate(t, updates, 4).Tokens; got.InputTokens != 15 || got.CachedInputTokens != 3 || got.OutputTokens != 4 || got.ReasoningOutputTokens != 2 || got.TotalTokens != 19 {
		t.Fatalf("message token usage = %#v, want 15 input, 3 cached, 4 output, 2 reasoning, 19 total", got)
	}
	if got := requireUpdate(t, updates, 4).Model; got != "fable" {
		t.Fatalf("message token model = %q, want fable", got)
	}
	if got := requireUpdate(t, updates, 5).Tokens; got.InputTokens != 18 || got.CachedInputTokens != 4 || got.OutputTokens != 6 || got.ReasoningOutputTokens != 3 || got.TotalTokens != 24 {
		t.Fatalf("final token usage = %#v, want 18 input, 4 cached, 6 output, 3 reasoning, 24 total", got)
	}
	if got := requireUpdate(t, updates, 6); got.Status != runner.FinalStateCompleted || got.Model != "fable" {
		t.Fatalf("TurnCompleted status = %q, want completed", got.Status)
	}
}

func TestAgentBackendEmitsLaterAssistantModelChange(t *testing.T) {
	t.Parallel()

	fixture := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"session-model-change","model":"fable"}`,
		`{"type":"assistant","session_id":"session-model-change","message":{"id":"msg-model-change","type":"message","role":"assistant","model":"qwen3-coder","content":[{"type":"text","text":"updated"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"result","subtype":"success","session_id":"session-model-change","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n")
	fixturePath := filepath.Join(t.TempDir(), "model-change.jsonl")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	backend := newTestBackend(t, Options{CommandFactory: catCommand(fixturePath, "", 0)})

	var updates []runner.AgentUpdate
	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "observe model change",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var modelUpdates []runner.AgentUpdate
	for _, update := range updates {
		if update.Type == runner.AgentUpdateModelUpdated {
			modelUpdates = append(modelUpdates, update)
		}
	}
	if len(modelUpdates) != 1 {
		t.Fatalf("model updates = %#v, want one later assistant change", modelUpdates)
	}
	if got := modelUpdates[0].RuntimeIdentity.ResolvedModel; got != (agentidentity.Value{Value: "qwen3-coder", Provenance: agentidentity.ProvenanceRuntime}) {
		t.Fatalf("resolved model = %#v, want runtime qwen3-coder", got)
	}
	if got := requireLastUpdate(t, updates).Model; got != "qwen3-coder" {
		t.Fatalf("completed model = %q, want qwen3-coder", got)
	}
}

func TestClaudeUsageAgentUsageNormalizesCacheTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		usage claudeUsage
		want  runner.AgentTokenUsage
	}{
		{
			name: "cache creation and reads",
			usage: claudeUsage{
				InputTokens:              50,
				CacheCreationInputTokens: 20,
				CacheReadInputTokens:     100_000,
				OutputTokens:             5,
			},
			want: runner.AgentTokenUsage{
				InputTokens:       100_070,
				CachedInputTokens: 100_000,
				OutputTokens:      5,
				TotalTokens:       100_075,
			},
		},
		{
			name: "reported total larger than normalized total",
			usage: claudeUsage{
				InputTokens:           10,
				CachedInputTokens:     3,
				OutputTokens:          4,
				TotalTokens:           25,
				ReasoningOutputTokens: 2,
			},
			want: runner.AgentTokenUsage{
				InputTokens:           13,
				CachedInputTokens:     3,
				OutputTokens:          4,
				ReasoningOutputTokens: 2,
				TotalTokens:           25,
			},
		},
		{
			name: "reported total cannot undercount cache tokens",
			usage: claudeUsage{
				InputTokens:          10,
				CacheReadInputTokens: 90,
				OutputTokens:         4,
				TotalTokens:          14,
			},
			want: runner.AgentTokenUsage{
				InputTokens:       100,
				CachedInputTokens: 90,
				OutputTokens:      4,
				TotalTokens:       104,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.usage.agentUsage(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("agentUsage() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAgentBackendRunTurnErrorResult(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory: fixtureCommand(t, "error_result.jsonl", "claude stderr tail", 0),
	})

	var updates []runner.AgentUpdate
	result, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "hit max turns",
		Model:     "fable",
	}, appendUpdate(&updates))
	if !errors.Is(err, ErrTurnFailed) {
		t.Fatalf("RunTurn() error = %v, want ErrTurnFailed", err)
	}
	for _, want := range []string{"error_max_turns", "claude stderr tail"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("RunTurn() error = %q, want %q", err.Error(), want)
		}
	}
	if result.SessionID != "session-error" {
		t.Fatalf("RunTurn() result SessionID = %q, want session-error", result.SessionID)
	}
	if got := requireLastUpdate(t, updates); got.Type != runner.AgentUpdateTurnCompleted || got.Status != runner.FinalStateFailed {
		t.Fatalf("last update = %#v, want failed TurnCompleted", got)
	}
}

func TestFinalTurnErrorAfterStreamClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		canceled bool
		state    turnState
		waitErr  error
		wantErr  error
		wantText string
	}{
		{name: "cancellation takes precedence", canceled: true, waitErr: errors.New("process exited"), wantErr: context.Canceled},
		{name: "uncanceled missing result", waitErr: errors.New("exit status 9"), wantErr: ErrMissingResult, wantText: "process exited: exit status 9"},
		{name: "late cancellation preserves success", canceled: true, state: turnState{sawResult: true, resultSubtype: "success"}},
		{name: "late cancellation preserves result error", canceled: true, state: turnState{sawResult: true, resultIsError: true, resultSubtype: "error_during_execution"}, wantErr: ErrTurnFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := finalTurnError(ctx, tt.state, tt.waitErr, "")
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("finalTurnError() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("finalTurnError() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("finalTurnError() error = %q, want text %q", err, tt.wantText)
			}
		})
	}
}

func TestAgentBackendRunTurnSkipsMalformedLines(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory: fixtureCommand(t, "malformed.jsonl", "", 0),
	})

	var updates []runner.AgentUpdate
	result, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "continue after malformed",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.SessionID != "session-malformed" {
		t.Fatalf("RunTurn() SessionID = %q, want session-malformed", result.SessionID)
	}
	if got := joinedDeltas(updates); got != "after malformed" {
		t.Fatalf("message output = %q, want after malformed", got)
	}
}

func TestAgentBackendRunTurnReadsOversizedLine(t *testing.T) {
	t.Parallel()

	longText := strings.Repeat("x", 70*1024)
	fixture := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"session-oversized","model":"fable"}`,
		fmt.Sprintf(`{"type":"assistant","session_id":"session-oversized","message":{"id":"msg-oversized","type":"message","role":"assistant","model":"fable","content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":1}}}`, longText),
		`{"type":"result","subtype":"success","session_id":"session-oversized","usage":{"input_tokens":1,"output_tokens":1}}`,
	}, "\n")
	fixturePath := filepath.Join(t.TempDir(), "oversized.jsonl")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	backend := newTestBackend(t, Options{
		CommandFactory: catCommand(fixturePath, "", 0),
	})

	var updates []runner.AgentUpdate
	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "large line",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if got := joinedDeltas(updates); got != longText {
		t.Fatalf("message output length = %d, want %d", len(got), len(longText))
	}
}

func TestAgentBackendRunTurnMissingSessionID(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory: fixtureCommand(t, "missing_session.jsonl", "", 0),
	})

	var updates []runner.AgentUpdate
	result, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "missing session",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.SessionID != "" || result.ThreadID != "" || result.TurnID != "" {
		t.Fatalf("RunTurn() result = %#v, want empty IDs", result)
	}
	for _, update := range updates {
		if update.Type == runner.AgentUpdateTurnStarted {
			t.Fatalf("unexpected TurnStarted update without session id: %#v", update)
		}
	}
	if got := joinedDeltas(updates); got != "no session" {
		t.Fatalf("message output = %q, want no session", got)
	}
}

func TestAgentBackendRunTurnPartialMessages(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory:         fixtureCommand(t, "partial.jsonl", "", 0),
		IncludePartialMessages: true,
	})

	var updates []runner.AgentUpdate
	result, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "stream partials",
		Model:     "fable",
	}, appendUpdate(&updates))
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.SessionID != "session-partial" {
		t.Fatalf("RunTurn() SessionID = %q, want session-partial", result.SessionID)
	}
	if got := joinedDeltas(updates); got != "partial text" {
		t.Fatalf("message output = %q, want partial text", got)
	}
}

func TestAgentBackendBuildsCommandArgumentsAndWritesPromptToStdin(t *testing.T) {
	t.Parallel()

	observedPath := filepath.Join(t.TempDir(), "observed.json")
	workspace := t.TempDir()
	backend := newTestBackend(t, Options{
		CommandFactory: func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeHelperProcess", "--")
			cmd.Env = append(os.Environ(),
				"CLAUDECODE_HELPER=argv",
				"CLAUDECODE_OBSERVED_PATH="+observedPath,
			)
			return cmd
		},
		PermissionMode:         "plan",
		Effort:                 "high",
		AllowedTools:           []string{"Bash(git *)", "Edit"},
		DisallowedTools:        []string{"WebFetch"},
		IncludePartialMessages: true,
		ExtraArgs:              []string{"--custom", "value"},
	})

	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:          workspace,
		TempDir:            filepath.Join(workspace, ".detent", "tmp"),
		Prompt:             "prompt from stdin",
		Model:              "fable",
		ExtraWritableRoots: []string{"/tmp/root-a", " ", "/tmp/root-b"},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var observed helperObservation
	raw, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "fable",
		"--effort", "high",
		"--permission-mode", "plan",
		"--allowedTools", "Bash(git *)", "Edit",
		"--disallowedTools", "WebFetch",
		"--include-partial-messages",
		"--add-dir", "/tmp/root-a",
		"--add-dir", "/tmp/root-b",
		"--custom", "value",
	}
	if !reflect.DeepEqual(observed.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", observed.Args, wantArgs)
	}
	if observed.Stdin != "prompt from stdin" {
		t.Fatalf("stdin = %q, want prompt from stdin", observed.Stdin)
	}
	if got, want := canonicalPath(t, observed.Workdir), canonicalPath(t, workspace); got != want {
		t.Fatalf("workdir = %q, want %q", got, want)
	}
	for name, got := range map[string]string{
		"TMPDIR": observed.TMPDIR,
		"TMP":    observed.TMP,
		"TEMP":   observed.TEMP,
	} {
		if want := filepath.Join(workspace, ".detent", "tmp"); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestAgentBackendReadOnlyTurnOverridesWritableConfiguration(t *testing.T) {
	t.Parallel()

	observedPath := filepath.Join(t.TempDir(), "observed.json")
	backend := newTestBackend(t, Options{
		CommandFactory: func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeHelperProcess", "--")
			cmd.Env = append(os.Environ(),
				"CLAUDECODE_HELPER=argv",
				"CLAUDECODE_OBSERVED_PATH="+observedPath,
			)
			return cmd
		},
		PermissionMode:  "bypassPermissions",
		AllowedTools:    []string{"Bash", "Edit", "Write"},
		DisallowedTools: []string{"Read"},
		ExtraArgs:       []string{"--dangerously-skip-permissions"},
	})

	workspace := t.TempDir()
	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:          workspace,
		Prompt:             "Inspect maintenance criteria.",
		Model:              "fable",
		ReadOnly:           true,
		ExtraWritableRoots: []string{"/tmp/writable"},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var observed helperObservation
	raw, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "fable",
		"--permission-mode", "plan",
		"--safe-mode",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-chrome",
		"--tools", "Read", "Glob", "Grep",
	}
	if !reflect.DeepEqual(observed.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", observed.Args, wantArgs)
	}
}

func TestAgentBackendAddsResumeSessionArgument(t *testing.T) {
	t.Parallel()

	observedPath := filepath.Join(t.TempDir(), "observed.json")
	backend := newTestBackend(t, Options{
		CommandFactory: func(ctx context.Context) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeHelperProcess", "--")
			cmd.Env = append(os.Environ(),
				"CLAUDECODE_HELPER=argv",
				"CLAUDECODE_OBSERVED_PATH="+observedPath,
			)
			return cmd
		},
	})

	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace: t.TempDir(),
		Prompt:    "continue from prior session",
		Model:     "fable",
		Resume: runner.AgentResume{
			SessionID: "session-existing",
		},
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var observed helperObservation
	raw, err := os.ReadFile(observedPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	wantArgs := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--model", "fable",
		"--resume", "session-existing",
	}
	if !reflect.DeepEqual(observed.Args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", observed.Args, wantArgs)
	}
	if observed.Stdin != "continue from prior session" {
		t.Fatalf("stdin = %q, want resume prompt", observed.Stdin)
	}
}

func TestAgentBackendRequestTurnTimeoutOverridesOptions(t *testing.T) {
	t.Parallel()

	observedPath := filepath.Join(t.TempDir(), "observed.json")
	var gotRemaining time.Duration
	backend := newTestBackend(t, Options{
		CommandFactoryWithArgs: func(ctx context.Context, _ []string) *exec.Cmd {
			deadline, ok := ctx.Deadline()
			if ok {
				gotRemaining = time.Until(deadline)
			}
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeHelperProcess", "--")
			cmd.Env = append(os.Environ(),
				"CLAUDECODE_HELPER=argv",
				"CLAUDECODE_OBSERVED_PATH="+observedPath,
			)
			return cmd
		},
		TurnTimeout: time.Hour,
	})

	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:   t.TempDir(),
		Prompt:      "prompt from stdin",
		TurnTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if gotRemaining == 0 {
		t.Fatal("command context deadline is zero, want request turn timeout")
	}
	if gotRemaining <= 0 || gotRemaining > 10*time.Second {
		t.Fatalf("command context deadline remaining = %v, want request timeout", gotRemaining)
	}
}

func TestClaudeCodeHelperProcess(t *testing.T) {
	if os.Getenv("CLAUDECODE_HELPER") != "argv" {
		return
	}

	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	workdir, err := os.Getwd()
	if err != nil {
		os.Exit(3)
	}
	observed := helperObservation{
		Args:    argsAfterDashDash(os.Args),
		Stdin:   string(stdin),
		Workdir: workdir,
		TMPDIR:  os.Getenv("TMPDIR"),
		TMP:     os.Getenv("TMP"),
		TEMP:    os.Getenv("TEMP"),
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(os.Getenv("CLAUDECODE_OBSERVED_PATH"), raw, 0o600); err != nil {
		os.Exit(5)
	}
	fmt.Fprintln(os.Stdout, `{"type":"system","subtype":"init","session_id":"session-argv","model":"fable"}`)
	fmt.Fprintln(os.Stdout, `{"type":"result","subtype":"success","session_id":"session-argv"}`)
	os.Exit(0)
}

func TestPackageDoesNotImportConfigOrCLI(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s) error = %v", entry.Name(), err)
		}
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if path == "github.com/digitaldrywood/detent/internal/config" ||
				path == "github.com/digitaldrywood/detent/internal/cli" {
				t.Fatalf("%s imports forbidden package %s", entry.Name(), path)
			}
		}
	}
}

type helperObservation struct {
	Args    []string `json:"args"`
	Stdin   string   `json:"stdin"`
	Workdir string   `json:"workdir"`
	TMPDIR  string   `json:"tmpdir"`
	TMP     string   `json:"tmp"`
	TEMP    string   `json:"temp"`
}

func newTestBackend(t *testing.T, options Options) *AgentBackend {
	t.Helper()

	backend, err := NewAgentBackend(options)
	if err != nil {
		t.Fatalf("NewAgentBackend() error = %v", err)
	}
	return backend
}

func fixtureCommand(t *testing.T, name string, stderr string, exitCode int) CommandFactory {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Abs() error = %v", err)
	}
	return catCommand(path, stderr, exitCode)
}

func catCommand(path string, stderr string, exitCode int) CommandFactory {
	return func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestClaudeCodeFixtureProcess", "--")
		cmd.Env = append(os.Environ(),
			"CLAUDECODE_HELPER=fixture",
			"CLAUDECODE_FIXTURE_PATH="+path,
			"CLAUDECODE_FIXTURE_STDERR="+stderr,
			fmt.Sprintf("CLAUDECODE_FIXTURE_EXIT_CODE=%d", exitCode),
		)
		return cmd
	}
}

func TestClaudeCodeFixtureProcess(t *testing.T) {
	if os.Getenv("CLAUDECODE_HELPER") != "fixture" {
		return
	}

	raw, err := os.ReadFile(os.Getenv("CLAUDECODE_FIXTURE_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := os.Stdout.Write(raw); err != nil {
		os.Exit(3)
	}
	if stderr := os.Getenv("CLAUDECODE_FIXTURE_STDERR"); stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	exitCode, err := strconv.Atoi(os.Getenv("CLAUDECODE_FIXTURE_EXIT_CODE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	os.Exit(exitCode)
}

func appendUpdate(updates *[]runner.AgentUpdate) runner.AgentUpdateHandler {
	return func(update runner.AgentUpdate) error {
		*updates = append(*updates, update)
		return nil
	}
}

func updateTypes(updates []runner.AgentUpdate) []runner.AgentUpdateType {
	types := make([]runner.AgentUpdateType, 0, len(updates))
	for _, update := range updates {
		types = append(types, update.Type)
	}
	return types
}

func requireUpdate(t *testing.T, updates []runner.AgentUpdate, index int) runner.AgentUpdate {
	t.Helper()

	if len(updates) <= index {
		t.Fatalf("updates len = %d, want index %d", len(updates), index)
	}
	return updates[index]
}

func requireLastUpdate(t *testing.T, updates []runner.AgentUpdate) runner.AgentUpdate {
	t.Helper()

	if len(updates) == 0 {
		t.Fatal("updates len = 0, want at least one update")
	}
	return updates[len(updates)-1]
}

func joinedDeltas(updates []runner.AgentUpdate) string {
	var out strings.Builder
	for _, update := range updates {
		if update.Type == runner.AgentUpdateMessageDelta {
			out.WriteString(update.Delta)
		}
	}
	return out.String()
}

func argsAfterDashDash(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			return append([]string(nil), args[i+1:]...)
		}
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
	}
	return canonical
}
