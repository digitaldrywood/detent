package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func requireConversationRemoval(record conversationRecord) error {
	if conversationExecutionLive(record.Execution.Status) || conversationRunnerBound(record.Execution) {
		return conversationStale("Finish or stop the running turn before archiving or deleting the conversation")
	}
	return nil
}

func (s *Service) authorizeConversationRemoval(ctx context.Context, r tenantstore.Reader, scope nativeScope, record conversationRecord) error {
	if record.OrganizationID != scope.organization || record.ProjectID != scope.project || record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.credential.ID {
		return nativeNotFound()
	}
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return err
	}
	if record.ReadAudience != "" {
		audience, err := conversationReadAudienceRecord(ctx, r, scope.organization, scope.credential)
		if err != nil || audience != record.ReadAudience {
			return nativeNotFound()
		}
	}
	return authorizeConversationLinkedItemPG(ctx, r, scope, record)
}

func (s *Service) archiveConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id := c.Param("conversation")
	original, err := s.conversations.readPublicationConversationForOrganization(ctx, scope.organization, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeConversationRemoval(ctx, r, scope, original)
	}); err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := requireConversationRemoval(original); err != nil {
		return s.nativeAPIError(c, err)
	}
	record := original
	record.Archived, record.Revision, record.EventSeq, record.UpdatedAt = true, original.Revision+1, original.EventSeq+1, s.config.now()
	raw, err := json.Marshal(projectConversation(record))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	payload, err := messageContent(conversationMessageRecord{Data: raw})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ref, err := s.conversations.store.publishPayload(ctx, string(scope.organization), payload)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	contentID := contentArchiveID(string(scope.organization), ref)
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationRemoval(ctx, w, scope, conversationRecordFromStore(stored)); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return nativeConflict(tracker.Revision(stored.Revision))
		}
		if err := requireConversationRemoval(conversationRecordFromStore(stored)); err != nil {
			return err
		}
		stored.Execution, stored.Preferences = original.Execution, original.Preferences
		stored.Archived, stored.UpdatedAt = true, record.UpdatedAt
		if _, err := w.UpdateConversation(ctx, stored, stored.Revision); err != nil {
			return err
		}
		if err := w.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: scope.organization, ID: contentID, Reference: ref, CreatedAt: record.UpdatedAt}); err != nil {
			return err
		}
		event, err := w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: scope.organization, ConversationID: id, ContentReference: contentID, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{}`), CreatedAt: record.UpdatedAt}})
		record.EventSeq = event.Seq
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	s.conversations.committed(record)
	return c.JSON(http.StatusOK, projectConversation(record))
}

func (s *Service) deleteConversation(c echo.Context) error {
	scope := nativeRequestScope(c)
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	id := c.Param("conversation")
	original, err := s.conversations.readPublicationConversationForOrganization(ctx, scope.organization, id)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		record := conversationRecordFromStore(stored)
		if err := s.authorizeConversationRemoval(ctx, w, scope, record); err != nil {
			return err
		}
		if stored.Revision != original.Revision || stored.EventSequence != original.EventSeq {
			return nativeConflict(tracker.Revision(stored.Revision))
		}
		if err := requireConversationRemoval(record); err != nil {
			return err
		}
		if err := requireConversationRemoval(original); err != nil {
			return err
		}
		owner, ok := w.(conversationstore.RemovalWriter)
		if !ok {
			return errors.New("conversation removal owner is required")
		}
		return owner.DeleteConversation(ctx, scope.project, id, s.config.now())
	})
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	s.conversations.broker.notify(id)
	return c.NoContent(http.StatusNoContent)
}
