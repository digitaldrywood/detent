package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/symphony-go/internal/orchestrator"
	"github.com/digitaldrywood/symphony-go/internal/telemetry"
)

const issueDescriptionLimit = 250

type Refresher interface {
	RequestRefresh(context.Context) (RefreshResponse, error)
}

type RefreshResponse = orchestrator.RefreshResponse

func (s *Server) apiState(c echo.Context) error {
	generatedAt := apiNow()
	snapshot, ok := s.hub.Latest()
	if !ok {
		return c.JSON(http.StatusOK, snapshotErrorResponse(generatedAt, "snapshot_unavailable", "Snapshot unavailable"))
	}

	return c.JSON(http.StatusOK, stateResponse(snapshot, generatedAt))
}

func (s *Server) apiIssue(c echo.Context) error {
	snapshot, ok := s.hub.Latest()
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	}

	payload, ok := issueResponse(c.Param("issue"), snapshot)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	}

	return c.JSON(http.StatusOK, payload)
}

func (s *Server) apiRefresh(c echo.Context) error {
	if s.refresher == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}

	payload, err := s.refresher.RequestRefresh(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("orchestrator_unavailable", "Orchestrator is unavailable"))
	}
	if payload.RequestedAt.IsZero() {
		payload.RequestedAt = apiNow()
	}
	if payload.Operations == nil {
		payload.Operations = []string{}
	}

	return c.JSON(http.StatusAccepted, payload)
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

func stateResponse(snapshot telemetry.Snapshot, generatedAt time.Time) stateAPIResponse {
	return stateAPIResponse{
		GeneratedAt:    generatedAt,
		Counts:         countsResponse(snapshot),
		Running:        runningEntries(snapshot.Running),
		Retrying:       retryEntries(snapshot.Queue),
		Blocked:        blockedEntries(snapshot.Blocked),
		Stats:          statsAPIResponse{Status: "enabled"},
		CodexTotals:    totalsResponse(snapshot.Tokens),
		LifetimeTotals: lifetimeTotalsResponse{},
		RecentSessions: recentSessionEntries(snapshot.Completed),
		RateLimits:     snapshot.RateLimits,
		Budget:         budgetResponse(snapshot.Budget),
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
			RecentEvents:    recentEvents(running.LastEventAt, running.LastEvent, running.LastMessage),
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
			IssueID:          entry.ID,
			IssueIdentifier:  entry.Identifier,
			IssueURL:         optionalString(entry.URL),
			IssueTitle:       optionalTrimmedString(entry.Title),
			IssueDescription: issueDescription(entry.Description),
			BudgetAlert:      false,
			State:            entry.State,
			WorkerHost:       optionalString(entry.WorkerHost),
			WorkspacePath:    optionalString(entry.WorkspacePath),
			SessionID:        optionalString(entry.SessionID),
			TurnCount:        entry.TurnCount,
			LastEvent:        optionalString(entry.LastEvent),
			LastMessage:      optionalString(entry.LastMessage),
			StartedAt:        timestampString(entry.StartedAt),
			LastEventAt:      timestampStringPtr(entry.LastEventAt),
			Tokens:           tokenCountsResponse(entry.Tokens),
		})
	}
	return payload
}

func retryEntries(entries []telemetry.Queued) []retryAPIResponse {
	payload := make([]retryAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, retryAPIResponse{
			IssueID:          entry.ID,
			IssueIdentifier:  entry.Identifier,
			IssueURL:         optionalString(entry.URL),
			IssueTitle:       optionalTrimmedString(entry.Title),
			IssueDescription: issueDescription(entry.Description),
			BudgetAlert:      false,
			Attempt:          entry.Attempt,
			DueAt:            dueAtString(entry),
			Error:            optionalString(entry.Error),
			WorkerHost:       optionalString(entry.WorkerHost),
			WorkspacePath:    optionalString(entry.WorkspacePath),
		})
	}
	return payload
}

func blockedEntries(entries []telemetry.Blocked) []blockedAPIResponse {
	payload := make([]blockedAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, blockedAPIResponse{
			IssueID:          entry.ID,
			IssueIdentifier:  entry.Identifier,
			IssueURL:         optionalString(entry.URL),
			IssueTitle:       optionalTrimmedString(entry.Title),
			IssueDescription: issueDescription(entry.Description),
			BudgetAlert:      false,
			State:            entry.State,
			Error:            optionalString(entry.Error),
			WorkerHost:       optionalString(entry.WorkerHost),
			WorkspacePath:    optionalString(entry.WorkspacePath),
			SessionID:        optionalString(entry.SessionID),
			BlockedAt:        timestampStringPtr(entry.BlockedAt),
			LastEvent:        optionalString(entry.LastEvent),
			LastMessage:      optionalString(entry.LastMessage),
			LastEventAt:      timestampStringPtr(entry.LastEventAt),
		})
	}
	return payload
}

func recentSessionEntries(entries []telemetry.Completed) []recentSessionAPIResponse {
	payload := make([]recentSessionAPIResponse, 0, len(entries))
	for _, entry := range entries {
		payload = append(payload, recentSessionAPIResponse{
			IssueID:        entry.ID,
			Identifier:     entry.Identifier,
			IssueURL:       optionalString(entry.URL),
			StartedAt:      timestampString(entry.StartedAt),
			CompletedAt:    timestampString(entry.CompletedAt),
			Turns:          entry.Turns,
			InputTokens:    entry.Tokens.Input,
			OutputTokens:   entry.Tokens.Output,
			TotalTokens:    entry.Tokens.Total,
			RuntimeSeconds: entry.RuntimeSeconds,
			FinalState:     optionalString(entry.FinalState),
			Model:          optionalString(entry.Model),
			BudgetAlert:    false,
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
		WorkerHost:    optionalString(entry.WorkerHost),
		WorkspacePath: optionalString(entry.WorkspacePath),
		SessionID:     optionalString(entry.SessionID),
		State:         entry.State,
		Error:         optionalString(entry.Error),
		BlockedAt:     timestampStringPtr(entry.BlockedAt),
		LastEvent:     optionalString(entry.LastEvent),
		LastMessage:   optionalString(entry.LastMessage),
		LastEventAt:   timestampStringPtr(entry.LastEventAt),
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

func budgetResponse(budget telemetry.Budget) budgetAPIResponse {
	return budgetAPIResponse{
		Enabled:          budget.Enabled,
		TodaySpendUSD:    budget.CurrentSpendUSD,
		CurrentSpendUSD:  budget.CurrentSpendUSD,
		ProjectedCostUSD: budget.ProjectedCostUSD,
		PerDayMaxUSD:     budget.PerDayMaxUSD,
		PerIssueMaxUSD:   budget.PerIssueMaxUSD,
		Refusals:         budget.Refusals,
	}
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

func timestampStringPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return timestampString(*value)
}

func apiNow() time.Time {
	return time.Now().UTC().Truncate(time.Second)
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
	GeneratedAt    time.Time                  `json:"generated_at"`
	Counts         countsAPIResponse          `json:"counts"`
	Running        []runningAPIResponse       `json:"running"`
	Retrying       []retryAPIResponse         `json:"retrying"`
	Blocked        []blockedAPIResponse       `json:"blocked"`
	Stats          statsAPIResponse           `json:"stats"`
	CodexTotals    tokenTotalsAPIResponse     `json:"codex_totals"`
	LifetimeTotals lifetimeTotalsResponse     `json:"lifetime_totals"`
	RecentSessions []recentSessionAPIResponse `json:"recent_sessions"`
	RateLimits     *telemetry.RateLimits      `json:"rate_limits"`
	Budget         budgetAPIResponse          `json:"budget"`
}

type countsAPIResponse struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
	Blocked  int `json:"blocked"`
}

type runningAPIResponse struct {
	IssueID          string                 `json:"issue_id"`
	IssueIdentifier  string                 `json:"issue_identifier"`
	IssueURL         *string                `json:"issue_url"`
	IssueTitle       *string                `json:"issue_title"`
	IssueDescription *string                `json:"issue_description"`
	BudgetAlert      bool                   `json:"budget_alert?"`
	State            string                 `json:"state"`
	WorkerHost       *string                `json:"worker_host"`
	WorkspacePath    *string                `json:"workspace_path"`
	SessionID        *string                `json:"session_id"`
	TurnCount        int                    `json:"turn_count"`
	LastEvent        *string                `json:"last_event"`
	LastMessage      *string                `json:"last_message"`
	StartedAt        *string                `json:"started_at"`
	LastEventAt      *string                `json:"last_event_at"`
	Tokens           tokenCountsAPIResponse `json:"tokens"`
}

type retryAPIResponse struct {
	IssueID          string  `json:"issue_id"`
	IssueIdentifier  string  `json:"issue_identifier"`
	IssueURL         *string `json:"issue_url"`
	IssueTitle       *string `json:"issue_title"`
	IssueDescription *string `json:"issue_description"`
	BudgetAlert      bool    `json:"budget_alert?"`
	Attempt          int     `json:"attempt"`
	DueAt            *string `json:"due_at"`
	Error            *string `json:"error"`
	WorkerHost       *string `json:"worker_host"`
	WorkspacePath    *string `json:"workspace_path"`
}

type blockedAPIResponse struct {
	IssueID          string  `json:"issue_id"`
	IssueIdentifier  string  `json:"issue_identifier"`
	IssueURL         *string `json:"issue_url"`
	IssueTitle       *string `json:"issue_title"`
	IssueDescription *string `json:"issue_description"`
	BudgetAlert      bool    `json:"budget_alert?"`
	State            string  `json:"state"`
	Error            *string `json:"error"`
	WorkerHost       *string `json:"worker_host"`
	WorkspacePath    *string `json:"workspace_path"`
	SessionID        *string `json:"session_id"`
	BlockedAt        *string `json:"blocked_at"`
	LastEvent        *string `json:"last_event"`
	LastMessage      *string `json:"last_message"`
	LastEventAt      *string `json:"last_event_at"`
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

type lifetimeTotalsResponse struct {
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	TotalTokens    int64 `json:"total_tokens"`
	RuntimeSeconds int64 `json:"runtime_seconds"`
	Sessions       int64 `json:"sessions"`
	Runs           int64 `json:"runs"`
}

type recentSessionAPIResponse struct {
	IssueID        string  `json:"issue_id"`
	Identifier     string  `json:"identifier"`
	IssueURL       *string `json:"issue_url"`
	StartedAt      *string `json:"started_at"`
	CompletedAt    *string `json:"completed_at"`
	Turns          int     `json:"turns"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	RuntimeSeconds float64 `json:"runtime_seconds"`
	FinalState     *string `json:"final_state"`
	Model          *string `json:"model"`
	BudgetAlert    bool    `json:"budget_alert?"`
}

type budgetAPIResponse struct {
	Enabled          bool                      `json:"enabled"`
	TodaySpendUSD    float64                   `json:"today_spend_usd"`
	CurrentSpendUSD  float64                   `json:"current_spend_usd"`
	ProjectedCostUSD float64                   `json:"projected_cost_usd"`
	PerDayMaxUSD     *float64                  `json:"per_day_max_usd"`
	PerIssueMaxUSD   *float64                  `json:"per_issue_max_usd"`
	Refusals         []telemetry.BudgetRefusal `json:"refusals,omitempty"`
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
	WorkerHost    *string `json:"worker_host"`
	WorkspacePath *string `json:"workspace_path"`
	SessionID     *string `json:"session_id"`
	State         string  `json:"state"`
	Error         *string `json:"error"`
	BlockedAt     *string `json:"blocked_at"`
	LastEvent     *string `json:"last_event"`
	LastMessage   *string `json:"last_message"`
	LastEventAt   *string `json:"last_event_at"`
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
