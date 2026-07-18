package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/demofixtures"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type RunStopper interface {
	StopRun(context.Context, orchestrator.StopRunRequest) (orchestrator.StopRunResult, error)
}

const stopRunAcceptedTrigger = `{"kanbanActionSucceeded":{"message":"Stop accepted; board routing is continuing in the background."}}`

type stopRunRequestPayload struct {
	IssueID           string `json:"issue_id" form:"issue_id"`
	WorkAttemptID     int64  `json:"work_attempt_id" form:"work_attempt_id"`
	DetentSessionID   int64  `json:"detent_session_id" form:"detent_session_id"`
	ProviderSessionID string `json:"provider_session_id" form:"provider_session_id"`
	Destination       string `json:"destination" form:"destination"`
	Priority          int    `json:"priority" form:"priority"`
	Reason            string `json:"reason" form:"reason"`
	Confirm           bool   `json:"confirm" form:"confirm"`
}

func (s *Server) apiStopRunDialog(c echo.Context) error {
	attempt, err := stopRunAttemptParam(c)
	if err != nil {
		return stopRunErrorResponse(c, http.StatusBadRequest, "invalid_run_identity", "attempt must be a non-negative integer", templates.StopRunDialogData{})
	}
	request, err := stopRunRequestFromQuery(c, attempt)
	if err != nil {
		return stopRunErrorResponse(c, http.StatusBadRequest, "invalid_run_identity", err.Error(), templates.StopRunDialogData{})
	}
	snapshot := s.latestSnapshot(c.Request().Context())
	if scenario, demoStop, scenarioErr := s.stopRunDemoScenario(c); scenarioErr != nil {
		return scenarioErr
	} else if demoStop {
		snapshot = demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant)
	}
	running, ok := activeRunForStop(snapshot, request)
	if !ok {
		if blocked, retryOK := stoppedRunForTransitionRetry(snapshot, request); retryOK {
			data := stopRunDialogDataFromBlocked(blocked, request.ProjectID)
			data.Error = stopRunTransitionRetryMessage(blocked)
			return render(c, templates.StopRunDialogContent(data))
		}
		data := stopRunDialogDataFromRequest(request)
		data.Outcome = "stale"
		data.Error = "The selected run is no longer active or its identity changed. The work item was not moved."
		return stopRunErrorResponse(c, http.StatusConflict, "stale_run", data.Error, data)
	}
	data := stopRunDialogData(running)
	if s.runStopper == nil {
		data.Error = "The orchestrator is unavailable, so this run cannot be stopped from the dashboard."
		data.CanSubmit = false
	}
	return render(c, templates.StopRunDialogContent(data))
}

func (s *Server) apiStopRun(c echo.Context) error {
	if s.runStopper == nil {
		return stopRunErrorResponse(c, http.StatusServiceUnavailable, "orchestrator_unavailable", "Orchestrator is unavailable", templates.StopRunDialogData{})
	}
	attempt, err := stopRunAttemptParam(c)
	if err != nil {
		return stopRunErrorResponse(c, http.StatusBadRequest, "invalid_run_identity", "attempt must be a non-negative integer", templates.StopRunDialogData{})
	}
	payload, err := stopRunPayload(c)
	if err != nil {
		return stopRunErrorResponse(c, http.StatusUnprocessableEntity, "invalid_request", "Request body must be valid JSON or form data", templates.StopRunDialogData{})
	}
	request := orchestrator.StopRunRequest{
		ProjectID:         strings.TrimSpace(c.Param("project_id")),
		IssueID:           strings.TrimSpace(payload.IssueID),
		Attempt:           attempt,
		WorkAttemptID:     payload.WorkAttemptID,
		DetentSessionID:   payload.DetentSessionID,
		ProviderSessionID: strings.TrimSpace(payload.ProviderSessionID),
		Destination:       strings.TrimSpace(payload.Destination),
		Priority:          payload.Priority,
		Reason:            strings.TrimSpace(payload.Reason),
	}
	scenario, demoStop, scenarioErr := s.stopRunDemoScenario(c)
	if scenarioErr != nil {
		return scenarioErr
	}
	snapshot := s.latestSnapshot(c.Request().Context())
	if demoStop {
		snapshot = demofixtures.SnapshotForScenario(scenario.ProjectID, scenario.Variant)
	}
	data := stopRunDialogDataFromRequest(request)
	running, runningOK := activeRunForStop(snapshot, request)
	if runningOK {
		data = stopRunDialogData(running)
	}
	if !payload.Confirm {
		data.Error = "Confirm this operation with confirm=true."
		data.CanSubmit = true
		return stopRunErrorResponse(c, http.StatusPreconditionRequired, "confirmation_required", data.Error, data)
	}
	var result orchestrator.StopRunResult
	if demoStop {
		if !runningOK {
			return stopRunErrorResponse(c, http.StatusConflict, "stale_run", "The selected run is no longer active or its identity changed. The work item was not moved.", data)
		}
		result, err = demoStopRunResult(request, running)
	} else {
		result, err = s.runStopper.StopRun(c.Request().Context(), request)
	}
	if err != nil {
		status, code, message := stopRunAPIError(err, result)
		data.Outcome = result.Outcome
		data.Error = message
		data.RetryTransition = errors.Is(err, orchestrator.ErrStopRunTransition)
		data.CanSubmit = data.RetryTransition
		if result.Destination != "" {
			data.Destination = result.Destination
		}
		data.Priority = result.Priority
		data.PriorityName = result.PriorityName
		data.Reason = result.Reason
		return stopRunErrorResponse(c, status, code, message, data)
	}
	data.Outcome = result.Outcome
	data.Destination = result.Destination
	data.Priority = result.Priority
	data.PriorityName = result.PriorityName
	data.Reason = result.Reason
	data.CanSubmit = false
	if htmxRequest(c) {
		trigger := kanbanDialogSucceeded
		if result.Outcome == "pending" {
			trigger = stopRunAcceptedTrigger
		}
		c.Response().Header().Set("HX-Trigger", trigger)
		return render(c, templates.StopRunDialogContent(data))
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) stopRunDemoScenario(c echo.Context) (demoScenario, bool, error) {
	scenario, ok, err := s.demoScenarioOrError(c)
	return scenario, ok && scenario.Variant == "stop-run-picker", err
}

func demoStopRunResult(request orchestrator.StopRunRequest, running telemetry.Running) (orchestrator.StopRunResult, error) {
	destination := strings.TrimSpace(request.Destination)
	validDestination := destination == orchestrator.StopRunDestinationBlocked || destination == orchestrator.StopRunDestinationBacklog || destination == orchestrator.StopRunDestinationCancelled || destination == orchestrator.StopRunDestinationTodo
	if !validDestination || utf8.RuneCountInString(request.Reason) > orchestrator.StopRunReasonMaxLength || destination == orchestrator.StopRunDestinationTodo && (request.Priority < 1 || request.Priority > 4) {
		return orchestrator.StopRunResult{}, orchestrator.ErrStopRunInvalidRoute
	}
	priorityName := ""
	if destination == orchestrator.StopRunDestinationTodo {
		for _, option := range running.StopPriorityOptions {
			if option.Rank == request.Priority {
				priorityName = option.Name
				break
			}
		}
		if priorityName == "" {
			return orchestrator.StopRunResult{}, orchestrator.ErrStopRunInvalidRoute
		}
	}
	now := time.Now().UTC()
	return orchestrator.StopRunResult{ProjectID: request.ProjectID, IssueID: request.IssueID, Identifier: running.Identifier, Attempt: request.Attempt, WorkAttemptID: request.WorkAttemptID, DetentSessionID: request.DetentSessionID, ProviderSessionID: request.ProviderSessionID, Destination: destination, Priority: request.Priority, PriorityName: priorityName, Reason: request.Reason, Outcome: "pending", RequestedAt: now}, nil
}

func stopRunAttemptParam(c echo.Context) (int, error) {
	attempt, err := strconv.Atoi(strings.TrimSpace(c.Param("attempt")))
	if err != nil || attempt < 0 {
		return 0, errors.New("invalid attempt")
	}
	return attempt, nil
}

func stopRunRequestFromQuery(c echo.Context, attempt int) (orchestrator.StopRunRequest, error) {
	workAttemptID, err := optionalRunIdentity(c.QueryParam("work_attempt_id"))
	if err != nil {
		return orchestrator.StopRunRequest{}, errors.New("work_attempt_id must be a positive integer")
	}
	detentSessionID, err := optionalRunIdentity(c.QueryParam("detent_session_id"))
	if err != nil {
		return orchestrator.StopRunRequest{}, errors.New("detent_session_id must be a positive integer")
	}
	return orchestrator.StopRunRequest{
		ProjectID:         strings.TrimSpace(c.Param("project_id")),
		IssueID:           strings.TrimSpace(c.QueryParam("issue_id")),
		Attempt:           attempt,
		WorkAttemptID:     workAttemptID,
		DetentSessionID:   detentSessionID,
		ProviderSessionID: strings.TrimSpace(c.QueryParam("provider_session_id")),
	}, nil
}

func optionalRunIdentity(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	identity, err := strconv.ParseInt(value, 10, 64)
	if err != nil || identity <= 0 {
		return 0, errors.New("invalid identity")
	}
	return identity, nil
}

func stopRunPayload(c echo.Context) (stopRunRequestPayload, error) {
	var payload stopRunRequestPayload
	if err := c.Bind(&payload); err != nil {
		return stopRunRequestPayload{}, err
	}
	if raw := strings.TrimSpace(c.FormValue("confirm")); raw != "" {
		confirmed, err := strconv.ParseBool(raw)
		if err != nil {
			return stopRunRequestPayload{}, err
		}
		payload.Confirm = confirmed
	}
	payload.IssueID = strings.TrimSpace(payload.IssueID)
	payload.ProviderSessionID = strings.TrimSpace(payload.ProviderSessionID)
	payload.Destination = strings.TrimSpace(payload.Destination)
	payload.Reason = strings.TrimSpace(payload.Reason)
	return payload, nil
}

func activeRunForStop(snapshot telemetry.Snapshot, request orchestrator.StopRunRequest) (telemetry.Running, bool) {
	for _, running := range snapshot.Running {
		if request.ProjectID != "" && running.ProjectID != "" && request.ProjectID != running.ProjectID {
			continue
		}
		if running.ID != request.IssueID || running.Attempt != request.Attempt {
			continue
		}
		if request.WorkAttemptID > 0 && request.WorkAttemptID != running.WorkAttemptID {
			continue
		}
		if request.DetentSessionID > 0 && request.DetentSessionID != running.DetentSessionID {
			continue
		}
		if request.ProviderSessionID != "" && request.ProviderSessionID != running.SessionID {
			continue
		}
		return running, true
	}
	return telemetry.Running{}, false
}

func stopRunDialogData(running telemetry.Running) templates.StopRunDialogData {
	repository, _ := splitRunIdentifier(running.Identifier)
	if repository == "" {
		repository = running.ProjectID
	}
	identifier := strings.TrimSpace(running.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(running.ID)
	}
	role := strings.TrimSpace(running.RuntimeIdentity.Role)
	if role == "" {
		role = "default"
	}
	destination := strings.TrimSpace(running.StopDestination)
	if destination == "" {
		destination = "Blocked"
	}
	return templates.StopRunDialogData{
		ProjectID:         running.ProjectID,
		Repository:        repository,
		IssueID:           running.ID,
		Identifier:        identifier,
		IssueURL:          running.URL,
		Title:             running.Title,
		Role:              role,
		Stage:             running.State,
		Destination:       destination,
		Priority:          firstStopRunPriority(running.StopPriorityOptions),
		PriorityOptions:   stopRunPriorityOptionsForDialog(running.StopPriorityOptions),
		Attempt:           running.Attempt,
		WorkAttemptID:     running.WorkAttemptID,
		DetentSessionID:   running.DetentSessionID,
		ProviderSessionID: running.SessionID,
		CanSubmit:         true,
	}
}

func stopRunDialogDataFromRequest(request orchestrator.StopRunRequest) templates.StopRunDialogData {
	return templates.StopRunDialogData{
		ProjectID:         request.ProjectID,
		Repository:        request.ProjectID,
		IssueID:           request.IssueID,
		Identifier:        request.IssueID,
		Role:              "unknown",
		Stage:             "unknown",
		Destination:       "Blocked",
		Priority:          1,
		PriorityOptions:   stopRunPriorityOptionsForDialog(nil),
		Attempt:           request.Attempt,
		WorkAttemptID:     request.WorkAttemptID,
		DetentSessionID:   request.DetentSessionID,
		ProviderSessionID: request.ProviderSessionID,
	}
}

func stoppedRunForTransitionRetry(snapshot telemetry.Snapshot, request orchestrator.StopRunRequest) (telemetry.Blocked, bool) {
	for _, blocked := range snapshot.Blocked {
		if blocked.Source != telemetry.BlockedSourceOperatorStop || !strings.EqualFold(strings.TrimSpace(blocked.RecoveryReason), "transition_failed") && !strings.Contains(strings.ToLower(strings.TrimSpace(blocked.Error)), "retry the transition") {
			continue
		}
		if request.ProjectID != "" && blocked.ProjectID != "" && request.ProjectID != blocked.ProjectID {
			continue
		}
		if blocked.ID != request.IssueID || blocked.Attempt != request.Attempt {
			continue
		}
		if request.WorkAttemptID > 0 && request.WorkAttemptID != blocked.WorkAttemptID {
			continue
		}
		if request.DetentSessionID > 0 && request.DetentSessionID != blocked.DetentSessionID {
			continue
		}
		if request.ProviderSessionID != "" && request.ProviderSessionID != blocked.SessionID {
			continue
		}
		return blocked, true
	}
	return telemetry.Blocked{}, false
}

func stopRunDialogDataFromBlocked(blocked telemetry.Blocked, fallbackProjectID string) templates.StopRunDialogData {
	projectID := strings.TrimSpace(blocked.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(fallbackProjectID)
	}
	repository, _ := splitRunIdentifier(blocked.Identifier)
	if repository == "" {
		repository = projectID
	}
	identifier := strings.TrimSpace(blocked.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(blocked.ID)
	}
	priority := blocked.Priority
	if priority == 0 {
		priority = 1
	}
	return templates.StopRunDialogData{
		ProjectID:         projectID,
		Repository:        repository,
		IssueID:           blocked.ID,
		Identifier:        identifier,
		IssueURL:          blocked.URL,
		Title:             blocked.Title,
		Role:              "stopped",
		Stage:             "Run stopped",
		Destination:       blocked.Destination,
		Priority:          priority,
		PriorityName:      blocked.PriorityName,
		PriorityOptions:   stopRunPriorityOptionsForDialog(nil),
		Reason:            blocked.StopReason,
		Attempt:           blocked.Attempt,
		WorkAttemptID:     blocked.WorkAttemptID,
		DetentSessionID:   blocked.DetentSessionID,
		ProviderSessionID: blocked.SessionID,
		Outcome:           "transition_failed",
		CanSubmit:         true,
		RetryTransition:   true,
	}
}

func stopRunTransitionRetryMessage(blocked telemetry.Blocked) string {
	destination := strings.TrimSpace(blocked.Destination)
	if destination == "" {
		destination = "the configured hold state"
	}
	message := "The run stopped, but moving the work item to " + destination + " failed. Redispatch remains suppressed; correct the tracker error and retry this operation."
	if detail := strings.TrimSpace(blocked.Error); detail != "" {
		message += " " + detail
	}
	return message
}

func splitRunIdentifier(identifier string) (string, string) {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 || index == len(identifier)-1 {
		return "", identifier
	}
	return identifier[:index], identifier[index:]
}

func stopRunAPIError(err error, result orchestrator.StopRunResult) (int, string, string) {
	switch {
	case errors.Is(err, orchestrator.ErrStopRunInvalidIdentity):
		return http.StatusBadRequest, "invalid_run_identity", "The selected run identity is invalid."
	case errors.Is(err, orchestrator.ErrStopRunInvalidRoute):
		return http.StatusUnprocessableEntity, "invalid_stop_destination", "Choose Blocked, Backlog, Cancelled, or Todo with an available priority. Reasons must be 280 characters or fewer."
	case errors.Is(err, orchestrator.ErrStopRunStale):
		return http.StatusConflict, "stale_run", "The selected run is no longer active or its identity changed. The work item was not moved."
	case errors.Is(err, orchestrator.ErrStopRunTransition):
		destination := strings.TrimSpace(result.Destination)
		if destination == "" {
			destination = "the configured hold state"
		}
		return http.StatusBadGateway, "tracker_transition_failed", "The run stopped, but moving the work item to " + destination + " failed. Redispatch remains suppressed; correct the tracker error and retry this operation. " + err.Error()
	case errors.Is(err, project.ErrProjectNotFound), errors.Is(err, project.ErrProjectStopped), errors.Is(err, project.ErrNotRunning), errors.Is(err, project.ErrMissingOrchestrator), errors.Is(err, orchestrator.ErrStopped):
		return http.StatusServiceUnavailable, "orchestrator_unavailable", "Orchestrator is unavailable"
	default:
		return http.StatusInternalServerError, "stop_run_failed", "Stopping the selected run failed: " + err.Error()
	}
}

func stopRunPriorityOptionsForDialog(options []telemetry.StopRunPriorityOption) []telemetry.StopRunPriorityOption {
	if len(options) == 0 {
		return []telemetry.StopRunPriorityOption{{Rank: 1, Name: "Urgent"}, {Rank: 2, Name: "High"}, {Rank: 3, Name: "Medium"}, {Rank: 4, Name: "Low"}}
	}
	return append([]telemetry.StopRunPriorityOption(nil), options...)
}

func firstStopRunPriority(options []telemetry.StopRunPriorityOption) int {
	options = stopRunPriorityOptionsForDialog(options)
	if len(options) == 0 {
		return 0
	}
	return options[0].Rank
}

func stopRunErrorResponse(c echo.Context, status int, code string, message string, data templates.StopRunDialogData) error {
	if htmxRequest(c) {
		data.Error = message
		return render(c, templates.StopRunDialogContent(data))
	}
	return c.JSON(status, errorResponse(code, message))
}
