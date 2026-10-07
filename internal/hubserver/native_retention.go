package hubserver

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) maintainNativeRetention(ctx context.Context, now time.Time) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE attachments SET deleted_at=? WHERE deleted_at IS NULL AND work_item_id IS NULL AND julianday(created_at)<=julianday(?)", formatHubTime(now), formatHubTime(now.Add(-attachment.OrphanTTL))); err != nil {
		return err
	}
	period := "CASE WHEN ws.detent_state = 'Cancelled' THEN p.archive_cancelled_after_days ELSE p.archive_completed_after_days END"
	restored := `(SELECT e.recorded_at FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='issue.edited' AND json_extract(e.data_json,'$.operation')='restore' ORDER BY e.sequence DESC LIMIT 1)`
	entered := "julianday(" + nativeTerminalEnteredAt + ")"
	rows, err := tx.QueryContext(ctx, `SELECT i.organization_id, i.project_id, i.native_id, `+period+`
FROM issues i JOIN projects p ON p.id=i.project_id AND p.organization_id=i.organization_id JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE p.profile='native' AND i.archived=0 AND ws.terminal=1 AND `+period+` IS NOT NULL
AND max(`+entered+`, COALESCE(julianday(`+restored+`), `+entered+`)) < julianday(?) - (`+period+`)
AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.issue_id=i.id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?))
AND NOT EXISTS (SELECT 1 FROM change_issue_links l JOIN change_requests c ON c.id=l.change_id
WHERE l.organization_id=i.organization_id AND l.project_id=i.project_id AND l.work_item_id=i.native_id
AND COALESCE(json_extract(c.record_json,'$.current_version_id'),'')<>''
AND COALESCE(json_extract(c.record_json,'$.landed.version_id'),'')<>json_extract(c.record_json,'$.current_version_id'))
ORDER BY i.id LIMIT 200`, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	type candidate struct {
		scope nativeScope
		item  string
		days  int
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.scope.organization, &item.scope.project, &item.item, &item.days); err != nil {
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		item.scope.sourceActor = nativeIntegrationActor("auto_archive")
		issue, _, err := readNativeIssue(ctx, tx, item.scope, item.item)
		if err != nil {
			return err
		}
		issue.Archived = true
		data := tracker.CollaborationData{Fields: []string{"archived"}, Operation: "archive", ReasonDetail: fmt.Sprintf("Archived automatically after %d days", item.days)}
		if _, err := persistNativeIssue(ctx, tx, item.scope, issue, "issue.edited", data, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
