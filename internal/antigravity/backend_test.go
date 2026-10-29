package antigravity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		{mode: "error_result", wantError: "antigravity turn ERROR: invalid model selection"},
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
			var command *exec.Cmd
			backend, err := NewAgentBackend(Options{Provider: "google", Effort: "high", CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAntigravityProcess$", "--")
				command.Args = append(command.Args, args...)
				command.Env = append(os.Environ(), "DETENT_ANTIGRAVITY_FIXTURE="+test.mode, "GOCOVERDIR="+t.TempDir())
				return command
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			req := runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: "fixture-model", Prompt: "hello world", ToolInstructions: "use tools", MaxTurns: 3, Environment: procgroup.Environment{Variables: map[string]string{"DETENT_ANTIGRAVITY_MARKER": "propagated"}}}
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
			result, err := backend.RunTurn(ctx, req, func(update runner.AgentUpdate) error {
				updates = append(updates, update)
				if update.Type == runner.AgentUpdateToolStarted && test.cancel {
					cancel()
				}
				if update.Type == runner.AgentUpdateMessageDelta && test.reject {
					return errors.New("callback rejected")
				}
				return nil
			})
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
			if output != "hello world\n" || toolOutput != "one two" || toolCommand != "echo one" {
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
	if err := json.Unmarshal(scanner.Bytes(), &input); err != nil || input.Event != "user" || input.Message.Content != "hello world\n\nuse tools" {
		os.Exit(5)
	}
	if scanner.Scan() {
		os.Exit(6)
	}
	cwd, err := os.Stat(".")
	if err != nil || os.Getenv("DETENT_ANTIGRAVITY_MARKER") != "propagated" {
		os.Exit(7)
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
	step(map[string]any{"step_index": 3, "state": "ACTIVE", "step_type": "agent_response", "text_delta": "hello world"})
	step(map[string]any{"step_index": 3, "state": "DONE", "step_type": "agent_response", "text_delta": "\n", "usage": map[string]any{"input_tokens": 13, "output_tokens": 3, "thinking_tokens": 2, "cache_read_tokens": 4}})
	if mode == "no_result" {
		os.Exit(0)
	}
	send(map[string]any{"event": "result", "result": map[string]any{"conversation_id": "fixture-conversation", "status": "SUCCESS", "response": "hello world\n", "usage": map[string]any{"input_tokens": 23, "output_tokens": 8}}})
	os.Exit(0)
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
			backend, err := NewAgentBackend(Options{CommandFactory: func(context.Context, []string) *exec.Cmd {
				t.Fatal("restricted request launched Antigravity")
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if test.policy.Tier != "" {
				ctx = isolation.WithPolicy(ctx, test.policy)
			}
			_, err = backend.RunTurn(ctx, test.req, nil)
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
	backend, err := NewAgentBackend(Options{})
	if err != nil {
		t.Fatal(err)
	}
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

func TestRealAntigravitySmoke(t *testing.T) {
	if os.Getenv("DETENT_ANTIGRAVITY_SMOKE") != "1" {
		t.Skip("set DETENT_ANTIGRAVITY_SMOKE=1 with an installed, signed-in Antigravity CLI")
	}
	if _, err := exec.LookPath("agy"); err != nil {
		t.Skip("Antigravity CLI is unavailable")
	}
	dir := t.TempDir()
	backend, err := NewAgentBackend(Options{TurnTimeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var output string
	var tokens runner.AgentTokenUsage
	result, err := backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: os.Getenv("DETENT_ANTIGRAVITY_MODEL"), Prompt: "Reply with exactly AGY_SMOKE_OK. Do not use tools.", MaxTurns: 2}, func(u runner.AgentUpdate) error {
		switch u.Type {
		case runner.AgentUpdateMessageDelta:
			output += u.Delta
		case runner.AgentUpdateTokenUsage:
			tokens = u.Tokens
		}
		return nil
	})
	if err != nil || result.SessionID == "" || !strings.Contains(output, "AGY_SMOKE_OK") || tokens.TotalTokens == 0 {
		t.Fatalf("smoke = %#v, %q, %#v, %v", result, output, tokens, err)
	}
}
