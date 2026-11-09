package postgres

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) RunnerProjectSchedulingRanks(ctx context.Context, runnerID string, limit int) (map[tracker.ProjectID]int, error) {
	if runnerID == "" || limit < 1 || limit > runnerstore.MaxRunnerProjectList {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT p.id,COALESCE(rank.rank,p.scheduling_rank)
 FROM runner_identities r JOIN projects p ON p.organization_id=r.organization_id
 LEFT JOIN runner_project_ranks rank ON rank.organization_id=r.organization_id AND rank.runner_id=r.runner_id AND rank.project_id=p.id
 WHERE r.organization_id=$1 AND r.runner_id=$2 AND p.deleted_at IS NULL
 AND (r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.organization_id=r.organization_id AND g.token_id=r.token_id AND g.project_id=p.id))
 ORDER BY p.id LIMIT $3`, s.organization, runnerID, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	ranks := make(map[tracker.ProjectID]int)
	for rows.Next() {
		var project tracker.ProjectID
		var rank int
		if err := rows.Scan(&project, &rank); err != nil {
			return nil, mapError(ctx, err)
		}
		ranks[project] = rank
		if len(ranks) > limit {
			return nil, runnerstore.ErrLimitExceeded
		}
	}
	return ranks, mapError(ctx, rows.Err())
}

func (s *session) RunnerProposedSchedulingRanks(ctx context.Context, proposed runnerstore.Identity, limit int) (map[tracker.ProjectID]int, error) {
	if err := s.checkOrganization(proposed.OrganizationID); err != nil {
		return nil, err
	}
	if proposed.RunnerID == "" || limit < 1 || limit > runnerstore.MaxRunnerProjectList || proposed.Scope != "organization" && proposed.Scope != "projects" || len(proposed.ProjectIDs) > 100 || proposed.Scope == "organization" && len(proposed.ProjectIDs) != 0 || len(proposed.ProjectRanks) > runnerstore.MaxRunnerProjectRanks {
		return nil, corestore.ErrInvalidPage
	}
	projects := make([]string, len(proposed.ProjectIDs))
	for i, id := range proposed.ProjectIDs {
		if id == "" {
			return nil, corestore.ErrInvalidPage
		}
		projects[i] = string(id)
	}
	keys := make([]string, 0, len(proposed.ProjectRanks))
	for id, rank := range proposed.ProjectRanks {
		if id == "" || rank < 0 || int64(rank) > 2147483647 {
			return nil, corestore.ErrInvalidPage
		}
		keys = append(keys, string(id))
	}
	slices.Sort(keys)
	values := make([]int64, len(keys))
	for i, key := range keys {
		values[i] = int64(proposed.ProjectRanks[tracker.ProjectID(key)])
	}
	rows, err := s.db.Query(ctx, `SELECT p.id,COALESCE(proposed_rank.rank,p.scheduling_rank)
 FROM projects p LEFT JOIN unnest($4::text[],$5::integer[]) proposed_rank(project_id,rank) ON proposed_rank.project_id=p.id
 WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND ($2='organization' OR p.id=ANY($3::text[]))
 ORDER BY p.id LIMIT $6`, s.organization, proposed.Scope, projects, keys, values, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	ranks := make(map[tracker.ProjectID]int)
	for rows.Next() {
		var id tracker.ProjectID
		var rank int
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, mapError(ctx, err)
		}
		ranks[id] = rank
		if len(ranks) > limit {
			return nil, runnerstore.ErrLimitExceeded
		}
	}
	return ranks, mapError(ctx, rows.Err())
}
