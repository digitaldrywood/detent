package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) NextHealthIssueWork(ctx context.Context, now time.Time) (healthstore.IssueWork, error) {
	var row healthstore.IssueWork
	err := s.db.QueryRow(ctx, `SELECT q.organization_id,q.finding_id,q.project_id,q.source_revision,q.source_digest,q.pending,q.next_check_at,q.checked_at,q.after_fingerprint,q.revision FROM health_issue_queue q JOIN projects p ON p.organization_id=q.organization_id AND p.id=q.project_id WHERE q.organization_id=$1 AND q.pending AND q.next_check_at<=$2 AND p.profile='native' AND p.deleted_at IS NULL ORDER BY q.next_check_at,q.checked_at NULLS FIRST,q.finding_id,q.project_id LIMIT 1`, s.organization, now).Scan(&row.OrganizationID, &row.FindingID, &row.ProjectID, &row.SourceRevision, &row.SourceDigest, &row.Pending, &row.NextCheckAt, &row.CheckedAt, &row.AfterFingerprint, &row.PublicationRevision)
	return row, mapError(ctx, err)
}

func (s *session) EnqueueHealthIssueWork(ctx context.Context, row healthstore.IssueWork) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO health_issue_queue(organization_id,finding_id,project_id,source_revision,source_digest,next_check_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(organization_id,finding_id,project_id) DO UPDATE SET source_revision=excluded.source_revision,revision=health_issue_queue.revision+1,source_digest=excluded.source_digest,pending=health_issue_queue.pending OR health_issue_queue.source_digest<>excluded.source_digest,after_fingerprint=CASE WHEN health_issue_queue.source_digest<>excluded.source_digest THEN '' ELSE health_issue_queue.after_fingerprint END,next_check_at=CASE WHEN health_issue_queue.source_digest<>excluded.source_digest THEN excluded.next_check_at ELSE health_issue_queue.next_check_at END WHERE health_issue_queue.source_revision<excluded.source_revision`, s.organization, row.FindingID, row.ProjectID, row.SourceRevision, row.SourceDigest, row.NextCheckAt)
	return mapError(ctx, err)
}

func (s *session) AdvanceHealthIssueWork(ctx context.Context, row healthstore.IssueWork, now time.Time) (healthstore.IssueWork, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return row, err
	}
	tag, err := s.db.Exec(ctx, `UPDATE health_issue_queue SET checked_at=$6,next_check_at=$6,revision=revision+1 WHERE organization_id=$1 AND finding_id=$2 AND project_id=$3 AND source_revision=$4 AND source_digest=$5 AND revision=$7 AND pending`, s.organization, row.FindingID, row.ProjectID, row.SourceRevision, row.SourceDigest, now, row.PublicationRevision)
	if err == nil && tag.RowsAffected() != 1 {
		return row, corestore.ErrRevisionConflict
	}
	row.PublicationRevision++
	row.CheckedAt = &now
	return row, mapError(ctx, err)
}

func (s *session) FinishHealthIssueWork(ctx context.Context, row healthstore.IssueWork, pending bool, next time.Time) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE health_issue_queue SET pending=$6,next_check_at=$7,after_fingerprint=$8,revision=revision+1 WHERE organization_id=$1 AND finding_id=$2 AND project_id=$3 AND source_revision=$4 AND source_digest=$5 AND revision=$9`, s.organization, row.FindingID, row.ProjectID, row.SourceRevision, row.SourceDigest, pending, next, row.AfterFingerprint, row.PublicationRevision)
	if err == nil && tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return mapError(ctx, err)
}

func (s *session) HealthIssueReports(ctx context.Context, finding string, project tracker.ProjectID, after string, limit int) ([]healthstore.IssueReport, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,finding_id,project_id,work_item_id,intake_fingerprint,reported_evidence_digest,reported_resolved_at FROM health_issue_reports WHERE organization_id=$1 AND finding_id=$2 AND project_id=$3 AND intake_fingerprint>$4 ORDER BY intake_fingerprint LIMIT $5`, s.organization, finding, project, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []healthstore.IssueReport
	for rows.Next() {
		var row healthstore.IssueReport
		if err := rows.Scan(&row.OrganizationID, &row.FindingID, &row.ProjectID, &row.WorkItemID, &row.IntakeFingerprint, &row.EvidenceDigest, &row.ResolvedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) HealthIssueReport(ctx context.Context, finding string, project tracker.ProjectID, fingerprint string) (healthstore.IssueReport, error) {
	var row healthstore.IssueReport
	err := s.db.QueryRow(ctx, `SELECT organization_id,finding_id,project_id,work_item_id,intake_fingerprint,reported_evidence_digest,reported_resolved_at FROM health_issue_reports WHERE organization_id=$1 AND finding_id=$2 AND project_id=$3 AND intake_fingerprint=$4`, s.organization, finding, project, fingerprint).Scan(&row.OrganizationID, &row.FindingID, &row.ProjectID, &row.WorkItemID, &row.IntakeFingerprint, &row.EvidenceDigest, &row.ResolvedAt)
	return row, mapError(ctx, err)
}

func (s *session) LatestHealthIssueReport(ctx context.Context, project tracker.ProjectID, fingerprint string) (healthstore.IssueReport, error) {
	var row healthstore.IssueReport
	err := s.db.QueryRow(ctx, `SELECT organization_id,finding_id,project_id,work_item_id,intake_fingerprint,reported_evidence_digest,reported_resolved_at FROM health_issue_reports WHERE organization_id=$1 AND project_id=$2 AND intake_fingerprint=$3 ORDER BY publication_sequence DESC LIMIT 1`, s.organization, project, fingerprint).Scan(&row.OrganizationID, &row.FindingID, &row.ProjectID, &row.WorkItemID, &row.IntakeFingerprint, &row.EvidenceDigest, &row.ResolvedAt)
	return row, mapError(ctx, err)
}

func (s *session) AuthorizeHealthIssue(ctx context.Context, finding string, revision int64, project tracker.ProjectID, write bool) error {
	query := `SELECT f.id FROM health_findings f JOIN health_finding_projects l ON l.organization_id=f.organization_id AND l.finding_id=f.id JOIN projects p ON p.organization_id=l.organization_id AND p.id=l.project_id WHERE f.organization_id=$1 AND f.id=$2 AND f.revision=$3 AND l.project_id=$4 AND f.class='instance' AND p.profile='native' AND p.deleted_at IS NULL`
	if write && !s.readOnly {
		query += ` FOR SHARE OF f`
	}
	var id string
	err := s.db.QueryRow(ctx, query, s.organization, finding, revision, project).Scan(&id)
	return mapError(ctx, err)
}

func (s *session) RecordHealthIssueReport(ctx context.Context, row healthstore.IssueReport) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.IntakeFingerprint == "" || row.WorkItemID == "" || len(row.EvidenceDigest) != 64 {
		return corestore.ErrInvalidPage
	}
	if err := s.LinkHealthIssue(ctx, LinkHealthIssueInput{OrganizationID: row.OrganizationID, FindingID: row.FindingID, ProjectID: row.ProjectID, WorkItemID: string(row.WorkItemID)}); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `UPDATE health_issue_reports SET publication_sequence=DEFAULT WHERE organization_id=$1 AND finding_id=$2 AND project_id=$3 AND intake_fingerprint=$4 AND work_item_id<>$5`, s.organization, row.FindingID, row.ProjectID, row.IntakeFingerprint, row.WorkItemID); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `INSERT INTO health_issue_reports(organization_id,finding_id,project_id,work_item_id,intake_fingerprint,reported_evidence_digest,reported_resolved_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(organization_id,finding_id,project_id,intake_fingerprint) DO UPDATE SET work_item_id=excluded.work_item_id,reported_evidence_digest=excluded.reported_evidence_digest,reported_resolved_at=excluded.reported_resolved_at`, s.organization, row.FindingID, row.ProjectID, row.WorkItemID, row.IntakeFingerprint, row.EvidenceDigest, row.ResolvedAt)
	return mapError(ctx, err)
}

func (s *session) CanResolveHealthIssue(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, cutoff time.Time) (bool, error) {
	{
		query := `SELECT f.id FROM health_finding_issues l JOIN health_findings f ON f.organization_id=l.organization_id AND f.id=l.finding_id WHERE l.organization_id=$1 AND l.project_id=$2 AND l.work_item_id=$3 ORDER BY l.finding_id LIMIT 10001`
		if !s.readOnly {
			query += ` FOR SHARE OF f`
		}
		rows, err := s.db.Query(ctx, query, s.organization, project, item)
		if err != nil {
			return false, mapError(ctx, err)
		}
		count := 0
		for rows.Next() {
			count++
			if count > 10000 {
				rows.Close()
				return false, corestore.ErrInvalidPage
			}
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, mapError(ctx, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, mapError(ctx, err)
		}
	}
	var result bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM health_finding_issues WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)
 AND NOT EXISTS(SELECT 1 FROM health_finding_issues l JOIN health_findings f ON f.organization_id=l.organization_id AND f.id=l.finding_id WHERE l.organization_id=$1 AND l.project_id=$2 AND l.work_item_id=$3 AND (f.class<>'instance' OR f.resolved_at IS NULL OR f.resolved_at>=$4 OR f.last_seen_at>=$4))
 AND NOT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='workflow.transitioned' AND actor_kind='human' AND from_state='Backlog' AND to_state<>'Backlog')`, s.organization, project, item, cutoff).Scan(&result)
	return result, mapError(ctx, err)
}

func (s *session) HealthIssueAutoCancelledBefore(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, opened time.Time) (bool, error) {
	var result bool
	err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT actor_kind='integration' AND actor_principal_id='health_detector' AND from_state='Backlog' AND to_state='Cancelled' AND recorded_at<$4 FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1),false)`, s.organization, project, item, opened).Scan(&result)
	return result, mapError(ctx, err)
}
