package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) LegacyHealthFindings(ctx context.Context, project tracker.ProjectID, state string, since *time.Time, after int64, reporter string, limit int) (healthstore.Page, error) {
	page := healthstore.Page{Findings: []healthstore.Finding{}, Baselines: []healthstore.Baseline{}}
	if project == "" || limit < 1 || limit > 101 || after < 0 || (state != "all" && state != "open" && state != "resolved") {
		return page, corestore.ErrInvalidPage
	}
	projectRow, err := s.Project(ctx, project)
	if err != nil {
		return page, err
	}
	if projectRow.DeletedAt != nil {
		return page, corestore.ErrNotFound
	}
	page.ChangeSequence, err = s.ProjectEventSequence(ctx, project)
	if err != nil {
		return page, err
	}
	rows, err := s.queries.HealthFindings(ctx, pgquery.HealthFindingsParams{OrganizationID: s.organization, ProjectID: project, State: state, Since: since, AfterSequence: pgtype.Int8{Int64: after, Valid: true}, Reporter: reporter, PageLimit: int32(limit)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Findings, err = s.healthRecords(ctx, rows)
	if err != nil {
		return page, err
	}
	ids := make([]string, len(rows))
	positions := make(map[string]int, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
		positions[row.ID] = i
	}
	links, err := s.queries.HealthFindingIssues(ctx, pgquery.HealthFindingIssuesParams{OrganizationID: s.organization, ProjectID: project, FindingIds: ids, Reporter: reporter})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(links) > 10000 {
		return page, corestore.ErrInvalidPage
	}
	for _, link := range links {
		i := positions[link.FindingID]
		page.Findings[i].Issues = append(page.Findings[i].Issues, healthstore.Issue{ProjectID: link.ProjectID, WorkItemID: tracker.NativeWorkItemID(link.WorkItemID)})
	}
	tick, err := s.queries.HealthLastTick(ctx, s.organization)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.LastTickAt = &tick
	gaps, err := s.queries.HealthBaselines(ctx, pgquery.HealthBaselinesParams{OrganizationID: s.organization, ProjectID: project})
	if err != nil {
		return page, mapError(ctx, err)
	}
	if len(gaps) > 10000 {
		return page, corestore.ErrInvalidPage
	}
	for _, gap := range gaps {
		page.Baselines = append(page.Baselines, healthstore.Baseline{ProjectID: gap.ProjectID, Signal: gap.Signal, Lane: gap.Lane})
	}
	return page, nil
}
