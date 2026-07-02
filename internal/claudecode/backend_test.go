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
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestAgentBackendRunTurnSuccess(t *testing.T) {
	t.Parallel()

	backend := newTestBackend(t, Options{
		CommandFactory: fixtureCommand(t, "success.jsonl", "", 0),
		StallTimeout:   time.Second,
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
	if got := requireUpdate(t, updates, 4).Tokens; got.InputTokens != 10 || got.CachedInputTokens != 5 || got.OutputTokens != 4 || got.TotalTokens != 14 {
		t.Fatalf("message token usage = %#v, want 10 input, 5 cached, 4 output, 14 total", got)
	}
	if got := requireUpdate(t, updates, 5).Tokens; got.InputTokens != 12 || got.CachedInputTokens != 6 || got.OutputTokens != 6 || got.TotalTokens != 18 {
		t.Fatalf("final token usage = %#v, want 12 input, 6 cached, 6 output, 18 total", got)
	}
	if got := requireUpdate(t, updates, 6); got.Status != runner.FinalStateCompleted {
		t.Fatalf("TurnCompleted status = %q, want completed", got.Status)
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
		AllowedTools:           []string{"Bash(git *)", "Edit"},
		DisallowedTools:        []string{"WebFetch"},
		IncludePartialMessages: true,
		ExtraArgs:              []string{"--custom", "value"},
	})

	_, err := backend.RunTurn(context.Background(), runner.AgentTurnRequest{
		Workspace:          workspace,
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
		script := "cat " + shellQuote(path)
		if stderr != "" {
			script += "; printf '%s' " + shellQuote(stderr) + " >&2"
		}
		if exitCode != 0 {
			script += fmt.Sprintf("; exit %d", exitCode)
		}
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
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
