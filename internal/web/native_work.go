package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type nativeClientSource interface {
	NativeClient() *hubclient.NativeClient
}

func (s *Server) nativeWorkReadBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Method != http.MethodGet || !requestAPICredentialsSupplied(c.Request()) {
			return next(c)
		}
		native := false
		for _, tracked := range s.registry.List() {
			if _, ok := tracked.Connector().(nativeClientSource); ok {
				native = true
				break
			}
		}
		if !native {
			return next(c)
		}
		credential, err := s.authorizeAPIRequest(c, apiAuthOptions{})
		if err != nil {
			return err
		}
		if len(credential.ProjectIDs) > 0 && !strings.HasPrefix(c.Path(), "/projects/:project_id/issues/") {
			return echo.NewHTTPError(http.StatusForbidden, "Project-scoped credentials cannot read aggregate dashboard data; use scoped Hub APIs")
		}
		return next(c)
	}
}

func nativeScopedDashboard(c echo.Context, data *templates.DashboardData) {
	credential, ok := apiCredentialFromContext(c.Request().Context())
	if !ok || len(credential.ProjectIDs) == 0 {
		return
	}
	projects := make([]templates.ProjectSmallMultiple, 0, 1)
	for _, entry := range data.Projects {
		if entry.ID == c.Param("project_id") {
			projects = append(projects, entry)
		}
	}
	data.Projects = projects
}

func (s *Server) nativeFormAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 512<<10)
		if requestAPICredentialsSupplied(c.Request()) {
			return s.apiAuth(true)(next)(c)
		}
		if requestSameOriginDashboardSource(c.Request()) && apiStaticTokenEqual(c.FormValue("form_token"), s.apiKeyDashboardToken()) {
			if err := s.applyPreAuthIPRateLimit(c); err != nil {
				return err
			}
			s.setAPICredential(c, apikey.StaticCredential())
			return next(c)
		}
		return echo.NewHTTPError(http.StatusForbidden, "Open this form in Detent before submitting it")
	}
}

func (s *Server) nativeReadAccess(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if requestAPICredentialsSupplied(c.Request()) {
			credential, err := s.authorizeAPIRequest(c, apiAuthOptions{})
			if err != nil {
				return err
			}
			if !apikey.HasScope(credential.Scopes, apikey.ScopeRead) || !apikey.AllowsProject(credential.ProjectIDs, c.Param("project_id")) {
				return echo.NewHTTPError(http.StatusNotFound, "Project not found")
			}
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return next(c)
	}
}

func (s *Server) nativeFormData(c echo.Context) (templates.NativeFormData, *hubclient.NativeClient, error) {
	data := templates.NativeFormData{Action: c.QueryParam("action"), Key: uuid.NewString()}
	if !requestAPICredentialsSupplied(c.Request()) {
		data.FormToken = s.apiKeyDashboardToken()
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	id := c.Param("project_id")
	tracked, ok := s.registry.Get(project.ID(id))
	if !ok {
		return data, nil, echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	source, ok := tracked.Connector().(nativeClientSource)
	if !ok {
		return data, nil, echo.NewHTTPError(http.StatusNotFound, "Native collaboration is unavailable")
	}
	client := source.NativeClient()
	if client == nil {
		return data, nil, echo.NewHTTPError(http.StatusNotFound, "Native collaboration is unavailable")
	}
	ctx := c.Request().Context()
	data.Dashboard, ok = s.projectDashboardData(ctx, id, s.latestSnapshot(ctx))
	if !ok {
		return data, nil, echo.NewHTTPError(http.StatusNotFound, "Project not found")
	}
	applyDashboardPreferences(c.Request(), &data.Dashboard)
	nativeScopedDashboard(c, &data.Dashboard)
	if c.Request().Method == http.MethodPost {
		data.Issue.WorkItemID = tracker.NativeWorkItemID(c.Param("issue_ref"))
		for _, state := range data.Dashboard.Kanban.States {
			data.Project.States = append(data.Project.States, tracker.NativeState{Name: state})
		}
		return data, client, nil
	}
	var err error
	data.Project, err = client.Project(ctx)
	if err != nil {
		return data, nil, nativeReadError(err)
	}
	if c.Param("issue_ref") == "" {
		data.Action = "create"
		return data, client, nil
	}
	data.Issue, err = client.Issue(ctx, tracker.NativeWorkItemID(c.Param("issue_ref")))
	if err != nil {
		return data, nil, nativeReadError(err)
	}
	data.Revision = strconv.FormatInt(int64(data.Issue.Revision), 10)
	data.State = data.Issue.State
	if data.Action == "" {
		data.Action = "edit"
	}
	if data.Action == "edit" {
		data.Title, data.Body = data.Issue.Title, data.Issue.Body
		if data.Issue.Priority != nil {
			data.Priority = strconv.Itoa(*data.Issue.Priority)
		}
	}
	return data, client, nil
}

func nativeReadError(err error) error {
	var apiErr *hubclient.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusForbidden) {
		return echo.NewHTTPError(http.StatusNotFound, "Resource not found")
	}
	return echo.NewHTTPError(http.StatusBadGateway, "Hub could not load this resource. Reload to retry.").SetInternal(err)
}

func (s *Server) nativeIssueForm(c echo.Context) error {
	data, client, err := s.nativeFormData(c)
	if err != nil {
		return err
	}
	if data.Action == "comment_edit" {
		page, err := client.Comments(c.Request().Context(), data.Issue.WorkItemID, c.QueryParam("cursor"))
		if err != nil {
			return nativeReadError(err)
		}
		for _, comment := range page.Items {
			if comment.ID == c.QueryParam("comment") {
				data.CommentID, data.Body = comment.ID, comment.Body
				data.Revision = strconv.FormatInt(int64(comment.Revision), 10)
			}
		}
		if data.CommentID == "" {
			return echo.NewHTTPError(http.StatusNotFound, "Comment not found")
		}
	}
	return render(c, templates.NativeIssueForm(data))
}

func (s *Server) nativeIssueSubmit(c echo.Context) error {
	data, client, err := s.nativeFormData(c)
	if err != nil {
		return err
	}
	data.Action, data.Key, data.Revision = c.FormValue("action"), c.FormValue("key"), c.FormValue("revision")
	data.Title, data.Body, data.State = c.FormValue("title"), c.FormValue("body"), c.FormValue("state")
	data.Priority, data.Related, data.Operation = c.FormValue("priority"), c.FormValue("related"), c.FormValue("operation")
	data.CommentID = c.FormValue("comment")
	target, err := submitNativeForm(c, client, data)
	if err != nil {
		status := http.StatusBadGateway
		data.Error = "Hub is unavailable. Your text is preserved; retry this submission."
		var formError *echo.HTTPError
		if errors.As(err, &formError) {
			status = http.StatusUnprocessableEntity
			data.Error = "Changes were not saved. Check the form values and retry."
		}
		var apiErr *hubclient.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusUnprocessableEntity, http.StatusBadRequest:
				status = http.StatusUnprocessableEntity
				data.Error = "Changes were not saved. Check the form values and permitted workflow, then retry."
			case http.StatusConflict:
				status, data.Conflict = http.StatusConflict, true
				data.Error = "This resource changed after you opened the form. Your text is preserved below. Open the current issue and compare before editing again."
			case http.StatusUnauthorized, http.StatusForbidden:
				status = http.StatusForbidden
				data.Error = "Your Hub credential cannot make this change. Operator access or an active worker lease is required."
			case http.StatusNotFound:
				status = http.StatusNotFound
				data.Error = "The resource was not found in this project."
			default:
				if apiErr.Status >= 500 {
					status = http.StatusBadGateway
					data.Error = "Hub is unavailable. Your text is preserved; retry this submission."
				}
			}
		}
		c.Response().Header().Set("Content-Type", echo.MIMETextHTMLCharsetUTF8)
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().WriteHeader(status)
		return render(c, templates.NativeIssueForm(data))
	}
	s.requestKanbanRefresh(c.Request().Context())
	c.Response().Header().Set("HX-Redirect", target)
	return c.Redirect(http.StatusSeeOther, target)
}

func submitNativeForm(c echo.Context, client *hubclient.NativeClient, data templates.NativeFormData) (string, error) {
	ctx := c.Request().Context()
	mutation := tracker.Mutation{IdempotencyKey: data.Key}
	if data.Key == "" || len(data.Key) > 128 {
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid mutation key")
	}
	id := data.Issue.WorkItemID
	if (id == "") != (data.Action == "create") {
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid form action")
	}
	revision, err := strconv.ParseInt(data.Revision, 10, 64)
	if data.Action != "create" && data.Action != "comment" && data.Action != "change" && (err != nil || revision <= 0) {
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid revision")
	}
	var priority *int
	if data.Priority != "" {
		value, err := strconv.Atoi(data.Priority)
		if err != nil || value < 0 || value > 3 {
			return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid priority")
		}
		priority = &value
	}
	switch data.Action {
	case "create":
		issue, err := client.CreateIssue(ctx, tracker.CreateIssue{Mutation: mutation, Title: data.Title, Body: data.Body, State: data.State, Priority: priority})
		return templates.NativeIssuePath(data.Dashboard.ProjectID, issue.WorkItemID), err
	case "edit":
		_, err = client.UpdateIssue(ctx, id, tracker.UpdateIssue{Mutation: mutation, ExpectedRevision: tracker.Revision(revision), Title: &data.Title, Body: &data.Body, Priority: priority})
	case "transition":
		_, err = client.Transition(ctx, id, tracker.Transition{Mutation: mutation, ExpectedRevision: tracker.Revision(revision), State: data.State, Reason: "user_requested"})
	case "dependency":
		_, err = client.Dependency(ctx, id, tracker.DependencyMutation{Mutation: mutation, ExpectedRevision: tracker.Revision(revision), RelatedWorkItemID: tracker.NativeWorkItemID(strings.TrimSpace(data.Related)), Operation: data.Operation})
	case "comment":
		_, err = client.CreateComment(ctx, id, tracker.CreateComment{Mutation: mutation, Body: data.Body})
	case "comment_edit":
		_, err = client.UpdateComment(ctx, id, data.CommentID, tracker.UpdateComment{Mutation: mutation, ExpectedRevision: tracker.Revision(revision), Body: data.Body})
	case "change":
		var linked []tracker.NativeWorkItemID
		for _, related := range strings.Fields(data.Related) {
			linked = append(linked, tracker.NativeWorkItemID(related))
		}
		change, changeErr := client.CreateChange(ctx, id, tracker.CreateChange{Mutation: mutation, Title: data.Title, Body: data.Body, LinkedIssues: linked})
		return templates.ChangePath(data.Dashboard.ProjectID, id, change.ID), changeErr
	default:
		return "", echo.NewHTTPError(http.StatusUnprocessableEntity, "invalid form action")
	}
	return templates.NativeIssuePath(data.Dashboard.ProjectID, id), err
}

func (s *Server) loadNativeWork(c echo.Context, client *hubclient.NativeClient, id tracker.NativeWorkItemID) (*templates.NativeWorkData, error) {
	ctx := c.Request().Context()
	data := &templates.NativeWorkData{}
	var err error
	data.Issue, err = client.Issue(ctx, id)
	if err != nil {
		return nil, nativeReadError(err)
	}
	data.Project, err = client.Project(ctx)
	if err != nil {
		return nil, nativeReadError(err)
	}
	data.Comments, err = client.Comments(ctx, id, c.QueryParam("cursor"))
	if err != nil {
		data.DiscussionError = "Discussion could not be loaded. Return to the first page and retry."
	}
	policy, err := client.ProjectPolicy(ctx)
	if err != nil {
		data.PolicyError = "Approved repository policy is unavailable."
	} else {
		data.Policy = &policy
	}
	tracked, ok := s.registry.Get(project.ID(c.Param("project_id")))
	if ok {
		if reader, ok := tracked.Connector().(tracker.ChangeReader); ok {
			data.Attempts, err = reader.FetchNativeAttempts(ctx, id)
			if err != nil {
				data.ExecutionError = "Run history is unavailable."
			}
		}
	}
	if s.runnerFleet != nil {
		eligibility, err := s.runnerFleet.ProjectEligibility(ctx, c.Param("project_id"))
		if err == nil {
			data.Eligibility = &eligibility
		}
	}
	return data, nil
}

func (s *Server) nativeIssueExport(c echo.Context) error {
	data, _, err := s.nativeFormData(c)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Content-Disposition", "attachment; filename=issue.json")
	return c.JSON(http.StatusOK, data.Issue)
}
