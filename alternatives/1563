package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
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

type conversationDeltaFact struct {
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
	case conversationMessageResource:
		raw, ok := value.Data.(json.RawMessage)
		empty := !ok || len(raw) == 0 || string(raw) == "{}" || string(raw) == "null"
		return conversationMessageFact{ID: value.ID, TurnID: value.TurnID, EmptyData: empty, Role: value.Role, Kind: value.Kind, AttemptID: value.AttemptID, MessageID: value.ID, ContentID: value.contentID, Revision: value.revision, Delivery: value.Delivery, TextBytes: len(value.Text), UpdatedAt: value.UpdatedAt}
	case conversation.Question:
		return conversationQuestionFact{QuestionID: value.ID, Status: value.Status, AnsweredBy: value.AnsweredBy, UpdatedAt: value.UpdatedAt, Answered: value.Answers != nil}
	default:
		return body
	}
}

func (s *conversationStore) resolveEventContent(ctx context.Context, query nativeQueryer, id string, events []conversation.Event) error {
	ctx = context.WithValue(ctx, conversationReadContentsKey{}, conversationContentReads{})
	for index := range events {
		event := &events[index]
		resolved, err := resolveStorageJSONWith(ctx, event.Data, func(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
			if s.contents == nil {
				return nil, errors.New("conversation content storage is unavailable")
			}
			return s.contents.ReadContent(ctx, organization, ref)
		})
		if err != nil {
			return err
		}
		event.Data = resolved
		var body any
		switch event.Type {
		case conversation.EventMessageAccepted, conversation.EventMessageUpdated:
			var fact conversationMessageFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			if fact.MessageID == "" || fact.Revision == 0 {
				continue
			}
			message, err := scanConversationMessage(query.QueryRowContext(ctx, "SELECT "+conversationMessageColumns+" FROM conversation_messages WHERE conversation_id=? AND id=?", id, fact.MessageID))
			if err != nil {
				return fmt.Errorf("resolve event message: %w", err)
			}
			if fact.ContentID != "" {
				message.ContentID = fact.ContentID
			} else if message.Kind == conversation.MessageTool && message.Actor.Kind == conversation.ActorCoordinator {
				var text, tool, item, status string
				err := query.QueryRowContext(ctx, `SELECT text,tool,item_id,status FROM conversation_tool_revisions WHERE conversation_id=? AND message_id=? AND revision=?`, id, fact.MessageID, fact.Revision).Scan(&text, &tool, &item, &status)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("resolve inline tool revision: %w", err)
				}
				if err == nil {
					message.Text, message.ContentID = text, ""
					message.Data, err = json.Marshal(map[string]string{"tool": tool, "item_id": item, "status": status})
					if err != nil {
						return err
					}
				}
			}
			if err := s.resolveMessageContents(ctx, query, []*conversationMessageRecord{&message}); err != nil {
				return err
			}
			if fact.TextBytes < 0 || fact.TextBytes > len(message.Text) {
				return errors.New("event message text boundary is invalid")
			}
			message.Text = message.Text[:fact.TextBytes]
			message.Delivery, message.UpdatedAt = fact.Delivery, fact.UpdatedAt
			message.AttemptID = ""
			if fact.AttemptID != nil {
				message.AttemptID = *fact.AttemptID
			}
			message.TurnID = ""
			if fact.TurnID != nil {
				message.TurnID = *fact.TurnID
			}
			if fact.EmptyData {
				message.Data = json.RawMessage("{}")
			}
			messages := []*conversationMessageRecord{&message}
			if err := resolveMessageReferences(ctx, query, messages); err != nil {
				return err
			}
			if err := resolveMessageAttachments(ctx, query, messages); err != nil {
				return err
			}
			body = projectMessage(message)
		case conversation.EventMessageDelta:
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &fields); err != nil {
				return err
			}
			if _, ok := fields["text_bytes"]; !ok {
				continue
			}
			var fact conversationDeltaFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			message, err := scanConversationMessage(query.QueryRowContext(ctx, "SELECT "+conversationMessageColumns+" FROM conversation_messages WHERE conversation_id=? AND id=?", id, fact.MessageID))
			if err != nil {
				return err
			}
			if err := s.resolveMessageContents(ctx, query, []*conversationMessageRecord{&message}); err != nil {
				return err
			}
			if fact.Offset < 0 || fact.Bytes < 0 || fact.Offset > int64(len(message.Text))-int64(fact.Bytes) {
				return errors.New("event delta text boundary is invalid")
			}
			text := message.Text[fact.Offset : fact.Offset+int64(fact.Bytes)]
			body = conversationDeltaBody(fact.MessageID, fact.Offset, text)
		case conversation.EventQuestionOpened, conversation.EventQuestionUpdated:
			var fact conversationQuestionFact
			if err := json.Unmarshal(event.Data, &fact); err != nil {
				return err
			}
			if fact.QuestionID == "" {
				continue
			}
			question, err := scanConversationQuestion(query.QueryRowContext(ctx, "SELECT "+conversationQuestionColumns+" FROM conversation_questions WHERE conversation_id=? AND id=?", id, fact.QuestionID))
			if err != nil {
				return fmt.Errorf("resolve event question: %w", err)
			}
			question.Status, question.AnsweredBy, question.UpdatedAt = fact.Status, fact.AnsweredBy, fact.UpdatedAt
			if !fact.Answered {
				question.Answers = nil
			}
			body = question.Question
		default:
			continue
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		event.Data = encoded
	}
	return nil
}
