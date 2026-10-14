package piagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

// These cases catch accepting an ack as completion, broken response correlation,
// missing provider failures, duplicate streamed/final output or usage, and hangs
// when a peer stops reading or exits without a settled event.
func TestRunTurnRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	for _, test := range []struct {
		mode      string
		wantError string
		cancel    bool
		reject    bool
	}{
		{mode: "success"}, {mode: "symlink_workspace"}, {mode: "settled_before_ack"}, {mode: "retry"}, {mode: "no_delta"},
		{mode: "ack_only", wantError: "exited before completion"},
		{mode: "turn_limit", wantError: "session turn limit exceeded"},
		{mode: "max_duration", wantError: "context deadline exceeded"},
		{mode: "malformed", wantError: "invalid Pi RPC record"},
		{mode: "truncated", wantError: "unexpected EOF"},
		{mode: "wrong_id", wantError: "unexpected Pi RPC response"},
		{mode: "provider_error", wantError: "bad credentials"},
		{mode: "reject", wantError: "prompt rejected"},
		{mode: "handled", wantError: "did not start a run"},
		{mode: "eof", wantError: "exited before completion"},
		{mode: "mismatched_model", wantError: "differs from requested"},
		{mode: "success", wantError: "callback rejected", reject: true},
		{mode: "hang", wantError: "context canceled", cancel: true},
		{mode: "blocked_write", wantError: "context deadline exceeded"},
	} {
		t.Run(test.mode+fmt.Sprint(test.cancel, test.reject), func(t *testing.T) {
			dir := t.TempDir()
			if test.mode == "symlink_workspace" {
				link := filepath.Join(t.TempDir(), "workspace")
				if err := os.Symlink(dir, link); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("directory symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
				dir = link
			}
			var command *exec.Cmd
			backend, err := NewAgentBackend(Options{Provider: "fixture", ThinkingLevel: "high", CommandFactory: func(ctx context.Context, args []string) *exec.Cmd {
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPiRPCProcess$", "--")
				command.Args = append(command.Args, args...)
				command.Env = append(os.Environ(), "DETENT_PI_FIXTURE="+test.mode, "GOCOVERDIR="+t.TempDir())
				return command
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			req := runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: "fixture-model", Prompt: "hello\u2028world\u2029", MaxTurns: 3, Environment: procgroup.Environment{Variables: map[string]string{"DETENT_PI_MARKER": "propagated"}}}
			if test.mode == "turn_limit" {
				req.MaxTurns = 1
			}
			if test.mode == "max_duration" {
				req.MaxDuration = 500 * time.Millisecond
			}
			if test.mode == "blocked_write" {
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
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if command == nil || command.ProcessState == nil {
				t.Fatalf("process was not waited: %#v", command)
			}
			if len(updates) == 0 || updates[0].Type != runner.AgentUpdateProcessStarted || updates[0].WorkerProcess.PID == 0 {
				t.Fatal("process identity must precede RPC work")
			}
			if test.wantError != "" {
				return
			}
			if result.SessionID != "fixture-session" || result.ThreadID != result.SessionID {
				t.Fatalf("result = %#v", result)
			}
			var output, toolOutput string
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
				case runner.AgentUpdateToolStarted, runner.AgentUpdateToolCompleted:
					toolTypes = append(toolTypes, u.Type)
				}
			}
			if output != "hello\u2028world\u2029" || toolOutput != "one two" {
				t.Fatalf("output = %q, tool = %q", output, toolOutput)
			}
			if !reflect.DeepEqual(toolTypes, []runner.AgentUpdateType{runner.AgentUpdateToolStarted, runner.AgentUpdateToolCompleted}) {
				t.Fatalf("tool events = %v", toolTypes)
			}
			wantInput, wantTotal := int64(17), int64(22)
			if test.mode == "retry" {
				wantInput, wantTotal = 34, 44
			}
			if usage.InputTokens != wantInput || usage.CachedInputTokens != wantInput/17*4 || usage.OutputTokens != wantInput/17*5 || usage.TotalTokens != wantTotal {
				t.Fatalf("usage = %#v", usage)
			}
			if identity.Model != "fixture-model" || identity.RuntimeIdentity.Provider.Value != "fixture" || identity.RuntimeIdentity.ReasoningEffort.Value != "high" || identity.ProviderSessionID != result.SessionID {
				t.Fatalf("identity = %#v", identity)
			}
			if updates[len(updates)-1].Type != runner.AgentUpdateTurnCompleted || updates[len(updates)-1].Status != runner.FinalStateCompleted {
				t.Fatalf("last update = %#v", updates[len(updates)-1])
			}
		})
	}
}

// The process helper deliberately remains alive after completion, just like RPC.
// All fixture output and inherited environment stay inside the test scratch dir.
func TestPiRPCProcess(t *testing.T) {
	mode := os.Getenv("DETENT_PI_FIXTURE")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	next := func() map[string]any {
		if !scanner.Scan() {
			os.Exit(2)
		}
		var r map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			os.Exit(3)
		}
		return r
	}
	send := func(record any) {
		if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
			os.Exit(4)
		}
	}
	request := next()
	if request["type"] != "get_state" || request["id"] != "state" {
		os.Exit(5)
	}
	model := "fixture-model"
	if mode == "mismatched_model" {
		model = "different"
	}
	send(map[string]any{"type": "response", "id": "state", "command": "get_state", "success": true, "data": map[string]any{"sessionId": "fixture-session", "model": map[string]string{"id": model, "provider": "fixture"}, "thinkingLevel": "high"}})
	if mode == "blocked_write" {
		for {
			time.Sleep(time.Hour)
		}
	}
	request = next()
	if request["type"] != "prompt" || request["id"] != "prompt" || request["message"] != "hello\u2028world\u2029" {
		os.Exit(6)
	}
	cwd, err := os.Stat(".")
	if err != nil || os.Getenv("DETENT_PI_MARKER") != "propagated" {
		os.Exit(7)
	}
	// macOS may name the same directory through /var and /private/var.
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		temp, err := os.Stat(os.Getenv(key))
		if err != nil || !os.SameFile(cwd, temp) {
			fmt.Fprintf(os.Stderr, "fixture %s does not name the working directory\n", key)
			os.Exit(7)
		}
	}
	for _, arg := range []string{"--mode", "rpc", "--no-extensions", "--no-approve", "--no-skills", "--no-prompt-templates", "--provider", "fixture", "--model", "fixture-model", "--thinking", "high", "--tools"} {
		if !slices.Contains(os.Args, arg) {
			os.Exit(8)
		}
	}
	if mode == "malformed" {
		fmt.Fprintln(os.Stdout, "not json")
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "truncated" {
		fmt.Fprint(os.Stdout, `{"type":"agent_settled"}`)
		os.Exit(0)
	}
	if mode == "wrong_id" {
		send(map[string]any{"type": "response", "id": "wrong", "command": "prompt", "success": true})
		for {
			time.Sleep(time.Hour)
		}
	}
	if mode == "eof" {
		fmt.Fprintln(os.Stderr, "fixture early exit")
		os.Exit(9)
	}

	disposition := "started"
	if mode == "handled" {
		disposition = "handled"
	}
	if mode != "settled_before_ack" {
		send(map[string]any{"type": "response", "id": "prompt", "command": "prompt", "success": mode != "reject", "error": "prompt rejected", "data": map[string]string{"disposition": disposition}})
	}
	send(map[string]any{"type": "agent_start"})
	send(map[string]any{"type": "turn_start"})
	if mode == "turn_limit" {
		send(map[string]any{"type": "turn_start"})
	}
	if mode == "ack_only" {
		send(map[string]any{"type": "agent_end"})
		os.Exit(0)
	}
	send(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "hello"}})
	send(map[string]any{"type": "tool_execution_start", "toolCallId": "tool-1", "toolName": "bash", "args": map[string]string{"command": "echo one"}})
	if mode == "hang" || mode == "max_duration" {
		for {
			time.Sleep(time.Hour)
		}
	}
	send(map[string]any{"type": "tool_execution_update", "toolCallId": "tool-1", "toolName": "bash", "partialResult": map[string]any{"content": []map[string]string{{"type": "text", "text": "one"}}}})
	send(map[string]any{"type": "tool_execution_end", "toolCallId": "tool-1", "toolName": "bash", "result": map[string]any{"content": []map[string]string{{"type": "text", "text": "one two"}}}})
	message := map[string]any{"role": "assistant", "model": "fixture-model", "provider": "fixture", "stopReason": "stop", "content": []map[string]string{{"type": "text", "text": "hello\u2028world\u2029"}}, "usage": map[string]any{"input": 10, "cacheRead": 4, "cacheWrite": 3, "output": 5, "reasoning": 2, "totalTokens": 22}}
	if mode == "retry" {
		message["stopReason"] = "error"
		message["errorMessage"] = "temporary failure"
		send(map[string]any{"type": "message_start", "message": message})
		send(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]string{"type": "text_delta", "delta": ""}})
		send(map[string]any{"type": "message_end", "message": message})
		send(map[string]any{"type": "agent_end", "willRetry": true})
		message["stopReason"] = "stop"
	}
	if mode == "provider_error" {
		message["stopReason"] = "error"
		message["errorMessage"] = "bad credentials"
	}
	send(map[string]any{"type": "message_start", "message": message})
	if mode != "no_delta" {
		send(map[string]any{"type": "message_update", "usage": message["usage"], "assistantMessageEvent": map[string]string{"type": "text_delta", "delta": "hello\u2028world\u2029"}})
	}
	send(map[string]any{"type": "message_end", "message": message})
	// Must not count usage again in turn_end/agent_end.
	send(map[string]any{"type": "turn_end", "message": message})
	send(map[string]any{"type": "agent_end", "messages": []any{message}, "willRetry": false})
	send(map[string]any{"type": "agent_settled"})
	if mode == "settled_before_ack" {
		send(map[string]any{"type": "response", "id": "prompt", "command": "prompt", "success": mode != "reject", "error": "prompt rejected", "data": map[string]string{"disposition": disposition}})
	}
	for scanner.Scan() {
	}
	os.Exit(0)
}

func TestRestrictedRequestsNeverLaunchPi(t *testing.T) {
	for _, test := range []struct {
		name   string
		req    runner.AgentTurnRequest
		policy isolation.Policy
		want   error
	}{
		{name: "read only", req: runner.AgentTurnRequest{ReadOnly: true}},
		{name: "sandbox", policy: isolation.Policy{Tier: isolation.Sandbox}, want: isolation.ErrSandboxUnavailable},
		{name: "resume session", req: runner.AgentTurnRequest{Resume: runner.AgentResume{SessionID: "session"}}, want: runner.ErrAgentResumeUnsupported},
		{name: "resume thread", req: runner.AgentTurnRequest{Resume: runner.AgentResume{ThreadID: "thread"}}, want: runner.ErrAgentResumeUnsupported},
		{name: "dynamic tools", req: runner.AgentTurnRequest{SupplementalTools: true}},
		{name: "subscription auth", req: runner.AgentTurnRequest{RequireSubscriptionAuth: true}, want: runner.ErrSubscriptionAuthRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, err := NewAgentBackend(Options{CommandFactory: func(context.Context, []string) *exec.Cmd { t.Fatal("restricted request launched Pi"); return nil }})
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
		{err: &infrastructureError{err: context.Canceled}},
		{err: errors.New("provider rate limit; no structured status or reset")},
	} {
		details, ok := backend.ClassifyCapacityError(test.err, nil, time.Time{})
		if ok != test.want || details.Kind != test.kind {
			t.Errorf("classification(%v) = %#v, %v", test.err, details, ok)
		}
	}
}

func TestRealPiSmoke(t *testing.T) {
	if os.Getenv("DETENT_PI_SMOKE") != "1" {
		t.Skip("set DETENT_PI_SMOKE=1 with an installed, authenticated Pi CLI")
	}
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("Pi CLI is unavailable")
	}
	dir := t.TempDir()
	backend, err := NewAgentBackend(Options{Provider: os.Getenv("DETENT_PI_PROVIDER"), SessionDir: filepath.Join(dir, "sessions"), TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var output string
	result, err := backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: os.Getenv("DETENT_PI_MODEL"), Prompt: "Reply with exactly PI_SMOKE_OK. Do not use tools.", MaxTurns: 1}, func(u runner.AgentUpdate) error {
		if u.Type == runner.AgentUpdateMessageDelta {
			output += u.Delta
		}
		return nil
	})
	if err != nil || result.SessionID == "" || !strings.Contains(output, "PI_SMOKE_OK") {
		t.Fatalf("smoke = %#v, %q, %v", result, output, err)
	}
}
