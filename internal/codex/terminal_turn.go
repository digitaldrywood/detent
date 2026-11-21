package codex

import (
	"context"
	"encoding/json"
	"time"
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

func legacyTurnCompletion(message Message, thread, turn string) (Update, bool) {
	switch message.Method {
	case "task_complete", "codex/event/task_complete":
	default:
		return Update{}, false
	}
	var event struct {
		ID             string
		ThreadID       string
		WireThreadID   string `json:"thread_id"`
		ConversationID string
		TurnID         string
		WireTurnID     string `json:"turn_id"`
		Msg            struct {
			Type         string
			ThreadID     string
			WireThreadID string `json:"thread_id"`
			TurnID       string
			WireTurnID   string `json:"turn_id"`
		}
	}
	if json.Unmarshal(message.Params, &event) != nil || (event.Msg.Type != "" && event.Msg.Type != "task_complete") {
		return Update{}, false
	}
	turnIDs := []string{event.TurnID, event.WireTurnID, event.Msg.TurnID, event.Msg.WireTurnID, event.ID}
	if firstNonBlank(turnIDs...) == "" {
		return Update{}, false
	}
	for _, id := range turnIDs {
		if id != "" && id != turn {
			return Update{}, false
		}
	}
	for _, id := range []string{event.ThreadID, event.WireThreadID, event.ConversationID, event.Msg.ThreadID, event.Msg.WireThreadID} {
		if id != "" && id != thread {
			return Update{}, false
		}
	}
	return Update{Type: UpdateTurnCompleted, Method: message.Method, ThreadID: thread, TurnID: turn, Status: "completed"}, true
}
