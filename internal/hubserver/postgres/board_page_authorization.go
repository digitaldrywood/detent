package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) BoardAuthorizedItems(ctx context.Context, authority accessstore.Authority, ids []tracker.NativeWorkItemID, changes []string, reporter string) (viewstore.BoardReadAuthority, error) {
	if len(ids) > 10000 {
		return viewstore.BoardReadAuthority{}, corestore.ErrInvalidPage
	}
	credential, ok := s.credentials[authority.CredentialID]
	if !ok {
		return viewstore.BoardReadAuthority{}, accessstore.ErrDenied
	}
	member := s.members[authority.UserID]
	rows, err := s.db.Query(ctx, `SELECT 'item',i.native_id,i.project_id FROM issues i JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
 WHERE i.organization_id=$1 AND i.native_id=ANY($2::text[]) AND p.deleted_at IS NULL AND p.profile='native'
 AND ($3::text='' OR i.actor_principal_id=$3)
 AND (NOT $4::boolean OR (($5::boolean OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=i.organization_id AND g.token_id=$6 AND g.project_id=i.project_id))
 AND ($7::text='' OR (EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=i.organization_id AND g.user_id=$7 AND g.project_id=i.project_id)
 AND EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=i.organization_id AND g.token_id=$8 AND g.project_id=i.project_id)))))
 UNION ALL SELECT 'change',c.id,c.project_id FROM change_requests c JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id WHERE c.organization_id=$1 AND c.project_id=$10 AND c.id=ANY($9::text[]) AND ($3::text='' OR i.actor_principal_id=$3)`, s.organization, ids, reporter, credential.NativeOnly || credential.Scope != "admin", credential.ProjectAccess == "all", credential.ID, authority.UserID, member.PrincipalID, changes, authority.ProjectID)
	if err != nil {
		return viewstore.BoardReadAuthority{}, mapError(ctx, err)
	}
	defer rows.Close()
	result := viewstore.BoardReadAuthority{Items: make(map[tracker.NativeWorkItemID]tracker.ProjectID, len(ids)), Changes: make(map[string]bool, len(changes))}
	for rows.Next() {
		var kind, id string
		var project tracker.ProjectID
		if err := rows.Scan(&kind, &id, &project); err != nil {
			return viewstore.BoardReadAuthority{}, mapError(ctx, err)
		}
		if kind == "change" {
			result.Changes[id] = true
		} else {
			result.Items[tracker.NativeWorkItemID(id)] = project
		}
	}
	return result, mapError(ctx, rows.Err())
}
