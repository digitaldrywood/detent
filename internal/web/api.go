package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	issueDescriptionLimit              = 250
	gitHubGraphQLRefreshPauseRemaining = int64(100)
)

type Refresher interface {
	RequestRefresh(context.Context) (RefreshResponse, error)
}

type TargetedRefresher interface {
	RequestTargetedRefresh(context.Context, RefreshTarget) (RefreshResponse, error)
}

type RefreshTarget struct {
	Repository        string `json:"repository,omitempty"`
	IssueNumber       int    `json:"issue_number,omitempty"`
	PullRequestNumber int    `json:"pull_request_number,omitempty"`
	SHA               string `json:"sha,omitempty"`
	Event             string `json:"event,omitempty"`
	DeliveryID        string `json:"delivery_id,omitempty"`
}

type RefreshResponse = orchestrator.RefreshResponse

func (s *Server) apiState(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		if scenario.ID == "api-state-no-snapshot" {
			return c.JSON(http.StatusOK, snapshotErrorResponse(demoBaseTime, "snapshot_unavailable", "Snapshot unavailable"))
		}
		snapshot := demoSnapshotForScenario(scenario)
		return c.JSON(http.StatusOK, stateResponse(snapshot, generatedAt(snapshot, demoBaseTime), s.instanceName()))
	}
	now := apiNow()
	snapshot, ok := s.hub.Latest()
	if !ok {
		return c.JSON(http.StatusOK, snapshotErrorResponse(now, "snapshot_unavailable", "Snapshot unavailable"))
	}
	snapshot = s.cachedEnrichedSnapshot(c.Request().Context(), snapshot)
	snapshot = s.withManualRefresh(snapshot)

	return c.JSON(http.StatusOK, stateResponse(snapshot, generatedAt(snapshot, now), s.instanceName()))
}

func (s *Server) apiProject(c echo.Context) error {
	if projectID, ok := projectAPIParam(c, "state"); ok {
		return s.apiProjectState(c, projectID)
	}
	if projectID, ok := projectAPIParam(c, "timeseries"); ok {
		return s.apiProjectTimeSeries(c, projectID)
	}
	return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
}

func projectAPIParam(c echo.Context, suffix string) (string, bool) {
	path := projectRouteParam(c)
	trimmedSuffix := "/" + strings.Trim(strings.TrimSpace(suffix), "/")
	if !strings.HasSuffix(path, trimmedSuffix) {
		return "", false
	}
	projectID := strings.Trim(strings.TrimSuffix(path, trimmedSuffix), "/")
	return projectID, projectID != ""
}

func (s *Server) apiProjectState(c echo.Context, projectID string) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		snapshot := demoSnapshotForScenario(scenario)
		projects := demoProjectsForVariant(scenario.Variant)
		project, ok := demoProjectByID(projects, projectID)
		if !ok {
			return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
		}
		scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: project.ID, DisplayName: project.Name, URL: project.URL})
		return c.JSON(http.StatusOK, stateResponse(scoped, generatedAt(scoped, demoBaseTime), s.instanceName()))
	}
	now := apiNow()
	snapshot, ok := s.hub.Latest()
	if !ok {
		return c.JSON(http.StatusOK, snapshotErrorResponse(now, "snapshot_unavailable", "Snapshot unavailable"))
	}
	snapshot = s.cachedEnrichedSnapshot(c.Request().Context(), snapshot)
	projects := s.projectSmallMultiples(c.Request().Context(), snapshot)
	project, ok := s.dashboardProject(projectID, projects, snapshot)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	scopedSnapshot := projectScopedSnapshotForProject(snapshot, telemetry.Project{
		ID:          project.ID,
		DisplayName: project.Name,
		URL:         project.URL,
	})
	scopedSnapshot.WorkflowMetrics = s.snapshotWorkflowMetrics(c.Request().Context(), scopedSnapshot)
	scopedSnapshot = s.withManualRefresh(scopedSnapshot)

	return c.JSON(http.StatusOK, stateResponse(scopedSnapshot, generatedAt(scopedSnapshot, now), s.instanceName()))
}

func (s *Server) apiTimeSeries(c echo.Context) error {
	window, bucket, response, status := timeSeriesQuery(c)
	if response != nil {
		return c.JSON(status, response)
	}
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		if scenario.Variant == "invalid-query" {
			return c.JSON(http.StatusBadRequest, errorResponse("invalid_duration", "window must be a duration such as 10m or 30s"))
		}
		projects := demoProjectsForVariant(scenario.Variant)
		return c.JSON(http.StatusOK, projectTimeSeriesResponse(projects, "", demoBaseTime, window, bucket))
	}

	snapshot := s.latestSnapshot(c.Request().Context())
	projects := s.projectSmallMultiples(c.Request().Context(), snapshot)
	return c.JSON(http.StatusOK, projectTimeSeriesResponse(projects, "", generatedAt(snapshot, apiNow()), window, bucket))
}

func (s *Server) apiProjectTimeSeries(c echo.Context, projectID string) error {
	window, bucket, response, status := timeSeriesQuery(c)
	if response != nil {
		return c.JSON(status, response)
	}
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		projects := demoProjectsForVariant(scenario.Variant)
		if _, ok := demoProjectByID(projects, projectID); !ok {
			return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
		}
		return c.JSON(http.StatusOK, projectTimeSeriesResponse(projects, projectID, demoBaseTime, window, bucket))
	}

	snapshot := s.latestSnapshot(c.Request().Context())
	projects := s.projectSmallMultiples(c.Request().Context(), snapshot)
	project, ok := s.dashboardProject(projectID, projects, snapshot)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	return c.JSON(http.StatusOK, projectTimeSeriesResponse(projects, project.ID, generatedAt(snapshot, apiNow()), window, bucket))
}

func (s *Server) apiIssue(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		payload, found := issueResponse(issueIdentifier(c), demoSnapshotForScenario(scenario))
		if !found {
			return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
		}
		return c.JSON(http.StatusOK, payload)
	}
	snapshot, ok := s.hub.Latest()
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	}
	snapshot = s.cachedEnrichedSnapshot(c.Request().Context(), snapshot)

	payload, ok := issueResponse(issueIdentifier(c), snapshot)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	}

	return c.JSON(http.StatusOK, payload)
}

func (s *Server) apiRefresh(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoRefresh(c, scenario)
	}
	if s.refresher == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}

	now := apiNow()
	if payload, ok := s.refreshRefusal(now); ok {
		if s.refreshes != nil {
			s.refreshes.recordResponse(payload)
		}
		if htmxRequest(c) {
			return render(c, templates.ManualRefreshFeedback(refreshAttemptFromResponse(payload)))
		}
		return c.JSON(http.StatusTooManyRequests, payload)
	}

	payload, err := s.refresher.RequestRefresh(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}
	if payload.RequestedAt.IsZero() {
		payload.RequestedAt = now
	}
	if payload.RequestID == "" {
		payload.RequestID = "manual-" + strconv.FormatInt(payload.RequestedAt.UnixNano(), 10)
	}
	if payload.Status == "" {
		if payload.Refused {
			payload.Status = telemetry.RefreshAttemptStatusRefused
		} else if payload.Coalesced {
			payload.Status = telemetry.RefreshAttemptStatusCoalesced
		} else if payload.Queued {
			payload.Status = telemetry.RefreshAttemptStatusInProgress
		}
	}
	if payload.Operations == nil {
		payload.Operations = []string{}
	}
	if s.refreshes != nil {
		s.refreshes.recordResponse(payload)
	}

	if htmxRequest(c) {
		return render(c, templates.ManualRefreshFeedback(refreshAttemptFromResponse(payload)))
	}
	return c.JSON(http.StatusAccepted, payload)
}

func (s *Server) refreshRefusal(now time.Time) (RefreshResponse, bool) {
	if s == nil || s.hub == nil {
		return RefreshResponse{}, false
	}
	snapshot, ok := s.hub.Latest()
	if !ok {
		return RefreshResponse{}, false
	}
	refusal, ok := refreshRefusalFromSnapshot(snapshot, now)
	if !ok {
		return RefreshResponse{}, false
	}
	lastErrorAt := now.UTC()
	return RefreshResponse{
		RequestID:   "manual-" + strconv.FormatInt(lastErrorAt.UnixNano(), 10),
		Status:      telemetry.RefreshAttemptStatusRefused,
		Refused:     true,
		RequestedAt: lastErrorAt,
		LastError:   refusal.reason,
		LastErrorAt: &lastErrorAt,
		RetryAt:     refusal.retryAt,
		Operations:  []string{"poll", "reconcile"},
	}, true
}

type refreshRefusal struct {
	reason  string
	retryAt *time.Time
}

func refreshRefusalFromSnapshot(snapshot telemetry.Snapshot, now time.Time) (refreshRefusal, bool) {
	if snapshot.RateLimits == nil {
		return refreshRefusal{}, false
	}
	if refusal, ok := refreshRefusalFromBucket("GitHub REST", snapshot.RateLimits.GitHubREST, now, 0); ok {
		return refusal, true
	}
	if refusal, ok := refreshRefusalFromRESTUsage(snapshot.RateLimits.RESTUsage, now); ok {
		return refusal, true
	}
	if refusal, ok := refreshRefusalFromBucket("GitHub GraphQL", snapshot.RateLimits.GitHubGraphQL, now, gitHubGraphQLRefreshPauseRemaining); ok {
		return refusal, true
	}
	return refreshRefusal{}, false
}

func refreshRefusalFromRESTUsage(usage *telemetry.RESTUsage, now time.Time) (refreshRefusal, bool) {
	if usage == nil || usage.BackoffUntil == nil || !usage.BackoffUntil.After(now) {
		return refreshRefusal{}, false
	}
	retryAt := usage.BackoffUntil.UTC()
	return refreshRefusal{
		reason:  "GitHub REST backoff is active until " + refreshRefusalTimeLabel(retryAt) + ". Force refresh was refused to preserve hard rate-limit constraints.",
		retryAt: &retryAt,
	}, true
}

func refreshRefusalFromBucket(name string, bucket *telemetry.RateLimitBucket, now time.Time, pauseRemaining int64) (refreshRefusal, bool) {
	if bucket == nil || bucket.ResetAt == nil || !bucket.ResetAt.After(now) {
		return refreshRefusal{}, false
	}
	if !refreshBucketPaused(bucket, pauseRemaining) {
		return refreshRefusal{}, false
	}
	retryAt := bucket.ResetAt.UTC()
	return refreshRefusal{
		reason:  fmt.Sprintf("%s backoff is active until %s. Force refresh was refused to preserve hard rate-limit constraints.", name, refreshRefusalTimeLabel(retryAt)),
		retryAt: &retryAt,
	}, true
}

func refreshBucketPaused(bucket *telemetry.RateLimitBucket, pauseRemaining int64) bool {
	if bucket.ResetInSeconds > 0 {
		return true
	}
	switch strings.TrimSpace(bucket.Status) {
	case telemetry.RateLimitStatusBackoff, telemetry.RateLimitStatusExhausted:
		return true
	}
	if pauseRemaining > 0 {
		return bucket.Remaining < pauseRemaining
	}
	return bucket.Limit > 0 && bucket.Remaining <= 0
}

func htmxRequest(c echo.Context) bool {
	return strings.EqualFold(c.Request().Header.Get("HX-Request"), "true")
}

func refreshRefusalTimeLabel(value time.Time) string {
	if value.IsZero() {
		return "n/a"
	}
	return value.UTC().Format("Jan 2 15:04:05 UTC")
}

func refreshAttemptFromResponse(response RefreshResponse) *telemetry.RefreshAttempt {
	return &telemetry.RefreshAttempt{
		ID:          strings.TrimSpace(response.RequestID),
		Status:      response.Status,
		RequestedAt: optionalRefreshTime(response.RequestedAt),
		Operations:  append([]string(nil), response.Operations...),
		Coalesced:   response.Coalesced,
		LastError:   strings.TrimSpace(response.LastError),
		LastErrorAt: cloneTimePtr(response.LastErrorAt),
	}
}

func (s *Server) apiUsage(c echo.Context) error {
	query, response, status := usageReportQuery(c)
	if response != nil {
		return c.JSON(status, response)
	}
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		if scenario.Variant == "invalid-date-range" {
			return c.JSON(http.StatusBadRequest, errorResponse("invalid_date_range", "from must be on or before to"))
		}
		if scenario.Variant == "reports-empty" {
			return c.JSON(http.StatusOK, usageReportResponse(store.UsageReport{By: query.By}, s.pricing))
		}
	}

	report, err := s.store.UsageReport(c.Request().Context(), query)
	if err != nil {
		s.logger.Error("usage report failed", slog.Any("error", err))
		return c.JSON(http.StatusInternalServerError, errorResponse("usage_report_failed", "Usage report failed"))
	}

	return c.JSON(http.StatusOK, usageReportResponse(report, s.pricing))
}

func (s *Server) apiWorkflowTimeline(c echo.Context) error {
	identity := store.IssueIdentity{
		IssueID:    strings.TrimSpace(c.QueryParam("issue_id")),
		Identifier: strings.TrimSpace(c.QueryParam("identifier")),
		IssueURL:   strings.TrimSpace(c.QueryParam("issue_url")),
	}
	if identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		return c.JSON(http.StatusBadRequest, errorResponse("missing_issue_identity", "issue_id, identifier, or issue_url is required"))
	}
	timeline, err := s.store.IssueWorkflowTimeline(c.Request().Context(), identity)
	if err != nil {
		s.logger.Error("workflow timeline failed", slog.Any("error", err))
		return c.JSON(http.StatusInternalServerError, errorResponse("workflow_timeline_failed", "Workflow timeline failed"))
	}
	return c.JSON(http.StatusOK, workflowTimelineResponse(timeline))
}

func (s *Server) methodNotAllowed(c echo.Context) error {
	return c.JSON(http.StatusMethodNotAllowed, errorResponse("method_not_allowed", "Method not allowed"))
}

func (s *Server) handleHTTPError(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	status := http.StatusInternalServerError
	payload := errorResponse("internal_server_error", "Internal server error")

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
		switch status {
		case http.StatusNotFound:
			payload = errorResponse("not_found", "Route not found")
		case http.StatusMethodNotAllowed:
			payload = errorResponse("method_not_allowed", "Method not allowed")
		}
	}

	if writeErr := c.JSON(status, payload); writeErr != nil {
		s.logger.Error("write http error response failed", slog.Any("error", writeErr))
	}
}

func stateResponse(snapshot telemetry.Snapshot, generatedAt time.Time, instanceName string) stateAPIResponse {
	return stateAPIResponse{
		GeneratedAt:     generatedAt,
		Status:          runtimeStatus(snapshot),
		Shutdown:        shutdownResponse(snapshot.Shutdown),
		Instance:        instanceResponse(snapshot.Instance, instanceName),
		Projects:        projectsAPIResponse(snapshot),
		Refresh:         snapshot.Refresh,
		Events:          recentEventsFromTelemetry(snapshot.Events, nil, "", ""),
		Counts:          countsResponse(snapshot),
		Running:         runningEntries(snapshot.Running),
		Retrying:        retryEntries(snapshot.Queue),
		Blocked:         blockedEntries(snapshot.Blocked),
		Stats:           statsAPIResponse{Status: "enabled"},
		Board:           boardResponse(snapshot),
		CodexTotals:     totalsResponse(snapshot.Tokens),
		Throughput:      throughputResponse(snapshot.Throughput),
		LifetimeTotals:  lifetimeTotalsResponseFromTelemetry(snapshot.LifetimeTotals),
		WorkflowMetrics: snapshot.WorkflowMetrics,
		RecentSessions:  recentSessionEntries(snapshot.Completed),
		RateLimits:      snapshot.RateLimits,
		Budget:          budgetResponse(snapshot.Budget),
	}
}

func projectsAPIResponse(snapshot telemetry.Snapshot) []telemetry.ProjectSnapshot {
	if len(snapshot.Projects) > 0 {
		return append([]telemetry.ProjectSnapshot(nil), snapshot.Projects...)
	}
	if snapshot.Project == (telemetry.Project{}) {
		return nil
	}
	return []telemetry.ProjectSnapshot{
		{
			Project:    snapshot.Project,
			Counts:     snapshot.Counts,
			Tokens:     snapshot.Tokens,
			Throughput: snapshot.Throughput,
			Auth:       snapshot.Auth,
		},
	}
}

func runtimeStatus(snapshot telemetry.Snapshot) string {
	if snapshot.Shutdown.Draining {
		return "draining"
	}
	return "running"
}

func shutdownResponse(shutdown telemetry.Shutdown) shutdownAPIResponse {
	status := strings.TrimSpace(shutdown.Status)
	if status == "" {
		status = "running"
	}
	return shutdownAPIResponse{
		Status:            status,
		Draining:          shutdown.Draining,
		SessionsRemaining: shutdown.SessionsRemaining,
		RequestedAt:       timestampStringPtr(shutdown.RequestedAt),
		CompletedAt:       timestampStringPtr(shutdown.CompletedAt),
		Result:            optionalString(strings.TrimSpace(shutdown.Result)),
	}
}

func instanceResponse(instance telemetry.Instance, displayName string) instanceAPIResponse {
	return instanceAPIResponse{
		DisplayName:             strings.TrimSpace(displayName),
		Name:                    strings.TrimSpace(instance.Name),
		GitHubLogin:             strings.TrimSpace(instance.GitHubLogin),
		AuthorizationScope:      strings.TrimSpace(instance.AuthorizationScope),
		AuthorizationConfigured: instance.AuthorizationConfigured,
	}
}

func boardResponse(snapshot telemetry.Snapshot) boardAPIResponse {
	return boardAPIResponse{
		StateDistribution: telemetry.BoardStateCounts(snapshot),
		Flow:              telemetry.BoardProgressPoints(snapshot),
	}
}

func issueResponse(identifier string, snapshot telemetry.Snapshot) (issueAPIResponse, bool) {
	if running, ok := findRunning(identifier, snapshot.Running); ok {
		return issueAPIResponse{
			IssueIdentifier: running.Identifier,
			IssueID:         running.ID,
			Status:          "running",
			Workspace:       workspaceResponse(running.WorkspacePath, running.WorkerHost),
			Attempts:        attemptsAPIResponse{},
			Running:         runningIssueResponse(running),
			Retry:           nil,
			Blocked:         nil,
			Logs:            logsAPIResponse{CodexSessionLogs: []logAPIResponse{}},
			RecentEvents:    recentEventsFromTelemetry(running.RecentEvents, running.LastEventAt, running.LastEvent, running.LastMessage),
			LastError:       nil,
			Tracked:         map[string]any{},
		}, true
	}
	if retry, ok := findRetry(identifier, snapshot.Queue); ok {
		err := optionalString(retry.Error)
		return issueAPIResponse{
			IssueIdentifier: retry.Identifier,
			IssueID:         retry.ID,
			Status:          "retrying",
			Workspace:       workspaceResponse(retry.WorkspacePath, retry.WorkerHost),
			Attempts: attemptsAPIResponse{
				RestartCount:        max(retry.Attempt-1, 0),
				CurrentRetryAttempt: retry.Attempt,
			},
			Running:      nil,
			Retry:        retryIssueResponse(retry),
			Blocked:      nil,
			Logs:         logsAPIResponse{CodexSessionLogs: []logAPIResponse{}},
			RecentEvents: []recentEventAPIResponse{},
			LastError:    err,
			Tracked:      map[string]any{},
		}, true
	}
	if blocked, ok := findBlocked(identifier, snapshot.Blocked); ok {
		err := optionalString(blocked.Error)
		return issueAPIResponse{
			IssueIdentifier: blocked.Identifier,
			IssueID:         blocked.ID,
			Status:          "blocked",
			Workspace:       workspaceResponse(blocked.WorkspacePath, blocked.WorkerHost),
			Attempts:        attemptsAPIResponse{},
			Running:         nil,
			Retry:           nil,
			Blocked:         blockedIssueResponse(blocked),
			Logs:            logsAPIResponse{CodexSessionLogs: []logAPIResponse{}},
			RecentEvents:    recentEvents(blocked.LastEventAt, blocked.LastEvent, blocked.LastMessage),
			LastError:       err,
			Tracked:         map[string]any{},
		}, true
	}

	return issueAPIResponse{}, false
}

func countsResponse(snapshot telemetry.Snapshot) countsAPIResponse {
	return countsAPIResponse{
		Running:  len(snapshot.Running),
		Retrying: len(snapshot.Queue),
		Blocked:  len(snapshot.Blocked),
	}
}

func runningEntries(entries []telemetry.Running) []runningAPIResponse {
	payload := make([]runningAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, runningAPIResponse{
			IssueID:               entry.ID,
			IssueIdentifier:       entry.Identifier,
			ProjectID:             entry.ProjectID,
			IssueURL:              optionalString(entry.URL),
			IssueTitle:            optionalTrimmedString(entry.Title),
			IssueDescription:      issueDescription(entry.Description),
			PullRequestURL:        optionalString(pullRequestURL(entry.Issue)),
			PullRequestNumber:     pullRequestNumber(entry.Issue),
			BudgetAlert:           false,
			State:                 entry.State,
			WorkerHost:            optionalString(entry.WorkerHost),
			WorkspacePath:         optionalString(entry.WorkspacePath),
			SessionID:             optionalString(entry.SessionID),
			TurnCount:             entry.TurnCount,
			LastEvent:             optionalString(entry.LastEvent),
			LastMessage:           optionalString(entry.LastMessage),
			StartedAt:             timestampString(entry.StartedAt),
			LastEventAt:           timestampStringPtr(entry.LastEventAt),
			CurrentLaneEnteredAt:  timestampStringPtr(entry.CurrentLaneEnteredAt),
			CurrentLaneAgeSeconds: entry.CurrentLaneAgeSeconds,
			RecentEvents:          recentEventsFromTelemetry(entry.RecentEvents, entry.LastEventAt, entry.LastEvent, entry.LastMessage),
			DiffAdded:             entry.DiffAdded,
			DiffRemoved:           entry.DiffRemoved,
			DiffFiles:             entry.DiffFiles,
			DiffStatus:            diffStatus(entry.DiffStatus),
			Tokens:                tokenCountsResponse(entry.Tokens),
		})
	}
	return payload
}

func retryEntries(entries []telemetry.Queued) []retryAPIResponse {
	payload := make([]retryAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, retryAPIResponse{
			IssueID:               entry.ID,
			IssueIdentifier:       entry.Identifier,
			ProjectID:             entry.ProjectID,
			IssueURL:              optionalString(entry.URL),
			IssueTitle:            optionalTrimmedString(entry.Title),
			IssueDescription:      issueDescription(entry.Description),
			PullRequestURL:        optionalString(pullRequestURL(entry.Issue)),
			PullRequestNumber:     pullRequestNumber(entry.Issue),
			BudgetAlert:           false,
			Attempt:               entry.Attempt,
			DueAt:                 dueAtString(entry),
			Error:                 optionalString(entry.Error),
			WorkerHost:            optionalString(entry.WorkerHost),
			WorkspacePath:         optionalString(entry.WorkspacePath),
			CurrentLaneEnteredAt:  timestampStringPtr(entry.CurrentLaneEnteredAt),
			CurrentLaneAgeSeconds: entry.CurrentLaneAgeSeconds,
		})
	}
	return payload
}

func blockedEntries(entries []telemetry.Blocked) []blockedAPIResponse {
	payload := make([]blockedAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, blockedAPIResponse{
			IssueID:               entry.ID,
			IssueIdentifier:       entry.Identifier,
			ProjectID:             entry.ProjectID,
			IssueURL:              optionalString(entry.URL),
			IssueTitle:            optionalTrimmedString(entry.Title),
			IssueDescription:      issueDescription(entry.Description),
			PullRequestURL:        optionalString(pullRequestURL(entry.Issue)),
			PullRequestNumber:     pullRequestNumber(entry.Issue),
			BudgetAlert:           false,
			State:                 entry.State,
			Error:                 optionalString(entry.Error),
			RecoveryReason:        optionalString(entry.RecoveryReason),
			RecoveryTarget:        optionalString(entry.RecoveryTarget),
			WorkerHost:            optionalString(entry.WorkerHost),
			WorkspacePath:         optionalString(entry.WorkspacePath),
			SessionID:             optionalString(entry.SessionID),
			BlockedAt:             timestampStringPtr(entry.BlockedAt),
			LastEvent:             optionalString(entry.LastEvent),
			LastMessage:           optionalString(entry.LastMessage),
			LastEventAt:           timestampStringPtr(entry.LastEventAt),
			CurrentLaneEnteredAt:  timestampStringPtr(entry.CurrentLaneEnteredAt),
			CurrentLaneAgeSeconds: entry.CurrentLaneAgeSeconds,
		})
	}
	return payload
}

func recentSessionEntries(entries []telemetry.Completed) []recentSessionAPIResponse {
	payload := make([]recentSessionAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, recentSessionAPIResponse{
			IssueID:           entry.ID,
			Identifier:        entry.Identifier,
			ProjectID:         entry.ProjectID,
			IssueURL:          optionalString(entry.URL),
			PullRequestURL:    optionalString(pullRequestURL(entry.Issue)),
			PullRequestNumber: pullRequestNumber(entry.Issue),
			StartedAt:         timestampString(entry.StartedAt),
			CompletedAt:       timestampString(entry.CompletedAt),
			Turns:             entry.Turns,
			InputTokens:       entry.Tokens.Input,
			OutputTokens:      entry.Tokens.Output,
			TotalTokens:       entry.Tokens.Total,
			RuntimeSeconds:    entry.RuntimeSeconds,
			FinalState:        optionalString(entry.FinalState),
			Model:             optionalString(entry.Model),
			BudgetAlert:       false,
		})
	}
	return payload
}

func runningIssueResponse(entry telemetry.Running) *runningIssueAPIResponse {
	return &runningIssueAPIResponse{
		WorkerHost:    optionalString(entry.WorkerHost),
		WorkspacePath: optionalString(entry.WorkspacePath),
		SessionID:     optionalString(entry.SessionID),
		TurnCount:     entry.TurnCount,
		State:         entry.State,
		StartedAt:     timestampString(entry.StartedAt),
		LastEvent:     optionalString(entry.LastEvent),
		LastMessage:   optionalString(entry.LastMessage),
		LastEventAt:   timestampStringPtr(entry.LastEventAt),
		DiffAdded:     entry.DiffAdded,
		DiffRemoved:   entry.DiffRemoved,
		DiffFiles:     entry.DiffFiles,
		DiffStatus:    diffStatus(entry.DiffStatus),
		Tokens:        tokenCountsResponse(entry.Tokens),
	}
}

func retryIssueResponse(entry telemetry.Queued) *retryIssueAPIResponse {
	return &retryIssueAPIResponse{
		Attempt:       entry.Attempt,
		DueAt:         dueAtString(entry),
		Error:         optionalString(entry.Error),
		WorkerHost:    optionalString(entry.WorkerHost),
		WorkspacePath: optionalString(entry.WorkspacePath),
	}
}

func blockedIssueResponse(entry telemetry.Blocked) *blockedIssueAPIResponse {
	return &blockedIssueAPIResponse{
		WorkerHost:     optionalString(entry.WorkerHost),
		WorkspacePath:  optionalString(entry.WorkspacePath),
		SessionID:      optionalString(entry.SessionID),
		State:          entry.State,
		Error:          optionalString(entry.Error),
		RecoveryReason: optionalString(entry.RecoveryReason),
		RecoveryTarget: optionalString(entry.RecoveryTarget),
		BlockedAt:      timestampStringPtr(entry.BlockedAt),
		LastEvent:      optionalString(entry.LastEvent),
		LastMessage:    optionalString(entry.LastMessage),
		LastEventAt:    timestampStringPtr(entry.LastEventAt),
	}
}

func recentEvents(at *time.Time, event string, message string) []recentEventAPIResponse {
	timestamp := timestampStringPtr(at)
	if timestamp == nil {
		return []recentEventAPIResponse{}
	}
	return []recentEventAPIResponse{
		{
			At:      *timestamp,
			Event:   optionalString(event),
			Message: optionalString(message),
		},
	}
}

func recentEventsFromTelemetry(events []telemetry.ActivityEvent, fallbackAt *time.Time, fallbackEvent string, fallbackMessage string) []recentEventAPIResponse {
	if len(events) == 0 {
		return recentEvents(fallbackAt, fallbackEvent, fallbackMessage)
	}

	payload := make([]recentEventAPIResponse, 0, len(events))
	for _, event := range events {
		timestamp := timestampString(event.At)
		if timestamp == nil {
			continue
		}
		payload = append(payload, recentEventAPIResponse{
			At:      *timestamp,
			Event:   optionalString(event.Event),
			Message: optionalString(event.Message),
		})
	}
	return payload
}

func findRunning(identifier string, entries []telemetry.Running) (telemetry.Running, bool) {
	for _, entry := range entries {
		if issueMatches(entry.Issue, identifier) {
			return entry, true
		}
	}
	return telemetry.Running{}, false
}

func findRetry(identifier string, entries []telemetry.Queued) (telemetry.Queued, bool) {
	for _, entry := range entries {
		if issueMatches(entry.Issue, identifier) {
			return entry, true
		}
	}
	return telemetry.Queued{}, false
}

func findBlocked(identifier string, entries []telemetry.Blocked) (telemetry.Blocked, bool) {
	for _, entry := range entries {
		if issueMatches(entry.Issue, identifier) {
			return entry, true
		}
	}
	return telemetry.Blocked{}, false
}

func issueMatches(issue telemetry.Issue, value string) bool {
	return value != "" && (issue.Identifier == value || issue.ID == value)
}

func workspaceResponse(path string, host string) workspaceAPIResponse {
	return workspaceAPIResponse{
		Path: optionalString(path),
		Host: optionalString(host),
	}
}

func totalsResponse(tokens telemetry.Tokens) tokenTotalsAPIResponse {
	return tokenTotalsAPIResponse{
		Input:          tokens.Input,
		Output:         tokens.Output,
		Total:          tokens.Total,
		RuntimeSeconds: tokens.RuntimeSeconds,
	}
}

func tokenCountsResponse(tokens telemetry.Tokens) tokenCountsAPIResponse {
	return tokenCountsAPIResponse{
		Input:  tokens.Input,
		Output: tokens.Output,
		Total:  tokens.Total,
	}
}

func throughputResponse(throughput telemetry.TokenThroughput) throughputAPIResponse {
	return throughputAPIResponse{
		TokensPerSecond: throughput.TokensPerSecond,
		WindowSeconds:   throughput.WindowSeconds,
		Tokens:          throughput.Tokens,
	}
}

func lifetimeTotalsResponseFromTelemetry(totals telemetry.LifetimeTotals) lifetimeTotalsResponse {
	reason := totals.DegradedReason
	if !totals.Available && reason == "" {
		reason = "runtime store unavailable"
	}
	return lifetimeTotalsResponse{
		Available:      totals.Available,
		DegradedReason: reason,
		InputTokens:    totals.InputTokens,
		OutputTokens:   totals.OutputTokens,
		TotalTokens:    totals.TotalTokens,
		RuntimeSeconds: totals.RuntimeSeconds,
		Sessions:       totals.Sessions,
		Runs:           totals.Runs,
	}
}

func budgetResponse(budget telemetry.Budget) budgetAPIResponse {
	days := budget.Days
	if days == nil {
		days = []telemetry.BudgetDay{}
	}

	return budgetAPIResponse{
		Enabled:           budget.Enabled,
		DegradedReason:    budget.DegradedReason,
		TodaySpendUSD:     budget.CurrentSpendUSD,
		CurrentSpendUSD:   budget.CurrentSpendUSD,
		ProjectedCostUSD:  budget.ProjectedCostUSD,
		ProjectedSpendUSD: budget.ProjectedSpendUSD,
		PerDayMaxUSD:      budget.PerDayMaxUSD,
		PerIssueMaxUSD:    budget.PerIssueMaxUSD,
		PeriodStart:       optionalTime(budget.PeriodStart),
		PeriodEnd:         optionalTime(budget.PeriodEnd),
		SpendPoints:       budget.SpendPoints,
		Days:              days,
		Refusals:          budget.Refusals,
	}
}

func usageReportQuery(c echo.Context) (store.UsageReportQuery, *apiErrorResponse, int) {
	group, ok := usageReportGroup(c.QueryParam("by"))
	if !ok {
		response := errorResponse("invalid_usage_group", "by must be one of day, project, issue, pr, model")
		return store.UsageReportQuery{}, &response, http.StatusBadRequest
	}

	from, response, status := usageDate("from", c.QueryParam("from"))
	if response != nil {
		return store.UsageReportQuery{}, response, status
	}
	to, response, status := usageDate("to", c.QueryParam("to"))
	if response != nil {
		return store.UsageReportQuery{}, response, status
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		response := errorResponse("invalid_date_range", "from must be on or before to")
		return store.UsageReportQuery{}, &response, http.StatusBadRequest
	}

	return store.UsageReportQuery{
		By:   group,
		From: from,
		To:   to,
	}, nil, 0
}

func usageReportGroup(value string) (store.UsageReportGroup, bool) {
	switch strings.TrimSpace(value) {
	case "", string(store.UsageReportByDay):
		return store.UsageReportByDay, true
	case string(store.UsageReportByProject):
		return store.UsageReportByProject, true
	case string(store.UsageReportByIssue):
		return store.UsageReportByIssue, true
	case string(store.UsageReportByPR):
		return store.UsageReportByPR, true
	case string(store.UsageReportByModel):
		return store.UsageReportByModel, true
	default:
		return "", false
	}
}

func usageDate(name string, value string) (time.Time, *apiErrorResponse, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil, 0
	}

	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		response := errorResponse("invalid_date", name+" must use YYYY-MM-DD")
		return time.Time{}, &response, http.StatusBadRequest
	}
	return parsed, nil, 0
}

func timeSeriesQuery(c echo.Context) (time.Duration, time.Duration, *apiErrorResponse, int) {
	window, response, status := durationQueryParam(c.QueryParam("window"), defaultTimeSeriesWindow, time.Second, maxTimeSeriesWindow, "window")
	if response != nil {
		return 0, 0, response, status
	}
	bucket, response, status := durationQueryParam(c.QueryParam("bucket"), defaultTimeSeriesBucket, time.Second, window, "bucket")
	if response != nil {
		return 0, 0, response, status
	}
	window, bucket = cappedTimeSeriesWindow(window, bucket)
	return window, bucket, nil, 0
}

func durationQueryParam(value string, fallback time.Duration, minValue time.Duration, maxValue time.Duration, name string) (time.Duration, *apiErrorResponse, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil, 0
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		response := errorResponse("invalid_duration", name+" must be a duration such as 10m or 30s")
		return 0, &response, http.StatusBadRequest
	}
	if duration < minValue {
		response := errorResponse("invalid_duration", name+" is below the minimum duration")
		return 0, &response, http.StatusBadRequest
	}
	if duration > maxValue {
		return maxValue, nil, 0
	}
	return duration, nil, 0
}

func usageReportResponse(report store.UsageReport, pricing budget.PricingTable) usageReportAPIResponse {
	rows := usageBucketResponses(report.By, report.Rows, pricing)
	response := usageReportAPIResponse{
		By:         string(report.By),
		From:       optionalString(report.From),
		To:         optionalString(report.To),
		Totals:     usageTotalsResponse(report.Totals, pricing),
		Series:     []usageBucketAPIResponse{},
		Breakdowns: []usageBucketAPIResponse{},
	}
	if report.By == store.UsageReportByDay {
		response.Series = rows
		return response
	}

	response.Breakdowns = rows
	return response
}

func usageBucketResponses(group store.UsageReportGroup, rows []store.UsageReportRow, pricing budget.PricingTable) []usageBucketAPIResponse {
	payload := make([]usageBucketAPIResponse, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, usageBucketResponse(group, row, pricing))
	}
	return payload
}

func usageBucketResponse(group store.UsageReportGroup, row store.UsageReportRow, pricing budget.PricingTable) usageBucketAPIResponse {
	return usageBucketAPIResponse{
		Bucket:         row.Key,
		Label:          row.Key,
		Date:           usageBucketDate(group, row.Key),
		InputTokens:    row.InputTokens,
		OutputTokens:   row.OutputTokens,
		TotalTokens:    row.TotalTokens,
		RuntimeSeconds: row.RuntimeSeconds,
		Events:         row.Events,
		SpendUSD:       usageSpendUSD(row.Models, pricing),
		Models:         usageModelResponses(row.Models, pricing),
	}
}

func usageTotalsResponse(totals store.UsageReportTotals, pricing budget.PricingTable) usageTotalsAPIResponse {
	return usageTotalsAPIResponse{
		InputTokens:    totals.InputTokens,
		OutputTokens:   totals.OutputTokens,
		TotalTokens:    totals.TotalTokens,
		RuntimeSeconds: totals.RuntimeSeconds,
		Events:         totals.Events,
		SpendUSD:       usageSpendUSD(totals.Models, pricing),
		Models:         usageModelResponses(totals.Models, pricing),
	}
}

func usageModelResponses(models []store.UsageReportModel, pricing budget.PricingTable) []usageModelAPIResponse {
	payload := make([]usageModelAPIResponse, 0, len(models))
	for _, model := range models {
		payload = append(payload, usageModelAPIResponse{
			Model:          model.Model,
			InputTokens:    model.InputTokens,
			OutputTokens:   model.OutputTokens,
			TotalTokens:    model.TotalTokens,
			RuntimeSeconds: model.RuntimeSeconds,
			Events:         model.Events,
			SpendUSD:       usageSpendUSD([]store.UsageReportModel{model}, pricing),
		})
	}
	return payload
}

func usageSpendUSD(models []store.UsageReportModel, pricing budget.PricingTable) float64 {
	spend := store.TokenSpend{
		ByModel: make([]store.ModelTokenSpend, 0, len(models)),
	}
	for _, model := range models {
		spend.ByModel = append(spend.ByModel, store.ModelTokenSpend{
			Model:        model.Model,
			InputTokens:  model.InputTokens,
			OutputTokens: model.OutputTokens,
			TotalTokens:  model.TotalTokens,
			Sessions:     model.Events,
		})
	}
	return budget.SpendUSD(spend, pricing)
}

func usageBucketDate(group store.UsageReportGroup, key string) *string {
	if group != store.UsageReportByDay {
		return nil
	}
	return optionalString(key)
}

func dueAtString(entry telemetry.Queued) *string {
	if entry.DueAt != nil {
		return timestampStringPtr(entry.DueAt)
	}
	if entry.DueInMillis > 0 {
		dueAt := apiNow().Add(time.Duration(entry.DueInMillis) * time.Millisecond)
		return timestampString(dueAt)
	}
	return nil
}

func workflowTimelineResponse(timeline store.WorkflowTimeline) workflowTimelineAPIResponse {
	events := make([]workflowPhaseEventAPIResponse, 0, len(timeline.Events))
	for _, event := range timeline.Events {
		events = append(events, workflowPhaseEventAPIResponse{
			ID:                event.ID,
			ProjectID:         event.ProjectID,
			RunID:             event.RunID,
			SessionID:         event.SessionID,
			IssueID:           event.IssueID,
			Identifier:        event.Identifier,
			IssueURL:          event.IssueURL,
			PRNumber:          event.PRNumber,
			PhaseType:         string(event.PhaseType),
			PhaseName:         event.PhaseName,
			PreviousPhaseName: event.PreviousPhaseName,
			Reason:            event.Reason,
			Status:            event.Status,
			StartedAt:         event.StartedAt,
			FinishedAt:        optionalTime(event.FinishedAt),
			DurationSeconds:   event.DurationSeconds,
			CommandName:       event.CommandName,
			ExitCode:          event.ExitCode,
			Turns:             event.Turns,
			InputTokens:       event.InputTokens,
			OutputTokens:      event.OutputTokens,
			TotalTokens:       event.TotalTokens,
			EndpointFamily:    event.EndpointFamily,
		})
	}
	return workflowTimelineAPIResponse{Events: events}
}

func issueDescription(description string) *string {
	value := strings.TrimSpace(description)
	if value == "" {
		return nil
	}

	runes := []rune(value)
	if len(runes) <= issueDescriptionLimit {
		return &value
	}

	truncated := string(runes[:issueDescriptionLimit-3]) + "..."
	return &truncated
}

func optionalTrimmedString(value string) *string {
	value = strings.TrimSpace(value)
	return optionalString(value)
}

func diffStatus(value string) string {
	return strings.TrimSpace(value)
}

func pullRequestNumber(issue telemetry.Issue) *int {
	if issue.PullRequest == nil || issue.PullRequest.Number <= 0 {
		return nil
	}
	number := issue.PullRequest.Number
	return &number
}

func pullRequestURL(issue telemetry.Issue) string {
	if issue.PullRequest == nil {
		return ""
	}
	if prURL := strings.TrimSpace(issue.PullRequest.URL); prURL != "" {
		return prURL
	}
	if issue.PullRequest.Number <= 0 {
		return ""
	}
	baseURL := pullRequestRepositoryBaseURL(issue)
	if baseURL == "" {
		return ""
	}
	return baseURL + "/pull/" + strconv.Itoa(issue.PullRequest.Number)
}

func pullRequestRepositoryBaseURL(issue telemetry.Issue) string {
	if issue.PullRequest != nil {
		if baseURL := repositoryBaseURLFromRecordURL(issue.PullRequest.URL); baseURL != "" {
			return baseURL
		}
	}
	if baseURL := repositoryBaseURLFromRecordURL(issue.URL); baseURL != "" {
		return baseURL
	}
	if repository := issueRepository(issue.Identifier); repository != "" {
		return "https://github.com/" + repository
	}
	return ""
}

func repositoryBaseURLFromRecordURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if owner == "" || repo == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/" + owner + "/" + repo
}

func issueRepository(identifier string) string {
	repo, _, ok := strings.Cut(strings.TrimSpace(identifier), "#")
	if !ok {
		return ""
	}
	return strings.TrimSpace(repo)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func timestampString(value time.Time) *string {
	if value.IsZero() {
		return nil
	}

	formatted := value.UTC().Truncate(time.Second).Format(time.RFC3339)
	return &formatted
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func timestampStringPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return timestampString(*value)
}

func apiNow() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func generatedAt(snapshot telemetry.Snapshot, fallback time.Time) time.Time {
	if snapshot.GeneratedAt.IsZero() {
		return fallback
	}
	return snapshot.GeneratedAt.UTC().Truncate(time.Second)
}

func issueIdentifier(c echo.Context) string {
	if issue := c.Param("issue"); issue != "" {
		return issue
	}
	return c.Param("*")
}

func errorResponse(code string, message string) apiErrorResponse {
	return apiErrorResponse{
		Error: apiError{
			Code:    code,
			Message: message,
		},
	}
}

func snapshotErrorResponse(generatedAt time.Time, code string, message string) snapshotErrorAPIResponse {
	return snapshotErrorAPIResponse{
		GeneratedAt: generatedAt,
		Error: apiError{
			Code:    code,
			Message: message,
		},
	}
}

type stateAPIResponse struct {
	GeneratedAt     time.Time                   `json:"generated_at"`
	Status          string                      `json:"status"`
	Shutdown        shutdownAPIResponse         `json:"shutdown"`
	Instance        instanceAPIResponse         `json:"instance"`
	Projects        []telemetry.ProjectSnapshot `json:"projects,omitempty"`
	Refresh         telemetry.Refresh           `json:"refresh"`
	Events          []recentEventAPIResponse    `json:"events"`
	Counts          countsAPIResponse           `json:"counts"`
	Running         []runningAPIResponse        `json:"running"`
	Retrying        []retryAPIResponse          `json:"retrying"`
	Blocked         []blockedAPIResponse        `json:"blocked"`
	Stats           statsAPIResponse            `json:"stats"`
	Board           boardAPIResponse            `json:"board"`
	CodexTotals     tokenTotalsAPIResponse      `json:"codex_totals"`
	Throughput      throughputAPIResponse       `json:"throughput"`
	LifetimeTotals  lifetimeTotalsResponse      `json:"lifetime_totals"`
	WorkflowMetrics telemetry.WorkflowMetrics   `json:"workflow_metrics"`
	RecentSessions  []recentSessionAPIResponse  `json:"recent_sessions"`
	RateLimits      *telemetry.RateLimits       `json:"rate_limits"`
	Budget          budgetAPIResponse           `json:"budget"`
}

type shutdownAPIResponse struct {
	Status            string  `json:"status"`
	Draining          bool    `json:"draining"`
	SessionsRemaining int     `json:"sessions_remaining"`
	RequestedAt       *string `json:"requested_at,omitempty"`
	CompletedAt       *string `json:"completed_at,omitempty"`
	Result            *string `json:"result,omitempty"`
}

type instanceAPIResponse struct {
	DisplayName             string `json:"display_name,omitempty"`
	Name                    string `json:"name,omitempty"`
	GitHubLogin             string `json:"github_login,omitempty"`
	AuthorizationScope      string `json:"authorization_scope,omitempty"`
	AuthorizationConfigured bool   `json:"authorization_configured"`
}

type boardAPIResponse struct {
	StateDistribution []telemetry.BoardStateCount    `json:"state_distribution"`
	Flow              []telemetry.BoardProgressPoint `json:"flow"`
}

type countsAPIResponse struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
	Blocked  int `json:"blocked"`
}

type runningAPIResponse struct {
	IssueID               string                   `json:"issue_id"`
	IssueIdentifier       string                   `json:"issue_identifier"`
	ProjectID             string                   `json:"project_id,omitempty"`
	IssueURL              *string                  `json:"issue_url"`
	IssueTitle            *string                  `json:"issue_title"`
	IssueDescription      *string                  `json:"issue_description"`
	PullRequestURL        *string                  `json:"pull_request_url"`
	PullRequestNumber     *int                     `json:"pull_request_number"`
	BudgetAlert           bool                     `json:"budget_alert?"`
	State                 string                   `json:"state"`
	WorkerHost            *string                  `json:"worker_host"`
	WorkspacePath         *string                  `json:"workspace_path"`
	SessionID             *string                  `json:"session_id"`
	TurnCount             int                      `json:"turn_count"`
	LastEvent             *string                  `json:"last_event"`
	LastMessage           *string                  `json:"last_message"`
	StartedAt             *string                  `json:"started_at"`
	LastEventAt           *string                  `json:"last_event_at"`
	CurrentLaneEnteredAt  *string                  `json:"current_lane_entered_at"`
	CurrentLaneAgeSeconds int64                    `json:"current_lane_age_seconds"`
	RecentEvents          []recentEventAPIResponse `json:"recent_events"`
	DiffAdded             int                      `json:"diff_added"`
	DiffRemoved           int                      `json:"diff_removed"`
	DiffFiles             int                      `json:"diff_files"`
	DiffStatus            string                   `json:"diff_status"`
	Tokens                tokenCountsAPIResponse   `json:"tokens"`
}

type retryAPIResponse struct {
	IssueID               string  `json:"issue_id"`
	IssueIdentifier       string  `json:"issue_identifier"`
	ProjectID             string  `json:"project_id,omitempty"`
	IssueURL              *string `json:"issue_url"`
	IssueTitle            *string `json:"issue_title"`
	IssueDescription      *string `json:"issue_description"`
	PullRequestURL        *string `json:"pull_request_url"`
	PullRequestNumber     *int    `json:"pull_request_number"`
	BudgetAlert           bool    `json:"budget_alert?"`
	Attempt               int     `json:"attempt"`
	DueAt                 *string `json:"due_at"`
	Error                 *string `json:"error"`
	WorkerHost            *string `json:"worker_host"`
	WorkspacePath         *string `json:"workspace_path"`
	CurrentLaneEnteredAt  *string `json:"current_lane_entered_at"`
	CurrentLaneAgeSeconds int64   `json:"current_lane_age_seconds"`
}

type blockedAPIResponse struct {
	IssueID               string  `json:"issue_id"`
	IssueIdentifier       string  `json:"issue_identifier"`
	ProjectID             string  `json:"project_id,omitempty"`
	IssueURL              *string `json:"issue_url"`
	IssueTitle            *string `json:"issue_title"`
	IssueDescription      *string `json:"issue_description"`
	PullRequestURL        *string `json:"pull_request_url"`
	PullRequestNumber     *int    `json:"pull_request_number"`
	BudgetAlert           bool    `json:"budget_alert?"`
	State                 string  `json:"state"`
	Error                 *string `json:"error"`
	RecoveryReason        *string `json:"recovery_reason"`
	RecoveryTarget        *string `json:"recovery_target"`
	WorkerHost            *string `json:"worker_host"`
	WorkspacePath         *string `json:"workspace_path"`
	SessionID             *string `json:"session_id"`
	BlockedAt             *string `json:"blocked_at"`
	LastEvent             *string `json:"last_event"`
	LastMessage           *string `json:"last_message"`
	LastEventAt           *string `json:"last_event_at"`
	CurrentLaneEnteredAt  *string `json:"current_lane_entered_at"`
	CurrentLaneAgeSeconds int64   `json:"current_lane_age_seconds"`
}

type statsAPIResponse struct {
	Status string  `json:"status"`
	Reason *string `json:"reason"`
}

type tokenCountsAPIResponse struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

type tokenTotalsAPIResponse struct {
	Input          int64   `json:"input_tokens"`
	Output         int64   `json:"output_tokens"`
	Total          int64   `json:"total_tokens"`
	RuntimeSeconds float64 `json:"seconds_running"`
}

type throughputAPIResponse struct {
	TokensPerSecond float64 `json:"tokens_per_second"`
	WindowSeconds   int64   `json:"window_seconds"`
	Tokens          int64   `json:"tokens"`
}

type lifetimeTotalsResponse struct {
	Available      bool   `json:"available"`
	DegradedReason string `json:"degraded_reason,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	RuntimeSeconds int64  `json:"runtime_seconds"`
	Sessions       int64  `json:"sessions"`
	Runs           int64  `json:"runs"`
}

type recentSessionAPIResponse struct {
	IssueID           string  `json:"issue_id"`
	Identifier        string  `json:"identifier"`
	ProjectID         string  `json:"project_id,omitempty"`
	IssueURL          *string `json:"issue_url"`
	PullRequestURL    *string `json:"pull_request_url"`
	PullRequestNumber *int    `json:"pull_request_number"`
	StartedAt         *string `json:"started_at"`
	CompletedAt       *string `json:"completed_at"`
	Turns             int     `json:"turns"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	RuntimeSeconds    float64 `json:"runtime_seconds"`
	FinalState        *string `json:"final_state"`
	Model             *string `json:"model"`
	BudgetAlert       bool    `json:"budget_alert?"`
}

type budgetAPIResponse struct {
	Enabled           bool                         `json:"enabled"`
	DegradedReason    string                       `json:"degraded_reason,omitempty"`
	TodaySpendUSD     float64                      `json:"today_spend_usd"`
	CurrentSpendUSD   float64                      `json:"current_spend_usd"`
	ProjectedCostUSD  float64                      `json:"projected_cost_usd"`
	ProjectedSpendUSD float64                      `json:"projected_spend_usd,omitempty"`
	PerDayMaxUSD      *float64                     `json:"per_day_max_usd"`
	PerIssueMaxUSD    *float64                     `json:"per_issue_max_usd"`
	PeriodStart       *time.Time                   `json:"period_start,omitempty"`
	PeriodEnd         *time.Time                   `json:"period_end,omitempty"`
	SpendPoints       []telemetry.BudgetSpendPoint `json:"spend_points,omitempty"`
	Days              []telemetry.BudgetDay        `json:"days"`
	Refusals          []telemetry.BudgetRefusal    `json:"refusals,omitempty"`
}

type usageReportAPIResponse struct {
	By         string                   `json:"by"`
	From       *string                  `json:"from"`
	To         *string                  `json:"to"`
	Totals     usageTotalsAPIResponse   `json:"totals"`
	Series     []usageBucketAPIResponse `json:"series"`
	Breakdowns []usageBucketAPIResponse `json:"breakdowns"`
}

type usageTotalsAPIResponse struct {
	InputTokens    int64                   `json:"input_tokens"`
	OutputTokens   int64                   `json:"output_tokens"`
	TotalTokens    int64                   `json:"total_tokens"`
	RuntimeSeconds int64                   `json:"runtime_seconds"`
	Events         int64                   `json:"events"`
	SpendUSD       float64                 `json:"spend_usd"`
	Models         []usageModelAPIResponse `json:"models"`
}

type usageBucketAPIResponse struct {
	Bucket         string                  `json:"bucket"`
	Label          string                  `json:"label"`
	Date           *string                 `json:"date"`
	InputTokens    int64                   `json:"input_tokens"`
	OutputTokens   int64                   `json:"output_tokens"`
	TotalTokens    int64                   `json:"total_tokens"`
	RuntimeSeconds int64                   `json:"runtime_seconds"`
	Events         int64                   `json:"events"`
	SpendUSD       float64                 `json:"spend_usd"`
	Models         []usageModelAPIResponse `json:"models"`
}

type usageModelAPIResponse struct {
	Model          string  `json:"model"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	RuntimeSeconds int64   `json:"runtime_seconds"`
	Events         int64   `json:"events"`
	SpendUSD       float64 `json:"spend_usd"`
}

type workflowTimelineAPIResponse struct {
	Events []workflowPhaseEventAPIResponse `json:"events"`
}

type workflowPhaseEventAPIResponse struct {
	ID                int64      `json:"id"`
	ProjectID         string     `json:"project_id"`
	RunID             int64      `json:"run_id,omitempty"`
	SessionID         int64      `json:"session_id,omitempty"`
	IssueID           string     `json:"issue_id,omitempty"`
	Identifier        string     `json:"identifier,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	PRNumber          *int64     `json:"pr_number,omitempty"`
	PhaseType         string     `json:"phase_type"`
	PhaseName         string     `json:"phase_name"`
	PreviousPhaseName string     `json:"previous_phase_name,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	Status            string     `json:"status,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	DurationSeconds   int64      `json:"duration_seconds"`
	CommandName       string     `json:"command_name,omitempty"`
	ExitCode          *int64     `json:"exit_code,omitempty"`
	Turns             int64      `json:"turns,omitempty"`
	InputTokens       int64      `json:"input_tokens,omitempty"`
	OutputTokens      int64      `json:"output_tokens,omitempty"`
	TotalTokens       int64      `json:"total_tokens,omitempty"`
	EndpointFamily    string     `json:"endpoint_family,omitempty"`
}

type issueAPIResponse struct {
	IssueIdentifier string                   `json:"issue_identifier"`
	IssueID         string                   `json:"issue_id"`
	Status          string                   `json:"status"`
	Workspace       workspaceAPIResponse     `json:"workspace"`
	Attempts        attemptsAPIResponse      `json:"attempts"`
	Running         *runningIssueAPIResponse `json:"running"`
	Retry           *retryIssueAPIResponse   `json:"retry"`
	Blocked         *blockedIssueAPIResponse `json:"blocked"`
	Logs            logsAPIResponse          `json:"logs"`
	RecentEvents    []recentEventAPIResponse `json:"recent_events"`
	LastError       *string                  `json:"last_error"`
	Tracked         map[string]any           `json:"tracked"`
}

type workspaceAPIResponse struct {
	Path *string `json:"path"`
	Host *string `json:"host"`
}

type attemptsAPIResponse struct {
	RestartCount        int `json:"restart_count"`
	CurrentRetryAttempt int `json:"current_retry_attempt"`
}

type runningIssueAPIResponse struct {
	WorkerHost    *string                `json:"worker_host"`
	WorkspacePath *string                `json:"workspace_path"`
	SessionID     *string                `json:"session_id"`
	TurnCount     int                    `json:"turn_count"`
	State         string                 `json:"state"`
	StartedAt     *string                `json:"started_at"`
	LastEvent     *string                `json:"last_event"`
	LastMessage   *string                `json:"last_message"`
	LastEventAt   *string                `json:"last_event_at"`
	DiffAdded     int                    `json:"diff_added"`
	DiffRemoved   int                    `json:"diff_removed"`
	DiffFiles     int                    `json:"diff_files"`
	DiffStatus    string                 `json:"diff_status"`
	Tokens        tokenCountsAPIResponse `json:"tokens"`
}

type retryIssueAPIResponse struct {
	Attempt       int     `json:"attempt"`
	DueAt         *string `json:"due_at"`
	Error         *string `json:"error"`
	WorkerHost    *string `json:"worker_host"`
	WorkspacePath *string `json:"workspace_path"`
}

type blockedIssueAPIResponse struct {
	WorkerHost     *string `json:"worker_host"`
	WorkspacePath  *string `json:"workspace_path"`
	SessionID      *string `json:"session_id"`
	State          string  `json:"state"`
	Error          *string `json:"error"`
	RecoveryReason *string `json:"recovery_reason"`
	RecoveryTarget *string `json:"recovery_target"`
	BlockedAt      *string `json:"blocked_at"`
	LastEvent      *string `json:"last_event"`
	LastMessage    *string `json:"last_message"`
	LastEventAt    *string `json:"last_event_at"`
}

type logsAPIResponse struct {
	CodexSessionLogs []logAPIResponse `json:"codex_session_logs"`
}

type logAPIResponse struct {
	Label string  `json:"label"`
	Path  string  `json:"path"`
	URL   *string `json:"url"`
}

type recentEventAPIResponse struct {
	At      string  `json:"at"`
	Event   *string `json:"event"`
	Message *string `json:"message"`
}

type apiErrorResponse struct {
	Error apiError `json:"error"`
}

type snapshotErrorAPIResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	Error       apiError  `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
