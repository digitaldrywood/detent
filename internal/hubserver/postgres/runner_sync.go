package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerSyncVersions(ctx context.Context, identity runnerstore.Identity) (map[tracker.ProjectID]runnerauth.SyncProjectVersion, error) {
	if identity.RunnerID == "" || identity.OrganizationID != s.organization {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT p.id, COALESCE(policy.policy_id,''), COALESCE(config.config_revision,''), COALESCE(events.sequence,0),
 EXISTS (SELECT 1 FROM project_workflow_states ws WHERE ws.organization_id=$1 AND ws.project_id=p.id AND ws.dispatchable AND NOT ws.terminal
 AND EXISTS (SELECT 1 FROM queue_entries q WHERE q.organization_id=ws.organization_id AND q.project_id=ws.project_id AND q.scope=q.project_id AND q.state=ws.name))
FROM (
 SELECT id FROM projects WHERE organization_id=$1 AND deleted_at IS NULL AND $3='organization'
 UNION ALL
 SELECT g.project_id FROM token_grants g JOIN projects pr ON pr.organization_id=g.organization_id AND pr.id=g.project_id AND pr.deleted_at IS NULL
 WHERE g.organization_id=$1 AND g.token_id=$4 AND $3<>'organization'
 LIMIT $5
) p
LEFT JOIN project_policies policy ON policy.organization_id=$1 AND policy.project_id=p.id
LEFT JOIN runner_project_configurations config ON config.organization_id=$1 AND config.runner_id=$2 AND config.project_id=p.id
LEFT JOIN project_event_cursors events ON events.organization_id=$1 AND events.project_id=p.id`, s.organization, identity.RunnerID, identity.Scope, identity.TokenID, runnerstore.MaxRunnerProjectList+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	versions := make(map[tracker.ProjectID]runnerauth.SyncProjectVersion)
	for rows.Next() {
		var project tracker.ProjectID
		var v runnerauth.SyncProjectVersion
		if err := rows.Scan(&project, &v.Policy, &v.Configuration, &v.Sequence, &v.Queued); err != nil {
			return nil, mapError(ctx, err)
		}
		versions[project] = v
	}
	if len(versions) > runnerstore.MaxRunnerProjectList {
		return nil, runnerstore.ErrLimitExceeded
	}
	return versions, mapError(ctx, rows.Err())
}
