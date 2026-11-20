package codex

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/sessiontimeout"
)

func TestReconcileScopedTerminalAfterTenCalls(t *testing.T) {
	for _, test := range []struct {
		name, result, method string
		notification         bool
	}{
		{name: "matching notification", result: `{"thread":{"id":"thread","turns":[{"id":"other","status":"completed"}]}}`, method: "turn/completed", notification: true},
		{name: "missing notification", result: `{"thread":{"id":"thread","turns":[{"id":"turn","status":"completed"}]}}`, method: "thread/read"},
		{name: "stale thread snapshot", result: `{"thread":{"id":"other","turns":[{"id":"turn","status":"completed"}]}}`, method: "turn/completed", notification: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var messages []Message
			for ordinal := 1; ordinal <= 10; ordinal++ {
				marker := fmt.Sprintf(`{"threadId":"thread","turnId":"turn","item":{"type":"reasoning","id":"reason-%d"}}`, ordinal)
				messages = append(messages, notificationMessage(t, "item/started", marker))
				command := fmt.Sprintf("exec %d", ordinal)
				if ordinal == 3 || ordinal == 6 || ordinal == 9 {
					command = "wait"
				}
				item := fmt.Sprintf(`{"threadId":"thread","turnId":"turn","item":{"id":"tool-%d","type":"commandExecution","command":%q,"cwd":"/workspace","status":"completed","exitCode":0,"aggregatedOutput":"checked"}}`, ordinal, command)
				messages = append(messages, notificationMessage(t, "item/started", item), notificationMessage(t, "item/completed", item))
			}
			messages = append(messages, notificationMessage(t, "item/completed", `{"threadId":"thread","turnId":"turn","item":{"id":"final","type":"agentMessage","phase":"final_answer","text":"Done"}}`))
			messages = append(messages, responseMessage(t, threadReadRequestID, test.result))
			messages = append(messages, notificationMessage(t, "turn/completed", `{"threadId":"thread","turn":{"id":"other","status":"completed"}}`))
			if test.notification {
				messages = append(messages, notificationMessage(t, "turn/completed", `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`))
			}
			transport := &fakeAppServerTransport{received: messages}
			server, err := NewAppServer(staticTransportFactory{transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			loop := newTurnToolLoop(RunTurnRequest{DetectToolLoops: true, Workspace: "/workspace"}, "thread", "turn", server.logger)
			lifetime := sessiontimeout.Start(t.Context(), 0, time.Minute, nil)
			defer lifetime.Close()
			var calls, completions int
			err = server.streamTurn(t.Context(), lifetime, transport, "thread", "turn", nil, nil, func(update Update) error {
				if update.Type == UpdateToolCompleted {
					calls++
				}
				if update.Type == UpdateTurnCompleted {
					completions++
					if update.Method != test.method || update.TurnID != "turn" {
						t.Fatalf("unexpected completion: %+v", update)
					}
				}
				return nil
			}, loop)
			if err != nil || calls != 10 || completions != 1 {
				t.Fatalf("completed after %d calls and %d terminal events: %v", calls, completions, err)
			}
			if sent := transport.sentMessages(); len(sent) != 1 || sent[0].Method != "thread/read" {
				t.Fatalf("control requests = %+v", sent)
			}
		})
	}
}
