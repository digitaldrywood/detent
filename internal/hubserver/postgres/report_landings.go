package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportcheckpoints"
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
	facts, err := reportcheckpoints.LandingFacts(ctx, s.db, s.organization, args, query.Limit, query.From.UTC())
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return facts, nil
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
