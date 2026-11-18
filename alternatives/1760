package conversations

import (
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationMessageFact struct {
	TurnID    *string                  `json:"turn_id"`
	EmptyData bool                     `json:"empty_data"`
	ID        string                   `json:"id"`
	Role      conversation.Role        `json:"role"`
	Kind      conversation.MessageKind `json:"kind"`
	AttemptID *string                  `json:"attempt_id"`

	MessageID string                `json:"message_id"`
	ContentID string                `json:"content_id,omitempty"`
	Revision  int64                 `json:"message_revision"`
	Delivery  conversation.Delivery `json:"delivery"`
	TextBytes int                   `json:"text_bytes"`
	UpdatedAt time.Time             `json:"updated_at"`
}

type ConversationDeltaFact struct {
	Text      string `json:"text,omitempty"`
	Revision  int64  `json:"message_revision"`
	MessageID string `json:"message_id"`
	Offset    int64  `json:"seq"`
	Bytes     int    `json:"text_bytes"`
}

type conversationQuestionFact struct {
	QuestionID string                      `json:"question_id"`
	Status     conversation.QuestionStatus `json:"status"`
	AnsweredBy string                      `json:"answered_by"`
	UpdatedAt  time.Time                   `json:"updated_at"`
	Answered   bool                        `json:"answered"`
}

func compactConversationEvent(body any) any {
	switch value := body.(type) {
	case ConversationMessageResource:
		raw, ok := value.Data.(json.RawMessage)
		empty := !ok || len(raw) == 0 || string(raw) == "{}" || string(raw) == "null"
		return conversationMessageFact{ID: value.ID, TurnID: value.TurnID, EmptyData: empty, Role: value.Role, Kind: value.Kind, AttemptID: value.AttemptID, MessageID: value.ID, ContentID: value.contentID, Revision: value.revision, Delivery: value.Delivery, TextBytes: len(value.Text), UpdatedAt: value.UpdatedAt}
	case conversation.Question:
		return conversationQuestionFact{QuestionID: value.ID, Status: value.Status, AnsweredBy: value.AnsweredBy, UpdatedAt: value.UpdatedAt, Answered: value.Answers != nil}
	default:
		return body
	}
}
