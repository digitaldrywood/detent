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
	issue := &tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: item}}
	raw, err := marshalNative([]tracker.NativeWorkItemID{item})
	if err != nil {
		return nil, err
	}
	err = readNativeRelatedBatch(ctx, query, scope, map[tracker.NativeWorkItemID]*tracker.NativeIssue{item: issue}, raw)
	return issue.Related, err
}

func readNativeRelatedBatch(ctx context.Context, query nativeQueryer, scope nativeScope, items map[tracker.NativeWorkItemID]*tracker.NativeIssue, selection string) error {
	for _, issue := range items {
		issue.Related = []tracker.NativeDependency{}
	}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	condition = "(" + condition + ") AND (" + visibility + ")"
	grantArgs = append(grantArgs, visibilityArgs...)
	args := append([]any{selection, scope.organization, scope.project, scope.organization}, grantArgs...)
	rows, err := query.QueryContext(ctx, `SELECT source.native_id, i.native_id, i.project_id, i.project_id || '#' || i.number, ws.detent_state, ws.terminal, i.number, i.title, o.slug, p.slug
FROM issue_relationships r JOIN issues source ON source.id IN (r.first_issue_id, r.second_issue_id)
JOIN issues i ON i.id = CASE WHEN source.id = r.first_issue_id THEN r.second_issue_id ELSE r.first_issue_id END
JOIN workflow_states ws ON ws.id = i.workflow_state_id
JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id
WHERE source.native_id IN (SELECT value FROM json_each(?)) AND source.organization_id = ? AND source.project_id = ? AND i.organization_id = ?
AND r.relationship_type = 'related' AND (`+condition+`) ORDER BY source.native_id,i.native_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var link tracker.NativeDependency
		var organizationSlug, projectSlug string
		if err := rows.Scan(&id, &link.ID, &link.ProjectID, &link.Identifier, &link.State, &link.Terminal, &link.Number, &link.Title, &organizationSlug, &projectSlug); err != nil {
			return errors.Join(err, rows.Close())
		}
		link.WebURL = issueBrowserPath(organizationSlug, projectSlug, link.Number)
		issue := items[id]
		if issue == nil {
			return sql.ErrNoRows
		}
		issue.Related = append(issue.Related, link)
	}
	return errors.Join(rows.Err(), rows.Close())
}
