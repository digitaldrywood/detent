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
		name, steerResult, terminalStatus   string
		parallel, wrongScope, changedSource bool
		methods                             []string
		stop                                bool
	}{
		{name: "acknowledged steer then stop", steerResult: `{"turnId":"turn"}`, terminalStatus: "interrupted", methods: []string{"turn/steer", "turn/interrupt"}, stop: true},
		{name: "unacknowledged steer", steerResult: `{"turnId":"other"}`, terminalStatus: "completed", methods: []string{"turn/steer"}},
		{name: "terminal completion won race", steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/interrupt"}},
		{name: "parallel siblings", parallel: true, terminalStatus: "completed"},
		{name: "another provider turn", wrongScope: true, terminalStatus: "completed"},
		{name: "changed source after steer", changedSource: true, steerResult: `{"turnId":"turn"}`, terminalStatus: "completed", methods: []string{"turn/steer", "turn/steer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var messages []Message
			turn := "turn"
			if test.wrongScope {
				turn = "other"
			}
			for ordinal := 1; ordinal <= 6; ordinal++ {
				item := fmt.Sprintf(`{"threadId":"thread","turnId":%q,"item":{"type":"commandExecution","id":"tool-%d","command":"go test ./...","cwd":"/workspace","status":"completed","exitCode":1,"aggregatedOutput":"failed"}}`, turn, ordinal)
				messages = append(messages, notificationMessage(t, "item/started", item))
				if !test.parallel {
					messages = append(messages, notificationMessage(t, "item/completed", item), notificationMessage(t, "item/completed", item))
				}
				if ordinal == 3 && test.steerResult != "" {
					messages = append(messages, responseMessage(t, loopSteerRequestID, test.steerResult))
				}
			}
			if test.parallel {
				for _, message := range slices.Clone(messages) {
					message.Method = "item/completed"
					messages = append(messages, message)
				}
			}
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
			err = server.streamTurn(lifetime, transport, "thread", "turn", nil, nil, nil, loop)
			var stopped *progresscheck.NoProgressError
			if test.stop {
				if !errors.As(err, &stopped) || stopped.Evidence.Repeats != 6 || stopped.Evidence.Period != 1 {
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
