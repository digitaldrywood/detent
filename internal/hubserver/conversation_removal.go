package hubserver

import (
	"context"
	"database/sql"
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
	}, func(ctx context.Context, p *conversationPublicationPlan) error {
		p.record.Archived = true
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
