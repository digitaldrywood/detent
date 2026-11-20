package codex

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func requestTurnSnapshot(ctx context.Context, transport Transport, thread string) error {
	return sendRequest(ctx, transport, threadReadRequestID, "thread/read", map[string]any{"threadId": thread, "includeTurns": true})
}

func finalAnswerForTurn(msg Message, thread, turn string) bool {
	if msg.Method != "item/completed" {
		return false
	}
	var params struct {
		ThreadID string
		TurnID   string
		Item     struct{ Type, Phase string }
	}
	return json.Unmarshal(msg.Params, &params) == nil && params.ThreadID == thread && params.TurnID == turn && params.Item.Type == "agentMessage" && params.Item.Phase == "final_answer"
}

func terminalTurnSnapshot(raw json.RawMessage, thread, turn string) (Update, bool) {
	var response struct {
		Thread struct {
			ID    string
			Turns []struct {
				ID     string
				Status string
				Error  json.RawMessage
			}
		}
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != thread {
		return Update{}, false
	}
	for _, item := range response.Thread.Turns {
		if item.ID != turn {
			continue
		}
		switch item.Status {
		case "completed", "failed", "interrupted":
			body, message := turnCompletedBackendError(nil, item.Error, "", "", nil)
			return Update{Type: UpdateTurnCompleted, Method: "thread/read", ThreadID: thread, TurnID: turn, Status: item.Status, BackendErrorBody: body, BackendErrorMessage: message}, true
		}
	}
	return Update{}, false
}

func (s *AppServer) reconcileTerminalTurn(ctx context.Context, thread, turn string) (result Update, confirmed bool) {
	ctx, cancel := context.WithTimeout(ctx, min(s.readTimeout, 5*time.Second))
	defer cancel()
	transport, err := s.transportFactory.NewTransport(ctx)
	if err != nil {
		return Update{}, false
	}
	defer func() {
		if err := closeTransport(ctx, transport, s.readTimeout); err != nil {
			s.logger.WarnContext(ctx, "close provider terminal reconciliation transport", "error", err)
		}
	}()
	if err := s.initialize(ctx, transport, nil); err != nil {
		return Update{}, false
	}
	if err := requestTurnSnapshot(ctx, transport, thread); err != nil {
		return Update{}, false
	}
	raw, err := s.awaitResponse(ctx, transport, threadReadRequestID, "", nil, nil, nil)
	if err != nil {
		return Update{}, false
	}
	return terminalTurnSnapshot(raw, thread, turn)
}

func finishProviderTurn(ctx context.Context, update Update, toolLoop *turnToolLoop) error {
	switch update.Status {
	case "", "completed":
		return nil
	case "interrupted":
		if toolLoop != nil && toolLoop.interrupted.Action != "" {
			return toolLoop.confirmStop(ctx)
		}
	}
	return &TurnFailedError{Status: update.Status, Message: update.BackendErrorMessage, Body: update.BackendErrorBody}
}

type turnReceive struct {
	message Message
	err     error
}

func readTurnStream(ctx context.Context, transport Transport, thread, turn string, terminal func()) (func(context.Context) (Message, error), func()) {
	readCtx, cancel := context.WithCancel(ctx)
	messages := make(chan turnReceive, progresscheck.ToolLoopWindow)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			message, err := transport.Receive(readCtx)
			if err != nil || confirmedTerminalMessage(message, thread, turn) {
				terminal()
			}
			select {
			case messages <- turnReceive{message: message, err: err}:
			case <-readCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return func(ctx context.Context) (Message, error) {
			select {
			case result := <-messages:
				return result.message, result.err
			case <-ctx.Done():
				return Message{}, context.Cause(ctx)
			}
		}, func() {
			cancel()
			<-done
		}
}

func confirmedTerminalMessage(message Message, thread, turn string) bool {
	if message.Method == "turn/completed" {
		update, _, err := updateFromMessage(message)
		return err == nil && update.ThreadID == thread && update.TurnID == turn && update.Status != "" && update.Status != "inProgress"
	}
	if message.Method == "" && requestIDMatches(message.ID, threadReadRequestID) {
		_, confirmed := terminalTurnSnapshot(message.Result, thread, turn)
		return confirmed
	}
	return false
}
