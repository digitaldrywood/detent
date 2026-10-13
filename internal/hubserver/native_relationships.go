package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func changeRelatedIssuesTx(ctx context.Context, tx *sql.Tx, first, second tracker.WorkItemID, operation string, now time.Time) error {
	if first > second {
		first, second = second, first
	}
	if operation == "remove" {
		_, err := tx.ExecContext(ctx, "DELETE FROM issue_relationships WHERE first_issue_id = ? AND second_issue_id = ? AND relationship_type = 'related'", first, second)
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO issue_relationships (first_issue_id, second_issue_id, relationship_type, created_at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING", first, second, "related", formatHubTime(now))
	return err
}

func readNativeRelated(ctx context.Context, query nativeQueryer, scope nativeScope, item tracker.NativeWorkItemID) ([]tracker.NativeDependency, error) {
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	args := append([]any{item, scope.organization, scope.project, scope.organization}, grantArgs...)
	rows, err := query.QueryContext(ctx, `SELECT i.native_id, i.project_id, i.project_id || '#' || i.number, ws.detent_state, ws.terminal
FROM issue_relationships r JOIN issues source ON source.id IN (r.first_issue_id, r.second_issue_id)
JOIN issues i ON i.id = CASE WHEN source.id = r.first_issue_id THEN r.second_issue_id ELSE r.first_issue_id END
JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE source.native_id = ? AND source.organization_id = ? AND source.project_id = ? AND i.organization_id = ?
AND r.relationship_type = 'related' AND (`+condition+`) ORDER BY i.native_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	related := []tracker.NativeDependency{}
	for rows.Next() {
		var link tracker.NativeDependency
		if err := rows.Scan(&link.ID, &link.ProjectID, &link.Identifier, &link.State, &link.Terminal); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		related = append(related, link)
	}
	return related, errors.Join(rows.Err(), rows.Close())
}
