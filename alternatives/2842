package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (s *session) ReadLandingFacts(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.LandingFact, error) {
	args, err := s.reportQueryArgs(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.project_id,i.native_id,c.id,c.landed_at,coalesce(tries.n,0),coalesce(tries.complete,false),c.landed_version_id,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref,coalesce(tries.observed,0)
FROM change_requests c JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id
JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
LEFT JOIN LATERAL (
 WITH candidate_attempts AS MATERIALIZED (
   SELECT a.id,a.started_at FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=c.landed_at AND c.landed_at>=$4 ORDER BY a.started_at,a.id LIMIT $11
 ), attempts AS (
   SELECT a.id,a.started_at,f.landing_receipt,f.landed,f.landing_change_id
   FROM candidate_attempts a
   LEFT JOIN LATERAL (SELECT rf.* FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=i.organization_id AND e.attempt_id=a.id ORDER BY e.sequence DESC LIMIT 1) f ON true
   WHERE f.role='merge' OR f.landing_receipt OR f.event_id IS NULL
 ), numbered AS (SELECT *,row_number() OVER(ORDER BY started_at,id) n FROM attempts), success AS (
   SELECT min(n) n FROM numbered WHERE landed AND landing_change_id=c.id
 )
 SELECT success.n,success.n IS NOT NULL AND NOT EXISTS(SELECT 1 FROM numbered a WHERE a.n<=success.n AND NOT coalesce(a.landing_receipt,false)) AS complete,(SELECT count(*) FROM candidate_attempts) observed FROM success
) tries ON true
WHERE `+reportIssueFilter+` AND c.landed_at<$5 AND (c.landed_at>=$4 OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' AND e.from_state='Done' AND e.recorded_at>=$4 AND e.recorded_at<$5) OR EXISTS(SELECT 1 FROM report_escape_facts f WHERE f.organization_id=c.organization_id AND f.change_id=c.id AND f.observed_at>=$4 AND f.observed_at<$5)) AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
ORDER BY c.landed_at DESC,c.id LIMIT $11`, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []reportstore.LandingFact{}
	for rows.Next() {
		var fact reportstore.LandingFact
		var observed int
		if err := rows.Scan(&fact.ProjectID, &fact.WorkItemID, &fact.ChangeID, &fact.At, &fact.Attempts, &fact.Complete, &fact.VersionID, &fact.HeadSHA, &fact.MergeSHA, &fact.BaseRef, &observed); err != nil {
			return nil, mapError(ctx, err)
		}
		if observed > query.Limit && !fact.Complete {
			return nil, reportstore.ErrPopulationExceeded
		}
		out = append(out, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(out) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}
