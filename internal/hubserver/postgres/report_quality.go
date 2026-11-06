package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (s *session) ReadEscapeFacts(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.EscapeFact, error) {
	args, err := s.reportQueryArgs(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT i.project_id,i.native_id,c.id,coalesce(e.version_id,''),e.created_at,e.actor_kind,e.actor_principal_id,f.occurrence_id,f.kind,f.cause,f.observed_at,f.instance_id,f.human_override
FROM report_escape_facts f JOIN change_evidence e ON e.organization_id=f.organization_id AND e.change_id=f.change_id AND e.sequence=f.evidence_sequence JOIN change_requests c ON c.organization_id=e.organization_id AND c.id=e.change_id JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id
WHERE `+reportIssueFilter+` AND f.observed_at<$5 AND e.created_at<$5 AND (f.observed_at>=$4 OR c.landed_at>=$4)
ORDER BY f.observed_at,e.created_at,e.sequence LIMIT $11`, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []reportstore.EscapeFact{}
	for rows.Next() {
		var fact reportstore.EscapeFact
		if err := rows.Scan(&fact.ProjectID, &fact.WorkItemID, &fact.ChangeID, &fact.VersionID, &fact.CreatedAt, &fact.ActorKind, &fact.ActorID, &fact.OccurrenceID, &fact.Kind, &fact.Cause, &fact.ObservedAt, &fact.InstanceID, &fact.HumanOverride); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(out) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}
