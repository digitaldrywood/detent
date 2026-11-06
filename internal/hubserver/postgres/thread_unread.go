package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
)

func (s *session) RequestThreadUnread(ctx context.Context, request threadstore.UnreadRequest) (threadstore.Unread, error) {
	var result threadstore.Unread
	if request.ID == "" || request.Principal == "" || request.CredentialID == "" || request.ScopeHash == "" || len(request.Projects) > 100 {
		return result, corestore.ErrInvalidPage
	}
	inserted, err := s.db.Exec(ctx, `INSERT INTO team_thread_unread_summaries(organization_id,id,principal_id,credential_id,user_id,member_principal,key_kind,require_credential_grant,reporter_principal,scope_hash,turn) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,(SELECT coalesce(max(turn),0)+1 FROM team_thread_unread_summaries WHERE organization_id=$1)) ON CONFLICT(organization_id,principal_id) DO NOTHING`, s.organization, request.ID, request.Principal, request.CredentialID, request.UserID, request.MemberPrincipal, request.KeyKind, request.RequireCredentialGrant, request.Reporter, request.ScopeHash)
	if err != nil {
		return result, mapError(ctx, err)
	}
	added := inserted.RowsAffected()
	updated, err := s.db.Exec(ctx, `UPDATE team_thread_unread_summaries SET credential_id=$4,user_id=$5,member_principal=$6,key_kind=$7,require_credential_grant=$8,reporter_principal=$9,scope_hash=$10,requested_revision=requested_revision+1,scan_revision=0,after_thread='',through_thread='',partial_count=0,unread_count=NULL WHERE organization_id=$1 AND id=$2 AND principal_id=$3 AND scope_hash<>$10`, s.organization, request.ID, request.Principal, request.CredentialID, request.UserID, request.MemberPrincipal, request.KeyKind, request.RequireCredentialGrant, request.Reporter, request.ScopeHash)
	if err != nil {
		return result, mapError(ctx, err)
	}
	changed := updated.RowsAffected()
	if added == 1 || changed == 1 {
		if _, err := s.db.Exec(ctx, `DELETE FROM team_thread_unread_projects WHERE organization_id=$1 AND summary_id=$2`, s.organization, request.ID); err != nil {
			return result, mapError(ctx, err)
		}
		for _, project := range request.Projects {
			if _, err := s.db.Exec(ctx, `INSERT INTO team_thread_unread_projects(organization_id,summary_id,project_id) VALUES($1,$2,$3)`, s.organization, request.ID, project); err != nil {
				return result, mapError(ctx, err)
			}
		}
	} else if request.Refresh {
		if _, err := s.db.Exec(ctx, `UPDATE team_thread_unread_summaries SET requested_revision=greatest(requested_revision,CASE WHEN scan_revision=0 THEN completed_revision+1 ELSE scan_revision+1 END) WHERE organization_id=$1 AND id=$2`, s.organization, request.ID); err != nil {
			return result, mapError(ctx, err)
		}
	}
	var count pgtype.Int8
	err = s.db.QueryRow(ctx, `SELECT unread_count,requested_revision>completed_revision FROM team_thread_unread_summaries WHERE organization_id=$1 AND id=$2 AND scope_hash=$3`, s.organization, request.ID, request.ScopeHash).Scan(&count, &result.Pending)
	if err != nil {
		return result, mapError(ctx, err)
	}
	if count.Valid {
		result.Count = &count.Int64
	}
	return result, nil
}
func (s *session) ThreadUnreadJob(ctx context.Context) (threadstore.UnreadJob, error) {
	var job threadstore.UnreadJob
	err := s.db.QueryRow(ctx, `SELECT id,principal_id,credential_id,user_id,member_principal,key_kind,require_credential_grant,reporter_principal,scope_hash,requested_revision,completed_revision,scan_revision,after_thread,through_thread,partial_count FROM team_thread_unread_summaries WHERE organization_id=$1 AND requested_revision>completed_revision ORDER BY turn,id LIMIT 1`, s.organization).Scan(&job.ID, &job.Principal, &job.CredentialID, &job.UserID, &job.MemberPrincipal, &job.KeyKind, &job.RequireCredentialGrant, &job.Reporter, &job.ScopeHash, &job.Requested, &job.Completed, &job.Scan, &job.After, &job.Through, &job.Partial)
	if err != nil {
		return job, mapError(ctx, err)
	}
	if job.Scan == 0 {
		err = s.db.QueryRow(ctx, `SELECT coalesce(max(id),'') FROM team_threads WHERE organization_id=$1`, s.organization).Scan(&job.Through)
	}
	return job, mapError(ctx, err)
}
func (s *session) ThreadUnreadPage(ctx context.Context, job threadstore.UnreadJob, limit int) ([]threadstore.UnreadRow, error) {
	if limit < 1 || limit > 51 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `WITH page AS (SELECT id,scope,project_id,work_item_id,post_count FROM team_threads WHERE organization_id=$1 AND id>$2 AND id<=$3 ORDER BY id LIMIT $4)
SELECT t.id,t.post_count>coalesce(r.last_seq,0)
AND EXISTS(SELECT 1 FROM api_tokens credential WHERE credential.organization_id=$1 AND credential.id=$6 AND credential.revoked_at IS NULL AND (credential.expires_at IS NULL OR credential.expires_at>clock_timestamp()))
AND ($7='' OR EXISTS(SELECT 1 FROM hosted_members m JOIN api_tokens principal ON principal.organization_id=m.organization_id AND principal.id=m.principal_id WHERE m.organization_id=$1 AND m.user_id=$7 AND m.active AND principal.revoked_at IS NULL AND ($8='' OR m.principal_id=$8)))
AND (t.scope='general' OR (
EXISTS(SELECT 1 FROM team_thread_unread_projects p JOIN projects project ON project.organization_id=p.organization_id AND project.id=p.project_id WHERE p.organization_id=$1 AND p.summary_id=$9 AND p.project_id=t.project_id AND project.deleted_at IS NULL)
AND ($10='service' OR ($7<>'' AND EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=$1 AND g.user_id=$7 AND g.project_id=t.project_id)) OR ($7='' AND EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=$1 AND g.token_id=$6 AND g.project_id=t.project_id)))
AND ($8='' OR $10='service' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=$1 AND g.token_id=$8 AND g.project_id=t.project_id))
AND (NOT $11 OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=$1 AND g.token_id=$6 AND g.project_id=t.project_id))
AND (t.scope<>'issue' OR EXISTS(SELECT 1 FROM issues i WHERE i.organization_id=$1 AND i.project_id=t.project_id AND i.native_id=t.work_item_id AND NOT i.archived AND ($12='' OR i.actor_principal_id=$12)))))
FROM page t LEFT JOIN team_thread_reads r ON r.organization_id=$1 AND r.thread_id=t.id AND r.principal_id=$5 ORDER BY t.id`, s.organization, job.After, job.Through, limit, job.Principal, job.CredentialID, job.UserID, job.MemberPrincipal, job.ID, job.KeyKind, job.RequireCredentialGrant, job.Reporter)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []threadstore.UnreadRow{}
	for rows.Next() {
		var row threadstore.UnreadRow
		if err := rows.Scan(&row.ID, &row.Unread); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, row)
	}
	return out, mapError(ctx, rows.Err())
}
func (s *session) ApplyThreadUnread(ctx context.Context, job threadstore.UnreadJob, complete bool) error {
	scan := job.Scan
	if scan == 0 {
		scan = job.Requested
	}
	result, err := s.db.Exec(ctx, `UPDATE team_thread_unread_summaries SET turn=(SELECT coalesce(max(turn),0)+1 FROM team_thread_unread_summaries WHERE organization_id=$1),unread_count=CASE WHEN $3 THEN $4 ELSE unread_count END,completed_revision=CASE WHEN $3 THEN $5 ELSE completed_revision END,scan_revision=CASE WHEN $3 THEN 0 ELSE $5 END,after_thread=CASE WHEN $3 THEN '' ELSE $6 END,through_thread=CASE WHEN $3 THEN '' ELSE $7 END,partial_count=CASE WHEN $3 THEN 0 ELSE $4 END WHERE organization_id=$1 AND id=$2 AND requested_revision=$8 AND scan_revision=$9 AND after_thread=$10 AND scope_hash=$11`, s.organization, job.ID, complete, job.Partial, scan, job.After, job.Through, job.Requested, job.Scan, job.OriginalAfter, job.ScopeHash)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	if n != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
