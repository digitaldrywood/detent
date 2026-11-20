package codex

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
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

type terminalTransportPair struct {
	transports []Transport
}

func (p *terminalTransportPair) NewTransport(context.Context) (Transport, error) {
	if len(p.transports) == 0 {
		return nil, errors.New("unexpected additional provider transport")
	}
	transport := p.transports[0]
	p.transports = p.transports[1:]
	return transport, nil
}

func TestRunTurnPreservesCompletionConfirmedAtProviderDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := newBlockingAppServerTransport([]Message{
			responseMessage(t, initializeRequestID, `{}`),
			responseMessage(t, threadStartRequestID, `{"thread":{"id":"thread"}}`),
			responseMessage(t, turnStartRequestID, `{"turn":{"id":"turn"}}`),
			notificationMessage(t, "item/completed", `{"threadId":"thread","turnId":"turn","item":{"id":"final","type":"agentMessage","phase":"final_answer","text":"Done"}}`),
			responseMessage(t, threadReadRequestID, `{"thread":{"id":"thread","turns":[{"id":"turn","status":"inProgress"}]}}`),
		})
		reconcile := &fakeAppServerTransport{received: []Message{
			responseMessage(t, initializeRequestID, `{}`),
			responseMessage(t, threadReadRequestID, `{"thread":{"id":"thread","turns":[{"id":"turn","status":"completed"}]}}`),
		}}
		factory := &terminalTransportPair{transports: []Transport{first, reconcile}}
		server, err := NewAppServer(factory)
		if err != nil {
			t.Fatal(err)
		}
		completions := 0
		result, err := server.RunTurn(t.Context(), RunTurnRequest{Model: "example-model", DetectToolLoops: true, TurnTimeout: time.Second}, func(update Update) error {
			if update.Type == UpdateTurnCompleted {
				completions++
			}
			return nil
		})
		if err != nil || result.ThreadID != "thread" || result.TurnID != "turn" || completions != 1 || len(factory.transports) != 0 {
			t.Fatalf("result=%+v error=%v completions=%d unused transports=%d", result, err, completions, len(factory.transports))
		}
	})
}
