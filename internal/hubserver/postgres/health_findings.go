package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthRecord(row pgquery.HealthFinding) healthstore.Finding {
	return healthstore.Finding{Sequence: row.Sequence.Int64, ID: row.ID, Fingerprint: row.Fingerprint, Signal: row.Signal, Class: row.Class, SubjectKind: row.SubjectKind, SubjectID: row.SubjectID, OpenedAt: row.OpenedAt, LastSeenAt: row.LastSeenAt, ResolvedAt: row.ResolvedAt, Severity: row.Severity, Summary: row.Summary, NextAction: row.NextAction, Evidence: eventstore.Reference(row.EvidenceObjectKey, row.EvidenceSha256, row.EvidenceBytes, row.EvidenceContentBytes), EmailUnavailable: row.EmailUnavailable, SlackUnavailableAt: row.SlackUnavailableAt, SlackStatusCode: int(row.SlackStatusCode)}
}

func (s *session) healthRecords(ctx context.Context, rows []pgquery.HealthFinding) ([]healthstore.Finding, error) {
	result := make([]healthstore.Finding, len(rows))
	ids := make([]string, len(rows))
	positions := make(map[string]int, len(rows))
	for i, row := range rows {
		result[i] = healthRecord(row)
		ids[i] = row.ID
		positions[row.ID] = i
	}
	projects, err := s.queries.HealthFindingProjects(ctx, pgquery.HealthFindingProjectsParams{OrganizationID: s.organization, FindingIds: ids})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	if len(projects) > 10000 {
		return nil, corestore.ErrInvalidPage
	}
	for _, project := range projects {
		i := positions[project.FindingID]
		result[i].Projects = append(result[i].Projects, project.ProjectID)
	}
	return result, nil
}

func (s *session) HealthSummaryFindings(ctx context.Context, limit int) ([]healthstore.Finding, error) {
	if limit < 1 || limit > 100 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.HealthSummaryFindings(ctx, pgquery.HealthSummaryFindingsParams{OrganizationID: s.organization, Limit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	return s.healthRecords(ctx, rows)
}

func (s *session) HealthFindings(ctx context.Context, project tracker.ProjectID, state string, since *time.Time, after int64, reporter string, limit int) (healthstore.Page, error) {
	page := healthstore.Page{Findings: []healthstore.Finding{}, Baselines: []healthstore.Baseline{}}
	if project == "" || limit < 1 || limit > 101 || after < 0 || (state != "all" && state != "open" && state != "resolved") {
		return page, corestore.ErrInvalidPage
	}
	header, err := s.db.Query(ctx, `SELECT COALESCE(c.sequence,0),t.last_tick_at,b.signal,b.lane,EXISTS(SELECT 1 FROM health_finding_projects cold WHERE cold.organization_id=p.organization_id AND cold.project_id=p.id AND cold.page_sequence=0)
 FROM projects p LEFT JOIN project_event_cursors c ON c.organization_id=p.organization_id AND c.project_id=p.id
 LEFT JOIN health_detector_ticks t ON t.organization_id=p.organization_id
 LEFT JOIN health_baseline_unavailable b ON b.organization_id=p.organization_id AND b.project_id=p.id
 WHERE p.organization_id=$1 AND p.id=$2 AND p.deleted_at IS NULL ORDER BY b.signal,b.lane LIMIT 10001`, s.organization, project)
	if err != nil {
		return page, mapError(ctx, err)
	}
	found := false
	for header.Next() {
		found = true
		var signal, lane *string
		var pending bool
		if err := header.Scan(&page.ChangeSequence, &page.LastTickAt, &signal, &lane, &pending); err != nil {
			header.Close()
			return page, mapError(ctx, err)
		}
		if pending {
			header.Close()
			return page, healthstore.ErrPagePending
		}
		if signal != nil {
			page.Baselines = append(page.Baselines, healthstore.Baseline{ProjectID: project, Signal: *signal, Lane: *lane})
		}
	}
	err = header.Err()
	header.Close()
	if err != nil {
		return page, mapError(ctx, err)
	}
	if !found {
		return page, corestore.ErrNotFound
	}
	if len(page.Baselines) > 10000 {
		return page, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `WITH selected AS MATERIALIZED (
 SELECT f.* FROM health_finding_projects p JOIN health_findings f ON f.organization_id=p.organization_id AND f.id=p.finding_id
 WHERE p.organization_id=$1 AND p.project_id=$2
 AND ($3='all' OR (f.resolved_at IS NOT NULL)=($3='resolved'))
 AND (($3='all' AND f.resolved_at IS NULL) OR $4::timestamptz IS NULL OR f.last_seen_at>=$4 OR f.resolved_at>=$4)
 AND p.page_sequence>$5
 AND ($6='' OR EXISTS (SELECT 1 FROM health_finding_issues l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id
 WHERE l.organization_id=f.organization_id AND l.finding_id=f.id AND l.project_id=$2 AND i.actor_principal_id=$6))
 ORDER BY p.page_sequence LIMIT $7
 ) SELECT `+healthPageColumns+`,l.work_item_id FROM selected f
 LEFT JOIN (health_finding_issues l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id AND ($6='' OR i.actor_principal_id=$6))
 ON l.organization_id=f.organization_id AND l.finding_id=f.id AND l.project_id=$2
 ORDER BY f.sequence,l.work_item_id LIMIT 10001`, s.organization, project, state, since, after, reporter, limit)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 10000 {
			return page, corestore.ErrInvalidPage
		}
		var item *string
		row, checkpoint, err := scanHealthPage(rows, &item)
		if err != nil {
			return page, mapError(ctx, err)
		}
		if checkpoint != row.Generation {
			return page, healthstore.ErrPagePending
		}
		if len(page.Findings) == 0 || page.Findings[len(page.Findings)-1].ID != row.ID {
			page.Findings = append(page.Findings, row)
		}
		if item != nil {
			last := &page.Findings[len(page.Findings)-1]
			last.Issues = append(last.Issues, healthstore.Issue{ProjectID: project, WorkItemID: tracker.NativeWorkItemID(*item)})
		}
	}
	return page, mapError(ctx, rows.Err())
}
