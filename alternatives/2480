package hubserver

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func requireReporterTriage(scope hubscope.NativeScope, issue tracker.NativeIssue) error {
	if scope.Credential.HostedRole == "reporter" && issue.State != "Triage" {
		return &hubapi.NativeError{Code: "report_already_with_team", Message: "This issue is already with the team. Add a comment to share more details.", Status: http.StatusForbidden, PublicMessage: true}
	}
	return nil
}

func reporterReadContext(ctx context.Context, scope hubscope.NativeScope) context.Context {
	if scope.Credential.HostedRole != "reporter" {
		return ctx
	}
	visibility, args := scope.IssueVisibilitySQL("ri")
	statement := "SELECT ri.native_id FROM main.issues ri WHERE ri.organization_id=? AND (" + visibility + ")"
	args = append([]any{scope.Organization}, args...)
	if scope.Project != "" {
		statement += " AND ri.project_id=?"
		args = append(args, scope.Project)
	}
	existing, _ := reportsScopeFrom(ctx)
	if existing.Issues != "" {
		statement += " AND ri.native_id IN (" + existing.Issues + ")"
		args = append(args, existing.IssueArgs...)
	}
	existing.Issues, existing.IssueArgs = statement, args
	existing.ReporterPrincipal = scope.Actor().PrincipalID
	return context.WithValue(ctx, reportsScopeKey{}, existing)
}
