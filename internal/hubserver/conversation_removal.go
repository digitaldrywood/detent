package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
