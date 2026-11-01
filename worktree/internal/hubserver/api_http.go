package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const maxAPIRequestBodyBytes = 1 << 20

const maxChangeVersionRequestBytes = 28 << 20

// maxAttemptDiffRequestBytes bounds the one endpoint whose body is legitimately
// larger than every other: a stored attempt diff carries the patches
// themselves, and the attempt diff contract bounds those at tracker.MaxDiffBytes.
// The allowance is that bound at the worst case of JSON string escaping plus
// room for the framing, so a diff the contract accepts is never refused by the
// transport instead. The factor is six because an encoder writes <, >, & and
// every control byte as a six-byte \uXXXX sequence, and a patch of HTML or
// generated code can be dense with them.
const maxAttemptDiffRequestBytes = 6*tracker.MaxDiffBytes + (1 << 20)

// maxActionRunReportBytes bounds a runner's action run report, which carries
// the run's whole output up to workspacesession.MaxExecOutputBytes, with the
// same six-fold escaping allowance: coloured build output is dense with ESC.
const maxActionRunReportBytes = 6*workspacesession.MaxExecOutputBytes + (1 << 20)

func (s *Service) registerRoutes(e *echo.Echo) {
	e.Pre(s.timeWriterUse)
	e.Pre(s.serverTiming)
	e.JSONSerializer = timedJSONSerializer{}
	if s.config.CredentialMaintenance {
		s.registerCredentialMaintenanceRoutes(e)
		return
	}
	if s.config.Hosted != nil {
		if s.hostedShared() {
			e.Pre(s.hostedSharedEntry)
			s.registerHostedSharedRoutes(e)
		}
		e.Use(s.hostedBoundary)
		s.registerHostedRoutes(e)
	} else {
		s.registerLocalAppRoutes(e)
	}
	s.registerOperatorTools(e)
	s.registerTeamThreadRoutes(e)
	s.registerNativeRoutes(e)
	s.registerProjectSecretRoutes(e)
	s.registerSpritePoolRoutes(e)
	s.registerCostUsageRoutes(e)
	s.registerRunnerRoutes(e)
	s.registerConversationRoutes(e)
	read := s.requireAPIScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin)
	worker := s.requireAPIScope(apiScopeWorker)
	operator := s.requireAPIScope(apiScopeOperator)
	admin := s.requireAPIScope(apiScopeAdmin)
	if s.config.Hosted == nil {
		e.POST("/api/v2/admin/backup", s.backupDatabase, s.requireInstanceAdmin())
	}
	e.GET("/api/v1/repositories/:owner/:repo/policy", s.getProjectPolicy, read)
	e.PUT("/api/v1/repositories/:owner/:repo/policy", s.approveProjectPolicy, s.requireInstanceAdmin())
	e.DELETE("/api/v1/repositories/:owner/:repo/policy", s.revokeProjectPolicy, s.requireInstanceAdmin())

	e.GET("/health", s.health, read)
	e.GET("/api/v1/work-items", s.listWorkItems, read)
	e.GET("/api/v1/work-items/:id", s.getWorkItem, read)
	e.POST("/api/v1/claims", s.claimWorkItem, worker)
	e.POST("/api/v1/leases/:id/renew", s.renewLease, worker)
	e.POST("/api/v1/leases/:id/release", s.releaseLease, worker)
	e.POST("/api/v1/work-items/:id/events", s.appendWorkItemEvent, worker)
	e.POST("/api/v1/work-items/:id/workflow", s.changeWorkItemWorkflow, operator)
	e.POST("/api/v1/work-items/:id/dependencies", s.changeWorkItemDependency, operator)
	e.POST("/api/v1/work-items/:id/priority", s.changeWorkItemPriority, operator)
	e.POST("/api/v1/work-items/:id/order", s.changeWorkItemOrder, operator)
	e.POST("/api/v1/machines/register", s.registerMachine, worker)
	e.POST("/api/v1/machines/:id/heartbeat", s.heartbeatMachine, worker)
	e.GET("/api/v1/repositories/freshness", s.repositoryFreshness, read)
	e.GET("/api/v1/outbox/health", s.outboxHealth, read)
	e.POST("/api/v1/tokens", s.createAPIToken, admin)
	e.POST("/api/v1/tokens/:id/rotate", s.rotateAPIToken, admin)
	e.DELETE("/api/v1/tokens/:id", s.revokeAPIToken, admin)
	e.POST("/api/v1/webhooks/github", s.githubWebhook)
}

func decodeAPIJSON(c echo.Context, target any) error {
	if c == nil || c.Request() == nil {
		return errors.New("request is required")
	}
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, apiRequestBodyLimit(c))
	decoder := json.NewDecoder(request.Body)
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Scope != apiScopeWorker || !strings.HasSuffix(c.Path(), "/heartbeat") {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

// apiRequestBodyLimit is how many bytes this route's body may carry.
func apiRequestBodyLimit(c echo.Context) int64 {
	if c.Path() == nativeBase+"/conversations/:conversation/actions" {
		return 4 * coordinatorWorkflowArgumentBytes
	}
	if limit := SharedEntryRequestBodyLimit(c.Request().Method, c.Path()); limit > cloudassert.MaxBodyBytes {
		return limit
	}
	if c.Path() == nativeBase+"/work-items/:item/source-intake" || c.Path() == nativeBase+"/onboarding/issue-intake/result" {
		return 64 << 20
	}
	if c.Path() == nativeBase+"/workspaces/:workspace/worker/action-runs" {
		return maxActionRunReportBytes
	}
	return maxAPIRequestBodyBytes
}

func SharedEntryRequestBodyLimit(method, path string) int64 {
	if method != http.MethodPost {
		return cloudassert.MaxBodyBytes
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 2 && parts[0] == "organizations" {
		parts = parts[2:]
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return cloudassert.MaxBodyBytes
		}
	}
	if len(parts) < 6 || parts[0] != "api" || parts[1] != "v2" || parts[2] != "organizations" || parts[4] != "projects" {
		return cloudassert.MaxBodyBytes
	}
	if len(parts) == 9 && parts[6] == "work-items" && parts[8] == "events" {
		return 8 * maxAPIRequestBodyBytes
	}
	if len(parts) == 13 && parts[6] == "work-items" && parts[8] == "changes" && parts[10] == "versions" && parts[12] == "landing" {
		return maxChangeVersionRequestBytes
	}
	if len(parts) == 11 && parts[6] == "work-items" && parts[8] == "changes" && parts[10] == "versions" {
		return maxChangeVersionRequestBytes
	}
	if (len(parts) == 9 && parts[8] == "diff" || len(parts) == 10 && parts[8] == "diff" && parts[9] == "check") && parts[6] == "attempts" {
		return maxAttemptDiffRequestBytes
	}
	return cloudassert.MaxBodyBytes
}

func (s *Service) invalidAPIRequest(c echo.Context, err error) error {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return s.refusedAPIError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large", apiErrorResponse{Code: "payload_too_large", Message: "Request body is too large"})
	}
	message, reason := "Request body is invalid", "Malformed JSON request body"
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		message += ": " + err.Error()
		reason = "Request body contains an unknown field"
	} else {
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) && typeError.Field != "" {
			message += ": invalid type for field " + typeError.Field
			reason = "Request body contains an invalid field type"
		}
	}
	return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_request", reason, apiErrorResponse{Code: "invalid_request", Message: message})
}

func (s *Service) internalAPIError(c echo.Context, code string, message string, err error) error {
	s.config.Logger.Error(message, append([]any{"error", err}, s.database.readTimeoutAttributes(err)...)...)
	return c.JSON(http.StatusInternalServerError, apiErrorResponse{Code: code, Message: message})
}

func (s *Service) refusedAPIError(c echo.Context, status int, code, reason string, response any, attributes ...any) error {
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError && status != http.StatusUnauthorized && (status != http.StatusNotFound || slices.Contains(attributes, any("not_found_step")) || slices.Contains(attributes, any("not_found_source")) && (strings.HasSuffix(c.Path(), "/events") || strings.Contains(c.Path(), "/leases/"))) {
		scope := nativeRequestScope(c)
		attributes = append([]any{"error", reason, "code", code, "refusal_code", code, "reason", reason,
			"method", c.Request().Method, "route", c.Path(), "organization_id", string(scope.organization), "project_id", string(scope.project)}, attributes...)
		s.config.Logger.Warn("API request refused", attributes...)
	}
	if response == nil {
		return c.NoContent(status)
	}
	return c.JSON(status, response)
}

func apiWorkItemID(c echo.Context) (tracker.WorkItemID, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%w: %q", tracker.ErrInvalidWorkItemID, c.Param("id"))
	}
	return tracker.WorkItemID(value), nil
}

func apiLeaseID(c echo.Context) (tracker.LeaseID, error) {
	value := strings.TrimSpace(c.Param("id"))
	if value == "" {
		return "", tracker.ErrLeaseNotFound
	}
	return tracker.LeaseID(value), nil
}

func apiTTL(seconds int64) (time.Duration, error) {
	if seconds <= 0 || seconds > int64((24*time.Hour)/time.Second) {
		return 0, errors.New("ttl_seconds must be between 1 and 86400")
	}
	return time.Duration(seconds) * time.Second, nil
}

func (s *Service) outboxHealth(c echo.Context) error {
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_query", "limit must be between 1 and 200", apiErrorResponse{Code: "invalid_query", Message: "limit must be between 1 and 200"})
	}
	cursor, err := decodeTimelineCursor(c.QueryParam("cursor"))
	if err != nil {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_cursor", "Outbox cursor is invalid", apiErrorResponse{Code: "invalid_cursor", Message: "Outbox cursor is invalid"})
	}
	response, err := s.readOutboxHealthPage(c.Request().Context(), limit, cursor)
	if err != nil {
		return s.internalAPIError(c, "outbox_health_unavailable", "Outbox health could not be read", err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) readOutboxHealthPage(ctx context.Context, limit int, cursor timelineCursor) (outboxHealthResponse, error) {
	health, err := s.OutboxHealth(ctx)
	if err != nil {
		return outboxHealthResponse{}, err
	}
	actions := health.OperatorActions[:0]
	for _, action := range health.OperatorActions {
		if action.ID > cursor.ID {
			actions = append(actions, action)
		}
	}
	health.OperatorActions = actions
	response := outboxHealthResponse{OutboxHealth: health}
	if len(response.OperatorActions) > limit {
		response.OperatorActions = response.OperatorActions[:limit]
		response.NextCursor, err = encodeTimelineCursor(timelineCursor{Version: 1, ID: response.OperatorActions[len(response.OperatorActions)-1].ID})
		if err != nil {
			return outboxHealthResponse{}, err
		}
	}
	return response, nil
}

type outboxHealthResponse struct {
	OutboxHealth
	NextCursor string `json:"next_cursor,omitempty"`
}
