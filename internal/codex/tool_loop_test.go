package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestStreamToolLoopControl(t *testing.T) {
	for _, test := range []struct {
		name, steerResult, terminalStatus           string
		toolType, pollResult                        string
		parallel, wrongScope, changedOutput         bool
		serializedSiblings, operator, freshEvidence bool
		methods                                     []string
		stop                                        bool
		lateAck                                     bool
		freshThird, sealFirst                       bool
		customTool, nestedSibling                   bool
		missingResponse, unavailableSourceProbe     bool
	}{
		{name: "custom tool output joins its native execution without reasoning markers", sealFirst: true, customTool: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "custom tool with two native siblings supplies no repeat", sealFirst: true, customTool: true, nestedSibling: true, terminalStatus: "completed"},
		{name: "response closes before final tool result", sealFirst: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "third response contains new sibling evidence", freshThird: true, terminalStatus: "completed"},
		{name: "running MCP job", toolType: "mcpToolCall", pollResult: `"result":{"content":[],"structuredContent":{"job_id":"build-1","status":"running"}}`, terminalStatus: "completed"},
		{name: "running dynamic job", toolType: "dynamicToolCall", pollResult: `"contentItems":[{"type":"inputText","text":"{\"job_id\":\"build-1\",\"status\":\"running\"}"}]`, customTool: true, sealFirst: true, terminalStatus: "completed"},
		{name: "opaque job status", toolType: "mcpToolCall", pollResult: `"result":{"content":[{"type":"text","text":"The job is done"}]}`, terminalStatus: "completed"},
		{name: "unknown job status", toolType: "mcpToolCall", pollResult: `"result":{"job_id":"build-1","status":"unknown"}`, terminalStatus: "completed"},
		{name: "completed result for another job", toolType: "mcpToolCall", pollResult: `"result":{"content":[],"structuredContent":{"job_id":"build-2","status":"completed"}}`, terminalStatus: "completed"},
		{name: "completed MCP job", toolType: "mcpToolCall", pollResult: `"result":{"content":[],"structuredContent":{"job_id":"build-1","status":"completed"}}`, steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "missing dynamic job", toolType: "dynamicToolCall", pollResult: `"contentItems":[{"type":"inputText","text":"{\"job_id\":\"build-1\",\"status\":\"not_found\"}"}]`, customTool: true, sealFirst: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "reasoning markers without closed raw responses", missingResponse: true, terminalStatus: "completed"},
		{name: "source probe expires after acknowledged steer", unavailableSourceProbe: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "late acknowledgement delays stop", steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true, lateAck: true},
		{name: "unacknowledged steer", steerResult: `{"turnId":"other"}`, terminalStatus: "completed", methods: []string{"turn/steer"}},
		{name: "terminal completion won race", steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/interrupt"}},
		{name: "parallel siblings", parallel: true, terminalStatus: "completed"},
		{name: "serialized siblings", serializedSiblings: true, terminalStatus: "completed"},
		{name: "fresh sibling evidence each decision", freshEvidence: true, terminalStatus: "completed"},
		{name: "operator calls", operator: true, terminalStatus: "completed"},
		{name: "another provider turn", wrongScope: true, terminalStatus: "completed"},
		{name: "changed output after steer", changedOutput: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/steer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			root := "/workspace"
			var terminalReady chan struct{}
			if test.unavailableSourceProbe {
				root = t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("source"), 0o600); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", "tracked"}} {
					cmd := exec.CommandContext(ctx, "git", args...)
					cmd.Dir = root
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("prepare fixture with git %v: %v: %s", args, err, output)
					}
				}
				diffs := 0
				ctx = shell.WithCommandStarter(ctx, func(_ context.Context, cmd *exec.Cmd) (func() error, error) {
					if slices.Contains(cmd.Args, "diff") {
						diffs++
						if diffs == 4 {
							return nil, context.DeadlineExceeded
						}
					}
					return cmd.Wait, cmd.Start()
				})
				terminalReady = make(chan struct{})
			}
			var messages []Message
			turn := "turn"
			if test.wrongScope {
				turn = "other"
			}
			repeats := 6
			if test.lateAck {
				repeats = 7
			}
			if test.freshThird {
				repeats = 3
			}
			for ordinal := 1; ordinal <= repeats; ordinal++ {
				if !test.customTool && (ordinal == 1 || !test.parallel && !test.serializedSiblings) {
					marker := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"reasoning","id":"reason-%d"}}`, turn, ordinal)
					messages = append(messages, notificationMessage(t, "item/started", marker))
				}
				if ordinal == 4 && test.lateAck {
					messages = append(messages, responseMessage(t, loopSteerRequestID, test.steerResult))
				}
				source := "agent"
				if test.operator {
					source = "userShell"
				}
				output := "failed"
				if test.changedOutput && ordinal > 3 {
					output = "failed differently"
				}
				item := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"commandExecution","id":"tool-%d","source":%q,"command":"go test ./...","cwd":%q,"status":"completed","exitCode":1,"aggregatedOutput":%q}}`, turn, ordinal, source, root, output)
				if test.toolType != "" {
					item = fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":%q,"id":"tool-%d","server":"builder","namespace":"tools","tool":"job_status","status":"completed","arguments":{"job_id":"build-1"},%s}}`, turn, test.toolType, ordinal, test.pollResult)
				}
				if !test.customTool {
					messages = append(messages, notificationMessage(t, "item/started", item))
				}
				if test.customTool {
					messages = append(messages, notificationMessage(t, "rawResponseItem/completed", fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"custom_tool_call","call_id":"call-%d"}}`, turn, ordinal)))
				} else if source == "agent" && !test.missingResponse && !test.serializedSiblings {
					messages = append(messages, rawLoopCall(t, turn, fmt.Sprintf("tool-%d", ordinal)))
				}
				var sibling string
				if test.freshEvidence || test.freshThird && ordinal == 3 {
					messages = append(messages, rawLoopCall(t, turn, fmt.Sprintf("sibling-%d", ordinal)))
					sibling = fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"commandExecution","id":"sibling-%d","source":"agent","command":"other check","cwd":"/workspace","status":"completed","exitCode":0,"aggregatedOutput":"new result %d"}}`, turn, ordinal, ordinal)
				}
				sealed := notificationMessage(t, "rawResponse/completed", fmt.Sprintf(`{"threadId":"thread","turnId":%q,"responseId":"response-%d"}`, turn, ordinal))
				if test.sealFirst {
					messages = append(messages, sealed)
				}
				if test.customTool {
					messages = append(messages, notificationMessage(t, "item/started", item))
				}
				if !test.parallel {
					messages = append(messages, notificationMessage(t, "item/completed", item), notificationMessage(t, "item/completed", item))
				}
				if !test.sealFirst && !test.missingResponse && !test.serializedSiblings {
					messages = append(messages, sealed)
				}
				if sibling != "" {
					messages = append(messages, notificationMessage(t, "item/started", sibling), notificationMessage(t, "item/completed", sibling))
				}
				if test.nestedSibling {
					nested := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"fileChange","id":"nested-%d"}}`, turn, ordinal)
					messages = append(messages, notificationMessage(t, "item/started", nested), notificationMessage(t, "item/completed", nested))
				}
				if test.customTool {
					messages = append(messages, notificationMessage(t, "rawResponseItem/completed", fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"custom_tool_call_output","call_id":"call-%d"}}`, turn, ordinal)))
				}
				if ordinal == 3 && !test.lateAck && test.steerResult != "" {
					messages = append(messages, responseMessage(t, loopSteerRequestID, test.steerResult))
				}
			}
			if test.parallel {
				for _, message := range slices.Clone(messages) {
					if message.Method == "item/started" {
						message.Method = "item/completed"
						messages = append(messages, message)
					}
				}
			}
			marker := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"reasoning","id":"reason-next"}}`, turn)
			messages = append(messages, notificationMessage(t, "item/started", marker))
			messages = append(messages, notificationMessage(t, "turn/completed", `{"threadId":"thread","turn":{"id":"other","status":"interrupted"}}`), notificationMessage(t, "turn/completed", fmt.Sprintf(`{"threadId":"thread","turn":{"id":"turn","status":%q}}`, test.terminalStatus)))
			transport := &fakeAppServerTransport{received: messages}
			if terminalReady != nil {
				transport.pending = map[string]chan struct{}{"terminal": terminalReady}
			}
			server, err := NewAppServer(staticTransportFactory{transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			var recorded []tracker.ExecutionLoopIntervention
			loop := newTurnToolLoop(RunTurnRequest{
				DetectToolLoops: true,
				Workspace:       root,
				RecordLoopIntervention: func(event tracker.ExecutionLoopIntervention) {
					recorded = append(recorded, event)
				},
			}, "thread", "turn", server.logger)
			lifetime := sessiontimeout.Start(ctx, 0, time.Minute, nil)
			defer lifetime.Close()
			err = server.streamTurn(ctx, lifetime, transport, "thread", "turn", nil, nil, func(update Update) error {
				if terminalReady != nil && update.Type == UpdateToolCompleted && update.ItemID == "tool-6" {
					close(terminalReady)
					terminalReady = nil
				}
				return nil
			}, loop)
			var stopped *progresscheck.NoProgressError
			if test.stop {
				if !errors.As(err, &stopped) || stopped.Evidence.Repeats != repeats || stopped.Evidence.Period != 1 {
					t.Fatalf("stop = %v, evidence %+v", err, stopped)
				}
				if len(recorded) != 2 || recorded[0].Action != "steer" || recorded[0].Repeats != 3 ||
					recorded[1].Action != "stop" || recorded[1].Repeats != repeats ||
					recorded[0].Fingerprint == "" || recorded[0].Fingerprint != recorded[1].Fingerprint {
					t.Fatalf("confirmed interventions = %+v", recorded)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var methods []string
			for _, message := range transport.sentMessages() {
				methods = append(methods, message.Method)
				var params struct {
					ThreadID       string
					TurnID         string
					ExpectedTurnID string
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ThreadID != "thread" || firstNonBlank(params.TurnID, params.ExpectedTurnID) != "turn" {
					t.Fatalf("wrong control scope: %+v", params)
				}
			}
			if !slices.Equal(methods, test.methods) {
				t.Fatalf("control requests %v, want %v", methods, test.methods)
			}
		})
	}
}

func rawLoopCall(t *testing.T, turn, call string) Message {
	return notificationMessage(t, "rawResponseItem/completed", fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"function_call","call_id":%q}}`, turn, call))
}
