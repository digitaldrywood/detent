package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) operationsReport(c echo.Context) (operations.Report, error) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	if value := c.QueryParam("since"); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.After(now) {
			return operations.Report{}, echo.NewHTTPError(http.StatusBadRequest, "since must be an RFC3339 timestamp at or before now")
		}
		since = parsed
	}
	report, err := s.store.OperationsReport(c.Request().Context(), now, since)
	if err != nil {
		return operations.Report{}, err
	}
	report.Instance = s.instanceName()
	snapshot := s.latestSnapshot(c.Request().Context())

	issues := dailyDigestSnapshotIssues(snapshot)
	mergeDepths := map[string]int{}
	seen := map[string]bool{}
	for _, d := range report.Decisions {
		seen[d.ProjectID+"\x00"+d.Issue] = true
	}
	for _, issue := range issues {
		if issue.PullRequest != nil && issue.PullRequest.MergeQueueEntry != nil {
			depth := issue.PullRequest.MergeQueueEntry.Depth
			if depth > mergeDepths[issue.ProjectID] {
				mergeDepths[issue.ProjectID] = depth
			}
		}

		if issue.RequiredGate != nil && issue.RequiredGate.HumanAction != "" && !seen[issue.ProjectID+"\x00"+issue.Identifier] {
			report.Decisions = append(report.Decisions, operations.Decision{ProjectID: issue.ProjectID, Issue: issue.Identifier, Question: issue.RequiredGate.HumanAction, URL: issue.URL})
		}
		for i := range report.Actions {
			a := &report.Actions[i]
			if a.ProjectID == issue.ProjectID && a.Issue == issue.ID {
				a.Issue = issue.Identifier
				a.EvidenceURL = issue.URL
			}
		}
	}
	for _, queued := range snapshot.Queue {
		if strings.EqualFold(queued.State, "Merging") && mergeDepths[queued.ProjectID] == 0 {
			report.QueueDepth++
		}
	}
	for _, depth := range mergeDepths {
		report.QueueDepth += depth
	}
	for i := range report.Actions {
		a := &report.Actions[i]
		if a.EvidenceURL == "" {
			a.EvidenceURL = "/api/v1/projects/" + url.PathEscape(a.ProjectID) + "/issues/explanation?reference=" + url.QueryEscape(a.Issue)
		}
	}
	for i := range report.Decisions {
		d := &report.Decisions[i]
		if d.URL == "" {
			d.URL = "/api/v1/projects/" + url.PathEscape(d.ProjectID) + "/issues/explanation?reference=" + url.QueryEscape(d.Issue)
		}
	}
	sort.Slice(report.Decisions, func(i, j int) bool {
		a, b := report.Decisions[i], report.Decisions[j]
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.Issue < b.Issue
	})
	return report, nil
}

func (s *Server) apiOperations(c echo.Context) error {
	report, err := s.operationsReport(c)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Server) operationsPage(c echo.Context) error {
	report, err := s.operationsReport(c)
	if err != nil {
		return err
	}
	if c.Request().Header.Get("HX-Request") == "true" {
		return render(c, templates.OperationsSnapshot(report))
	}
	data := s.analyticsDashboardData(c.Request().Context(), s.latestSnapshot(c.Request().Context()))
	data.ActiveNav = "operations"
	applyDashboardPreferences(c.Request(), &data)
	shell := templates.DashboardShellDataFromDashboard(data)
	shell.Title = instancePageTitle(report.Instance, "Operations")
	return render(c, templates.OperationsPage(shell, report))
}
