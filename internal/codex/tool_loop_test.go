package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/sessiontimeout"
)

func TestStreamToolLoopControl(t *testing.T) {
	for _, test := range []struct {
		name, steerResult, terminalStatus           string
		parallel, wrongScope, changedSource         bool
		serializedSiblings, operator, freshEvidence bool
		methods                                     []string
		stop                                        bool
		lateAck                                     bool
	}{
		{name: "acknowledged steer then stop", steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "late acknowledgement delays stop", steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true, lateAck: true},
		{name: "unacknowledged steer", steerResult: `{"turnId":"other"}`, terminalStatus: "completed", methods: []string{"turn/steer"}},
		{name: "terminal completion won race", steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/interrupt"}},
		{name: "parallel siblings", parallel: true, terminalStatus: "completed"},
		{name: "serialized siblings", serializedSiblings: true, terminalStatus: "completed"},
		{name: "fresh sibling evidence each decision", freshEvidence: true, terminalStatus: "completed"},
		{name: "operator calls", operator: true, terminalStatus: "completed"},
		{name: "another provider turn", wrongScope: true, terminalStatus: "completed"},
		{name: "changed source after steer", changedSource: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/steer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var messages []Message
			turn := "turn"
			if test.wrongScope {
				turn = "other"
			}
			repeats := 6
			if test.lateAck {
				repeats = 7
			}
			for ordinal := 1; ordinal <= repeats; ordinal++ {
				if ordinal == 1 || !test.parallel && !test.serializedSiblings {
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
				item := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"commandExecution","id":"tool-%d","source":%q,"command":"go test ./...","cwd":"/workspace","status":"completed","exitCode":1,"aggregatedOutput":"failed"}}`, turn, ordinal, source)
				messages = append(messages, notificationMessage(t, "item/started", item))
				if !test.parallel {
					messages = append(messages, notificationMessage(t, "item/completed", item), notificationMessage(t, "item/completed", item))
				}
				if ordinal == 3 && !test.lateAck && test.steerResult != "" {
					messages = append(messages, responseMessage(t, loopSteerRequestID, test.steerResult))
				}
				if test.freshEvidence {
					sibling := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"commandExecution","id":"sibling-%d","source":"agent","command":"other check","cwd":"/workspace","status":"completed","exitCode":0,"aggregatedOutput":"new result %d"}}`, turn, ordinal, ordinal)
					messages = append(messages, notificationMessage(t, "item/started", sibling), notificationMessage(t, "item/completed", sibling))
				}
			}
			if test.parallel {
				for _, message := range slices.Clone(messages) {
					message.Method = "item/completed"
					messages = append(messages, message)
				}
			}
			marker := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"reasoning","id":"reason-next"}}`, turn)
			messages = append(messages, notificationMessage(t, "item/started", marker))
			messages = append(messages, notificationMessage(t, "turn/completed", `{"threadId":"thread","turn":{"id":"other","status":"interrupted"}}`), notificationMessage(t, "turn/completed", fmt.Sprintf(`{"threadId":"thread","turn":{"id":"turn","status":%q}}`, test.terminalStatus)))
			transport := &fakeAppServerTransport{received: messages}
			server, err := NewAppServer(staticTransportFactory{transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			loop := newTurnToolLoop(RunTurnRequest{DetectToolLoops: true, Workspace: "/workspace"}, "thread", "turn", server.logger)
			samples := 0
			loop.source = func(context.Context, string) string {
				samples++
				if test.changedSource && samples > 3 {
					return "changed"
				}
				return "unchanged"
			}
			lifetime := sessiontimeout.Start(t.Context(), 0, time.Minute, nil)
			defer lifetime.Close()
			err = server.streamTurn(t.Context(), lifetime, transport, "thread", "turn", nil, nil, nil, loop)
			var stopped *progresscheck.NoProgressError
			if test.stop {
				if !errors.As(err, &stopped) || stopped.Evidence.Repeats != repeats || stopped.Evidence.Period != 1 {
					t.Fatalf("stop = %v, evidence %+v", err, stopped)
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
