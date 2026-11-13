package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
)

func (s *Service) registerRoutes(e *echo.Echo) {
	e.Pre(s.timeWriterUse)
	e.Pre(s.serverTiming)
	e.JSONSerializer = timedJSONSerializer{}
	if s.config.CredentialMaintenance {
		s.registerCredentialMaintenanceRoutes(e)
		return
	}
	s.registerPostgresRoutes(e)
}

func (s *Service) registerPostgresRoutes(e *echo.Echo) {
	e.POST("/api/v1/webhooks/github", s.githubWebhook)
	e.GET("/api/v2/organizations/:organization/secrets/status", s.hostedSecretStatus, s.hostedSecretStatusAuthority)
	if s.config.Hosted != nil {
		if s.config.Hosted.SharedEntry != nil {
			e.Pre(s.hostedSharedEntry)
		}
		e.Use(s.hostedBoundary)
	}
	e.GET("/health", s.health, s.requireAPIScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	s.registerNativeRoutes(e)
	s.registerArtifactCatalogRoutes(e)
	s.registerProjectSecretRoutes(e)
	s.registerSpritePoolRoutes(e)
	s.registerRunnerRoutes(e)
	s.registerCostUsageRoutes(e)
	s.registerConversationRoutes(e)
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		s.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{s}, chat.WithClock(s.config.now))
		s.registerHostedSharedRoutes(e)
	}
	s.registerTeamThreadRoutes(e)
	s.registerOperatorTools(e)
	s.registerPostgresAppRoutes(e)
}

func (s *Service) postgresFeatureUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, hubapi.APIErrorResponse{Code: "storage_owner_unavailable", Message: "This Hub feature is unavailable with PostgreSQL storage"})
}

func (s *Service) invalidAPIRequest(c echo.Context, err error) error {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return s.refusedAPIError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large", hubapi.APIErrorResponse{Code: "payload_too_large", Message: "Request body is too large"})
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
	return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_request", reason, hubapi.APIErrorResponse{Code: "invalid_request", Message: message})
}

func (s *Service) internalAPIError(c echo.Context, code string, message string, err error) error {
	attributes := append([]any{"error", err}, s.database.readTimeoutAttributes(err)...)
	attributes = append(attributes, postgresErrorAttributes(err)...)
	s.config.Logger.Error(message, attributes...)
	return c.JSON(http.StatusInternalServerError, hubapi.APIErrorResponse{Code: code, Message: message})
}

func postgresErrorAttributes(err error) []any {
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError == nil {
		return nil
	}
	attributes := make([]any, 0, 8)
	state := string(postgresError.Code)
	validState := len(state) == 5
	for _, char := range state {
		if (char < '0' || char > '9') && (char < 'A' || char > 'Z') {
			validState = false
		}
	}
	if validState {
		attributes = append(attributes, "sqlstate", state)
	}
	if postgresError.SchemaName != "public" && postgresError.SchemaName != "platform" {
		return attributes
	}
	attributes = append(attributes, "schema", postgresError.SchemaName)
	if ordinaryPostgresIdentifier(postgresError.TableName) {
		attributes = append(attributes, "table", postgresError.TableName)
	}
	if ordinaryPostgresIdentifier(postgresError.ColumnName) {
		attributes = append(attributes, "column", postgresError.ColumnName)
	}
	return attributes
}

func ordinaryPostgresIdentifier(identifier string) bool {
	if len(identifier) == 0 || len(identifier) > 63 {
		return false
	}
	for index := range len(identifier) {
		char := identifier[index]
		if char != '_' && (char < 'a' || char > 'z') && (index == 0 || char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func (s *Service) refusedAPIError(c echo.Context, status int, code, reason string, response any, attributes ...any) error {
	if status >= http.StatusInternalServerError {
		scope := hubscope.NativeRequestScope(c)
		s.config.Logger.Error("API request unavailable", "error", code, "code", code, "refusal_code", code, "status", status,
			"method", c.Request().Method, "route", c.Path(), "organization_id", string(scope.Organization), "project_id", string(scope.Project))
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError && status != http.StatusUnauthorized && (status != http.StatusNotFound || slices.Contains(attributes, any("not_found_step")) || slices.Contains(attributes, any("not_found_source")) && (strings.HasSuffix(c.Path(), "/events") || strings.Contains(c.Path(), "/leases/"))) {
		scope := hubscope.NativeRequestScope(c)
		attributes = append([]any{"error", reason, "code", code, "refusal_code", code, "reason", reason,
			"method", c.Request().Method, "route", c.Path(), "organization_id", string(scope.Organization), "project_id", string(scope.Project)}, attributes...)
		s.config.Logger.Warn("API request refused", attributes...)
	}
	if response == nil {
		return c.NoContent(status)
	}
	return c.JSON(status, response)
}

func apiTTL(seconds int64) (time.Duration, error) {
	if seconds <= 0 || seconds > int64((24*time.Hour)/time.Second) {
		return 0, errors.New("ttl_seconds must be between 1 and 86400")
	}
	return time.Duration(seconds) * time.Second, nil
}

func (s *Service) outboxHealth(c echo.Context) error {
	limit, err := hubapi.ParsePageLimit(c.QueryParam("limit"))
	if err != nil {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_query", "limit must be between 1 and 200", hubapi.APIErrorResponse{Code: "invalid_query", Message: "limit must be between 1 and 200"})
	}
	cursor, err := decodeTimelineCursor(c.QueryParam("cursor"))
	if err != nil {
		return s.refusedAPIError(c, http.StatusUnprocessableEntity, "invalid_cursor", "Outbox cursor is invalid", hubapi.APIErrorResponse{Code: "invalid_cursor", Message: "Outbox cursor is invalid"})
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
