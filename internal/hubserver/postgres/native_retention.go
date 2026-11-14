package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) WorkItemRetentionCursor(ctx context.Context) (corestore.WorkItemRetentionCursor, error) {
	var result corestore.WorkItemRetentionCursor
	var positioned bool
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT positioned,archive_terminal_at,sequence FROM storage_conversion_cursors WHERE organization_id=$1 AND family='issue-retention'`, s.organization).Scan(&positioned, &at, &result.ID)
	if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, mapError(ctx, err)
	}
	if !positioned {
		if at != nil || result.ID != 0 {
			return result, corestore.ErrInvalidCursor
		}
		return result, nil
	}
	if at == nil || result.ID < 1 {
		return result, corestore.ErrInvalidCursor
	}
	result.TerminalEnteredAt = *at
	return result, nil
}

func (s *session) AdvanceWorkItemRetention(ctx context.Context, from, to corestore.WorkItemRetentionCursor) (bool, error) {
	if from.ID < 0 || to.ID < 0 || from.ID > 0 && from.TerminalEnteredAt.IsZero() || to.ID > 0 && to.TerminalEnteredAt.IsZero() {
		return false, corestore.ErrInvalidCursor
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO storage_conversion_cursors (organization_id,family) VALUES ($1,'issue-retention') ON CONFLICT DO NOTHING`, s.organization); err != nil {
		return false, mapError(ctx, err)
	}
	var before, after any
	if from.ID > 0 {
		before = from.TerminalEnteredAt
	}
	if to.ID > 0 {
		after = to.TerminalEnteredAt
	}
	result, err := s.db.Exec(ctx, `UPDATE storage_conversion_cursors SET positioned=$2,archive_terminal_at=$3,sequence=$4,turn=turn+1 WHERE organization_id=$1 AND family='issue-retention' AND positioned=$5 AND archive_terminal_at IS NOT DISTINCT FROM $6::timestamptz AND sequence=$7`, s.organization, to.ID > 0, after, to.ID, from.ID > 0, before, from.ID)
	if err != nil {
		return false, mapError(ctx, err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *session) WorkItemRetentionCandidates(ctx context.Context, now time.Time, after corestore.WorkItemRetentionCursor, limit int) (corestore.WorkItemRetentionPage, error) {
	var page corestore.WorkItemRetentionPage
	if now.IsZero() || limit < 1 || limit > 4 || after.ID < 0 || after.ID > 0 && after.TerminalEnteredAt.IsZero() {
		return page, corestore.ErrInvalidPage
	}
	var afterTime any
	if after.ID > 0 {
		afterTime = after.TerminalEnteredAt
	}
	rows, err := s.db.Query(ctx, `SELECT project_id,native_id,revision,event_sequence,terminal_entered_at,id FROM issues WHERE organization_id=$1 AND NOT archived AND terminal_entered_at<=$2 AND ($3::timestamptz IS NULL OR (terminal_entered_at,id)>($3,$4)) ORDER BY terminal_entered_at,id LIMIT $5`, s.organization, now, afterTime, after.ID, limit)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var candidate corestore.WorkItemRetentionCandidate
		if err := rows.Scan(&candidate.ProjectID, &candidate.WorkItemID, &candidate.Revision, &candidate.EventSequence, &candidate.Cursor.TerminalEnteredAt, &candidate.Cursor.ID); err != nil {
			return page, mapError(ctx, err)
		}
		page.Items = append(page.Items, candidate)
		page.Next = candidate.Cursor
	}
	if len(page.Items) < limit {
		page.Next = corestore.WorkItemRetentionCursor{}
	}
	return page, mapError(ctx, rows.Err())
}

func (s *session) WorkItemRetentionPolicy(ctx context.Context, item corestore.WorkItem, settingsRevision int64, now time.Time) (int64, bool, error) {
	if item.OrganizationID != s.organization || item.ID < 1 || item.Revision < 1 || settingsRevision < 1 || now.IsZero() {
		return 0, false, corestore.ErrInvalidPage
	}
	var days int64
	err := s.db.QueryRow(ctx, `SELECT CASE WHEN lower(w.name)='cancelled' THEN p.archive_cancelled_after_days ELSE p.archive_completed_after_days END
FROM issues i JOIN projects project ON project.organization_id=i.organization_id AND project.id=i.project_id
JOIN project_settings p ON p.organization_id=i.organization_id AND p.project_id=i.project_id
JOIN project_workflow_states w ON w.organization_id=i.organization_id AND w.project_id=i.project_id AND w.name=i.workflow_state_name
WHERE i.organization_id=$1 AND i.id=$2 AND i.project_id=$3 AND i.native_id=$4 AND i.revision=$5 AND i.event_sequence=$6 AND NOT i.archived AND i.terminal_entered_at IS NOT NULL
AND p.revision=$7 AND project.profile='native' AND project.deleted_at IS NULL AND w.terminal
AND CASE WHEN lower(w.name)='cancelled' THEN p.archive_cancelled_after_days ELSE p.archive_completed_after_days END IS NOT NULL
AND GREATEST(i.terminal_entered_at,i.archive_restored_at,CASE WHEN NOT i.archive_restore_known THEN i.native_updated_at ELSE i.terminal_entered_at END)
 < $8::timestamptz-make_interval(days=>(CASE WHEN lower(w.name)='cancelled' THEN p.archive_cancelled_after_days ELSE p.archive_completed_after_days END)::integer)
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=i.organization_id AND l.issue_id=i.id AND l.released_at IS NULL AND l.expires_at>$8)
AND NOT EXISTS (SELECT 1 FROM native_attempts a WHERE a.organization_id=i.organization_id AND a.issue_id=i.id AND a.status='running' AND (a.checkpoint_object_key IS NOT NULL AND a.cleanup_acknowledged_at IS NULL
OR NOT EXISTS (SELECT 1 FROM leases l WHERE l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND (l.released_at IS NOT NULL OR l.expires_at<=$8))))
AND NOT EXISTS (SELECT 1 FROM change_issue_links x JOIN change_requests c ON c.organization_id=x.organization_id AND c.project_id=x.project_id AND c.id=x.change_id WHERE x.organization_id=i.organization_id AND x.project_id=i.project_id AND x.work_item_id=i.native_id AND (c.current_version_id IS NULL OR c.landed_version_id IS DISTINCT FROM c.current_version_id))`, s.organization, item.ID, item.ProjectID, item.NativeID, item.Revision, item.EventSequence, settingsRevision, now).Scan(&days)
	if errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, mapError(ctx, err)
	}
	return days, true, nil
}

var _ corestore.WorkItemRetentionWriter = (*session)(nil)
