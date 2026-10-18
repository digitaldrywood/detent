package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

func requireConversationRemoval(record conversationRecord) error {
	if conversationExecutionLive(record.Execution.Status) || conversationRunnerBound(record.Execution) {
		return conversationStale("Finish or stop the running turn before archiving or deleting the conversation")
	}
	return nil
}

func (s *Service) archiveConversation(c echo.Context) error {
	value, err := s.changeConversation(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), func(ctx context.Context, tx *sql.Tx, scope nativeScope, record *conversationRecord, _ time.Time) error {
		if err := s.conversations.authorizeWrite(ctx, tx, scope, *record); err != nil {
			return err
		}
		if err := requireConversationRemoval(*record); err != nil {
			return err
		}
		record.Archived = true
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, value)
}

func (s *Service) deleteConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	id := c.Param("conversation")
	service := s.conversations
	ctx := c.Request().Context()
	err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := service.authorizeWrite(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		if err := requireConversationRemoval(record); err != nil {
			return err
		}
		return service.store.deleteConversation(ctx, tx, record)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service.broker.notify(id)
	return c.NoContent(http.StatusNoContent)
}

func (s *conversationStore) deleteConversation(ctx context.Context, tx *sql.Tx, record conversationRecord) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_references WHERE conversation_id = ? OR (target_kind = 'conversation' AND target_id = ?)`, record.ID, record.ID); err != nil {
		return fmt.Errorf("delete conversation references: %w", err)
	}
	statements := []string{
		`DELETE FROM conversation_attachment_blobs WHERE artifact_ref IN (SELECT artifact_ref FROM conversation_attachments WHERE conversation_id = ?)`,
		`DELETE FROM conversation_attachments WHERE conversation_id = ?`,
		`DELETE FROM conversation_turn_batches WHERE attempt_id IN (SELECT attempt_id FROM conversation_starts WHERE conversation_id = ?)`,
		`DELETE FROM conversation_starts WHERE conversation_id = ?`,
		`DELETE FROM conversation_audience_events WHERE conversation_id = ?`,
		`DELETE FROM conversation_events WHERE conversation_id = ?`,
		`DELETE FROM conversation_commands WHERE conversation_id = ?`,
		`DELETE FROM conversation_questions WHERE conversation_id = ?`,
		`DELETE FROM conversation_messages WHERE conversation_id = ?`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, record.ID); err != nil {
			return fmt.Errorf("delete conversation history: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND organization_id = ? AND project_id = ?`, record.ID, record.OrganizationID, record.ProjectID); err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	return nil
}
