package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
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
	err := readNativeRelatedBatch(ctx, query, scope, map[tracker.NativeWorkItemID]*tracker.NativeIssue{item: issue})
	return issue.Related, err
}

func readNativeRelatedBatch(ctx context.Context, query nativeQueryer, scope nativeScope, items map[tracker.NativeWorkItemID]*tracker.NativeIssue) error {
	if len(items) == 0 {
		return nil
	}
	if scope.credential.HostedRole == "reporter" {
		pageQuery, pageErr := prepareNativePageQuery(ctx, query, nativeScope{organization: scope.organization}, "", "native_issue_actors")
		if pageErr != nil {
			return pageErr
		}
		query = pageQuery
	}
	ids := make([]any, 0, len(items))
	for id, issue := range items {
		ids = append(ids, id)
		issue.Related = []tracker.NativeDependency{}
	}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	condition = "(" + condition + ") AND (" + visibility + ")"
	grantArgs = append(grantArgs, visibilityArgs...)
	args := append(ids, scope.organization, scope.project, scope.organization)
	args = append(args, grantArgs...)
	rows, err := query.QueryContext(ctx, `SELECT source.native_id, i.native_id, i.project_id, i.project_id || '#' || i.number, ws.detent_state, ws.terminal, i.number, i.title, o.slug, p.slug
FROM issues source CROSS JOIN issue_relationships r ON r.first_issue_id=source.id OR r.second_issue_id=source.id
JOIN issues i ON i.id = CASE WHEN source.id = r.first_issue_id THEN r.second_issue_id ELSE r.first_issue_id END
JOIN workflow_states ws ON ws.id = i.workflow_state_id
JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id
WHERE source.native_id IN (`+placeholders(len(ids))+`) AND source.organization_id = ? AND source.project_id = ? AND i.organization_id = ?
AND r.relationship_type = 'related' AND (`+condition+`)`, args...)
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
	for _, issue := range items {
		slices.SortFunc(issue.Related, func(a, b tracker.NativeDependency) int {
			return strings.Compare(string(a.ID), string(b.ID))
		})
	}
	return errors.Join(rows.Err(), rows.Close())
}
