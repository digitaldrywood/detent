package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func (s *conversationStore) materializeRestartedMessages(ctx context.Context, now time.Time) (int64, error) {
	var count int64
	cursor := ""
	for {
		ctx := newConversationArchiveReads(ctx)
		message, err := scanConversationMessage(s.reader.QueryRowContext(ctx, "SELECT "+conversationMessageReadColumns+" FROM conversation_messages WHERE id>? AND role='assistant' AND delivery='responding' AND conversation_id IN (SELECT id FROM conversations WHERE work_item_id IS NULL) ORDER BY id LIMIT 1", cursor))
		if errors.Is(err, sql.ErrNoRows) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		cursor = message.ID
		if err := s.resolveMessageContents(ctx, s.reader, []*conversationMessageRecord{&message}); err != nil {
			return count, err
		}
		var organization string
		if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization); err != nil {
			return count, err
		}
		content, err := messageContent(message)
		if err != nil {
			return count, err
		}
		ctx, err = s.preparePayload(ctx, organization, content)
		if err != nil {
			return count, err
		}
		message.Delivery, message.UpdatedAt = conversation.DeliveryUnknown, now
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return count, err
		}
		err = func() error {
			var current bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM conversation_messages WHERE id=? AND revision=? AND delivery='responding')", message.ID, message.Revision).Scan(&current); err != nil {
				return err
			}
			if !current {
				return conversationStale("Message changed during restart content preparation")
			}
			if err := s.updateMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			count++
			return nil
		}()
		if err := errors.Join(err, rollbackConversationPreparation(tx)); err != nil {
			return count, err
		}
	}
}
