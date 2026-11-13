package hubserver

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (scope nativeScope) reporterOwns(actor tracker.Actor) bool {
	return scope.credential.HostedRole != "reporter" || scope.actor().PrincipalID != "" && actor.PrincipalID == scope.actor().PrincipalID
}

func (scope nativeScope) issueVisibilitySQL(alias string) (string, []any) {
	if scope.credential.HostedRole != "reporter" {
		return "1", nil
	}
	return "COALESCE(" + nativeIssuePrincipal(alias) + ", '') = ? AND ? <> ''", []any{scope.actor().PrincipalID, scope.actor().PrincipalID}
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
		return &hubapi.NativeError{Code: "report_already_with_team", Message: "This issue is already with the team. Add a comment to share more details.", Status: http.StatusForbidden, PublicMessage: true}
	}
	return nil
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
	if existing.Issues != "" {
		statement += " AND ri.native_id IN (" + existing.Issues + ")"
		args = append(args, existing.IssueArgs...)
	}
	existing.Issues, existing.IssueArgs = statement, args
	existing.ReporterPrincipal = scope.actor().PrincipalID
	return context.WithValue(ctx, reportsScopeKey{}, existing)
}
