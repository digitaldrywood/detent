package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const maxAPIRequestBodyBytes = 1 << 20

// maxAttemptDiffRequestBytes bounds the one endpoint whose body is legitimately
// larger than every other: a stored attempt diff carries the patches
// themselves, and decisions section 18.5 bounds those at tracker.MaxDiffBytes.
// The allowance is that bound with room for the JSON framing and the escaping
// a patch full of quotes and newlines costs, so a diff the contract accepts is
// never refused by the transport instead.
const maxAttemptDiffRequestBytes = 2*tracker.MaxDiffBytes + (1 << 20)

// maxActionRunReportBytes bounds the action run report for the same reason,
// one contract later: a report carries the run's whole output, and decisions
// section 18.12 bounds that at workspacesession.MaxExecOutputBytes. The generic
// limit is exactly that cap, so a run that produced the most output the
// contract allows would be refused by the transport rather than stored.
//
// The factor is six because that is the worst case of JSON string escaping, not
// a guess with headroom: a byte the encoder cannot write literally becomes
// \uXXXX, six bytes for one. ESC (0x1b) is the byte that makes this real rather
// than theoretical -- every ANSI colour sequence is full of them, coloured build
// output is exactly what a project action produces, and ESC is valid UTF-8 so it
// travels as text rather than base64. At any smaller factor an escape-dense run
// that finished would have its report refused, and the run that finished would
// never be recorded: it would sit running, which is the one outcome section
// 18.12 exists to prevent. Do not reduce it.
const maxActionRunReportBytes = 6*workspacesession.MaxExecOutputBytes + (1 << 20)

func (s *Service) registerRoutes(e *echo.Echo) {
	if s.config.CredentialMaintenance {
		s.registerCredentialMaintenanceRoutes(e)
		return
	}
	if s.config.Hosted != nil {
		e.Use(s.hostedBoundary)
		s.registerHostedRoutes(e)
	}
	s.registerNativeRoutes(e)
	s.registerRunnerRoutes(e)
	s.registerConversationRoutes(e)
	read := s.requireAPIScope(apiScopeWorker, apiScopeOperator, apiScopeAdmin)
	worker := s.requireAPIScope(apiScopeWorker)
	operator := s.requireAPIScope(apiScopeOperator)
	admin := s.requireAPIScope(apiScopeAdmin)
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
	decoder.DisallowUnknownFields()
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
	if c.Path() == nativeBase+"/attempts/:attempt/diff" {
		return maxAttemptDiffRequestBytes
	}
	if c.Path() == nativeBase+"/workspaces/:workspace/worker/action-runs" {
		return maxActionRunReportBytes
	}
	return maxAPIRequestBodyBytes
}

func invalidAPIRequest(c echo.Context, err error) error {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return c.JSON(http.StatusRequestEntityTooLarge, apiErrorResponse{Code: "payload_too_large", Message: "Request body is too large"})
	}
	return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_request", Message: "Request body is invalid"})
}

func (s *Service) internalAPIError(c echo.Context, code string, message string, err error) error {
	s.config.Logger.Error(message, "error", err)
	return c.JSON(http.StatusInternalServerError, apiErrorResponse{Code: code, Message: message})
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
	health, err := s.OutboxHealth(c.Request().Context())
	if err != nil {
		return s.internalAPIError(c, "outbox_health_unavailable", "Outbox health could not be read", err)
	}
	limit, err := parsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_query", Message: "limit must be between 1 and 200"})
	}
	cursor, err := decodeTimelineCursor(c.QueryParam("cursor"))
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, apiErrorResponse{Code: "invalid_cursor", Message: "Outbox cursor is invalid"})
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
			return s.internalAPIError(c, "outbox_health_unavailable", "Outbox health could not be read", err)
		}
	}
	return c.JSON(http.StatusOK, response)
}

type outboxHealthResponse struct {
	OutboxHealth
	NextCursor string `json:"next_cursor,omitempty"`
}
