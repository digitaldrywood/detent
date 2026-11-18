package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkItemReferences(ctx context.Context, project tracker.ProjectID, id string, limit int) ([]conversationstore.WorkItemReference, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := pgquery.New(s.db).WorkItemConversationReferences(ctx, pgquery.WorkItemConversationReferencesParams{OrganizationID: s.organization, ProjectID: project, TargetID: id, Limit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]conversationstore.WorkItemReference, 0, len(rows))
	for _, row := range rows {
		result = append(result, conversationstore.WorkItemReference{ConversationID: row.ConversationID, MessageID: row.MessageID, CreatedAt: row.CreatedAt, Visibility: conversation.Visibility(row.Visibility), OwnerPrincipalID: row.OwnerPrincipalID, ReadAudience: row.ReadAudience})
	}
	return result, nil
}

func (s *session) DeleteConversation(ctx context.Context, project tracker.ProjectID, id string, now time.Time) error {
	rows, err := pgquery.New(s.db).DeleteConversationRecord(ctx, pgquery.DeleteConversationRecordParams{OrganizationID: s.organization, ProjectID: project, ID: id, DeletedAt: &now})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return corestore.ErrNotFound
	}
	return nil
}
