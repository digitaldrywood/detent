package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestRunTurnStream(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	for _, test := range []struct {
		mode      string
		wantError string
		cancel    bool
		reject    bool
		capacity  bool
	}{
		{mode: "success"},
		{mode: "tools"},
		{mode: "error_result", wantError: "antigravity turn ERROR: invalid model selection", capacity: true},
		{mode: "no_result", wantError: "exited before a result", capacity: true},
		{mode: "no_init", wantError: "outside a conversation", capacity: true},
		{mode: "foreign_step", wantError: "different conversation", capacity: true},
		{mode: "malformed", wantError: "invalid Antigravity stream record", capacity: true},
		{mode: "truncated", wantError: "unexpected EOF", capacity: true},
		{mode: "turn_limit", wantError: "session turn limit exceeded"},
		{mode: "max_duration", wantError: "context deadline exceeded"},
		{mode: "unread_prompt", wantError: "context deadline exceeded", capacity: true},
		{mode: "success", wantError: "callback rejected", reject: true},
		{mode: "hang", wantError: "context canceled", cancel: true},
	} {
		t.Run(fmt.Sprintf("%s/%v/%v", test.mode, test.cancel, test.reject), func(t *testing.T) {
			dir := t.TempDir()
			home := t.TempDir()
			var command *exec.Cmd
			backend, err := NewAgentBackend(Options{Home: home, Provider: "google", Effort: "high", CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAntigravityProcess$", "--")
				command.Args = append(command.Args, args...)
				command.Env = append(os.Environ(), "DETENT_ANTIGRAVITY_FIXTURE="+test.mode, "DETENT_ANTIGRAVITY_EXPECTED_HOME="+home, "GOCOVERDIR="+t.TempDir())
				return command
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			req := runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: "fixture-model", Prompt: "hello\u2028world", ToolInstructions: "use tools", MaxTurns: 3, Environment: procgroup.Environment{Variables: map[string]string{"DETENT_ANTIGRAVITY_MARKER": "propagated"}}}
			switch test.mode {
			case "turn_limit":
				req.MaxTurns = 1
			case "max_duration":
				req.MaxDuration = 500 * time.Millisecond
			case "unread_prompt":
				req.Prompt = strings.Repeat("x", 1024*1024)
				req.TurnTimeout = 500 * time.Millisecond
			}
			var updates []runner.AgentUpdate
			var calls []runner.AgentToolCall
			tools := []runner.AgentTool{{Name: "attach_evidence", Description: "Attach evidence", InputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}}}`)}}
			handle := func(_ context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
				calls = append(calls, call)
				return runner.AgentToolResult{Success: true, Content: `{"status":"evidence-ok"}`}, nil
			}
			if test.mode != "tools" {
				tools = nil
			}
			result, err := backend.RunTurnWithTools(ctx, req, tools, handle, func(update runner.AgentUpdate) error {
				updates = append(updates, update)
				if update.Type == runner.AgentUpdateToolStarted && test.cancel {
					cancel()
				}
				if update.Type == runner.AgentUpdateMessageDelta && test.reject {
					return errors.New("callback rejected")
				}
				return nil
			})
			if leftovers, globErr := filepath.Glob(filepath.Join(dir, "antigravity-tools-*")); globErr != nil || len(leftovers) != 0 {
				t.Fatalf("tool directories left behind: %v %v", leftovers, globErr)
			}
			if command == nil || command.ProcessState == nil {
				t.Fatalf("process was not waited: %#v", command)
			}
			if len(updates) == 0 || updates[0].Type != runner.AgentUpdateProcessStarted || updates[0].WorkerProcess.PID == 0 {
				t.Fatal("process identity must precede stream work")
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				if got := backendcapacity.IsTransientOverload(err); got != test.capacity {
					t.Fatalf("capacity attribution = %v, want %v: %v", got, test.capacity, err)
				}
				if test.mode == "turn_limit" {
					var usage runner.AgentTokenUsage
					for _, u := range updates {
						if u.Type == runner.AgentUpdateTokenUsage {
							usage = u.Tokens
						}
					}
					if usage.TotalTokens != 17 {
						t.Fatalf("response over the turn limit was not reported: %#v", usage)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.SessionID != "fixture-conversation" || result.ThreadID != result.SessionID {
				t.Fatalf("result = %#v", result)
			}
			var output, toolOutput, toolCommand string
			var usage runner.AgentTokenUsage
			var identity runner.AgentUpdate
			var toolTypes []runner.AgentUpdateType
			for _, u := range updates {
				switch u.Type {
				case runner.AgentUpdateMessageDelta:
					output += u.Delta
				case runner.AgentUpdateTokenUsage:
					usage = u.Tokens
				case runner.AgentUpdateRuntimeIdentity:
					identity = u
				case runner.AgentUpdateToolOutput:
					toolOutput += u.Delta
				case runner.AgentUpdateToolStarted:
					toolCommand = u.Command
					toolTypes = append(toolTypes, u.Type)
				case runner.AgentUpdateToolCompleted:
					toolTypes = append(toolTypes, u.Type)
				}
			}
			wantOutput := "hello\u2028world\n"
			if test.mode == "tools" {
				if len(calls) != 1 || calls[0].Name != "attach_evidence" || string(calls[0].Arguments) != `{"note":"fixture"}` {
					t.Fatalf("tool calls = %#v", calls)
				}
				wantOutput = "evidence-ok" + wantOutput
			}
			if output != wantOutput || toolOutput != "one two" || toolCommand != "echo one" {
				t.Fatalf("output = %q, tool = %q, command = %q", output, toolOutput, toolCommand)
			}
			if !reflect.DeepEqual(toolTypes, []runner.AgentUpdateType{runner.AgentUpdateToolStarted, runner.AgentUpdateToolCompleted}) {
				t.Fatalf("tool events = %v", toolTypes)
			}
			want := runner.AgentTokenCounts{InputTokens: 27, CachedInputTokens: 4, OutputTokens: 11, ReasoningOutputTokens: 3, TotalTokens: 38}
			if usage.ThreadTotal == nil || *usage.ThreadTotal != want || usage.InputTokens != want.InputTokens || usage.TotalTokens != want.TotalTokens {
				t.Fatalf("usage = %#v", usage)
			}
			if identity.Model != "fixture-model" || identity.RuntimeIdentity.Provider.Value != "google" || identity.RuntimeIdentity.ReasoningEffort.Value != "high" || identity.ProviderSessionID != result.SessionID {
				t.Fatalf("identity = %#v", identity)
			}
			if last := updates[len(updates)-1]; last.Type != runner.AgentUpdateTurnCompleted || last.Status != runner.FinalStateCompleted {
				t.Fatalf("last update = %#v", last)
			}
		})
	}
}

func TestAntigravityProcess(t *testing.T) {
	mode := os.Getenv("DETENT_ANTIGRAVITY_FIXTURE")
	if mode == "" {
		return
	}
	send := func(record any) {
		if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
			os.Exit(4)
		}
	}
	step := func(fields map[string]any) {
		update := map[string]any{"conversation_id": "fixture-conversation"}
		for key, value := range fields {
			update[key] = value
		}
		send(map[string]any{"event": "step_update", "step_update": update})
	}
	for _, arg := range []string{"--input-format", "--output-format", "stream-json", "--dangerously-skip-permissions", "--disable-slash-commands", "--model", "fixture-model", "--effort", "high", "--print="} {
		if !slices.Contains(os.Args, arg) {
			fmt.Fprintf(os.Stderr, "fixture missing argument %s\n", arg)
			os.Exit(8)
		}
	}
	if mode == "unread_prompt" {
		for {
			time.Sleep(time.Hour)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	if !scanner.Scan() {
		os.Exit(2)
	}
	var input struct {
		Event   string `json:"event"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &input); err != nil || input.Event != "user" || input.Message.Content != "hello\u2028world\n\nuse tools" {
		os.Exit(5)
	}
	if scanner.Scan() {
		os.Exit(6)
	}
	cwd, err := os.Stat(".")
	if err != nil || os.Getenv("DETENT_ANTIGRAVITY_MARKER") != "propagated" {
		os.Exit(7)
	}
	home, err := os.Stat(os.Getenv("HOME"))
	expectedHome, expectedErr := os.Stat(os.Getenv("DETENT_ANTIGRAVITY_EXPECTED_HOME"))
	if err != nil || expectedErr != nil || !os.SameFile(home, expectedHome) {
		fmt.Fprintln(os.Stderr, "fixture HOME is not the dedicated Antigravity home")
		os.Exit(7)
	}
	toolDirectory := ""
	if index := slices.Index(os.Args, "--add-dir"); index >= 0 && index+1 < len(os.Args) {
		toolDirectory = os.Args[index+1]
	}
	if (mode == "tools") != (toolDirectory != "") {
		fmt.Fprintln(os.Stderr, "fixture tool directory does not match the mode")
		os.Exit(9)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		temp, err := os.Stat(os.Getenv(key))
		if err != nil || !os.SameFile(cwd, temp) {
			fmt.Fprintf(os.Stderr, "fixture %s does not name the working directory\n", key)
			os.Exit(7)
		}
	}
	switch mode {
	case "error_result":
		send(map[string]any{"event": "result", "result": map[string]any{"conversation_id": "", "status": "ERROR", "error": "invalid model selection"}})
		os.Exit(1)
	case "no_init":
		step(map[string]any{"step_index": 0, "state": "DONE", "step_type": "user_input"})
		os.Exit(0)
	case "malformed":
		fmt.Fprintln(os.Stdout, "not json")
		os.Exit(0)
	case "truncated":
		fmt.Fprint(os.Stdout, `{"event":"init","conversation_id":"fixture-conversation"}`)
		os.Exit(0)
	}
	send(map[string]any{"event": "init", "conversation_id": "fixture-conversation", "init": map[string]any{"permission_mode": "always-proceed"}})
	if mode == "foreign_step" {
		send(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": "other", "step_index": 0, "state": "DONE", "step_type": "user_input"}})
		os.Exit(0)
	}
	step(map[string]any{"step_index": 0, "state": "DONE", "step_type": "user_input"})
	if mode == "tools" {
		reply, err := callWorkerTool(toolDirectory)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(10)
		}
		step(map[string]any{"step_index": 1, "state": "ACTIVE", "step_type": "agent_response", "text_delta": reply})
	}
	step(map[string]any{"step_index": 1, "state": "DONE", "step_type": "agent_response", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "thinking_tokens": 1, "cache_read_tokens": 0}})
	if mode == "turn_limit" {
		step(map[string]any{"step_index": 2, "state": "DONE", "step_type": "agent_response", "usage": map[string]any{"input_tokens": 1}})
	}
	tool := map[string]any{"name": "run_command", "parameters": map[string]any{"CommandLine": "echo one"}}
	step(map[string]any{"step_index": 2, "state": "ACTIVE", "step_type": "tool", "tool_name": "run_command", "tool_info": tool})
	if mode == "hang" || mode == "max_duration" {
		for {
			time.Sleep(time.Hour)
		}
	}
	tool["output"] = "one two"
	step(map[string]any{"step_index": 2, "state": "DONE", "step_type": "tool", "tool_name": "run_command", "tool_info": tool})
	step(map[string]any{"step_index": 3, "state": "ACTIVE", "step_type": "agent_response", "text_delta": "hello\u2028world"})
	step(map[string]any{"step_index": 3, "state": "DONE", "step_type": "agent_response", "text_delta": "\n", "usage": map[string]any{"input_tokens": 13, "output_tokens": 3, "thinking_tokens": 2, "cache_read_tokens": 4}})
	if mode == "no_result" {
		os.Exit(0)
	}
	send(map[string]any{"event": "result", "result": map[string]any{"conversation_id": "fixture-conversation", "status": "SUCCESS", "response": "hello\u2028world\n", "usage": map[string]any{"input_tokens": 23, "output_tokens": 8}}})
	os.Exit(0)
}

func callWorkerTool(directory string) (string, error) {
	data, err := os.ReadFile(filepath.Join(directory, ".agents", "mcp_config.json"))
	if err != nil {
		return "", err
	}
	var config struct {
		Servers map[string]struct {
			URL     string            `json:"serverUrl"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", err
	}
	server, ok := config.Servers["detent"]
	if !ok || server.URL == "" {
		return "", errors.New("fixture tool config has no detent server")
	}
	session := ""
	post := func(body string) (string, error) {
		request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			return "", err
		}
		for key, value := range server.Headers {
			request.Header.Set(key, value)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			request.Header.Set("Mcp-Session-Id", session)
		}
		response, err := (&http.Client{Transport: &http.Transport{}}).Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		reply, err := io.ReadAll(response.Body)
		if err != nil {
			return "", err
		}
		if response.StatusCode >= 300 {
			return "", fmt.Errorf("fixture MCP status %d: %s", response.StatusCode, reply)
		}
		if id := response.Header.Get("Mcp-Session-Id"); id != "" {
			session = id
		}
		return string(reply), nil
	}
	if _, err := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`); err != nil {
		return "", err
	}
	if _, err := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return "", err
	}
	reply, err := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"attach_evidence","arguments":{"note":"fixture"}}}`)
	if err != nil {
		return "", err
	}
	if !strings.Contains(reply, "evidence-ok") {
		return "", fmt.Errorf("fixture tool reply = %s", reply)
	}
	return "evidence-ok", nil
}

func newRestrictedBackend(t *testing.T) *AgentBackend {
	t.Helper()
	backend, err := NewAgentBackend(Options{Home: t.TempDir(), CommandFactory: func(context.Context, []string) *exec.Cmd {
		t.Fatal("restricted request launched Antigravity")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestRestrictedRequestsNeverLaunchAntigravity(t *testing.T) {
	for _, test := range []struct {
		name   string
		req    runner.AgentTurnRequest
		policy isolation.Policy
		want   error
	}{
		{name: "read only", req: runner.AgentTurnRequest{ReadOnly: true}},
		{name: "text only", req: runner.AgentTurnRequest{TextOnly: true}},
		{name: "sandbox", policy: isolation.Policy{Tier: isolation.Sandbox}, want: isolation.ErrSandboxUnavailable},
		{name: "resume session", req: runner.AgentTurnRequest{Resume: runner.AgentResume{SessionID: "session"}}, want: runner.ErrAgentResumeUnsupported},
		{name: "resume thread", req: runner.AgentTurnRequest{Resume: runner.AgentResume{ThreadID: "thread"}}, want: runner.ErrAgentResumeUnsupported},
		{name: "dynamic tools", req: runner.AgentTurnRequest{SupplementalTools: true}},
		{name: "subscription auth", req: runner.AgentTurnRequest{RequireSubscriptionAuth: true}, want: runner.ErrSubscriptionAuthRequired},
		{name: "image attachment", req: runner.AgentTurnRequest{Attachments: []runner.AgentAttachment{{Name: "screen.png", MIME: "image/png", Content: []byte("png")}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newRestrictedBackend(t)
			ctx := t.Context()
			if test.policy.Tier != "" {
				ctx = isolation.WithPolicy(ctx, test.policy)
			}
			_, err := backend.RunTurn(ctx, test.req, nil)
			if err == nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if err := backend.VerifyResume(ctx, runner.AgentProcessRequest{}, test.req.Resume); !errors.Is(err, runner.ErrAgentResumeUnsupported) {
				t.Fatalf("VerifyResume = %v", err)
			}
		})
	}
}

func TestInfrastructureClassification(t *testing.T) {
	backend := newRestrictedBackend(t)
	for _, test := range []struct {
		err  error
		want bool
		kind string
	}{
		{err: &infrastructureError{err: errors.New("bad frame")}, want: true},
		{err: &infrastructureError{err: errors.New("launch failed"), startup: true}, want: true, kind: backendcapacity.StartupFailureKind},
		{err: &infrastructureError{err: context.DeadlineExceeded, startup: true}, want: true, kind: backendcapacity.StartupTimeoutKind},
		{err: &infrastructureError{err: context.DeadlineExceeded}},
		{err: &infrastructureError{err: context.Canceled}},
		{err: errors.New("antigravity turn ERROR: quota exhausted")},
	} {
		details, ok := backend.ClassifyCapacityError(test.err, nil, time.Time{})
		if ok != test.want || details.Kind != test.kind {
			t.Errorf("classification(%v) = %#v, %v", test.err, details, ok)
		}
	}
}

func TestNewAgentBackendRequiresLaunchContract(t *testing.T) {
	factory := func(context.Context, []string) *exec.Cmd { return nil }
	for _, options := range []Options{{Home: t.TempDir()}, {CommandFactory: factory}, {Home: "relative", CommandFactory: factory}} {
		if _, err := NewAgentBackend(options); err == nil {
			t.Errorf("NewAgentBackend(%#v) accepted an incomplete launch contract", options)
		}
	}
}
