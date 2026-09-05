package web

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) issueDetail(c echo.Context) error {
	ctx := c.Request().Context()
	projectID := strings.TrimSpace(c.Param("project_id"))
	issueRef := strings.TrimSpace(c.Param("issue_ref"))
	trackedProject, ok := s.registry.Get(project.ID(projectID))
	if !ok || trackedProject.Workflow().Config.Tracker.Kind != workflowconfig.TrackerLocalSQLite && trackedProject.Workflow().Config.Tracker.Kind != workflowconfig.TrackerHubNative {
		return echo.NewHTTPError(http.StatusNotFound, "Issue not found")
	}

	var issues []connector.Issue
	var err error
	if trackedProject.Workflow().Config.Tracker.Kind == workflowconfig.TrackerHubNative {
		issues, err = trackedProject.Connector().FetchIssueStatesByIDs(ctx, []string{issueRef})
	} else if resolver, ok := trackedProject.Connector().(connector.IssueReferenceResolver); ok {
		issues, err = resolver.FetchIssueStatesByIdentifiers(ctx, []string{issueRef})
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "Issue could not be loaded").SetInternal(err)
	}
	issue, ok := issueDetailMatch(issues, issueRef)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Issue not found")
	}

	events := []connector.IssueEvent{}
	if reader, ok := trackedProject.Connector().(connector.IssueEventReader); ok {
		events, err = reader.FetchIssueEvents(ctx, issue)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Issue history could not be loaded").SetInternal(err)
		}
	}

	dashboard, ok := s.projectDashboardData(ctx, projectID, s.latestSnapshot(ctx))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	applyDashboardPreferences(c.Request(), &dashboard)
	data := templates.NewIssueDetailData(dashboard, issue, events)
	if reader, ok := trackedProject.Connector().(tracker.ChangeReader); ok {
		data.HasChanges = true
		data.Changes, err = reader.FetchChanges(ctx, tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			data.ChangesError = "Change Requests could not be loaded. Reload this issue to retry."
		}
	}
	return render(c, templates.IssueDetailPage(data))
}

func issueDetailMatch(issues []connector.Issue, issueRef string) (connector.Issue, bool) {
	issueRef = strings.TrimSpace(issueRef)
	for _, issue := range issues {
		if strings.EqualFold(strings.TrimSpace(issue.Identifier), issueRef) || strings.TrimSpace(issue.ID) == issueRef {
			return issue, true
		}
	}
	if len(issues) == 1 {
		return issues[0], true
	}
	return connector.Issue{}, false
}
