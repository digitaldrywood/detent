package hubserver

import (
	"context"
	"net/http"
	"slices"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (scope nativeScope) reporterOwns(actor tracker.Actor) bool {
	return scope.credential.HostedRole != "reporter" || scope.actor().PrincipalID != "" && actor.PrincipalID == scope.actor().PrincipalID
}

func (scope nativeScope) issueVisibilitySQL(alias string) (string, []any) {
	if scope.credential.HostedRole != "reporter" {
		return "1", nil
	}
	return "COALESCE(json_extract(" + alias + ".actor_json, '$.principal_id'), '') = ? AND ? <> ''", []any{scope.actor().PrincipalID, scope.actor().PrincipalID}
}

func reporterStatus(state string) string {
	switch state {
	case "Triage":
		return "submitted"
	case "Backlog", "Todo":
		return "planned"
	case "In Progress", "In Review", "Rework", "Merging", "Blocked":
		return "in_progress"
	case "Done":
		return "done"
	case "Cancelled":
		return "cancelled"
	default:
		return ""
	}
}

func (scope nativeScope) reporterIssue(issue tracker.NativeIssue) tracker.NativeIssue {
	issue.ReporterStatus = ""
	if scope.credential.HostedRole == "reporter" {
		issue.ReporterStatus = reporterStatus(issue.State)
	}
	return issue
}

func requireReporterTriage(scope nativeScope, issue tracker.NativeIssue) error {
	if scope.credential.HostedRole == "reporter" && issue.State != "Triage" {
		return &nativeError{Code: "report_already_with_team", Message: "This issue is already with the team. Add a comment to share more details.", status: http.StatusForbidden, publicMessage: true}
	}
	return nil
}

func requireReporterItem(ctx context.Context, q nativeQueryer, scope nativeScope, item string) error {
	if scope.credential.HostedRole != "reporter" {
		return nil
	}
	condition, args := scope.issueVisibilitySQL("i")
	args = append([]any{scope.organization, scope.project, item}, args...)
	var id string
	return q.QueryRowContext(ctx, "SELECT i.native_id FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND ("+condition+")", args...).Scan(&id)
}

func reporterReadContext(ctx context.Context, scope nativeScope) context.Context {
	if scope.credential.HostedRole != "reporter" {
		return ctx
	}
	visibility, args := scope.issueVisibilitySQL("ri")
	statement := "SELECT ri.native_id FROM main.issues ri WHERE ri.organization_id=? AND (" + visibility + ")"
	args = append([]any{scope.organization}, args...)
	if scope.project != "" {
		statement += " AND ri.project_id=?"
		args = append(args, scope.project)
	}
	existing, _ := reportsScopeFrom(ctx)
	if existing.issues != "" {
		statement += " AND ri.native_id IN (" + existing.issues + ")"
		args = append(args, existing.issueArgs...)
	}
	existing.issues, existing.issueArgs = statement, args
	existing.reporterPrincipal = scope.actor().PrincipalID
	return context.WithValue(ctx, reportsScopeKey{}, existing)
}

func reporterIssueIDs(ctx context.Context, q nativeQueryer, scope nativeScope) (map[string]bool, error) {
	visibility, args := scope.issueVisibilitySQL("i")
	statement := "SELECT i.native_id FROM issues i WHERE i.organization_id=? AND (" + visibility + ")"
	args = append([]any{scope.organization}, args...)
	if scope.project != "" {
		statement += " AND i.project_id=?"
		args = append(args, scope.project)
	}
	ids, err := nativePageIDs(ctx, q, statement, args...)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(ids))
	for _, id := range ids {
		visible[id] = true
	}
	return visible, nil
}

func reporterChange(ctx context.Context, q nativeQueryer, scope nativeScope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
	if scope.credential.HostedRole != "reporter" {
		return change, true, nil
	}
	visible, err := reporterIssueIDs(ctx, q, scope)
	if err != nil {
		return tracker.ChangeRequest{}, false, err
	}
	if !visible[string(change.WorkItemID)] {
		return tracker.ChangeRequest{}, false, nil
	}
	change.LinkedIssues = slices.DeleteFunc(change.LinkedIssues, func(id tracker.NativeWorkItemID) bool { return !visible[string(id)] })
	return change, true, nil
}
