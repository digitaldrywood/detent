package conversations

import (
	"context"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func RequireConversationRemoval(record ConversationRecord) error {
	if conversationExecutionLive(record.Execution.Status) || conversationRunnerBound(record.Execution) {
		return ConversationStale("Finish or stop the running turn before archiving or deleting the conversation")
	}
	return nil
}

func (s *Server) AuthorizeConversationRemoval(ctx context.Context, r tenantstore.Reader, scope hubauth.Input, record ConversationRecord) error {
	if record.OrganizationID != scope.Organization || record.ProjectID != scope.Project || record.Visibility == conversation.VisibilityPrivate && record.OwnerPrincipalID != scope.Credential.ID {
		return hubapi.NotFound()
	}
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return err
	}
	if record.ReadAudience != "" {
		audience, err := ConversationReadAudienceRecord(ctx, r, scope.Organization, scope.Credential)
		if err != nil || audience != record.ReadAudience {
			return hubapi.NotFound()
		}
	}
	return AuthorizeConversationLinkedItemPG(ctx, r, scope, record)
}
