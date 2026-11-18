package codex

import (
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func earlyTurnFailure(msg Message, threadID, turnID string) *TurnFailedError {
	switch msg.Method {
	case "turn/completed", "error", "task_complete", "codex/event/task_complete", "codex/event/error":
	default:
		return nil
	}
	var params struct {
		TurnID         string          `json:"turnId"`
		ThreadID       string          `json:"threadId"`
		ConversationID string          `json:"conversationId"`
		WillRetry      bool            `json:"willRetry"`
		Status         any             `json:"status"`
		Error          json.RawMessage `json:"error"`
		Code           json.RawMessage `json:"codex_error_info"`
		Msg            json.RawMessage `json:"msg"`
		Turn           struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	if json.Unmarshal(msg.Params, &params) != nil || params.WillRetry {
		return nil
	}
	if id := firstNonBlank(params.ThreadID, params.ConversationID); id != "" && id != threadID {
		return nil
	}
	if id := firstNonBlank(params.TurnID, params.Turn.ID); turnID != "" && id != "" && id != turnID {
		return nil
	}
	if len(params.Msg) > 0 {
		msg.Params = params.Msg
		return earlyTurnFailure(msg, threadID, turnID)
	}
	code := turnErrorCode(params.Error)
	if code == "" {
		code = turnErrorCode(params.Turn.Error)
	}
	if code == "" {
		code = decodeTurnErrorCode(params.Code)
	}
	status := firstNonBlank(params.Turn.Status, turnCompletedTopLevelStatus(params.Status, params.Error, ""))
	if code == "" && compactNonNullJSON(params.Error) == "" && compactNonNullJSON(params.Turn.Error) == "" && (status == "" || status == "completed") {
		return nil
	}
	if code == "" {
		code = "other"
	}
	return &TurnFailedError{Status: "failed", ProviderCode: code, Operation: "turn/start", Message: code}
}

func turnErrorCode(raw json.RawMessage) string {
	var body struct {
		Code       json.RawMessage `json:"codexErrorInfo"`
		LegacyCode json.RawMessage `json:"codex_error_info"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	if code := decodeTurnErrorCode(body.Code); code != "" {
		return code
	}
	return decodeTurnErrorCode(body.LegacyCode)
}

func decodeTurnErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var code string
	if json.Unmarshal(raw, &code) == nil {
		return tracker.CodexErrorCode(code)
	}
	var variant map[string]json.RawMessage
	if json.Unmarshal(raw, &variant) == nil && len(variant) == 1 {
		for key := range variant {
			return tracker.CodexErrorCode(key)
		}
	}
	return "other"
}
