package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (s *session) ReadLandingFacts(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.LandingFact, error) {
	args, err := s.reportQueryArgs(query)
	if err != nil {
		return nil, err
	}
	if len(query.Projects) > maxReportProjects {
		return nil, reportstore.ErrPopulationExceeded
	}
	projectIDs, ok := args[1].([]string)
	if !ok {
		return nil, errors.New("report query projects have an unexpected type")
	}
	candidates, err := s.readLandingCandidates(ctx, projectIDs, query.From.UTC(), query.To.UTC())
	if err != nil {
		return nil, err
	}
	args = append(args, candidates)
	rows, err := s.db.Query(ctx, `SELECT i.project_id,i.native_id,c.id,c.landed_at,coalesce(tries.n,0),coalesce(tries.complete,false),c.landed_version_id,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref,coalesce(tries.observed,0)
FROM unnest($14::text[]) candidate(id) CROSS JOIN LATERAL (SELECT * FROM change_requests c WHERE c.organization_id=$1 AND c.id=candidate.id LIMIT 1) c
CROSS JOIN LATERAL (SELECT * FROM issues i WHERE i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=c.work_item_id LIMIT 1) i
JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.landed_version_id
LEFT JOIN LATERAL (
 WITH candidate_attempts AS MATERIALIZED (
   SELECT a.id,a.started_at FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.started_at<=c.landed_at AND c.landed_at>=$4 ORDER BY a.started_at,a.id LIMIT $11
 ), attempts AS (
   SELECT a.id,a.started_at,f.landing_receipt,f.landed,f.landing_change_id
   FROM candidate_attempts a
   LEFT JOIN LATERAL (SELECT rf.* FROM collaboration_events e JOIN report_event_facts rf ON rf.organization_id=e.organization_id AND rf.event_id=e.id WHERE e.organization_id=i.organization_id AND e.attempt_id=a.id AND e.attempt_id<>'' ORDER BY e.sequence DESC LIMIT 1) f ON true
   WHERE f.role='merge' OR f.landing_receipt OR f.event_id IS NULL
 ), numbered AS (SELECT *,row_number() OVER(ORDER BY started_at,id) n FROM attempts), success AS (
   SELECT min(n) n FROM numbered WHERE landed AND landing_change_id=c.id
 )
 SELECT success.n,success.n IS NOT NULL AND NOT EXISTS(SELECT 1 FROM numbered a WHERE a.n<=success.n AND NOT coalesce(a.landing_receipt,false)) AS complete,(SELECT count(*) FROM candidate_attempts) observed FROM success
) tries ON true
WHERE `+reportIssueFilter+` AND c.landed_at<$5 AND c.landed_head_sha=v.head_sha AND c.landed_merge_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'
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
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(out) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}

func (s *session) readLandingCandidates(ctx context.Context, projects []string, from, to time.Time) ([]string, error) {
	var exceeded bool
	var ids []string
	err := s.db.QueryRow(ctx, `WITH landed AS MATERIALIZED (
 SELECT c.id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=ANY($2::text[]) AND c.landed_at>=$3 AND c.landed_at<$4 ORDER BY c.project_id,c.landed_at,c.id LIMIT $5
), reopened AS MATERIALIZED (
 SELECT e.project_id,e.work_item_id FROM collaboration_events e
 WHERE e.organization_id=$1 AND e.project_id=ANY($2::text[]) AND e.type='workflow.transitioned' AND e.from_state='Done' AND e.recorded_at>=$3 AND e.recorded_at<$4
 ORDER BY e.project_id,e.recorded_at,e.work_item_id LIMIT $5
), reopened_changes AS MATERIALIZED (
 SELECT c.id FROM change_requests c WHERE c.organization_id=$1 AND c.project_id=ANY($2::text[])
 AND c.work_item_id=ANY(ARRAY(SELECT DISTINCT work_item_id FROM reopened)) AND c.landed_at<$4
 ORDER BY c.project_id,c.work_item_id,c.landed_at,c.id LIMIT $5
), observed AS MATERIALIZED (
 SELECT f.change_id FROM report_escape_facts f WHERE f.organization_id=$1 AND f.observed_at>=$3 AND f.observed_at<$4 ORDER BY f.observed_at,f.change_id LIMIT $5
), escaped AS MATERIALIZED (
 SELECT c.id FROM (SELECT DISTINCT change_id FROM observed) o CROSS JOIN LATERAL (SELECT c.id FROM change_requests c WHERE c.organization_id=$1 AND c.id=o.change_id AND c.project_id=ANY($2::text[]) LIMIT 1) c
)
SELECT (SELECT count(*) FROM landed)>=$5 OR (SELECT count(*) FROM reopened)>=$5 OR (SELECT count(*) FROM reopened_changes)>=$5 OR (SELECT count(*) FROM observed)>=$5,
 coalesce((SELECT array_agg(id ORDER BY id) FROM (SELECT id FROM landed UNION SELECT id FROM reopened_changes UNION SELECT id FROM escaped) keyed),'{}'::text[])`, s.organization, projects, from, to, maxOverviewCandidateRows+1).Scan(&exceeded, &ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if exceeded || len(ids) > maxOverviewCandidateRows {
		return nil, reportstore.ErrPopulationExceeded
	}
	return ids, nil
}
